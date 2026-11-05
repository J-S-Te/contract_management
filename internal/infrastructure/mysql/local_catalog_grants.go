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

type localCatalogGrantRecord struct {
	TenantID    string `gorm:"primaryKey"`
	EntryKind   string `gorm:"primaryKey"`
	EntryID     string `gorm:"primaryKey"`
	SubjectType string `gorm:"primaryKey"`
	SubjectID   string `gorm:"primaryKey"`
	SubjectName string
	Enabled     bool
	Version     uint64
	UpdatedBy   string
	UpdatedAt   time.Time
}

// TableName 指定独立目录授权事实表，不修改合同授权或归属。
// 参数：无。
// 返回值：固定目录授权表名。
func (localCatalogGrantRecord) TableName() string { return "con_local_catalog_grant" }

// managedCatalogEntry 按管理员真实范围确认条目，必要时与授权更新共同锁定。
// 参数：tx为当前数据库，filter为可信管理范围，kind/id为条目，lock控制行锁。
// 返回值：范围内条目存在时无错误；越租户/不存在统一返回不存在。
func managedCatalogEntry(tx *gorm.DB, filter contract.ScopeFilter, kind, id string, lock bool) error {
	if kind != "counterparties" && kind != "categories" {
		return application.ErrValidation
	}
	model := any(&localCounterpartyRecord{})
	if kind == "categories" {
		model = &localCategoryRecord{}
	}
	query := localCatalogScope(tx.Model(model), filter).Where("id=?", id)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row struct{ ID string }
	err := query.Select("id").Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return apperrors.ErrNotFound
	}
	return err
}

// ListLocalCatalogGrants 读取真实管理范围内条目的启用及撤销授权历史。
// 参数：ctx/filter为可信上下文，kind/id为目录条目。
// 返回值：授权历史或范围/存储错误。
func (r *Repository) ListLocalCatalogGrants(ctx context.Context, filter contract.ScopeFilter, kind, id string) ([]contract.LocalCatalogGrant, error) {
	db := r.db.WithContext(ctx)
	if err := managedCatalogEntry(db, filter, kind, id, false); err != nil {
		return nil, err
	}
	var rows []localCatalogGrantRecord
	if err := db.Where("tenant_id=? AND entry_kind=? AND entry_id=?", filter.TenantID, kind, id).Order("subject_type,subject_id").Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]contract.LocalCatalogGrant, 0, len(rows))
	for _, row := range rows {
		result = append(result, catalogGrantValue(row))
	}
	return result, nil
}

// SaveLocalCatalogGrant 在条目锁内按稳定目标和预期版本保存授权并审计。
// 参数：ctx/filter为可信范围，kind/id为条目，value为已核验目标，expected为版本，actor为操作者。
// 返回值：新版本；审计或并发失败回滚，不删除授权历史。
func (r *Repository) SaveLocalCatalogGrant(ctx context.Context, filter contract.ScopeFilter, kind, id string, value contract.LocalCatalogGrant, expected uint64, actor string) (contract.LocalCatalogGrant, error) {
	if filter.TenantID == "" || actor == "" || (value.SubjectType != "USER" && value.SubjectType != "ORG") || value.SubjectID == "" {
		return value, application.ErrValidation
	}
	var row localCatalogGrantRecord
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := managedCatalogEntry(tx, filter, kind, id, true); err != nil {
			return err
		}
		query := tx.Where("tenant_id=? AND entry_kind=? AND entry_id=? AND subject_type=? AND subject_id=?", filter.TenantID, kind, id, value.SubjectType, value.SubjectID)
		err := query.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if expected != 0 || !value.Enabled {
				return apperrors.ErrVersionConflict
			}
			row = localCatalogGrantRecord{TenantID: filter.TenantID, EntryKind: kind, EntryID: id, SubjectType: value.SubjectType, SubjectID: value.SubjectID, Version: 1}
		} else if err != nil {
			return err
		} else {
			if row.Version != expected || expected == 0 {
				return apperrors.ErrVersionConflict
			}
			row.Version++
		}
		if value.Enabled {
			row.SubjectName = value.SubjectName
		}
		row.Enabled, row.UpdatedBy, row.UpdatedAt = value.Enabled, actor, time.Now().UTC()
		if err = tx.Save(&row).Error; err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"entry_kind": kind, "entry_id": id, "subject_type": row.SubjectType, "subject_id": row.SubjectID, "enabled": row.Enabled, "old_version": expected, "new_version": row.Version})
		return insertAuditOutbox(tx, filter.TenantID, newID(), AuditOutboxEntry{ActorID: actor, Action: "CONTRACT_MANAGEMENT:LOCAL_CATALOG_GRANT", ResourceType: "local_catalog_grant", ResourceID: id, Payload: payload})
	})
	return catalogGrantValue(row), err
}

