package mysql

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (r *Repository) ListApprovedContracts(ctx context.Context, tenantID string, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var records []contractRecord
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND status IN ?", tenantID, contract.ApprovalPassedStatuses()).
		Omit("rendered_document", "template_values_json").Order("updated_at DESC").Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]contract.Contract, 0, len(records))
	for _, record := range records {
		result = append(result, contractFromRecord(record))
	}
	return r.attachPaymentTerms(ctx, tenantID, result)
}

func (r *Repository) ListApprovedContractReferences(ctx context.Context, tenantID, afterID string, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	query := r.db.WithContext(ctx).Model(&contractRecord{}).
		Where("tenant_id = ? AND status IN ?", tenantID, contract.ApprovalPassedStatuses())
	if afterID != "" {
		query = query.Where("id > ?", afterID)
	}
	var records []contractRecord
	if err := query.Select("id", "contract_number", "version").Order("id ASC").Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]contract.Contract, 0, len(records))
	for _, record := range records {
		result = append(result, contract.Contract{ID: record.ID, Number: valueOrEmpty(record.ContractNumber), Version: record.Version})
	}
	return result, nil
}

func (r *Repository) ListApprovedContractsScoped(ctx context.Context, filter contract.ScopeFilter, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var records []contractRecord
	if err := applyContractScope(r.db.WithContext(ctx).Model(&contractRecord{}), filter).
		Where("status IN ?", contract.ApprovalPassedStatuses()).Omit("rendered_document", "template_values_json").
		Order("updated_at DESC").Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]contract.Contract, 0, len(records))
	for _, record := range records {
		result = append(result, contractFromRecord(record))
	}
	return r.attachPaymentTerms(ctx, filter.TenantID, result)
}

func (r *Repository) ListApprovedContractReferencesScoped(ctx context.Context, filter contract.ScopeFilter, afterID string, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	query := applyContractScope(r.db.WithContext(ctx).Model(&contractRecord{}), filter).
		Where("status IN ?", contract.ApprovalPassedStatuses())
	if afterID != "" {
		query = query.Where("id > ?", afterID)
	}
	var records []contractRecord
	if err := query.Select("id", "contract_number", "version").Order("id ASC").Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]contract.Contract, 0, len(records))
	for _, record := range records {
		result = append(result, contract.Contract{ID: record.ID, Number: valueOrEmpty(record.ContractNumber), Version: record.Version})
	}
	return result, nil
}

// validateStampedUploadPolicy 保持上传策略可独立测试：归档合同不得直接覆盖；
// 已完成签署的合同必须走带原因的正式更换流程。真正的并发保护由事务内行锁提供。
func validateStampedUploadPolicy(status contract.Status, signingStatus contract.SigningStatus, hasSigning bool, replacementReason string) error {
	if status == contract.StatusArchived {
		return fmt.Errorf("%w: 已归档合同不能直接更换盖章文件，请先按归档恢复/重审流程处理", apperrors.ErrStateConflict)
	}
	if hasSigning && signingStatus == contract.SigningCompleted && strings.TrimSpace(replacementReason) == "" {
		return fmt.Errorf("%w: 已确认签署的合同必须通过正式版本更换流程重新核验", apperrors.ErrStateConflict)
	}
	return nil
}

