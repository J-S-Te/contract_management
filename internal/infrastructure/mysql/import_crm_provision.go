package mysql

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
)

// importCRMProvisionRecord 映射 con_import_crm_provision 表（迁移 000043）：
// 主键即 (tenant_id, source_system, external_record_id)，进度字段不复制 CRM 业务字段。
type importCRMProvisionRecord struct {
	TenantID           string `gorm:"primaryKey"`
	SourceSystem       string `gorm:"primaryKey"`
	ExternalRecordID   string `gorm:"primaryKey"`
	BatchID            *string
	CustomerID         *uint64
	CustomerCreated    bool
	OpportunityID      *uint64
	OpportunityCreated bool
	State              string
	Attempt            int
	LastErrorCode      *string
	LastErrorMessage   *string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// TableName 指定历史导入 CRM 开立台账表。
// 参数：无。
// 返回值：固定表名。
func (importCRMProvisionRecord) TableName() string { return "con_import_crm_provision" }

// importCRMProvisionValue 把持久记录转换为领域投影。
// 参数：row 为数据库记录，可空列按零值还原。
// 返回值：不含 CRM 业务字段的进度快照。
func importCRMProvisionValue(row importCRMProvisionRecord) contract.ImportCRMProvision {
	return contract.ImportCRMProvision{
		ImportCRMProvisionKey: contract.ImportCRMProvisionKey{
			TenantID: row.TenantID, SourceSystem: row.SourceSystem, ExternalRecordID: row.ExternalRecordID,
		},
		BatchID:            stringValue(row.BatchID),
		CustomerID:         uint64Value(row.CustomerID),
		CustomerCreated:    row.CustomerCreated,
		OpportunityID:      uint64Value(row.OpportunityID),
		OpportunityCreated: row.OpportunityCreated,
		State:              row.State,
		Attempt:            row.Attempt,
		LastErrorCode:      stringValue(row.LastErrorCode),
		LastErrorMessage:   stringValue(row.LastErrorMessage),
		CreatedAt:          row.CreatedAt,
		UpdatedAt:          row.UpdatedAt,
	}
}

// importCRMProvisionScope 按幂等主键收窄查询，杜绝跨租户读写。
// 参数：query 为台账查询，key 为可信幂等键。
// 返回值：已绑定三列主键条件的查询。
func importCRMProvisionScope(query *gorm.DB, key contract.ImportCRMProvisionKey) *gorm.DB {
	return query.Where("tenant_id=? AND source_system=? AND external_record_id=?",
		key.TenantID, key.SourceSystem, key.ExternalRecordID)
}

// GetImportCRMProvision 按幂等键读取开立进度。
// 参数：ctx 限定请求，key 为 (tenant_id, source_system, external_record_id)。
// 返回值：进度记录；不存在返回可判断的 apperrors.ErrNotFound，其余为存储错误。
func (r *Repository) GetImportCRMProvision(ctx context.Context, key contract.ImportCRMProvisionKey) (contract.ImportCRMProvision, error) {
	if err := key.Validate(); err != nil {
		return contract.ImportCRMProvision{}, err
	}
	var row importCRMProvisionRecord
	err := importCRMProvisionScope(r.db.WithContext(ctx), key).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.ImportCRMProvision{}, apperrors.ErrNotFound
	}
	if err != nil {
		return contract.ImportCRMProvision{}, err
	}
	return importCRMProvisionValue(row), nil
}

