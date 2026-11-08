package mysql

import (
	"context"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PrepareImportContractNumber 为CRM验真预留系统号，保留草稿与历史确认门禁。
// 参数：ctx为上下文，tenant/batch/revision/epoch为执行围栏，item/id为不可变来源及合同。
// 返回值：稳定系统号；来源、状态或围栏不符返回冲突，写入失败整体回滚。
func (r *Repository) PrepareImportContractNumber(ctx context.Context, tenant, batch string,
	revision, epoch uint64, item, id string) (number string, err error) {
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row importBatchRecord
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND id=?", tenant, batch).Take(&row).Error; e != nil {
			return e
		}
		b, e := decodeImportBatch(row)
		if e != nil {
			return e
		}
		if b.State != "RUNNING" || b.Revision != revision || b.Epoch != epoch {
			return application.ErrImportConflict
		}
		var matched bool
		for _, entry := range b.Items {
			if entry.ID == item && entry.ContractID == id && !entry.Confirmed &&
				(entry.Item.Mode == contract.ImportArchiveOnly || entry.Item.Mode == contract.ImportHistoricalClosed) &&
				contract.ValidateHistoryEvidence(entry.Item) == nil {
				matched = true
				break
			}
		}
		if !matched {
			return application.ErrImportConflict
		}
		var c contractRecord
		if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Omit("rendered_document", "content", "template_values_json").Where("tenant_id=? AND id=?", tenant, id).Take(&c).Error; e != nil {
			return e
		}
		if c.Version != 1 || c.Status != "draft" || c.OriginKind != "IMPORTED" || c.OriginImportItemID == nil ||
			*c.OriginImportItemID != item || c.CreatedBy != b.ActorID || c.SourceFileStatus != "READY" || (c.CRMCustomerID == nil || *c.CRMCustomerID == 0) {
			return application.ErrImportConflict
		}
		if c.ContractNumber != nil {
			number = strings.TrimSpace(*c.ContractNumber)
		}
		if number != "" {
			return nil
		}
		number, e = allocateContractNumberInTx(tx, tenant, id, c.ContractNumberFormat, time.Now().UTC())
		if e != nil {
			return e
		}
		result := tx.Model(&contractRecord{}).Where("tenant_id=? AND id=? AND version=1", tenant, id).Update("contract_number", number)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return application.ErrImportConflict
		}
		return nil
	})
	return number, err
}