// SaveStampedDocument 在本地事务内创建新的盖章文件版本。版本号在合同行锁下分配，
// 旧版本保留并标记 superseded；任何并发上传都无法复用同一版本号。
func (r *Repository) SaveStampedDocument(ctx context.Context, tenantID string, upload contract.StampedDocumentUpload) (contract.StampedDocument, error) {
	digest := fmt.Sprintf("%x", sha256.Sum256(upload.Document))
	now := upload.UploadedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var saved contract.StampedDocument
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current contractRecord
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "status", "version").
			Where("tenant_id = ? AND id = ?", tenantID, upload.ContractID).Take(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperrors.ErrNotFound
		}
		if err != nil {
			return err
		}
		var signing signingRecord
		signingErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND contract_id = ?", tenantID, upload.ContractID).Take(&signing).Error
		hasSigning := signingErr == nil
		if signingErr != nil && !errors.Is(signingErr, gorm.ErrRecordNotFound) {
			return signingErr
		}
		if err := validateStampedUploadPolicy(contract.Status(current.Status), contract.SigningStatus(signing.Status), hasSigning, upload.ReplacementReason); err != nil {
			return err
		}
		var maxVersion uint64
		if err := tx.Model(&stampedDocumentRecord{}).
			Where("tenant_id = ? AND contract_id = ?", tenantID, upload.ContractID).
			Select("COALESCE(MAX(version), 0)").Scan(&maxVersion).Error; err != nil {
			return err
		}
		// expected_version=0 仅对首次上传有效。一旦文件
		// 存在，缺失或过期版本绝不能静默覆盖它。
		if upload.ExpectedVersion != maxVersion {
			return fmt.Errorf("%w: 盖章文件当前版本为 %d，提交基于版本 %d", apperrors.ErrVersionConflict, maxVersion, upload.ExpectedVersion)
		}
		if maxVersion > 0 {
			if err := tx.Model(&stampedDocumentRecord{}).
				Where("tenant_id = ? AND contract_id = ? AND superseded_at IS NULL", tenantID, upload.ContractID).
				Updates(map[string]any{"superseded_at": now, "superseded_by": upload.UploadedBy}).Error; err != nil {
				return err
			}
		}
		version := maxVersion + 1
		record := stampedDocumentRecord{
			ContractID: upload.ContractID, TenantID: tenantID, Version: version,
			OriginalFilename: upload.OriginalFilename, ContentSHA256: digest, Document: upload.Document,
			UploadedAt: now, UploadedBy: upload.UploadedBy, FileGatewayState: "DISABLED",
		}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		if hasSigning {
			// 新版本使旧确认失效：当前确认字段清空，历史仍保留在确认表中。
			result := tx.Model(&signingRecord{}).
				Where("tenant_id = ? AND contract_id = ?", tenantID, upload.ContractID).
				Updates(map[string]any{
					"status": contract.SigningPendingReview, "version": gorm.Expr("version + 1"),
					"seal_verified": false, "signature_verified": false, "signed_at": nil, "confirmed_at": nil,
					"confirmed_document_version": nil, "confirmed_content_sha256": nil,
					"updated_at": now, "updated_by": upload.UploadedBy,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return apperrors.ErrStateConflict
			}
		} else {
			signing = signingRecord{
				ContractID: upload.ContractID, TenantID: tenantID, Method: "paper",
				Status: string(contract.SigningPendingReview), Version: 1, UpdatedAt: now, UpdatedBy: upload.UploadedBy,
			}
			if err := tx.Create(&signing).Error; err != nil {
				return err
			}
		}
		if err := markProjectDeliveryStamped(tx, tenantID, upload.ContractID); err != nil {
			return err
		}
		saved = contract.StampedDocument{
			ContractID: upload.ContractID, Version: version, OriginalFilename: upload.OriginalFilename,
			ContentSHA256: digest, Document: upload.Document, UploadedAt: now, UploadedBy: upload.UploadedBy,
			FileGatewayState: "DISABLED",
		}
		return nil
	})
	if err != nil {
		return contract.StampedDocument{}, err
	}
	return saved, nil
}

// MarkStampedDocumentGatewayPending 只更新指定版本/哈希的行，旧上传请求无法改写新版本状态。
func (r *Repository) MarkStampedDocumentGatewayPending(ctx context.Context, tenantID, contractID string, version uint64, contentSHA256 string) error {
	result := r.db.WithContext(ctx).Model(&stampedDocumentRecord{}).
		Where("tenant_id = ? AND contract_id = ? AND version = ? AND content_sha256 = ?", tenantID, contractID, version, contentSHA256).
		Updates(map[string]any{"file_gateway_state": "PENDING", "file_gateway_last_error": ""})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		// MySQL 对"值未变化"的 UPDATE 可能报告 0 行；先确认目标版本仍存在，
		// 只有确实不匹配时才判定冲突，避免幂等重试被误判。
		matched, matchErr := r.stampedVersionMatches(ctx, tenantID, contractID, version, contentSHA256)
		if matchErr != nil {
			return matchErr
		}
		if !matched {
			return fmt.Errorf("%w: 盖章文件版本 %d 已被替换", apperrors.ErrVersionConflict, version)
		}
	}
	return nil
}

// MarkStampedDocumentGatewayResult 记录指定版本的平台文件 ID 或脱敏错误，不删除旧 BLOB。
func (r *Repository) MarkStampedDocumentGatewayResult(ctx context.Context, tenantID, contractID string, version uint64, contentSHA256, state, value string) error {
	updates := map[string]any{"file_gateway_state": state}
	if state == "READY" {
		updates["platform_file_id"] = value
		updates["file_gateway_last_error"] = ""
	} else {
		updates["file_gateway_last_error"] = value[:min(len(value), 512)]
	}
	result := r.db.WithContext(ctx).Model(&stampedDocumentRecord{}).
		Where("tenant_id = ? AND contract_id = ? AND version = ? AND content_sha256 = ?", tenantID, contractID, version, contentSHA256).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		matched, matchErr := r.stampedVersionMatches(ctx, tenantID, contractID, version, contentSHA256)
		if matchErr != nil {
			return matchErr
		}
		if !matched {
			return fmt.Errorf("%w: 盖章文件版本 %d 已被替换", apperrors.ErrVersionConflict, version)
		}
	}
	return nil
}

