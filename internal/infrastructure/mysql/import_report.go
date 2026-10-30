package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
	"time"
)

// ImportReportSource 以同一只读RR事务提供获准历史集合和规范快照摘要。
// 参数：ctx为上下文，tenant来自独立验签的机器身份。
// 返回值：最多1000完整历史行，超量或未知政策失败关闭，不截断发布。
func (r *Repository) ImportReportSource(ctx context.Context, tenant string) (result contract.ImportReportSource, err error) {
	result = contract.ImportReportSource{SchemaVersion: 1, TenantID: tenant, Complete: true, Items: []contract.ImportReportRow{}}
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []contractRecord
		if e := tx.Omit("rendered_document", "content", "template_values_json", "service_items_json", "systems_json").Where("tenant_id=? AND import_report_release_version>0", tenant).Order("origin_import_item_id,id").Limit(1001).Find(&rows).Error; e != nil {
			return e
		}
		if len(rows) > 1000 {
			return contract.ErrImportManifest
		}
		for _, row := range rows {
			var policy contract.ImportPolicy
			if row.OriginKind != "IMPORTED" || row.OriginImportItemID == nil || row.ImportSourceRevision == 0 || row.ImportConfirmedAt == nil || json.Unmarshal(row.ImportPolicyJSON, &policy) != nil || contract.ValidateImportPolicy(contract.ImportMode(row.ImportMode), &policy) != nil || (row.ImportReportRequiredChannels != "API_ONLY" && row.ImportReportRequiredChannels != "API_AND_SOURCE_FACTS") {
				return contract.ErrImportManifest
			}
			result.Items = append(result.Items,
				contract.ImportReportRow{ItemID: *row.OriginImportItemID,
					SourceRevision:       row.ImportSourceRevision,
					ReportReleaseVersion: row.ImportReportReleaseVersion,
					RequiredChannels:     row.ImportReportRequiredChannels,
					ImportMode:           row.ImportMode,
					ReportMode:           policy.ReportMode,
					ContractID:           row.ID,
					ContractVersion:      row.Version,
					Status:               row.Status,
					StartDate:            importDate(row.StartDate),
					EndDate:              importDate(row.EndDate),
					OwnerUserID:          row.OwnerUserID,
					OwnerOrgID:           valueOrEmpty(row.OwnerOrgID),
					CRMCustomerID:        row.CRMCustomerID,
					OpportunityID:        row.OpportunityID,
					AmountMinor:          row.AmountMinor,
					Currency:             row.Currency,
					CreatedAt:            row.CreatedAt.UTC()})
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err == nil {
		result.SourceSnapshotID, err = contract.ImportDigest(result.Items)
	}
	return
}

// importDate 输出原业务DATE，不以导入时间填充缺失日期。
// 参数：value为数据库可空业务日期。
// 返回值：YYYY-MM-DD或nil。
func importDate(value *time.Time) *string {
	if value == nil {
		return nil
	}
	date := value.Format("2006-01-02")
	return &date
}
