package application

import (
	"context"
	"errors"
	"strings"
	"time"
)

// 集成 Outbox 的四条出站边。管理端按 kind 定位具体表，
// 不接受调用方传入表名，避免拼接 SQL。
const (
	IntegrationOutboxFinancial      = "financial"
	IntegrationOutboxResponsibility = "responsibility"
	IntegrationOutboxProject        = "project"
	IntegrationOutboxLink           = "link"
)

// IntegrationOutboxManagerPermission 是运维查看/重试集成死信所需权限；
// 与审计队列运维一致，复用现有高权限，避免新增未登记权限码。
const IntegrationOutboxManagerPermission = "approval.manage"

// IntegrationOutboxKinds 返回全部受管死信队列，供校验和文档复用。
func IntegrationOutboxKinds() []string {
	return []string{IntegrationOutboxFinancial, IntegrationOutboxResponsibility, IntegrationOutboxProject, IntegrationOutboxLink}
}

// ValidIntegrationOutboxKind 判断 kind 是否属于受管出站边。
func ValidIntegrationOutboxKind(kind string) bool {
	for _, candidate := range IntegrationOutboxKinds() {
		if kind == candidate {
			return true
		}
	}
	return false
}

// FailedIntegrationEvent 是一条已进入死信（dead）状态的出站事件。
// 只暴露运维定位必需字段，不返回 payload，避免把客户敏感载荷带到运营页面。
type FailedIntegrationEvent struct {
	ID          string
	Kind        string
	AggregateID string
	Event       string
	Attempts    uint
	LastError   string
	CreatedAt   time.Time
	// UpdatedAt 是最近一次状态变化可证明时间；历史表没有独立 updated_at 列，
	// 仓储取 locked_at（认领时间）与 created_at 的较晚者。
	UpdatedAt time.Time
}

// IntegrationOutboxRepository 由 MySQL 仓储实现，按 kind 映射到四类独立 Outbox 表。
type IntegrationOutboxRepository interface {
	ListFailedIntegrationEvents(ctx context.Context, tenantID, kind string, limit int) ([]FailedIntegrationEvent, error)
	// RetryFailedIntegrationEvent 仅把当前租户的 dead 行重置为可重试；
	// 返回 found=false 表示该租户不存在该 id/kind 的死信，不伪造成功。
	RetryFailedIntegrationEvent(ctx context.Context, tenantID, kind, id, actorID string) (bool, error)
}

// ErrIntegrationOutboxCountsUnsupported 表示当前仓储未实现死信计数能力。
// 就绪探针据此把该检查标为 disabled：既不是健康、也不能伪造 0。
var ErrIntegrationOutboxCountsUnsupported = errors.New("integration outbox dead counts unsupported")

// IntegrationOutboxDeadCounter 由 MySQL 仓储实现，供就绪探针统计死信积压。
// 与运维列表/重试接口分开：探针没有租户上下文，只按部署聚合计数，
// 不返回 id/payload；拆成独立接口也避免既有仓储实现被强制扩展。
type IntegrationOutboxDeadCounter interface {
	CountDeadIntegrationOutboxEvents(ctx context.Context) (map[string]int64, error)
}

// IntegrationOutboxDeadCounts 返回部署内四类集成 Outbox 的 dead 事件计数。
// 参数：ctx 为查询上下文，由调用方设置时限。
// 返回值：kind→数量，四类边固定齐全；查询失败原样返回错误，
// 调用方（/readyz）必须按不可就绪处理，不得回退为 0，也不得掩盖积压。
func (s *Service) IntegrationOutboxDeadCounts(ctx context.Context) (map[string]int64, error) {
	if s == nil || s.Repo == nil {
		return nil, ErrIntegrationOutboxCountsUnsupported
	}
	counter, ok := s.Repo.(IntegrationOutboxDeadCounter)
	if !ok {
		return nil, ErrIntegrationOutboxCountsUnsupported
	}
	counts, err := counter.CountDeadIntegrationOutboxEvents(ctx)
	if err != nil {
		return nil, err
	}
	// 固定四类边：仓储漏填的 kind 真实值即 0，保证探针输出结构稳定。
	normalized := make(map[string]int64, len(IntegrationOutboxKinds()))
	for _, kind := range IntegrationOutboxKinds() {
		normalized[kind] = counts[kind]
	}
	return normalized, nil
}

// ListFailedIntegrationEvents 返回当前租户指定队列的死信列表。
// 参数：actor 决定租户与权限，kind 为四类受管队列之一，limit 为上限。
// 返回值：未知 kind 或缺少仓储能力时返回校验错误；权限不足返回 ErrForbidden。
func (s *Service) ListFailedIntegrationEvents(ctx context.Context, actor Principal, kind string, limit int) ([]FailedIntegrationEvent, error) {
	if !actor.Has(IntegrationOutboxManagerPermission) {
		return nil, ErrForbidden
	}
	kind = strings.TrimSpace(kind)
	if !ValidIntegrationOutboxKind(kind) {
		return nil, ErrValidation
	}
	if strings.TrimSpace(actor.TenantID) == "" {
		return nil, ErrForbidden
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	repository, ok := s.Repo.(IntegrationOutboxRepository)
	if !ok {
		return nil, ErrValidation
	}
	return repository.ListFailedIntegrationEvents(ctx, actor.TenantID, kind, limit)
}

// RetryFailedIntegrationEvent 把一条死信重置为可重试并写业务审计。
// 参数：actor 决定租户、权限与审计操作者，kind/id 定位死信行。
// 返回值：found=false 表示未找到该租户的死信；权限/参数错误分别返回对应错误。
func (s *Service) RetryFailedIntegrationEvent(ctx context.Context, actor Principal, kind, id string) (bool, error) {
	if !actor.Has(IntegrationOutboxManagerPermission) {
		return false, ErrForbidden
	}
	kind = strings.TrimSpace(kind)
	id = strings.TrimSpace(id)
	if !ValidIntegrationOutboxKind(kind) || id == "" || len(id) > 64 {
		return false, ErrValidation
	}
	if strings.TrimSpace(actor.TenantID) == "" {
		return false, ErrForbidden
	}
	repository, ok := s.Repo.(IntegrationOutboxRepository)
	if !ok {
		return false, ErrValidation
	}
	return repository.RetryFailedIntegrationEvent(ctx, actor.TenantID, kind, id, actor.UserID)
}