func (r *Repository) stampedVersionMatches(ctx context.Context, tenantID, contractID string, version uint64, contentSHA256 string) (bool, error) {
	var count int64
	if err := r.db.WithContext(ctx).Model(&stampedDocumentRecord{}).
		Where("tenant_id = ? AND contract_id = ? AND version = ? AND content_sha256 = ?", tenantID, contractID, version, contentSHA256).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count == 1, nil
}

func (r *Repository) GetStampedDocument(ctx context.Context, tenantID, contractID string) (contract.StampedDocument, error) {
	var record stampedDocumentRecord
	err := r.db.WithContext(ctx).Where("tenant_id = ? AND contract_id = ?", tenantID, contractID).
		Order("version DESC").Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.StampedDocument{}, apperrors.ErrNotFound
	}
	if err != nil {
		return contract.StampedDocument{}, err
	}
	return contract.StampedDocument{ContractID: record.ContractID, Version: record.Version, OriginalFilename: record.OriginalFilename, ContentSHA256: record.ContentSHA256, Document: record.Document, UploadedAt: record.UploadedAt, UploadedBy: record.UploadedBy, PlatformFileID: record.PlatformFileID, FileGatewayState: record.FileGatewayState}, nil
}

func (r *Repository) ListSigningRecords(ctx context.Context, tenantID string, limit int) ([]contract.SigningRecord, error) {
	contracts, err := r.ListApprovedContracts(ctx, tenantID, limit)
	if err != nil || len(contracts) == 0 {
		return nil, err
	}
	ids := make([]string, 0, len(contracts))
	for _, item := range contracts {
		ids = append(ids, item.ID)
	}
	var signingRows []signingRecord
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND contract_id IN ?", tenantID, ids).Find(&signingRows).Error; err != nil {
		return nil, err
	}
	var documents []stampedDocumentRecord
	if err := r.db.WithContext(ctx).Select("contract_id", "version", "original_filename", "content_sha256", "uploaded_at").
		Where("tenant_id = ? AND contract_id IN ?", tenantID, ids).Order("contract_id ASC, version ASC").Find(&documents).Error; err != nil {
		return nil, err
	}
	byID := make(map[string]signingRecord, len(signingRows))
	for _, row := range signingRows {
		byID[row.ContractID] = row
	}
	docByID := make(map[string]stampedDocumentRecord, len(documents))
	for _, row := range documents {
		docByID[row.ContractID] = row
	}
	result := make([]contract.SigningRecord, 0, len(contracts))
	for _, item := range contracts {
		result = append(result, signingFromRecords(item, byID[item.ID], docByID[item.ID]))
	}
	return result, nil
}

const signingLedgerOrder = "CASE WHEN s.status IS NULL OR s.status <> 'completed' THEN 0 ELSE 1 END, c.updated_at DESC, c.id DESC"

// signingLedgerQuery 从合同左连接签署行，按合同数据范围过滤，避免先截取最近
// 200 份合同而漏掉较早的未完成签署。
func signingLedgerQuery(db *gorm.DB, filter contract.ScopeFilter) *gorm.DB {
	scope := applyContractScope(db.Model(&contractRecord{}), filter).Select("id")
	return db.Table("con_contract AS c").
		Joins("LEFT JOIN con_contract_signing AS s ON s.contract_id = c.id AND s.tenant_id = c.tenant_id").
		Where("c.id IN (?)", scope).
		Where("c.status IN ?", contract.ApprovalPassedStatuses())
}

func applySigningRecordFilters(query *gorm.DB, filter contract.SigningRecordQuery, today time.Time) *gorm.DB {
	if keyword := strings.TrimSpace(filter.Keyword); keyword != "" {
		pattern := "%" + keyword + "%"
		query = query.Where("(c.contract_number LIKE ? OR c.title LIKE ? OR c.customer_name LIKE ? OR s.courier_number LIKE ?)", pattern, pattern, pattern, pattern)
	}
	switch strings.TrimSpace(filter.Status) {
	case "":
	case "pending_shipment":
		query = query.Where("(s.status IS NULL OR s.status = ?) AND (c.end_date IS NULL OR c.end_date >= ?)", contract.SigningPendingShipment, today)
	case "in_return", "pending_review":
		// 页面把已过期的未完成记录统一显示为“已失效”，状态筛选必须与显示一致。
		query = query.Where("s.status = ? AND (c.end_date IS NULL OR c.end_date >= ?)", strings.TrimSpace(filter.Status), today)
	case "completed":
		query = query.Where("s.status = ?", contract.SigningCompleted)
	case "expired":
		query = query.Where("(s.status IS NULL OR s.status <> ?) AND c.end_date IS NOT NULL AND c.end_date < ?", contract.SigningCompleted, today)
	default:
		query = query.Where("1 = 0")
	}
	if method := strings.TrimSpace(filter.Method); method != "" {
		query = query.Where("COALESCE(s.method, 'paper') = ?", method)
	}
	return query
}