// UpsertStartImportCRMProvision 开启或重试一条开立记录：不存在时创建 PENDING/attempt=0；
// 已存在时只把 attempt+1 并清空上次失败痕迹，绝不回退或重置已完成的进度。
// 参数：ctx 限定请求，key 为幂等键，batchID 为本次导入批次（空串表示保持原值）。
// 返回值：写入后的权威记录；键非法或存储失败返回错误。
func (r *Repository) UpsertStartImportCRMProvision(ctx context.Context, key contract.ImportCRMProvisionKey, batchID string) (contract.ImportCRMProvision, error) {
	if err := key.Validate(); err != nil {
		return contract.ImportCRMProvision{}, err
	}
	if len(batchID) > 26 {
		return contract.ImportCRMProvision{}, contract.ErrImportCRMProvisionKey
	}
	now := time.Now().UTC()
	var row importCRMProvisionRecord
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 单语句幂等起点：唯一键冲突时只增加 attempt 并刷新批次/时间，
		// 便于并发或重跑时 CAS 的 attempt 前置值严格递增。
		if err := tx.Exec(`INSERT INTO con_import_crm_provision
 (tenant_id, source_system, external_record_id, batch_id, customer_id, customer_created, opportunity_id, opportunity_created, state, attempt, last_error_code, last_error_message, created_at, updated_at)
VALUES (?, ?, ?, ?, NULL, 0, NULL, 0, ?, 0, NULL, NULL, ?, ?)
ON DUPLICATE KEY UPDATE batch_id=COALESCE(VALUES(batch_id), batch_id), attempt=attempt+1,
 last_error_code=NULL, last_error_message=NULL, updated_at=VALUES(updated_at)`,
			key.TenantID, key.SourceSystem, key.ExternalRecordID, optionalString(batchID),
			contract.ImportCRMProvisionPending, now, now).Error; err != nil {
			return err
		}
		return importCRMProvisionScope(tx, key).Take(&row).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.ImportCRMProvision{}, apperrors.ErrNotFound
	}
	if err != nil {
		return contract.ImportCRMProvision{}, err
	}
	return importCRMProvisionValue(row), nil
}

// AdvanceImportCRMProvision 以 state+attempt 为 CAS 条件顺序推进状态，
// 并可在同一条 UPDATE 里写入本次取得的 CRM 客户/商机标识。
// 参数：ctx 限定请求，key 为幂等键，advance 携带 From/To/ExpectedAttempt 与可选标识。
// 返回值：推进后的权威记录；前置状态或 attempt 已变化返回 apperrors.ErrStateConflict，
// 非法推进（跳步、终态回退、缺少必要标识）返回领域错误且不写库。
func (r *Repository) AdvanceImportCRMProvision(ctx context.Context, key contract.ImportCRMProvisionKey, advance contract.ImportCRMProvisionAdvance) (contract.ImportCRMProvision, error) {
	if err := key.Validate(); err != nil {
		return contract.ImportCRMProvision{}, err
	}
	if err := advance.Validate(); err != nil {
		return contract.ImportCRMProvision{}, err
	}
	updates := map[string]any{
		"state": advance.To, "attempt": advance.ExpectedAttempt + 1, "updated_at": time.Now().UTC(),
		// 推进成功即清空上次失败痕迹，避免 COMPLETED 记录仍带旧错误码。
		"last_error_code": nil, "last_error_message": nil,
	}
	if advance.CustomerID != 0 {
		updates["customer_id"] = advance.CustomerID
		updates["customer_created"] = advance.CustomerCreated
	}
	if advance.OpportunityID != 0 {
		updates["opportunity_id"] = advance.OpportunityID
		updates["opportunity_created"] = advance.OpportunityCreated
	}
	result := importCRMProvisionScope(r.db.WithContext(ctx).Model(&importCRMProvisionRecord{}), key).
		Where("state=? AND attempt=?", advance.From, advance.ExpectedAttempt).
		Updates(updates)
	if result.Error != nil {
		return contract.ImportCRMProvision{}, result.Error
	}
	if result.RowsAffected == 0 {
		// 未命中说明并发者已推进/阻塞，或调用方的 attempt 已过期。
		return contract.ImportCRMProvision{}, apperrors.ErrStateConflict
	}
	return r.GetImportCRMProvision(ctx, key)
}

