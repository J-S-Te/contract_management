package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type standardPriceRecord struct {
	ID        string `gorm:"primaryKey"`
	TenantID  string
	Status    string
	Version   uint64
	DataJSON  []byte `gorm:"column:data_json;type:json"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 指定标准价版本表。
// 参数：无。
// 返回值：权威表名。
func (standardPriceRecord) TableName() string { return "con_standard_price" }

// ListStandardPrices 查询租户内目录的有界分页。
// 参数：ctx/tenant为请求边界，status为可选状态，offset/limit为已校验分页。
// 返回值：目录记录或数据库/结构错误。
func (r *Repository) ListStandardPrices(ctx context.Context, filter contract.ScopeFilter, status string, offset, limit int) ([]contract.StandardPrice, error) {
	q := r.db.WithContext(ctx).Where("tenant_id=?", filter.TenantID)
	if !filter.AllowAll {
		q = q.Where("(JSON_UNQUOTE(JSON_EXTRACT(data_json, '$.organization_id')) IS NULL OR JSON_UNQUOTE(JSON_EXTRACT(data_json, '$.organization_id')) = '' OR JSON_UNQUOTE(JSON_EXTRACT(data_json, '$.organization_id')) IN ?)", filter.OrganizationIDs)
	}
	if status != "" {
		q = q.Where("status=?", status)
	}
	var rows []standardPriceRecord
	if err := q.Order("id DESC").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]contract.StandardPrice, 0, len(rows))
	for _, row := range rows {
		var p contract.StandardPrice
		if err := json.Unmarshal(row.DataJSON, &p); err != nil {
			return nil, err
		}
		p.TenantID = filter.TenantID
		out = append(out, p)
	}
	return out, nil
}

// GetStandardPrice 读取当前租户的唯一价格版本。
// 参数：ctx/tenant为可信请求边界，id为目录版本身份。
// 返回值：标准价；不存在或结构错误时返回错误。
func (r *Repository) GetStandardPrice(ctx context.Context, tenant, id string) (contract.StandardPrice, error) {
	var row standardPriceRecord
	var p contract.StandardPrice
	err := r.db.WithContext(ctx).Where("tenant_id=? AND id=?", tenant, id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return p, apperrors.ErrNotFound
	}
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(row.DataJSON, &p)
	p.TenantID = tenant
	return p, err
}

// SaveStandardPrice 以行锁和乐观版本保存草稿及发布状态，同事务写审计。
// 参数：ctx为请求，p为已校验目录，expected为旧版本，actor为操作者，action为命令。
// 返回值：新版本；已发布修改、自审、并发或存储失败时全部回滚。
func (r *Repository) SaveStandardPrice(ctx context.Context, p contract.StandardPrice, expected uint64, actor, action string) (contract.StandardPrice, error) {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		var row standardPriceRecord
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND id=?", p.TenantID, p.ID).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if expected != 0 || action != "SAVE" {
				return apperrors.ErrVersionConflict
			}
			p.CreatedBy = actor
			p.CreatedAt = now
			p.Status = "DRAFT"
			p.PublishedBy = ""
			p.PublishedAt = nil
			p.Version = 1
		} else {
			if err != nil {
				return err
			}
			if row.Version != expected {
				return apperrors.ErrVersionConflict
			}
			var current contract.StandardPrice
			if err := json.Unmarshal(row.DataJSON, &current); err != nil {
				return err
			}
			p.CreatedBy = current.CreatedBy
			p.CreatedAt = current.CreatedAt
			p.Version = expected + 1
			switch action {
			case "SAVE":
				if row.Status != "DRAFT" {
					return apperrors.ErrStateConflict
				}
				p.Status = "DRAFT"
				p.PublishedBy = ""
				p.PublishedAt = nil
			case "PUBLISH":
				if row.Status != "DRAFT" {
					return apperrors.ErrStateConflict
				}
				if current.CreatedBy == actor || current.UpdatedBy == actor {
					return application.ErrForbidden
				}
				p = current
				p.TenantID = row.TenantID
				p.Status = "PUBLISHED"
				p.Version = expected + 1
				p.PublishedBy = actor
				p.PublishedAt = &now
			case "DISABLE":
				if row.Status != "PUBLISHED" {
					return apperrors.ErrStateConflict
				}
				p = current
				p.TenantID = row.TenantID
				p.Status = "DISABLED"
				p.Version = expected + 1
			default:
				return application.ErrValidation
			}
		}
		p.UpdatedBy = actor
		p.UpdatedAt = now
		data, err := json.Marshal(p)
		if err != nil {
			return err
		}
		row = standardPriceRecord{ID: p.ID, TenantID: p.TenantID, Status: p.Status, Version: p.Version, DataJSON: data, CreatedAt: p.CreatedAt, UpdatedAt: now}
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"price_id": p.ID, "version": p.Version, "status": p.Status})
		return insertAuditOutbox(tx, p.TenantID, newID(), AuditOutboxEntry{ActorID: actor, Action: "CONTRACT_MANAGEMENT:PRICE_" + action, ResourceType: "standard_price", ResourceID: p.ID, Payload: payload})
	})
	return p, err
}

// pricingJSON 将可选冻结定价编码为数据库JSON。
// 参数：p为服务端可信定价；nil表示历史未知。
// 返回值：JSON字节；nil保持SQL NULL。
func pricingJSON(p *contract.Pricing) []byte {
	if p == nil {
		return nil
	}
	data, _ := json.Marshal(p)
	return data
}

// validateFrozenFinancialPricing 纵深复验改额只改变成交行额，不改变原冻结标准依据。
// 参数：row为事务锁定的旧合同，change为待应用变更。
// 返回值：冻结身份、单价、数量、日期或完整性不一致时返回校验错误。
func validateFrozenFinancialPricing(row contractRecord, change application.ContractFinancialChange) error {
	old := contractFromRecord(row)
	if old.Pricing == nil {
		if change.Pricing != nil {
			return application.ErrValidation
		}
		return nil
	}
	if change.Pricing == nil {
		return application.ErrValidation
	}
	previous := *old.Pricing
	next := *change.Pricing
	previous.Lines = append([]contract.PricingLine(nil), previous.Lines...)
	next.Lines = append([]contract.PricingLine(nil), next.Lines...)
	if len(previous.Lines) != len(next.Lines) {
		return application.ErrValidation
	}
	for i := range previous.Lines {
		previous.Lines[i].AmountMinor = 0
		next.Lines[i].AmountMinor = 0
	}
	a, _ := json.Marshal(previous)
	b, _ := json.Marshal(next)
	if string(a) != string(b) {
		return application.ErrValidation
	}
	old.AmountMinor = change.AmountMinor
	old.Pricing = change.Pricing
	if _, err := contract.PricingTotal(old); err != nil {
		return application.ErrValidation
	}
	return nil
}