// ListSigningRecordsScoped 在服务端完成状态/关键字/方式筛选与分页，未完成签署优先，
// 并返回全量统计，避免页面只看到最近一页却把它当成完整台账。
func (r *Repository) ListSigningRecordsScoped(ctx context.Context, filter contract.ScopeFilter, query contract.SigningRecordQuery) (contract.SigningRecordPage, error) {
	if query.Limit <= 0 || query.Limit > 200 {
		query.Limit = 50
	}
	if query.Offset < 0 {
		query.Offset = 0
	}
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	db := r.db.WithContext(ctx)

	countQuery := applySigningRecordFilters(signingLedgerQuery(db, filter), query, today)
	var total int64
	if err := countQuery.Distinct("c.id").Count(&total).Error; err != nil {
		return contract.SigningRecordPage{}, err
	}
	ids := make([]string, 0, query.Limit)
	if int64(query.Offset) < total {
		pageQuery := applySigningRecordFilters(signingLedgerQuery(db, filter), query, today)
		if err := pageQuery.Select("c.id").Order(signingLedgerOrder).Offset(query.Offset).Limit(query.Limit).Pluck("c.id", &ids).Error; err != nil {
			return contract.SigningRecordPage{}, err
		}
	}
	contracts := make([]contract.Contract, 0, len(ids))
	if len(ids) > 0 {
		var records []contractRecord
		if err := db.Where("tenant_id = ? AND id IN ?", filter.TenantID, ids).Omit("rendered_document", "template_values_json").Find(&records).Error; err != nil {
			return contract.SigningRecordPage{}, err
		}
		byID := make(map[string]contractRecord, len(records))
		for _, record := range records {
			byID[record.ID] = record
		}
		for _, id := range ids {
			if record, ok := byID[id]; ok {
				contracts = append(contracts, contractFromRecord(record))
			}
		}
		var err error
		contracts, err = r.attachPaymentTerms(ctx, filter.TenantID, contracts)
		if err != nil {
			return contract.SigningRecordPage{}, err
		}
	}
	items, err := r.signingRecordsForContracts(ctx, filter.TenantID, contracts)
	if err != nil {
		return contract.SigningRecordPage{}, err
	}
	stats, err := r.signingStatsScoped(ctx, filter, today)
	if err != nil {
		return contract.SigningRecordPage{}, err
	}
	return contract.SigningRecordPage{
		Items: items, Total: total, Offset: query.Offset, Limit: query.Limit,
		HasMore: int64(query.Offset+len(ids)) < total, Stats: stats,
	}, nil
}

func (r *Repository) signingStatsScoped(ctx context.Context, filter contract.ScopeFilter, today time.Time) (contract.SigningStats, error) {
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	nextMonth := monthStart.AddDate(0, 1, 0)
	var stats contract.SigningStats
	err := signingLedgerQuery(r.db.WithContext(ctx), filter).Select(`COUNT(*) AS total,
		COALESCE(SUM(CASE WHEN s.status = 'completed' THEN 1 ELSE 0 END), 0) AS completed,
		COALESCE(SUM(CASE WHEN COALESCE(s.status, 'pending_shipment') IN ('pending_shipment', 'in_return', 'pending_review')
			AND (c.end_date IS NULL OR c.end_date >= ?) THEN 1 ELSE 0 END), 0) AS processing,
		COALESCE(SUM(CASE WHEN s.status = 'completed' AND s.confirmed_at >= ? AND s.confirmed_at < ? THEN 1 ELSE 0 END), 0) AS monthly,
		COALESCE(SUM(CASE WHEN (s.status IS NULL OR s.status <> 'completed') AND c.end_date IS NOT NULL AND c.end_date < ? THEN 1 ELSE 0 END), 0) AS expired`,
		today, monthStart, nextMonth, today).Scan(&stats).Error
	if err != nil {
		return contract.SigningStats{}, err
	}
	return stats, nil
}

func (r *Repository) signingRecordsForContracts(ctx context.Context, tenantID string, contracts []contract.Contract) ([]contract.SigningRecord, error) {
	ids := make([]string, 0, len(contracts))
	for _, item := range contracts {
		ids = append(ids, item.ID)
	}
	var signingRows []signingRecord
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND contract_id IN ?", tenantID, ids).Find(&signingRows).Error; err != nil {
		return nil, err
	}
	var documents []stampedDocumentRecord
	if err := r.db.WithContext(ctx).Select("contract_id", "version", "original_filename", "content_sha256", "uploaded_at").
		Where("tenant_id = ? AND contract_id IN ?", tenantID, ids).Order("contract_id ASC, version ASC").Find(&documents).Error; err != nil {
		return nil, err
	}
	byID := make(map[string]signingRecord, len(signingRows))
	for _, row := range signingRows {
		byID[row.ContractID] = row
	}
	docByID := make(map[string]stampedDocumentRecord, len(documents))
	for _, row := range documents {
		docByID[row.ContractID] = row
	}
	result := make([]contract.SigningRecord, 0, len(contracts))
	for _, item := range contracts {
		result = append(result, signingFromRecords(item, byID[item.ID], docByID[item.ID]))
	}
	return result, nil
}

