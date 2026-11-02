package application

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/oklog/ulid/v2"
)

// ErrContractSummarySourceUnavailable 表示仓储未提供合同摘要所需的只读查询能力。
// 这是服务部署错误而非“合同不存在”，HTTP 层必须按依赖不可用失败关闭，
// 不能返回 404 让 CRM 误判为“合同不属于该客户”。
var ErrContractSummarySourceUnavailable = errors.New("contract summary source is unavailable")

// 合同摘要 linked_at 的证据来源枚举。顺序即仓储证据优先级：
// CRM 接入单审核时间 → 审核流水 → 审计流水 → 接入单创建时间 → 合同更新时间近似。
const (
	ContractSummaryLinkSourceCRMReview             = "CRM_REVIEW"
	ContractSummaryLinkSourceReviewLog             = "REVIEW_LOG"
	ContractSummaryLinkSourceAuditLog              = "AUDIT_LOG"
	ContractSummaryLinkSourceIntakeCreated         = "INTAKE_CREATED"
	ContractSummaryLinkSourceApproxContractUpdated = "APPROX_CONTRACT_UPDATED"
)

// 合同摘要 linked_at 的精度枚举。
// exact 表示该时间是关联动作本身的落库时间；approx 表示只是可用的近似值，
// 可能早于或晚于真实关联时刻，调用方不得用它判断业务先后顺序。
const (
	ContractSummaryPrecisionExact  = "exact"
	ContractSummaryPrecisionApprox = "approx"
)

// ContractSummary 是 CRM 商机签约核验所需的最小合同投影。
// 只包含客户归属判断必需字段，不携带合同正文、金额或其它租户数据。
type ContractSummary struct {
	ContractID       string
	ContractNumber   string
	CRMCustomerID    uint64
	CRMOpportunityID uint64
	// LinkedAt 保持“最新一次”可证明关联时间的既有语义（换绑后时间会跟着跳变）；
	// 需要“首次关联”时用 FirstLinkedAt。历史数据没有任何审核证据时回退合同
	// updated_at，该回退值是“近似关联时间”，可能晚于真实关联时刻。
	LinkedAt time.Time
	// LinkedAtSource 标注 LinkedAt 的证据来源，取 ContractSummaryLinkSource* 之一。
	LinkedAtSource string
	// LinkedAtPrecision 取 ContractSummaryPrecisionExact/Approx，
	// 由 LinkedAtSource 推导：只有关联动作本身的时间算 exact。
	LinkedAtPrecision string
	// FirstLinkedAt 是同一证据层内最早一次可证明关联时间；
	// 无任何证据（回退 updated_at）时与 LinkedAt 相同。
	FirstLinkedAt time.Time
}

// ContractSummaryCandidate 是仓储按“当前租户 + contract_ref”查出的候选行。
// contract_ref 可能是稳定合同 ID，也可能是合同编号；两种匹配由仓储完成。
type ContractSummaryCandidate struct {
	ContractID       string
	ContractNumber   string
	CRMCustomerID    uint64
	CRMOpportunityID uint64
	// LinkedAt 是合同与商机关联的最新可证明时间：商机接入审核通过时间（reviewed_at）、
	// 同单 LINK_CONFIRMED 审核流水、con_audit_outbox 关联确认审计流水，
	// 或最后退回接入单创建时间。为零表示没有任何可证明的关联记录，
	// 调用方改用合同更新时间（近似值）。
	LinkedAt time.Time
	// LinkedAtSource 是 LinkedAt 的证据来源枚举。LinkedAt 非零时该字段必须取
	// ContractSummaryLinkSource* 之一，否则服务层失败关闭（不对外发出非法枚举）。
	LinkedAtSource string
	// FirstLinkedAt 是与 LinkedAt 同一证据层内最早一次可证明关联时间；为零时按 LinkedAt 补齐。
	FirstLinkedAt time.Time
	UpdatedAt     time.Time
}

// ContractSummarySource 由 MySQL 仓储实现；缺失实现时合同摘要端点失败关闭。
type ContractSummarySource interface {
	FindContractSummaryCandidate(ctx context.Context, tenantID, contractRef string) (ContractSummaryCandidate, error)
}

