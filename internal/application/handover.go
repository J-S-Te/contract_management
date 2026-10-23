package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 交接机器协议的应用实现。业务责任仍由合同系统持有：平台只负责冻结、逐项转移
// 与回执，本服务负责校验、幂等与版本条件，并把真实写入交给仓储在本地事务内完成。

var (
	ErrHandoverValidation = errors.New("handover request validation failed")
	ErrHandoverConflict   = errors.New("handover state conflict")
	ErrHandoverNotFound   = errors.New("handover record not found")
	ErrHandoverFrozen     = errors.New("user is frozen by an in-flight handover")
)

// 回执状态（与平台协议一致）。
const (
	HandoverReceiptApplied        = "APPLIED"
	HandoverReceiptAlreadyApplied = "ALREADY_APPLIED"
	HandoverReceiptRejected       = "REJECTED"
	HandoverReceiptRetryable      = "RETRYABLE"
)

// 冻结状态。
const (
	HandoverFreezeFrozen   = "FROZEN"
	HandoverFreezeReleased = "RELEASED"
)

// 责任类别：未结束合同负责人必须以真实负责人字段为准。
const HandoverCategoryContractOwner = "CONTRACT_OWNER"

// 审批节点责任：未完成审批的当前处理人也必须纳入交接，但转派必须走权威工作流命令。
const (
	HandoverCategoryApprovalAssignee = "APPROVAL_ASSIGNEE"
	HandoverResourceApprovalTask     = "approval_task"
)