func (r *Repository) GetSigningRecord(ctx context.Context, tenantID, contractID string) (contract.SigningRecord, error) {
	item, err := r.GetContract(ctx, tenantID, contractID)
	if err != nil {
		return contract.SigningRecord{}, err
	}
	var signing signingRecord
	err = r.db.WithContext(ctx).Where("tenant_id = ? AND contract_id = ?", tenantID, contractID).Take(&signing).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.SigningRecord{}, err
	}
	var document stampedDocumentRecord
	err = r.db.WithContext(ctx).Select("contract_id", "version", "original_filename", "content_sha256", "uploaded_at").
		Where("tenant_id = ? AND contract_id = ?", tenantID, contractID).Order("version DESC").Take(&document).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.SigningRecord{}, err
	}
	// 存量明文手机号懒回填（SEC-D7）：读取时顺手加密，掩码不依赖回填成功。
	r.repairSigningPhonePlaintext(ctx, tenantID, contractID, signing)
	return signingFromRecords(item, signing, document), nil
}

func (r *Repository) GetSigningRecordScoped(ctx context.Context, filter contract.ScopeFilter, contractID string) (contract.SigningRecord, error) {
	item, err := r.GetContractScoped(ctx, filter, contractID)
	if err != nil {
		return contract.SigningRecord{}, err
	}
	var signing signingRecord
	err = r.db.WithContext(ctx).Where("tenant_id = ? AND contract_id = ?", filter.TenantID, contractID).Take(&signing).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.SigningRecord{}, err
	}
	var document stampedDocumentRecord
	err = r.db.WithContext(ctx).Select("contract_id", "version", "original_filename", "content_sha256", "uploaded_at").
		Where("tenant_id = ? AND contract_id = ?", filter.TenantID, contractID).Order("version DESC").Take(&document).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.SigningRecord{}, err
	}
	// 存量明文手机号懒回填（SEC-D7）：读取时顺手加密，掩码不依赖回填成功。
	r.repairSigningPhonePlaintext(ctx, filter.TenantID, contractID, signing)
	return signingFromRecords(item, signing, document), nil
}

// maskStoredSigningPhone 是读路径的掩码投影（SEC-D7）：API 只返回 138****5678
// 形式的掩码号码，完整号码不出读路径。
//   - 密文：解密后掩码；解密失败（密钥缺失/篡改）返回固定占位，绝不回传密文；
//   - 存量明文（加密上线前写入）：直接掩码，回填由 repairSigningPhonePlaintext 负责。
func maskStoredSigningPhone(contractID, stored string) string {
	if stored == "" {
		return ""
	}
	if contract.IsSigningPhoneCiphertext(stored) {
		plain, err := contract.DecryptSigningPhone(contractID, stored)
		if err != nil {
			return "***"
		}
		return contract.MaskPhone(plain)
	}
	return contract.MaskPhone(stored)
}

// signingPhoneColumnLimit 是 000016_signing_recipient_phone.sql 的 VARCHAR(80)：
// 超长值无法回填为密文（宁可保留明文行 + 读掩码，也不制造截断或写失败）。
const signingPhoneColumnLimit = 80

// repairSigningPhonePlaintext 是存量明文手机号的懒回填策略（SEC-D7）：
// 读取签署记录时把仍为明文的行就地加密回填，无需停机迁移。
// 只尽力而为——密钥缺失、密文超长或并发更新都不影响本次读取（掩码投影已生效），
// 下一次读取会再次尝试，业务下次寄送更新也会以密文覆盖。
func (r *Repository) repairSigningPhonePlaintext(ctx context.Context, tenantID, contractID string, row signingRecord) {
	stored := valueOrEmpty(row.RecipientPhone)
	if stored == "" || contract.IsSigningPhoneCiphertext(stored) {
		return
	}
	repaired, err := contract.EncryptSigningPhone(contractID, stored)
	if err != nil || len(repaired) > signingPhoneColumnLimit {
		return
	}
	if err := r.db.WithContext(ctx).Model(&signingRecord{}).
		Where("tenant_id = ? AND contract_id = ? AND recipient_phone = ?", tenantID, contractID, stored).
		Update("recipient_phone", repaired).Error; err != nil {
		// 懒回填失败不阻塞读取：掩码已生效，下次读取/写入仍会重试回填。
	}
}

