package mysql

import (
	"context"
	"errors"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// LocalCatalogOwner 是仅供本地目录使用的可持久化归属与版本。
type LocalCatalogOwner struct {
	TenantID        string `gorm:"primaryKey"`
	ID              string `gorm:"primaryKey"`
	Enabled         bool
	Version         uint64
	OwnerIdentityID string
	OwnerOrgID      string
	OwnerProjectID  string
	CreatedBy       string
	UpdatedAt       time.Time
}

type localCounterpartyRecord struct {
	LocalCatalogOwner `gorm:"embedded"`
	Name              string
}

// TableName 指定合同自有交易对方表，绝不写CRM客户表。
// 参数：无。
// 返回值：固定本地表名。
func (localCounterpartyRecord) TableName() string { return "con_local_counterparty" }

type localCategoryRecord struct {
	LocalCatalogOwner `gorm:"embedded"`
	Code              string
	Name              string
}

// TableName 指定合同自有类别表，不复用服务项身份。
// 参数：无。
// 返回值：固定本地表名。
func (localCategoryRecord) TableName() string { return "con_local_category" }

// localCatalogScope 将真实租户及SELF/组织/项目授权同时应用到本地目录。
// 参数：query为目录查询，filter为应用层从会话解析的范围。
// 返回值：已收窄查询；空范围恒不匹配，绝不表示全量。
func localCatalogScope(query *gorm.DB, filter contract.ScopeFilter) *gorm.DB {
	query = query.Where("tenant_id=?", filter.TenantID)
	if filter.TenantID == "" {
		return query.Where("1=0")
	}
	if filter.AllowAll {
		return query
	}
	scope := query.Session(&gorm.Session{NewDB: true}).Where("1=0")
	if filter.AllowSelf && filter.IdentityID != "" {
		scope = scope.Or("owner_identity_id=?", filter.IdentityID)
	}
	if len(filter.OrganizationIDs) > 0 {
		scope = scope.Or("owner_org_id IN ?", filter.OrganizationIDs)
	}
	if len(filter.ProjectIDs) > 0 {
		scope = scope.Or("owner_project_id IN ?", filter.ProjectIDs)
	}
	return query.Where(scope)
}

// newLocalCatalogOwner 以可信范围和操作者建立本地目录归属。
// 参数：filter为真实范围，actor为会话用户ID。
// 返回值：服务端生成的身份、归属及初始版本。
func newLocalCatalogOwner(filter contract.ScopeFilter, actor string) LocalCatalogOwner {
	value := LocalCatalogOwner{TenantID: filter.TenantID, ID: newID(), Enabled: true, Version: 1, OwnerIdentityID: filter.IdentityID, CreatedBy: actor, UpdatedAt: time.Now().UTC()}
	if !filter.AllowSelf && len(filter.OrganizationIDs) > 0 {
		value.OwnerOrgID = filter.OrganizationIDs[0]
	}
	if !filter.AllowSelf && len(filter.ProjectIDs) > 0 {
		value.OwnerProjectID = filter.ProjectIDs[0]
	}
	return value
}

// ListLocalCounterparties 返回当前授权范围的有界交易对方列表。
// 参数：ctx为请求，filter为可信范围，keyword为名称检索，limit为最多200条的页大小。
// 返回值：目录记录或查询错误。
func (r *Repository) ListLocalCounterparties(ctx context.Context, filter contract.ScopeFilter, keyword string, limit int) ([]contract.LocalCounterparty, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	query := localCatalogScope(r.db.WithContext(ctx).Model(&localCounterpartyRecord{}), filter)
	if keyword != "" {
		// 目录检索同样是用户输入：% _ \ 必须按字面量匹配，不能当通配符扫描。
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

// GetLocalCounterparty 按可信范围解析稳定本地交易对方引用。
// 参数：ctx为请求，filter为真实范围，id为本地目录ID。
// 返回值：记录或不存在/存储错误；跨范围与不存在使用同一错误。
func (r *Repository) GetLocalCounterparty(ctx context.Context, filter contract.ScopeFilter, id string) (contract.LocalCounterparty, error) {
	var row localCounterpartyRecord
	err := localCatalogScope(r.db.WithContext(ctx), filter).Where("id=?", id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = apperrors.ErrNotFound
	}
	return localCounterpartyValue(row), err
}

// SaveLocalCounterparty 在真实范围与版本锁下保存或停用本地交易对方并写同事务审计。
// 参数：ctx为请求，filter为可信范围，value为白名单字段，expected为旧版本，actor为用户ID。
// 返回值：持久化记录；任何失败不保留部分目录或审计。
func (r *Repository) SaveLocalCounterparty(ctx context.Context, filter contract.ScopeFilter, value contract.LocalCounterparty, expected uint64, actor string) (contract.LocalCounterparty, error) {
	if filter.TenantID == "" || actor == "" {
		return value, apperrors.ErrNotFound
	}
	var row localCounterpartyRecord
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if value.ID == "" {
			if expected != 0 {
				return apperrors.ErrVersionConflict
			}
			row.LocalCatalogOwner = newLocalCatalogOwner(filter, actor)
		} else {
			err := localCatalogScope(tx.Clauses(clause.Locking{Strength: "UPDATE"}), filter).Where("id=?", value.ID).Take(&row).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apperrors.ErrNotFound
			}
			if err != nil {
				return err
			}
			if expected == 0 || row.Version != expected {
				return apperrors.ErrVersionConflict
			}
			row.Version++
		}
		row.Name = value.Name
		row.Enabled = value.Enabled
		row.UpdatedAt = time.Now().UTC()
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		return insertAuditOutbox(tx, filter.TenantID, newID(), AuditOutboxEntry{ActorID: actor, Action: "CONTRACT_MANAGEMENT:LOCAL_COUNTERPARTY_SAVE", ResourceType: "local_counterparty", ResourceID: row.ID})
	})
	return localCounterpartyValue(row), err
}