// ContractSummary 返回当前租户内属于指定 CRM 客户的合同最小投影。
// 参数：ctx 控制查询，tenantID 必须来自已验签机器令牌，contractRef 是合同 ID 或编号，
// crmCustomerID 是调用方声明的 CRM 客户。
// 返回值：客户不一致、合同未关联 CRM 商机或合同不存在时统一返回 apperrors.ErrNotFound，
// 避免向调用方泄漏其它客户的合同是否存在。
func (s *Service) ContractSummary(ctx context.Context, tenantID, contractRef string, crmCustomerID uint64) (ContractSummary, error) {
	tenantID = strings.TrimSpace(tenantID)
	contractRef = strings.TrimSpace(contractRef)
	if tenantID == "" || contractRef == "" || crmCustomerID == 0 {
		return ContractSummary{}, ErrValidation
	}
	source, ok := s.Repo.(ContractSummarySource)
	if !ok {
		return ContractSummary{}, ErrContractSummarySourceUnavailable
	}
	candidate, err := source.FindContractSummaryCandidate(ctx, tenantID, contractRef)
	if err != nil {
		return ContractSummary{}, err
	}
	// “属于该客户”只需要合同已有同一 CRM 客户；商机关联不是前置条件。
	// 合同可以由 CRM 客户先建后关联商机（合法历史/在建状态），不能因为
	// opportunity_id 为空就把客户一致的合同误报为“属于其它客户”。
	if candidate.CRMCustomerID == 0 || candidate.CRMCustomerID != crmCustomerID ||
		candidate.ContractID == "" || strings.TrimSpace(candidate.ContractNumber) == "" {
		return ContractSummary{}, apperrors.ErrNotFound
	}
	// CRM 契约要求 contract_id 是规范 ULID；非规范身份说明本地数据异常，
	// 不能把非法身份当成功响应发出去。
	if _, err := ulid.ParseStrict(candidate.ContractID); err != nil {
		return ContractSummary{}, ErrContractSummarySourceUnavailable
	}
	linkedAt := candidate.LinkedAt
	evidenceSource := candidate.LinkedAtSource
	firstLinkedAt := candidate.FirstLinkedAt
	if linkedAt.IsZero() {
		// 没有可证明的商机关联审核/审计记录时退回合同最后更新时间。
		// 该字段仍是非零时间，满足调用方“已关联时间必须存在”的校验，
		// 但它是“近似关联时间”：可能晚于真实关联时刻，不保证业务先后顺序。
		linkedAt = candidate.UpdatedAt
		firstLinkedAt = linkedAt
		evidenceSource = ContractSummaryLinkSourceApproxContractUpdated
	}
	if linkedAt.IsZero() {
		return ContractSummary{}, apperrors.ErrNotFound
	}
	precision, ok := contractSummaryLinkedAtPrecision(evidenceSource)
	if !ok {
		// 仓储漏标或标错来源时失败关闭：既不能对外发出枚举外的 linked_at_source，
		// 也不能把无法解释的时间冒充成“可证明的关联时间”。
		return ContractSummary{}, ErrContractSummarySourceUnavailable
	}
	if firstLinkedAt.IsZero() || firstLinkedAt.After(linkedAt) {
		// first_linked_at 只允许早于或等于 linked_at；缺失时退化为同一时刻。
		firstLinkedAt = linkedAt
	}
	return ContractSummary{
		ContractID:        candidate.ContractID,
		ContractNumber:    strings.TrimSpace(candidate.ContractNumber),
		CRMCustomerID:     candidate.CRMCustomerID,
		CRMOpportunityID:  candidate.CRMOpportunityID,
		LinkedAt:          linkedAt.UTC(),
		LinkedAtSource:    evidenceSource,
		LinkedAtPrecision: precision,
		FirstLinkedAt:     firstLinkedAt.UTC(),
	}, nil
}

// contractSummaryLinkedAtPrecision 把证据来源映射为对外精度。
// 参数：source 为 ContractSummaryLinkSource* 枚举值。
// 返回值：精度枚举；来源不在枚举内时返回 false，由调用方失败关闭。
//
// 只有关联动作本身的落库时间算 exact（CRM 审核通过时间、审核流水、审计流水）；
// 接入单创建时间早于确认动作，合同 updated_at 回退则完全不可靠，二者都是 approx。
func contractSummaryLinkedAtPrecision(source string) (string, bool) {
	switch source {
	case ContractSummaryLinkSourceCRMReview, ContractSummaryLinkSourceReviewLog, ContractSummaryLinkSourceAuditLog:
		return ContractSummaryPrecisionExact, true
	case ContractSummaryLinkSourceIntakeCreated, ContractSummaryLinkSourceApproxContractUpdated:
		return ContractSummaryPrecisionApprox, true
	default:
		return "", false
	}
}