func signingFromRecords(item contract.Contract, row signingRecord, document stampedDocumentRecord) contract.SigningRecord {
	status := contract.SigningPendingShipment
	method := "paper"
	version := uint64(0)
	if row.ContractID != "" {
		status, method, version = contract.SigningStatus(row.Status), row.Method, row.Version
	}
	return contract.SigningRecord{
		Contract: item, Method: method, Status: status, CourierNumber: valueOrEmpty(row.CourierNumber),
		RecipientName: valueOrEmpty(row.RecipientName), RecipientPhone: maskStoredSigningPhone(item.ID, valueOrEmpty(row.RecipientPhone)),
		RecipientAddress: valueOrEmpty(row.RecipientAddress), MailedAt: row.MailedAt, CustomerReceivedAt: row.CustomerReceivedAt,
		ReturnedDocumentName: document.OriginalFilename, ReturnedAt: timePtr(document.UploadedAt),
		DocumentVersion: document.Version, DocumentSHA256: document.ContentSHA256,
		ConfirmedDocumentVersion: uintValueOrZero(row.ConfirmedDocumentVersion), ConfirmedDocumentSHA256: valueOrEmpty(row.ConfirmedContentSHA256),
		SealVerified: row.SealVerified, SignatureVerified: row.SignatureVerified, SignedAt: row.SignedAt, ConfirmedAt: row.ConfirmedAt,
		ReminderCount: row.ReminderCount, LastRemindedAt: row.LastRemindedAt, Version: version,
	}
}

func (r *Repository) SaveSigningShipment(ctx context.Context, tenantID, contractID, actor string, shipment contract.SigningShipment) error {
	now := time.Now().UTC()
	// 列加密（SEC-D7）：签署人手机号属于 PII，AES-GCM 落库；密钥缺失或非法时
	// 直接返回错误（写入失败关闭），绝不回退明文存储。
	encryptedPhone, err := contract.EncryptSigningPhone(contractID, shipment.RecipientPhone)
	if err != nil {
		return fmt.Errorf("encrypt signing recipient phone: %w", err)
	}
	record := signingRecord{ContractID: contractID, TenantID: tenantID, Method: "paper", Status: string(contract.SigningInReturn), CourierNumber: stringPtr(shipment.CourierNumber), RecipientName: stringPtr(shipment.RecipientName), RecipientPhone: stringPtr(encryptedPhone), RecipientAddress: stringPtr(shipment.RecipientAddress), MailedAt: &shipment.MailedAt, Version: 1, UpdatedAt: now, UpdatedBy: actor}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "contract_id"}}, DoUpdates: clause.Assignments(map[string]any{"status": contract.SigningInReturn, "courier_number": shipment.CourierNumber, "recipient_name": shipment.RecipientName, "recipient_phone": encryptedPhone, "recipient_address": shipment.RecipientAddress, "mailed_at": shipment.MailedAt, "version": gorm.Expr("version + 1"), "updated_at": now, "updated_by": actor})}).Create(&record).Error; err != nil {
			return err
		}
		return insertSigningNotification(tx, tenantID, contractID, "signing_shipped", "合同已寄出", "合同签署文件已寄出，请关注回传进度", signingNotificationKey(contractID, "shipped"))
	})
}

func (r *Repository) MarkSigningReceived(ctx context.Context, tenantID, contractID, actor string) error {
	now := time.Now().UTC()
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&signingRecord{}).
			Where("tenant_id = ? AND contract_id = ? AND status = ? AND customer_received_at IS NULL", tenantID, contractID, contract.SigningInReturn).
			Updates(map[string]any{"customer_received_at": now, "version": gorm.Expr("version + 1"), "updated_at": now, "updated_by": actor})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return apperrors.ErrStateConflict
		}
		return insertSigningNotification(tx, tenantID, contractID, "signing_received", "合同已签收", "客户已签收合同，请继续跟进签署文件回传", signingNotificationKey(contractID, "received"))
	})
}

func (r *Repository) RecordSigningReminder(ctx context.Context, tenantID, contractID, actor string) error {
	now := time.Now().UTC()
	result := r.db.WithContext(ctx).Model(&signingRecord{}).Where("tenant_id = ? AND contract_id = ? AND status = ?", tenantID, contractID, contract.SigningInReturn).Updates(map[string]any{"reminder_count": gorm.Expr("reminder_count + 1"), "last_reminded_at": now, "version": gorm.Expr("version + 1"), "updated_at": now, "updated_by": actor})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return apperrors.ErrStateConflict
	}
	return nil
}