// catalogGrantValue 返回独立授权事实的当前版本。
// 参数：row为已限定租户和条目的数据库行。
// 返回值：不携带合同权限或客户端归属的授权。
func catalogGrantValue(row localCatalogGrantRecord) contract.LocalCatalogGrant {
	return contract.LocalCatalogGrant{SubjectType: row.SubjectType, SubjectID: row.SubjectID, SubjectName: row.SubjectName, Enabled: row.Enabled, Version: row.Version, UpdatedAt: row.UpdatedAt}
}

// visibleCatalogQuery 仅允许Owner及有效USER/ORG授权读取启用目录。
// 参数：query为固定目录表，tenant为可信租户，kind为固定种类，visibility来自平台身份及会员目录。
// 返回值：带真实授权条件的查询；无身份时恒不匹配，不改变合同Scope。
func visibleCatalogQuery(query *gorm.DB, tenant, kind string, visibility contract.LocalCatalogVisibility) *gorm.DB {
	table := "con_local_counterparty"
	if kind == "categories" {
		table = "con_local_category"
	}
	query = query.Where(table+".tenant_id=? AND "+table+".enabled=TRUE", tenant)
	if tenant == "" || visibility.IdentityID == "" {
		return query.Where("1=0")
	}
	target := "(g.subject_type='USER' AND g.subject_id=?)"
	args := []any{visibility.IdentityID, kind, visibility.IdentityID}
	if len(visibility.OrganizationIDs) > 0 {
		target += " OR (g.subject_type='ORG' AND g.subject_id IN ?)"
		args = append(args, visibility.OrganizationIDs)
	}
	return query.Where("("+table+".owner_identity_id=? OR EXISTS(SELECT 1 FROM con_local_catalog_grant g WHERE g.tenant_id="+table+".tenant_id AND g.entry_kind=? AND g.entry_id="+table+".id AND g.enabled=TRUE AND ("+target+")))", args...)
}

// ListVisibleLocalCounterparties 列出当前Owner或显式授权且启用的交易对方。
// 参数：ctx/tenant为可信范围，visibility为真实会员，keyword/limit为有界检索。
// 返回值：可见目录或SQL错误。
func (r *Repository) ListVisibleLocalCounterparties(ctx context.Context, tenant string, visibility contract.LocalCatalogVisibility, keyword string, limit int) ([]contract.LocalCounterparty, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	query := visibleCatalogQuery(r.db.WithContext(ctx).Model(&localCounterpartyRecord{}), tenant, "counterparties", visibility)
	if keyword != "" {
		// 授权可见列表与 owner 列表共用同一转义口径，避免同一关键字在两条链路命中不同行。
		query = query.Where("name LIKE ?", literalLikePattern(keyword))
	}
	var rows []localCounterpartyRecord
	if err := query.Order("name,id").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]contract.LocalCounterparty, 0, len(rows))
	for _, row := range rows {
		values = append(values, localCounterpartyValue(row))
	}
	return values, nil
}

// GetVisibleLocalCounterparty 在引用校验时再次执行真实授权和启用检查。
// 参数：ctx/tenant为可信范围，visibility为真实主体，id为条目。
// 返回值：当前快照或统一不存在错误。
func (r *Repository) GetVisibleLocalCounterparty(ctx context.Context, tenant string, visibility contract.LocalCatalogVisibility, id string) (contract.LocalCounterparty, error) {
	var row localCounterpartyRecord
	err := visibleCatalogQuery(r.db.WithContext(ctx).Model(&localCounterpartyRecord{}), tenant, "counterparties", visibility).Where("id=?", id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = apperrors.ErrNotFound
	}
	return localCounterpartyValue(row), err
}

// ListVisibleLocalCategories 列出当前Owner或显式授权且启用的类别。
// 参数：ctx/tenant为可信范围，visibility为真实会员，keyword/limit为有界检索。
// 返回值：可见类别或SQL错误。
func (r *Repository) ListVisibleLocalCategories(ctx context.Context, tenant string, visibility contract.LocalCatalogVisibility, keyword string, limit int) ([]contract.LocalCategory, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	query := visibleCatalogQuery(r.db.WithContext(ctx).Model(&localCategoryRecord{}), tenant, "categories", visibility)
	if keyword != "" {
		pattern := literalLikePattern(keyword)
		query = query.Where("(name LIKE ? OR code LIKE ?)", pattern, pattern)
	}
	var rows []localCategoryRecord
	if err := query.Order("code,id").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]contract.LocalCategory, 0, len(rows))
	for _, row := range rows {
		values = append(values, localCategoryValue(row))
	}
	return values, nil
}

// GetVisibleLocalCategory 在类别引用解析时检查Owner/授权、启用和当前租户。
// 参数：ctx/tenant为可信范围，visibility为真实主体，id为类别。
// 返回值：当前类别或统一不存在错误。
func (r *Repository) GetVisibleLocalCategory(ctx context.Context, tenant string, visibility contract.LocalCatalogVisibility, id string) (contract.LocalCategory, error) {
	var row localCategoryRecord
	err := visibleCatalogQuery(r.db.WithContext(ctx).Model(&localCategoryRecord{}), tenant, "categories", visibility).Where("id=?", id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = apperrors.ErrNotFound
	}
	return localCategoryValue(row), err
}
