package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/auditctx"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// integrationOutboxRow 是四类集成 Outbox 死信列表的公共投影。
type integrationOutboxRow struct {
	ID        string
	Aggregate string
	Attempts  uint
	LastError *string
	CreatedAt time.Time
	LockedAt  *time.Time
}

// integrationOutboxDeadList 按 kind 选择受管表；kind 已由应用层枚举校验，
// 这里仍做防御性拒绝，避免任何动态表名进入 SQL。
func (r *Repository) ListFailedIntegrationEvents(ctx context.Context, tenantID, kind string, limit int) ([]application.FailedIntegrationEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	var rows []integrationOutboxRow
	var err error
	switch kind {
	case application.IntegrationOutboxFinancial:
		err = r.db.WithContext(ctx).Model(&financialEffectiveOutboxRecord{}).
			Select("id, contract_id AS aggregate, attempts, last_error, created_at, locked_at").
			Where("tenant_id = ? AND delivery_status = ?", tenantID, "dead").
			Order("created_at DESC").Limit(limit).Scan(&rows).Error
	case application.IntegrationOutboxResponsibility:
		err = r.db.WithContext(ctx).Model(&contractResponsibilityOutbox{}).
			Select("id, contract_id AS aggregate, attempts, last_error, created_at, locked_at").
			Where("tenant_id = ? AND delivery_status = ?", tenantID, "dead").
			Order("created_at DESC").Limit(limit).Scan(&rows).Error
	case application.IntegrationOutboxProject:
		err = r.db.WithContext(ctx).Model(&projectDeliveryOutboxRecord{}).
			Select("id, contract_id AS aggregate, attempts, last_error, created_at, locked_at").
			Where("tenant_id = ? AND delivery_status = ?", tenantID, "dead").
			Order("created_at DESC").Limit(limit).Scan(&rows).Error
	case application.IntegrationOutboxLink:
		err = r.db.WithContext(ctx).Model(&opportunityLinkOutboxRecord{}).
			Select("id, intake_id AS aggregate, attempts, last_error, created_at, locked_at").
			Where("tenant_id = ? AND delivery_status = ?", tenantID, "dead").
			Order("created_at DESC").Limit(limit).Scan(&rows).Error
	default:
		return nil, application.ErrValidation
	}
	if err != nil {
		return nil, err
	}
	items := make([]application.FailedIntegrationEvent, 0, len(rows))
	for _, row := range rows {
		updatedAt := row.CreatedAt
		if row.LockedAt != nil && row.LockedAt.After(updatedAt) {
			updatedAt = *row.LockedAt
		}
		items = append(items, application.FailedIntegrationEvent{
			ID:          row.ID,
			Kind:        kind,
			AggregateID: row.Aggregate,
			Event:       integrationOutboxEvent(kind),
			Attempts:    row.Attempts,
			LastError:   stringValue(row.LastError),
			CreatedAt:   row.CreatedAt,
			UpdatedAt:   updatedAt,
		})
	}
	return items, nil
}

// RetryFailedIntegrationEvent 在单事务内把当前租户的一条 dead 行重置为可重试，
// 并写业务审计；不修改 payload、不标记 delivered，避免伪造投递成功。
// 参数：tenantID 来自会话主体，kind/id 定位死信，actorID 写入审计操作者。
// 返回值：found=false 表示该租户没有这条 dead 行；审计写入失败会回滚重试状态。
func (r *Repository) RetryFailedIntegrationEvent(ctx context.Context, tenantID, kind, id, actorID string) (bool, error) {
	model, ok := integrationOutboxModel(kind)
	if !ok {
		return false, application.ErrValidation
	}
	found := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var prior struct {
			Attempts  uint
			LastError *string
		}
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Model(model).
			Select("attempts, last_error").
			Where("tenant_id = ? AND id = ? AND delivery_status = ?", tenantID, id, "dead").
			Take(&prior).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		updated := tx.Model(model).
			Where("tenant_id = ? AND id = ? AND delivery_status = ?", tenantID, id, "dead").
			Updates(map[string]any{
				"delivery_status": "pending",
				"attempts":        0,
				"next_attempt_at": now,
				"locked_at":       nil,
				"last_error":      nil,
			})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return nil
		}
		payload, marshalErr := json.Marshal(map[string]any{
			"kind": kind, "id": id, "event": integrationOutboxEvent(kind),
			"previous_attempts": prior.Attempts, "previous_last_error": stringValue(prior.LastError),
		})
		if marshalErr != nil {
			return marshalErr
		}
		meta := auditctx.FromContext(tx.Statement.Context)
		requestID := meta.RequestID
		actor := strings.TrimSpace(actorID)
		if actor == "" {
			actor = meta.ActorID
		}
		if actor == "" {
			actor = "SYSTEM"
		}
		if auditErr := insertAuditOutbox(tx, tenantID, newID(), AuditOutboxEntry{
			ActorID: actor, Action: "CONTRACT_MANAGEMENT:INTEGRATION_OUTBOX_RETRY",
			ResourceType: "integration_outbox", ResourceID: id,
			Payload: payload, LoginIP: meta.LoginIP, ClientIP: meta.ClientIP, IPSource: meta.IPSource, RequestID: requestID,
		}); auditErr != nil {
			return auditErr
		}
		found = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

// CountDeadIntegrationOutboxEvents 汇总四类集成 Outbox 处于 dead 状态的事件数量。
// 就绪探针没有租户上下文，因此按部署全租户聚合；只返回计数，不读取 id/payload/错误详情，
// 未认证探针无法据此拿到业务数据。表名仍由 kind 白名单固定，不接受调用方输入。
// 参数：ctx 为查询上下文，由调用方设置超时。
// 返回值：kind→dead 数量，四类边固定齐全；任一查询失败即返回错误，不以 0 掩盖。
func (r *Repository) CountDeadIntegrationOutboxEvents(ctx context.Context) (map[string]int64, error) {
	kinds := application.IntegrationOutboxKinds()
	counts := make(map[string]int64, len(kinds))
	for _, kind := range kinds {
		model, ok := integrationOutboxModel(kind)
		if !ok {
			return nil, application.ErrValidation
		}
		var count int64
		if err := r.db.WithContext(ctx).Model(model).
			Where("delivery_status = ?", "dead").
			Count(&count).Error; err != nil {
			return nil, err
		}
		counts[kind] = count
	}
	return counts, nil
}

// integrationOutboxModel 返回受管表的 GORM 模型；未知 kind 返回 false。
func integrationOutboxModel(kind string) (any, bool) {
	switch kind {
	case application.IntegrationOutboxFinancial:
		return &financialEffectiveOutboxRecord{}, true
	case application.IntegrationOutboxResponsibility:
		return &contractResponsibilityOutbox{}, true
	case application.IntegrationOutboxProject:
		return &projectDeliveryOutboxRecord{}, true
	case application.IntegrationOutboxLink:
		return &opportunityLinkOutboxRecord{}, true
	default:
		return nil, false
	}
}

// integrationOutboxEvent 给出死信所属的稳定事件名，用于运维识别投递语义。
// 项目激活与 CRM 回写的历史载荷没有 event_type 字段，这里使用稳定标签。
func integrationOutboxEvent(kind string) string {
	switch kind {
	case application.IntegrationOutboxFinancial:
		return "contract.financial_effective.v1"
	case application.IntegrationOutboxResponsibility:
		return "contract.responsibility_changed.v1"
	case application.IntegrationOutboxProject:
		return "contract.activation"
	case application.IntegrationOutboxLink:
		return "crm.opportunity_contract_link"
	default:
		return ""
	}
}
