package mysql

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

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