// localCounterpartyValue 将持久记录转换为独立本地引用，保留真实租户供应用层复核。
// 参数：row为目录记录。
// 返回值：本地交易对方，不生成CRM身份。
func localCounterpartyValue(row localCounterpartyRecord) contract.LocalCounterparty {
	return contract.LocalCounterparty{ID: row.ID, TenantID: row.TenantID, Name: row.Name, Enabled: row.Enabled, Version: row.Version, OwnerIdentityID: row.OwnerIdentityID, OwnerOrgID: row.OwnerOrgID, CreatedBy: row.CreatedBy, UpdatedAt: row.UpdatedAt}
}

// ListLocalCategories 返回授权范围内的有界类别目录。
// 参数：ctx为请求，filter为真实范围，keyword为名称或编码检索，limit为最多200条的数量。
// 返回值：类别记录或查询错误。
func (r *Repository) ListLocalCategories(ctx context.Context, filter contract.ScopeFilter, keyword string, limit int) ([]contract.LocalCategory, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	query := localCatalogScope(r.db.WithContext(ctx).Model(&localCategoryRecord{}), filter)
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

// GetLocalCategory 按租户及真实范围解析本地稳定类别。
// 参数：ctx为请求，filter为真实范围，id为本地类别ID。
// 返回值：类别或不存在/存储错误。
func (r *Repository) GetLocalCategory(ctx context.Context, filter contract.ScopeFilter, id string) (contract.LocalCategory, error) {
	var row localCategoryRecord
	err := localCatalogScope(r.db.WithContext(ctx), filter).Where("id=?", id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = apperrors.ErrNotFound
	}
	return localCategoryValue(row), err
}

// SaveLocalCategory 创建、重命名或停用类别，稳定编码不可改写。
// 参数：ctx为请求，filter为真实范围，value为白名单字段，expected为旧版本，actor为会话用户。
// 返回值：新版本；冲突、越界或审计失败回滚。
func (r *Repository) SaveLocalCategory(ctx context.Context, filter contract.ScopeFilter, value contract.LocalCategory, expected uint64, actor string) (contract.LocalCategory, error) {
	if filter.TenantID == "" || actor == "" {
		return value, apperrors.ErrNotFound
	}
	var row localCategoryRecord
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if value.ID == "" {
			if expected != 0 {
				return apperrors.ErrVersionConflict
			}
			row.LocalCatalogOwner = newLocalCatalogOwner(filter, actor)
			row.Code = value.Code
		} else {
			err := localCatalogScope(tx.Clauses(clause.Locking{Strength: "UPDATE"}), filter).Where("id=?", value.ID).Take(&row).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apperrors.ErrNotFound
			}
			if err != nil {
				return err
			}
			if expected == 0 || row.Version != expected {
				return apperrors.ErrVersionConflict
			}
			if row.Code != value.Code {
				return apperrors.ErrStateConflict
			}
			row.Version++
		}
		row.Name, row.Enabled, row.UpdatedAt = value.Name, value.Enabled, time.Now().UTC()
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		return insertAuditOutbox(tx, filter.TenantID, newID(), AuditOutboxEntry{ActorID: actor, Action: "CONTRACT_MANAGEMENT:LOCAL_CATEGORY_SAVE", ResourceType: "local_category", ResourceID: row.ID})
	})
	return localCategoryValue(row), err
}

// localCategoryValue 返回带独立类别身份及版本的权威快照。
// 参数：row为数据库目录记录。
// 返回值：本地类别。
func localCategoryValue(row localCategoryRecord) contract.LocalCategory {
	return contract.LocalCategory{ID: row.ID, TenantID: row.TenantID, Code: row.Code, Name: row.Name, Enabled: row.Enabled, Version: row.Version, OwnerIdentityID: row.OwnerIdentityID, OwnerOrgID: row.OwnerOrgID, CreatedBy: row.CreatedBy, UpdatedAt: row.UpdatedAt}
}