func (r *Repository) ConfirmSigning(ctx context.Context, tenantID, contractID, actor string, confirmation contract.SigningConfirmation) error {
	now := time.Now().UTC()
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 与上传共用合同行锁：确认期间不可能插入新版本，读取到的“当前版本”是稳定事实。
		var contractRow contractRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id").Where("tenant_id = ? AND id = ?", tenantID, contractID).Take(&contractRow).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apperrors.ErrNotFound
			}
			return err
		}
		var document stampedDocumentRecord
		err := tx.Select("contract_id", "version", "content_sha256").
			Where("tenant_id = ? AND contract_id = ?", tenantID, contractID).Order("version DESC").Take(&document).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("%w: 没有可核验的盖章文件", apperrors.ErrStateConflict)
		}
		if err != nil {
			return err
		}
		if confirmation.DocumentVersion != document.Version || !strings.EqualFold(strings.TrimSpace(confirmation.ContentSHA256), document.ContentSHA256) {
			return fmt.Errorf("%w: 待确认版本 v%d 已被替换为 v%d，请重新打开文件核验", apperrors.ErrVersionConflict, confirmation.DocumentVersion, document.Version)
		}
		var signing signingRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND contract_id = ?", tenantID, contractID).Take(&signing).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apperrors.ErrStateConflict
			}
			return err
		}
		if signing.Status == string(contract.SigningCompleted) {
			// 同一版本、同一核验结果的重复请求按幂等成功处理；旧版本确认仍会因上方版本校验失败。
			if uintValueOrZero(signing.ConfirmedDocumentVersion) == document.Version && strings.EqualFold(valueOrEmpty(signing.ConfirmedContentSHA256), document.ContentSHA256) {
				return nil
			}
			return apperrors.ErrStateConflict
		}
		if signing.Status != string(contract.SigningPendingReview) {
			return apperrors.ErrStateConflict
		}
		result := tx.Model(&signingRecord{}).
			Where("tenant_id = ? AND contract_id = ? AND status = ? AND (confirmed_document_version IS NULL OR confirmed_document_version = ?)", tenantID, contractID, contract.SigningPendingReview, document.Version).
			Updates(map[string]any{
				"status": contract.SigningCompleted, "seal_verified": confirmation.SealVerified,
				"signature_verified": confirmation.SignatureVerified, "signed_at": confirmation.SignedAt,
				"confirmed_at": now, "confirmed_document_version": document.Version,
				"confirmed_content_sha256": document.ContentSHA256, "version": gorm.Expr("version + 1"),
				"updated_at": now, "updated_by": actor,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return apperrors.ErrVersionConflict
		}
		history := signingConfirmationRecord{
			ID: newID(), TenantID: tenantID, ContractID: contractID, DocumentVersion: document.Version,
			ContentSHA256: document.ContentSHA256, SealVerified: confirmation.SealVerified,
			SignatureVerified: confirmation.SignatureVerified, SignedAt: confirmation.SignedAt.UTC(),
			ConfirmedAt: now, ConfirmedBy: actor, CreatedAt: now,
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&history).Error; err != nil {
			return err
		}
		return insertSigningNotification(tx, tenantID, contractID, "signing_confirmed", "合同签署已确认",
			fmt.Sprintf("合同签署文件 v%d 已完成核验，签署流程已完成", document.Version),
			signingNotificationKey(contractID, fmt.Sprintf("confirmed:v%d", document.Version)))
	})
}

type signingConfirmationRecord struct {
	ID                string `gorm:"primaryKey"`
	TenantID          string
	ContractID        string
	DocumentVersion   uint64
	ContentSHA256     string
	SealVerified      bool
	SignatureVerified bool
	SignedAt          time.Time
	ConfirmedAt       time.Time
	ConfirmedBy       string
	CreatedAt         time.Time
}

func (signingConfirmationRecord) TableName() string {
	return "con_contract_signing_confirmation"
}

func signingNotificationKey(contractID, event string) string {
	return contractID + ":signing:" + event
}

func insertSigningNotification(tx *gorm.DB, tenantID, contractID, notificationType, title, content, dedupeKey string) error {
	var current contractRecord
	if err := tx.Select("owner_user_id").Where("tenant_id = ? AND id = ?", tenantID, contractID).Take(&current).Error; err != nil {
		return err
	}
	if current.OwnerUserID == "" {
		return nil
	}
	now := time.Now().UTC()
	record := notificationOutboxRecord{
		ID: newID(), TenantID: tenantID, RecipientKey: "user:" + current.OwnerUserID, RecipientUserID: stringPtr(current.OwnerUserID),
		NotificationType: notificationType, Title: title, Content: content, ContractID: stringPtr(contractID), DedupeKey: dedupeKey,
		DeliveryStatus: "pending", NextAttemptAt: now, CreatedAt: now,
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&record).Error
}

