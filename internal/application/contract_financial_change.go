package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// 受控“合同财务变更”（BIZ-XEVT2-05 合同侧第一波）。
//
// 背景：合同审批通过后，金额与付款计划此前没有任何修订通道，结算侧因此只能
// 收到同一合同第一笔财务事件；后续合法改额既无法产生新版本事件，又会把结算
// 永久钉在旧口径上。本用例只开放一条受控写路径，且默认关闭：
//
//  1. 开关 CONTRACT_FINANCIAL_CHANGE_ENABLED 必须显式为真（失败关闭）；
//  2. 必须具备 contract.edit 权限及其数据范围（沿用既有权限机制，不新增权限码）；
//  3. 合同必须已经过既有审批状态机并处于可变更的生效阶段
//     （approved/active/in_progress/pending_pay），且在途审批为空；
//  4. 变更必须在同一事务内完成 version+1、替换付款计划、写审计与
//     新版本财务事件（contract.financial_effective.v1，幂等键仍按版本）。
//
// 本用例**不**新增审批协议或工作流：变更级审批需要结算侧修订工单（第二波）与
// 产品决策，当前以“合同已审批 + 无在途审批 + 权限 + 审计”作为安全前置条件。
// 终态（completed/terminated/archived）合同视为财务事实已封账，不允许再改。

var (
	// ErrFinancialChangeDisabled 表示受控财务变更入口未启用（默认状态）。
	ErrFinancialChangeDisabled = errors.New("contract financial change is disabled")
	// ErrFinancialChangeApprovalRequired 表示合同没有可证明的有效审批，
	// 或仍有在途审批，因而不能做财务变更（失败关闭）。
	ErrFinancialChangeApprovalRequired = errors.New("contract financial change requires an approved contract without in-flight approval")
	// ErrFinancialChangeCurrencyLocked 表示试图改变已审批合同的币种。
	// 币种变更属于换合同语义，不在本受控入口范围内。
	ErrFinancialChangeCurrencyLocked = errors.New("contract financial change cannot change currency")
	// ErrFinancialChangeEventNotQueued 表示变更后的财务事件未能入队。
	// 该错误必须回滚整个变更，绝不能出现“合同已改额但结算收不到事件”。
	ErrFinancialChangeEventNotQueued = errors.New("contract financial change did not enqueue a financial effective event")
)

// MaxFinancialChangeReasonRunes 是变更原因的最大长度（与 lifecycle.reason 列一致）。
const MaxFinancialChangeReasonRunes = 500

// ContractFinancialChange 是一次合同财务变更的输入。
// 金额使用分（与合同存储一致），付款计划金额使用十进制字符串（与结算契约一致）。
type ContractFinancialChange struct {
	Pricing         *contract.Pricing `json:"pricing,omitempty"`
	ContractID      string
	ExpectedVersion uint64
	AmountMinor     int64
	Currency        string
	PaymentTerms    []contract.PaymentTerm
	Reason          string
}

// ContractFinancialChangeRepository 由仓储层实现：在单事务内完成版本跃迁、
// 付款计划替换、审计与财务事件入队。
type ContractFinancialChangeRepository interface {
	ApplyContractFinancialChange(context.Context, string, ContractFinancialChange, string) (contract.Contract, error)
}

// FinancialChangeEligibleStatuses 返回允许受控财务变更的合同状态。
// 只包含“已审批且未进入财务终态”的阶段：approved（已批准待生效）、
// active（生效中）、in_progress、pending_pay。completed/terminated/archived
// 视为已封账，不允许通过本入口改写金额或计划。
func FinancialChangeEligibleStatuses() []contract.Status {
	return []contract.Status{
		contract.StatusApproved,
		contract.StatusActive,
		contract.StatusInProgress,
		contract.StatusPendingPay,
	}
}

// FinancialChangeStatusEligible 判断单个状态是否允许受控财务变更。
func FinancialChangeStatusEligible(status contract.Status) bool {
	for _, candidate := range FinancialChangeEligibleStatuses() {
		if candidate == status {
			return true
		}
	}
	return false
}

