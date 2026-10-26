package mysql

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

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