// ResetImportCRMProvision 把上一次尝试的 BLOCKED 复位为 PENDING，开始同一幂等键的新一次尝试。// 已取得的 customer_id / opportunity_id 必须保留：重试据此复用客户与商机，不重复开立。
// 参数：ctx 限定请求，key 为幂等键，expectedAttempt 为调用方读到的 attempt，用作 CAS 前置条件。
// 返回值：复位后的权威记录；状态不是 BLOCKED 或 attempt 已变化返回 apperrors.ErrStateConflict。
func (r *Repository) ResetImportCRMProvision(ctx context.Context, key contract.ImportCRMProvisionKey, expectedAttempt int) (contract.ImportCRMProvision, error) {
	if err := key.Validate(); err != nil {
		return contract.ImportCRMProvision{}, err
	}
	if expectedAttempt < 0 {
		return contract.ImportCRMProvision{}, contract.ErrImportCRMProvisionState
	}
	result := importCRMProvisionScope(r.db.WithContext(ctx).Model(&importCRMProvisionRecord{}), key).
		Where("state=? AND attempt=?", contract.ImportCRMProvisionBlocked, expectedAttempt).
		Updates(map[string]any{
			"state":              contract.ImportCRMProvisionPending,
			"attempt":            expectedAttempt + 1,
			"last_error_code":    nil,
			"last_error_message": nil,
			"updated_at":         time.Now().UTC(),
		})
	if result.Error != nil {
		return contract.ImportCRMProvision{}, result.Error
	}
	if result.RowsAffected == 0 {
		return contract.ImportCRMProvision{}, apperrors.ErrStateConflict
	}
	return r.GetImportCRMProvision(ctx, key)
}

// RecordImportCRMProvisionError 把一次失败记为 BLOCKED 并保留可读原因。// 参数：ctx 限定请求，key 为幂等键，from/expectedAttempt 为 CAS 前置状态与尝试号，
// code 为稳定错误码（≤64），message 为可读原因（按列宽截断到 512 字节）。
// 返回值：写入后的权威记录；CAS 未命中返回 apperrors.ErrStateConflict，
// 错误码非法返回领域错误且不写库。
func (r *Repository) RecordImportCRMProvisionError(ctx context.Context, key contract.ImportCRMProvisionKey, from string, expectedAttempt int, code, message string) (contract.ImportCRMProvision, error) {
	if err := key.Validate(); err != nil {
		return contract.ImportCRMProvision{}, err
	}
	if err := contract.ValidateImportCRMProvisionErrorCode(code); err != nil {
		return contract.ImportCRMProvision{}, err
	}
	if expectedAttempt < 0 {
		return contract.ImportCRMProvision{}, contract.ErrImportCRMProvisionState
	}
	result := importCRMProvisionScope(r.db.WithContext(ctx).Model(&importCRMProvisionRecord{}), key).
		Where("state=? AND attempt=?", from, expectedAttempt).
		Updates(map[string]any{
			"state":              contract.ImportCRMProvisionBlocked,
			"last_error_code":    code,
			"last_error_message": contract.TruncateImportCRMProvisionMessage(message),
			"updated_at":         time.Now().UTC(),
		})
	if result.Error != nil {
		return contract.ImportCRMProvision{}, result.Error
	}
	if result.RowsAffected == 0 {
		return contract.ImportCRMProvision{}, apperrors.ErrStateConflict
	}
	return r.GetImportCRMProvision(ctx, key)
}

// UpdateImportContractOpportunity 把开立通道取得的商机引用回写到刚创建的导入合同。
// 为什么只改商机两列而不动 version：历史确认链路 confirmHistoricalItem 要求
// version == 1 且 status == draft 才允许写入 archived 终态，回写若递增 version
// 会让整批确认永久失败。因此这里是一条受白名单条件约束的幂等原地更新。
// 参数：ctx 限定请求，tenantID/contractID 定位合同，opportunityID 为 CRM 商机 ID，
// opportunityName 为商机名称，actor 为真实操作者（仅用于 updated_by 审计列）。
// 返回值：合同不存在或已离开可回写基线时返回 apperrors.ErrStateConflict，其余为存储错误。
func (r *Repository) UpdateImportContractOpportunity(ctx context.Context, tenantID, contractID string, opportunityID uint64, opportunityName, actor string) error {
	if tenantID == "" || contractID == "" || opportunityID == 0 {
		return apperrors.ErrStateConflict
	}
	result := r.db.WithContext(ctx).Model(&contractRecord{}).
		Where("tenant_id=? AND id=? AND version=1 AND status=? AND origin_kind=?", tenantID, contractID, contract.StatusDraft, "IMPORTED").
		Updates(map[string]any{
			"opportunity_id":   stringPtr(strconv.FormatUint(opportunityID, 10)),
			"opportunity_name": stringPtr(opportunityName),
			"updated_at":       time.Now().UTC(),
			"updated_by":       actor,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return apperrors.ErrStateConflict
	}
	return nil
}