func (r *Repository) TransitionDirect(ctx context.Context, tenantID, contractID string, expectedVersion uint64, target contract.Status, actorUserID, reason, idempotencyKey string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row contractRecord
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "tenant_id", "status", "version").
			Where("tenant_id = ? AND id = ?", tenantID, contractID).
			Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperrors.ErrNotFound
		}
		if err != nil {
			return err
		}
		if row.Version != expectedVersion {
			return apperrors.ErrVersionConflict
		}
		var activeStatusChanges int64
		if err := tx.Model(&approvalInstanceRecord{}).
			Where("tenant_id = ? AND contract_id = ? AND kind = ? AND status = ?", tenantID, contractID, "status_change", "running").
			Count(&activeStatusChanges).Error; err != nil {
			return err
		}
		if activeStatusChanges > 0 {
			return apperrors.ErrStateConflict
		}
		if target.RequiresApproval() || target == contract.StatusPending || target == contract.StatusApproved || target == contract.StatusActive {
			return apperrors.ErrStateConflict
		}
		from := contract.Status(row.Status)
		if err := updateStatus(tx, tenantID, contractID, from, target, actorUserID); err != nil {
			return err
		}
		in := workflows.StartApprovalActivityInput{TenantID: tenantID, ContractID: contractID}
		return insertLifecycle(tx, in, from, target, actorUserID, reason, idempotencyKey)
	})
}

func (r *Repository) CreateContract(ctx context.Context, c contract.Contract, actorUserID string) error {
	now := time.Now().UTC()
	templateValues, err := json.Marshal(c.TemplateValues)
	if err != nil {
		return err
	}
	systems, err := json.Marshal(c.Systems)
	if err != nil {
		return err
	}
	serviceItems, err := json.Marshal(c.ServiceItems)
	if err != nil {
		return err
	}
	if c.Status == "" {
		c.Status = contract.StatusDraft
	}
	record := contractRecord{
		ID: c.ID, TenantID: c.TenantID, ContractNumber: stringPtr(c.Number), ContractNumberFormat: c.NumberFormat, Title: c.Title,
		ContractType: c.Type, ServiceType: c.ServiceType, CustomerCreditLevel: stringPtr(c.CustomerCreditLevel),
		OpportunityID: stringPtr(c.OpportunityID), OpportunityName: stringPtr(c.OpportunityName), CRMCustomerID: uintPtr(c.CRMCustomerID), CustomerName: stringPtr(c.CustomerName), CustomerAddress: stringPtr(c.CustomerAddress), CustomerContact: stringPtr(c.CustomerContact), CustomerPhone: stringPtr(c.CustomerPhone), OwnerIdentityID: stringPtr(c.OwnerIdentityID), OwnerOrgID: stringPtr(c.OwnerOrgID), ProjectID: stringPtr(c.ProjectID), SystemsJSON: systems, ServiceItemsJSON: serviceItems,
		OwnerUserID: c.OwnerUserID, OwnerDisplayName: c.OwnerDisplayName, AmountMinor: c.AmountMinor, Currency: c.Currency, Content: c.Content,
		TemplateID: stringPtr(c.TemplateID), TemplateValuesJSON: templateValues, RenderedDocument: c.Document,
		SourceFileID: c.SourceFileID, SourceFileStatus: c.SourceFileStatus,
		Status: string(c.Status), StartDate: c.StartDate, EndDate: c.EndDate, ContentHash: stringPtr(c.ContentHash), Version: 1,
		CreatedAt: now, CreatedBy: actorUserID, UpdatedAt: now, UpdatedBy: actorUserID,
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		if err := replaceContractPaymentTerms(tx, c); err != nil {
			return err
		}
		return tx.Create(&lifecycleEventRecord{
			ID: newID(), TenantID: c.TenantID, ContractID: c.ID,
			FromStatus: string(contract.StatusDraft), ToStatus: string(contract.StatusDraft),
			ActorUserID: stringPtr(actorUserID), Reason: stringPtr("contract created"),
			IdempotencyKey: c.ID + ":created", OccurredAt: now,
		}).Error
	})
}

func (r *Repository) ListContractLifecycle(ctx context.Context, tenantID, contractID string) ([]contract.LifecycleEvent, error) {
	var records []lifecycleEventRecord
	if err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND contract_id = ?", tenantID, contractID).
		Order("occurred_at ASC, id ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	events := make([]contract.LifecycleEvent, 0, len(records))
	for _, record := range records {
		events = append(events, contract.LifecycleEvent{
			ID: record.ID, ContractID: record.ContractID,
			FromStatus: contract.Status(record.FromStatus), ToStatus: contract.Status(record.ToStatus),
			ActorUserID: valueOrEmpty(record.ActorUserID), Reason: valueOrEmpty(record.Reason), OccurredAt: record.OccurredAt,
		})
	}
	return events, nil
}
