package mysql

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/approval"
)

func (r *Repository) ListRules(ctx context.Context, tenantID string) ([]approval.Rule, error) {
	return r.listRules(ctx, tenantID, false)
}

func (r *Repository) ListEnabledRules(ctx context.Context, tenantID string) ([]approval.Rule, error) {
	return r.listRules(ctx, tenantID, true)
}

func (r *Repository) listRules(ctx context.Context, tenantID string, enabledOnly bool) ([]approval.Rule, error) {
	query := r.db.WithContext(ctx).Where("tenant_id = ?", tenantID)
	if enabledOnly {
		query = query.Where("enabled = ?", true)
	}
	var records []approvalRuleRecord
	if err := query.Order("priority DESC, id").Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]approval.Rule, 0, len(records))
	for _, record := range records {
		rule, err := ruleFromRecord(record)
		if err != nil {
			return nil, err
		}
		result = append(result, rule)
	}
	return result, nil
}

func (r *Repository) CreateRule(ctx context.Context, rule approval.Rule, actor string) error {
	record, err := ruleToRecord(rule)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	record.Version, record.CreatedAt, record.UpdatedAt = 1, now, now
	record.CreatedBy, record.UpdatedBy = actor, actor
	return r.db.WithContext(ctx).Create(&record).Error
}

func (r *Repository) UpdateRule(ctx context.Context, rule approval.Rule, actor string) error {
	record, err := ruleToRecord(rule)
	if err != nil {
		return err
	}
	result := r.db.WithContext(ctx).Model(&approvalRuleRecord{}).
		Where("tenant_id = ? AND id = ? AND version = ?", rule.TenantID, rule.ID, rule.Version).
		Updates(map[string]any{
			"name": record.Name, "priority": record.Priority, "enabled": record.Enabled,
			"expression_json": record.ExpressionJSON, "nodes_json": record.NodesJSON,
			"version": rule.Version + 1, "updated_at": time.Now().UTC(), "updated_by": actor,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return apperrors.ErrVersionConflict
	}
	return nil
}

func (r *Repository) DeleteRule(ctx context.Context, tenantID, id string, version uint64) error {
	result := r.db.WithContext(ctx).Where("tenant_id = ? AND id = ? AND version = ?", tenantID, id, version).Delete(&approvalRuleRecord{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return apperrors.ErrVersionConflict
	}
	return nil
}

func ruleToRecord(rule approval.Rule) (approvalRuleRecord, error) {
	expressionJSON, err := json.Marshal(rule.Expression)
	if err != nil {
		return approvalRuleRecord{}, err
	}
	nodesJSON, err := json.Marshal(rule.Nodes)
	if err != nil {
		return approvalRuleRecord{}, err
	}
	return approvalRuleRecord{ID: rule.ID, TenantID: rule.TenantID, Name: rule.Name, Priority: rule.Priority, Enabled: rule.Enabled, ExpressionJSON: expressionJSON, NodesJSON: nodesJSON, Version: rule.Version}, nil
}

func ruleFromRecord(record approvalRuleRecord) (approval.Rule, error) {
	rule := approval.Rule{ID: record.ID, TenantID: record.TenantID, Name: record.Name, Priority: record.Priority, Enabled: record.Enabled, Version: record.Version}
	if err := json.Unmarshal(record.ExpressionJSON, &rule.Expression); err != nil {
		return rule, fmt.Errorf("decode rule %s expression: %w", record.ID, err)
	}
	if err := json.Unmarshal(record.NodesJSON, &rule.Nodes); err != nil {
		return rule, fmt.Errorf("decode rule %s nodes: %w", record.ID, err)
	}
	return rule, nil
}
