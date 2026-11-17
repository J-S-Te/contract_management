package application

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	core "github.com/J-S-Te/license-core"
	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

var ErrContractRecordUnavailable = errors.New("contract record management unavailable")
var ErrContractRecordDeleted = errors.New("contract record deleted")

// ContractRecordStore 将记录管理隔离为可选能力，避免放大普通合同仓储接口。
type ContractRecordStore interface {
	PreviewContractRecord(context.Context, contract.ScopeFilter, contract.ScopeFilter, string) (contract.RecordPreview, error)
	ChangeContractRecord(context.Context, contract.ScopeFilter, contract.ScopeFilter, string, uint64, string, string, contract.RecordState) (contract.RecordMutation, error)
	ListDeletedContractRecords(context.Context, contract.ScopeFilter, contract.ScopeFilter, contract.ContractListQuery) (contract.DeletedRecordPage, error)
}

// ContractHistoryStore 只为冻结审批授权读取当前归属元数据，不授予正文读取。
type ContractHistoryStore interface {
	GetContractHistoryMetadata(context.Context, contract.ScopeFilter, string) (contract.Contract, error)
}

// approvalHistoryContract 用途：按当前读取范围授权不可变审批历史，禁止墓碑正文降级。
// 参数：ctx 和 actor 为请求与可信主体，id 为审批所属合同身份。
// 返回值：仅授权元数据或普通合同；授权不足及范围外读取明确拒绝。
func (s *Service) approvalHistoryContract(ctx context.Context, actor Principal, id string) (contract.Contract, error) {
	filter, ok := s.contractScope(actor, "contract.read")
	if !ok {
		return contract.Contract{}, ErrForbidden
	}
	if store, ok := s.Repo.(ContractHistoryStore); ok {
		return store.GetContractHistoryMetadata(ctx, filter, id)
	}
	return s.getContractScoped(ctx, filter, id)
}

type ContractDeleteBlockedError struct {
	Blockers []contract.RecordBlocker
}

// Error 用途：返回可安全公开的删除或恢复阻断说明。
// 参数：无。
// 返回值：固定业务说明，具体原因由结构化阻断项提供。
func (e *ContractDeleteBlockedError) Error() string {
	return "合同存在未收口事项或业务关联，请核对阻断原因"
}

// Unwrap 用途：保留已有业务状态冲突分类。
// 参数：无。
// 返回值：状态冲突错误，不将业务阻断伪装成依赖失败。
func (e *ContractDeleteBlockedError) Unwrap() error { return apperrors.ErrStateConflict }

// contractRecordScopes 用途：要求可信管理员、精确权限及两份有效数据范围。
// 参数：actor 为已经身份中间件验证的当前主体。
// 返回值：读取与管理范围；缺少任一授权返回禁止访问。
func (s *Service) contractRecordScopes(actor Principal) (contract.ScopeFilter, contract.ScopeFilter, error) {
	if actor.TenantID == "" || actor.UserID == "" || actor.IdentityID == "" || !containsString(actor.Roles, "admin") {
		return contract.ScopeFilter{}, contract.ScopeFilter{}, ErrForbidden
	}
	read, readOK := s.contractScope(actor, "contract.read")
	manage, manageOK := actor.Scope("contract.delete")
	if !readOK || !manageOK {
		return contract.ScopeFilter{}, contract.ScopeFilter{}, ErrForbidden
	}
	return read, manage, nil
}

// AuthorizeContractRecordManagement 用途：为管理请求解析前提供统一的精确授权检查。
// 参数：actor 为已经认证的当前主体。
// 返回值：角色、权限或范围不足时返回禁止访问，否则为空。
func (s *Service) AuthorizeContractRecordManagement(actor Principal) error {
	_, _, err := s.contractRecordScopes(actor)
	return err
}