type HandoverResponsibility struct {
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id"`
	ResourceName string    `json:"resource_name"`
	OwnerUserID  string    `json:"owner_user_id"`
	Category     string    `json:"category"`
	Version      uint64    `json:"version"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type HandoverFreeze struct {
	ID, TenantID, BatchID, RequestID, UserID, Status string
	FrozenAt                                         time.Time
	ReleasedAt                                       *time.Time
	Outcome                                          string
}

type HandoverTransferInput struct {
	TenantID, BatchID, ItemID, RequestID, ResourceType, ResourceID string
	FromUserID, ToUserID                                           string
	// ToDisplayName 由权威负责人目录解析后写入合同展示快照；调用方不应凭空填写。
	ToDisplayName   string
	ExpectedVersion uint64
	IdempotencyKey  string
}

type HandoverReceipt struct {
	TenantID, BatchID, ItemID string
	ResourceType, ResourceID  string
	FromUserID, ToUserID      string
	IdempotencyKey, Status    string
	AppliedVersion            uint64
	ReasonCode, EvidenceRef   string
	ReceivedAt                time.Time
}

// ApprovalWorkflowTransfers 由合同应用服务实现：用权威工作流命令转派审批节点。
type ApprovalWorkflowTransfers interface {
	ReassignApprovalNode(ctx context.Context, tenantID, approvalID, nodeID, fromUserID, toUserID, reason, idempotencyKey string) (string, error)
}

type handoverApprovalDispatcher struct{ transfers ApprovalWorkflowTransfers }

// NewHandoverApprovalDispatcher 把交接派发端口接到合同应用服务的权威工作流命令实现。
func NewHandoverApprovalDispatcher(transfers ApprovalWorkflowTransfers) HandoverApprovalTransferDispatcher {
	return handoverApprovalDispatcher{transfers: transfers}
}

func (dispatcher handoverApprovalDispatcher) DispatchApprovalTransfer(ctx context.Context, tenantID, approvalID, nodeID, fromUserID, toUserID, idempotencyKey, reason string) (string, error) {
	if dispatcher.transfers == nil {
		return "", errors.New("approval workflow transfers are not configured")
	}
	return dispatcher.transfers.ReassignApprovalNode(ctx, tenantID, approvalID, nodeID, fromUserID, toUserID, reason, idempotencyKey)
}

// HandoverApprovalTransferDispatcher 把审批节点转派交给权威工作流命令执行，
// 平台与交接服务都不直接改库冒充"引擎已转派"。返回的字符串是可供核对的证据引用。
type HandoverApprovalTransferDispatcher interface {
	DispatchApprovalTransfer(ctx context.Context, tenantID, approvalID, nodeID, fromUserID, toUserID, idempotencyKey, reason string) (string, error)
}

// HandoverOwnerResolver 从平台权威负责人目录解析承接人的展示名与有效身份；
// 交接不能只改 owner_user_id 而让 SELF 数据范围和姓名停留在前任。
type HandoverOwnerResolver interface {
	ResolveOwner(ctx context.Context, tenantID, userID string) (UserReference, error)
}

// HandoverFreezeChecker 让写路径在分配责任人前确认该用户未被交接冻结。
// 冻结必须覆盖正常创建、指派、导入与回调，而不只是交接协议端点。
type HandoverFreezeChecker interface {
	ActiveFreeze(context.Context, string, string) (HandoverFreeze, bool, error)
}

// HandoverRepository 是交接所需的最小持久化端口；实现必须在本地事务内完成责任变更、
// 版本递增与回执写入，保证"回执存在 ⇒ 变更已生效"。
type HandoverRepository interface {
	CreateFreeze(context.Context, HandoverFreeze) (HandoverFreeze, bool, error)
	ReleaseFreeze(context.Context, string, string, string, time.Time) error
	ActiveFreeze(context.Context, string, string) (HandoverFreeze, bool, error)
	ListOwnedResponsibilities(context.Context, string, string, string, int) ([]HandoverResponsibility, error)
	TransferOwnership(context.Context, HandoverTransferInput, time.Time) (HandoverReceipt, error)
	// SaveReceipt 持久化审批工作流转派回执；回执存在 ⇒ 工作流已实际执行。
	SaveReceipt(context.Context, HandoverReceipt) error
	FindReceipt(context.Context, string, string) (HandoverReceipt, bool, error)
}

// HandoverService 实现机器协议的服务端语义。
type HandoverService struct {
	repository HandoverRepository
	// approvals 为 nil 时审批责任项按"暂不可转派"处理（RETRYABLE），不静默跳过。
	approvals HandoverApprovalTransferDispatcher
	owners    HandoverOwnerResolver
	ids       func(time.Time) (string, error)
	now       func() time.Time
}

func NewHandoverService(repository HandoverRepository, ids func(time.Time) (string, error), now func() time.Time) (*HandoverService, error) {
	if repository == nil || ids == nil || now == nil {
		return nil, errors.New("handover service dependencies must not be nil")
	}
	return &HandoverService{repository: repository, ids: ids, now: now}, nil
}

// SetApprovalTransferDispatcher 注入审批节点转派通道（组合根接线）。
func (service *HandoverService) SetApprovalTransferDispatcher(dispatcher HandoverApprovalTransferDispatcher) {
	service.approvals = dispatcher
}

// SetOwnerResolver 注入权威负责人目录；未接线时保持旧行为（测试/兼容进程）。
func (service *HandoverService) SetOwnerResolver(resolver HandoverOwnerResolver) {
	service.owners = resolver
}

// Freeze 持久化"交接期间不得再分配新责任"；重复调用幂等返回同一冻结。
func (service *HandoverService) Freeze(ctx context.Context, tenantID, batchID, requestID, userID string) (HandoverFreeze, int, error) {
	tenantID, batchID, requestID, userID = strings.TrimSpace(tenantID), strings.TrimSpace(batchID), strings.TrimSpace(requestID), strings.TrimSpace(userID)
	if tenantID == "" || batchID == "" || requestID == "" || userID == "" {
		return HandoverFreeze{}, 0, ErrHandoverValidation
	}
	now := service.now().UTC()
	freezeID, err := service.ids(now)
	if err != nil {
		return HandoverFreeze{}, 0, err
	}
	// created 表示本次是否新建冻结；重复冻结同样返回既有记录（幂等）。
	freeze, _, err := service.repository.CreateFreeze(ctx, HandoverFreeze{
		ID: freezeID, TenantID: tenantID, BatchID: batchID, RequestID: requestID, UserID: userID,
		Status: HandoverFreezeFrozen, FrozenAt: now,
	})
	if err != nil {
		return HandoverFreeze{}, 0, err
	}
	// 冻结先行，随后统计当前权威责任总数（分页累计，不能用首页长度代表总数）。
	count, err := service.countResponsibilities(ctx, tenantID, userID)
	if err != nil {
		return HandoverFreeze{}, 0, err
	}
	return freeze, count, nil
}

// countResponsibilities 分页累计该用户的权威责任总数。
func (service *HandoverService) countResponsibilities(ctx context.Context, tenantID, userID string) (int, error) {
	total, cursor := 0, ""
	for {
		page, err := service.repository.ListOwnedResponsibilities(ctx, tenantID, userID, cursor, 200)
		if err != nil {
			return 0, err
		}
		total += len(page)
		if len(page) < 200 {
			return total, nil
		}
		cursor = page[len(page)-1].ResourceID
	}
}

// Responsibilities 返回分页权威责任清单；cursor 为空表示首页。
func (service *HandoverService) Responsibilities(ctx context.Context, tenantID, userID, cursor string, limit int) ([]HandoverResponsibility, string, error) {
	tenantID, userID = strings.TrimSpace(tenantID), strings.TrimSpace(userID)
	if tenantID == "" || userID == "" {
		return nil, "", ErrHandoverValidation
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	items, err := service.repository.ListOwnedResponsibilities(ctx, tenantID, userID, strings.TrimSpace(cursor), limit)
	if err != nil {
		return nil, "", err
	}
	next := ""
	for index := range items {
		// 仓储负责给出真实类别（合同负责人 / 审批处理人）；这里只补空缺，绝不覆盖。
		if strings.TrimSpace(items[index].ResourceType) == "" {
			items[index].ResourceType = "contract"
		}
		if strings.TrimSpace(items[index].Category) == "" {
			items[index].Category = HandoverCategoryContractOwner
		}
	}
	if len(items) == limit {
		next = items[len(items)-1].ResourceID
	}
	return items, next, nil
}

// Transfer 在本地事务内变更合同负责人并写入幂等回执。
func (service *HandoverService) Transfer(ctx context.Context, input HandoverTransferInput) (HandoverReceipt, error) {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.BatchID = strings.TrimSpace(input.BatchID)
	input.ItemID = strings.TrimSpace(input.ItemID)
	input.ResourceType = strings.TrimSpace(input.ResourceType)
	input.ResourceID = strings.TrimSpace(input.ResourceID)
	input.FromUserID = strings.TrimSpace(input.FromUserID)
	input.ToUserID = strings.TrimSpace(input.ToUserID)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if input.TenantID == "" || input.BatchID == "" || input.ItemID == "" || input.ResourceID == "" ||
		input.FromUserID == "" || input.ToUserID == "" || input.IdempotencyKey == "" {
		return HandoverReceipt{}, ErrHandoverValidation
	}
	// 承接人不能是交接主体本人，否则转移是空操作。
	if input.FromUserID == input.ToUserID {
		return HandoverReceipt{}, fmt.Errorf("successor must differ from the current owner: %w", ErrHandoverValidation)
	}
	if existing, found, err := service.repository.FindReceipt(ctx, input.TenantID, input.IdempotencyKey); err != nil {
		return HandoverReceipt{}, err
	} else if found {
		// 同幂等键重复投递：原样返回既有回执，绝不重复变更。
		return existing, nil
	}
	// 承接人自身也处于交接冻结时不得接收新责任。
	if freeze, found, err := service.repository.ActiveFreeze(ctx, input.TenantID, input.ToUserID); err != nil {
		return HandoverReceipt{}, err
	} else if found && freeze.Status == HandoverFreezeFrozen {
		return HandoverReceipt{}, fmt.Errorf("successor %s is frozen by handover %s: %w", input.ToUserID, freeze.BatchID, ErrHandoverFrozen)
	}
	// 承接人必须是平台权威目录中的有效用户；解析失败时失败关闭，不能只换一个 ID。
	if service.owners != nil {
		owner, err := service.owners.ResolveOwner(ctx, input.TenantID, input.ToUserID)
		if err != nil {
			return HandoverReceipt{}, fmt.Errorf("resolve handover successor %s: %w", input.ToUserID, err)
		}
		if strings.TrimSpace(owner.UserID) != input.ToUserID || strings.TrimSpace(owner.DisplayName) == "" {
			return HandoverReceipt{}, fmt.Errorf("successor %s has no authoritative owner profile: %w", input.ToUserID, ErrHandoverValidation)
		}
		input.ToDisplayName = strings.TrimSpace(owner.DisplayName)
	}
	if input.ResourceType == HandoverResourceApprovalTask {
		return service.transferApprovalTask(ctx, input)
	}
	return service.repository.TransferOwnership(ctx, input, service.now().UTC())
}

// transferApprovalTask 通过权威工作流命令转派审批节点：命令失败或通道未接线时返回
// RETRYABLE 回执（平台会退避重试），绝不直接改库冒充已转派。只有工作流确认执行后
// 才写 APPLIED，并把回执持久化，保证"回执存在 ⇒ 转派已生效"。
func (service *HandoverService) transferApprovalTask(ctx context.Context, input HandoverTransferInput) (HandoverReceipt, error) {
	now := service.now().UTC()
	approvalID, nodeID, ok := strings.Cut(input.ResourceID, ":")
	if !ok || strings.TrimSpace(approvalID) == "" || strings.TrimSpace(nodeID) == "" {
		return HandoverReceipt{}, fmt.Errorf("approval responsibility must be identified as approval:node: %w", ErrHandoverValidation)
	}
	base := HandoverReceipt{
		TenantID: input.TenantID, BatchID: input.BatchID, ItemID: input.ItemID,
		ResourceType: input.ResourceType, ResourceID: input.ResourceID,
		FromUserID: input.FromUserID, ToUserID: input.ToUserID, IdempotencyKey: input.IdempotencyKey,
		ReceivedAt: now,
	}
	if service.approvals == nil {
		base.Status, base.ReasonCode = HandoverReceiptRetryable, "APPROVAL_DISPATCH_UNAVAILABLE"
		return base, nil
	}
	evidence, err := service.approvals.DispatchApprovalTransfer(ctx, input.TenantID, approvalID, nodeID, input.FromUserID, input.ToUserID, input.IdempotencyKey, "handover:"+input.BatchID)
	if err != nil {
		base.Status, base.ReasonCode = HandoverReceiptRetryable, "APPROVAL_COMMAND_FAILED"
		return base, nil
	}
	base.Status, base.EvidenceRef = HandoverReceiptApplied, evidence
	if err := service.repository.SaveReceipt(ctx, base); err != nil {
		// 工作流已生效但回执未能落库：返回 RETRYABLE，平台重试时会命中幂等命令并补写回执。
		base.Status, base.ReasonCode = HandoverReceiptRetryable, "APPROVAL_RECEIPT_PERSIST_FAILED"
		return base, nil
	}
	return base, nil
}

// Receipt 查询既有回执；平台在超时后据此判断是否已生效。
func (service *HandoverService) Receipt(ctx context.Context, tenantID, idempotencyKey string) (HandoverReceipt, error) {
	receipt, found, err := service.repository.FindReceipt(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(idempotencyKey))
	if err != nil {
		return HandoverReceipt{}, err
	}
	if !found {
		return HandoverReceipt{}, ErrHandoverNotFound
	}
	return receipt, nil
}

// Complete 结束交接：COMPLETED 或 CANCELLED 都只解除冻结，已完成的转移保留。
func (service *HandoverService) Complete(ctx context.Context, tenantID, batchID, outcome string) error {
	tenantID, batchID, outcome = strings.TrimSpace(tenantID), strings.TrimSpace(batchID), strings.ToUpper(strings.TrimSpace(outcome))
	if tenantID == "" || batchID == "" || (outcome != "COMPLETED" && outcome != "CANCELLED") {
		return ErrHandoverValidation
	}
	return service.repository.ReleaseFreeze(ctx, tenantID, batchID, outcome, service.now().UTC())
}

// EnsureOwnerNotFrozen 确认即将承担责任的用户未被交接冻结；未接线检查器时保持既有行为。
func (s *Service) EnsureOwnerNotFrozen(ctx context.Context, tenantID, ownerUserID string) error {
	if s == nil || s.HandoverFreeze == nil || strings.TrimSpace(ownerUserID) == "" {
		return nil
	}
	freeze, found, err := s.HandoverFreeze.ActiveFreeze(ctx, tenantID, strings.TrimSpace(ownerUserID))
	if err != nil {
		return fmt.Errorf("handover freeze lookup failed: %w", err)
	}
	if found && freeze.Status == HandoverFreezeFrozen {
		return fmt.Errorf("owner %s is frozen by handover %s: %w", ownerUserID, freeze.BatchID, ErrHandoverFrozen)
	}
	return nil
}