// ChangeContractFinancialTerms 是受控合同财务变更用例。
//
// 参数：ctx 为请求上下文；actor 为已认证主体；contractID 为目标合同；
// change 为新金额、币种、付款计划、并发版本与业务原因。
// 返回值：变更后的合同；开关关闭、权限/数据范围不足、审批前置不满足、
// 版本冲突、计划不合法或存储失败时返回错误且不保留任何部分写入。
func (s *Service) ChangeContractFinancialTerms(ctx context.Context, actor Principal, contractID string, change ContractFinancialChange) (contract.Contract, error) {
	if !s.ContractFinancialChangeEnabled {
		// 开关必须在任何读取之前判定：默认关闭时连合同是否存在都不应暴露。
		return contract.Contract{}, ErrFinancialChangeDisabled
	}
	if err := s.checkBusinessLicense(ctx); err != nil {
		return contract.Contract{}, err
	}
	filter, ok := s.contractScope(actor, "contract.edit")
	if !ok || actor.UserID == "" {
		return contract.Contract{}, ErrForbidden
	}
	current, err := s.getContractScoped(ctx, filter, contractID)
	if err != nil {
		return current, err
	}
	// 历史导入基线（非 NORMAL 来源）由导入更正流程管理，不能在此绕过。
	if !contract.OperationalOrigin(current) {
		return current, ErrImportCorrectionRequired
	}
	// 审批前置：合同必须已通过既有审批状态机，且仍处于可变更的生效阶段。
	if !contract.Status(current.Status).ApprovalPassed() || !FinancialChangeStatusEligible(current.Status) {
		return current, apperrors.ErrStateConflict
	}
	if change.ExpectedVersion == 0 {
		return current, ErrValidation
	}
	if change.ExpectedVersion != current.Version {
		return current, apperrors.ErrVersionConflict
	}
	// 币种变更不得借用本入口：它改变的是结算口径而不只是金额。
	requestedCurrency := strings.ToUpper(strings.TrimSpace(change.Currency))
	if requestedCurrency != "" && requestedCurrency != strings.ToUpper(strings.TrimSpace(current.Currency)) {
		return current, ErrFinancialChangeCurrencyLocked
	}
	if change.AmountMinor <= 0 {
		return current, fmt.Errorf("%w: 变更后的合同金额必须大于零", ErrValidation)
	}
	reason := strings.TrimSpace(change.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > MaxFinancialChangeReasonRunes {
		return current, fmt.Errorf("%w: 财务变更原因必填且不超过%d字", ErrValidation, MaxFinancialChangeReasonRunes)
	}
	// 付款计划必须完整提供并合计等于新合同金额；空计划会被结算侧拒绝，
	// 也会让财务事件缺字段，因此这里直接失败关闭。
	if len(change.PaymentTerms) == 0 {
		return current, fmt.Errorf("%w: 财务变更必须提供完整付款计划", ErrValidation)
	}
	terms, normalizeErr := contract.NormalizePaymentTerms(change.PaymentTerms, change.AmountMinor, requestedCurrency)
	if normalizeErr != nil {
		return current, fmt.Errorf("%w: %v", ErrValidation, normalizeErr)
	}
	repository, ok := s.Repo.(ContractFinancialChangeRepository)
	if !ok {
		// 仓储不支持受控财务变更时失败关闭：不能退化为“只改合同不改事件”。
		return current, apperrors.ErrStateConflict
	}
	if current.Pricing == nil && change.Pricing != nil {
		return current, fmt.Errorf("%w: 首次建立基准须独立修订审批", ErrValidation)
	}
	candidate := current
	candidate.AmountMinor = change.AmountMinor
	candidate.Pricing = change.Pricing
	if candidate.Pricing == nil {
		candidate.Pricing = preservePricingAmounts(current.Pricing, change.AmountMinor, current.AmountMinor)
	}
	if candidate.Pricing != nil && candidate.Pricing.Reselect {
		return current, fmt.Errorf("%w: 重选须使用定价修订审批入口", ErrValidation)
	}
	candidate, err = s.preparePricing(ctx, actor, candidate, current.Pricing)
	if err != nil {
		return current, err
	}
	change.Pricing = candidate.Pricing
	change.ContractID = current.ID
	change.Currency = strings.ToUpper(strings.TrimSpace(current.Currency))
	change.PaymentTerms = terms
	change.Reason = reason
	return repository.ApplyContractFinancialChange(ctx, current.TenantID, change, actor.UserID)
}