// PreviewContractDeletion 用途：读取当前删除资格或恢复资格，不改变业务记录。
// 参数：ctx 为请求上下文，actor 为可信主体，id 为待核对合同身份。
// 返回值：限定范围的记录摘要与阻断项，或授权、缺失及依赖错误。
func (s *Service) PreviewContractDeletion(ctx context.Context, actor Principal, id string) (contract.RecordPreview, error) {
	read, manage, err := s.contractRecordScopes(actor)
	if err != nil {
		return contract.RecordPreview{}, err
	}
	if err = s.CheckLicense(ctx, core.READ_HISTORY); err != nil {
		return contract.RecordPreview{}, err
	}
	store, ok := s.Repo.(ContractRecordStore)
	if !ok {
		return contract.RecordPreview{}, ErrContractRecordUnavailable
	}
	return store.PreviewContractRecord(ctx, read, manage, strings.TrimSpace(id))
}

// DeleteContract 用途：逻辑删除无下游关联且已收口的合同，保留全部历史。
// 参数：ctx 为请求上下文，actor 为可信主体，id 和 version 为所见记录，reason 为删除原因。
// 返回值：当前记录状态及是否变更；并发、关联或授权不满足时返回错误。
func (s *Service) DeleteContract(ctx context.Context, actor Principal, id string, version uint64, reason string) (contract.RecordMutation, error) {
	return s.changeContractRecord(ctx, actor, id, version, reason, contract.RecordDeleted)
}

// RestoreContract 用途：恢复原合同记录，不重新发起审批、项目或财务派生。
// 参数：ctx 为请求上下文，actor 为可信主体，id 和 version 为所见记录，reason 为可选恢复原因。
// 返回值：恢复或幂等回执，或者明确的业务、并发与授权错误。
func (s *Service) RestoreContract(ctx context.Context, actor Principal, id string, version uint64, reason string) (contract.RecordMutation, error) {
	return s.changeContractRecord(ctx, actor, id, version, reason, contract.RecordActive)
}

// changeContractRecord 用途：统一校验输入，并交由事务仓储重新核对资格。
// 参数：ctx 和 actor 为请求及主体，id/version 为记录基线，reason 为说明，target 为目标记录状态。
// 返回值：原子状态回执；未授权时先拒绝，不接触仓储。
func (s *Service) changeContractRecord(ctx context.Context, actor Principal, id string, version uint64, reason string, target contract.RecordState) (contract.RecordMutation, error) {
	read, manage, err := s.contractRecordScopes(actor)
	if err != nil {
		return contract.RecordMutation{}, err
	}
	if err = s.checkBusinessLicense(ctx); err != nil {
		return contract.RecordMutation{}, err
	}
	reason = strings.TrimSpace(reason)
	if target == contract.RecordActive && reason == "" {
		reason = "误删恢复"
	}
	if version == 0 {
		return contract.RecordMutation{}, validationFeedback("expected_version", "required", "请重新读取合同后提交操作。")
	}
	if reason == "" || utf8.RuneCountInString(reason) > 1000 {
		return contract.RecordMutation{}, validationFeedback("reason", "length", "请填写 1 至 1000 个字符的操作原因。")
	}
	store, ok := s.Repo.(ContractRecordStore)
	if !ok {
		return contract.RecordMutation{}, ErrContractRecordUnavailable
	}
	return store.ChangeContractRecord(ctx, read, manage, strings.TrimSpace(id), version, actor.UserID, reason, target)
}

// ListDeletedContractRecords 用途：读取两份授权范围交集内的回收站摘要。
// 参数：ctx 和 actor 为请求及主体，query 为现有台账筛选与分页条件。
// 返回值：不含正文的删除记录分页，或者授权与存储错误。
func (s *Service) ListDeletedContractRecords(ctx context.Context, actor Principal, query contract.ContractListQuery) (contract.DeletedRecordPage, error) {
	read, manage, err := s.contractRecordScopes(actor)
	if err != nil {
		return contract.DeletedRecordPage{}, err
	}
	if err = s.CheckLicense(ctx, core.READ_HISTORY); err != nil {
		return contract.DeletedRecordPage{}, err
	}
	if query.Limit <= 0 || query.Limit > contractLedgerMaxPageSize {
		query.Limit = contractLedgerDefaultLimit
	}
	if query.Offset < 0 || query.Offset/query.Limit >= contractLedgerMaxPage {
		query.Offset = 0
	}
	store, ok := s.Repo.(ContractRecordStore)
	if !ok {
		return contract.DeletedRecordPage{}, ErrContractRecordUnavailable
	}
	return store.ListDeletedContractRecords(ctx, read, manage, query)
}
