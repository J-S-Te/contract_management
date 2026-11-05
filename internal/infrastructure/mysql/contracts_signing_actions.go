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

// SaveSigningShipment 在合同行锁内登记寄出信息，寄出状态判定与写入同处一个临界区。
// 参数：ctx为上下文，tenantID/contractID为身份，actor为操作者，shipment为寄出信息。
// 返回值：缺失合同 ErrNotFound；已上传盖章件或已完成签署时 ErrStateConflict，
// 通知与寄出信息同事务提交，冲突时整体回滚。
func (r *Repository) SaveSigningShipment(ctx context.Context, tenantID, contractID, actor string, shipment contract.SigningShipment) error {
	now := time.Now().UTC()
	// 列加密（SEC-D7）：签署人手机号属于 PII，AES-GCM 落库；密钥缺失或非法时
	// 直接返回错误（写入失败关闭），绝不回退明文存储。
	encryptedPhone, err := contract.EncryptSigningPhone(contractID, shipment.RecipientPhone)
	if err != nil {
		return fmt.Errorf("encrypt signing recipient phone: %w", err)
	}
	// 只有仍可寄出的台账状态才接受登记：pending_review 代表已上传盖章件，
	// completed 代表签署已完成且 confirmed_* 已冻结，二者都不允许被回退成 in_return。
	shippable := []string{string(contract.SigningPendingShipment), string(contract.SigningInReturn)}
	shipmentUpdates := map[string]any{
		"status":         contract.SigningInReturn,
		"courier_number": shipment.CourierNumber, "recipient_name": shipment.RecipientName,
		"recipient_phone": encryptedPhone, "recipient_address": shipment.RecipientAddress,
		"mailed_at": shipment.MailedAt, "version": gorm.Expr("version + 1"),
		"updated_at": now, "updated_by": actor,
	}
	firstShipment := signingRecord{ContractID: contractID, TenantID: tenantID, Method: "paper", Status: string(contract.SigningInReturn), CourierNumber: stringPtr(shipment.CourierNumber), RecipientName: stringPtr(shipment.RecipientName), RecipientPhone: stringPtr(encryptedPhone), RecipientAddress: stringPtr(shipment.RecipientAddress), MailedAt: &shipment.MailedAt, Version: 1, UpdatedAt: now, UpdatedBy: actor}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 与上传/确认路径同序加锁（合同行 → 签署行）：check-then-act 被收敛为
		// 行锁内的条件更新，并发上传无法在“判定可寄出”之后插入 pending_review。
		var contractRow contractRecord
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").
			Where("tenant_id = ? AND id = ?", tenantID, contractID).Take(&contractRow).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperrors.ErrNotFound
		}
		if err != nil {
			return err
		}
		updated := tx.Model(&signingRecord{}).
			Where("tenant_id = ? AND contract_id = ? AND status IN ?", tenantID, contractID, shippable).
			Updates(shipmentUpdates)
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected == 0 {
			var existing signingRecord
			err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("contract_id").
				Where("tenant_id = ? AND contract_id = ?", tenantID, contractID).Take(&existing).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if err == nil {
				return fmt.Errorf("%w: 已上传盖章件或已完成签署，不能登记寄出", apperrors.ErrStateConflict)
			}
			// 首寄：台账尚无行，按与条件更新一致的语义建立 in_return。
			// 并发首插由主键唯一性收敛，冲突后重试一次条件更新，仍 0 行即状态已变。
			inserted := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "contract_id"}}, DoNothing: true}).Create(&firstShipment)
			if inserted.Error != nil {
				return inserted.Error
			}
			if inserted.RowsAffected == 0 {
				retried := tx.Model(&signingRecord{}).
					Where("tenant_id = ? AND contract_id = ? AND status IN ?", tenantID, contractID, shippable).
					Updates(shipmentUpdates)
				if retried.Error != nil {
					return retried.Error
				}
				if retried.RowsAffected != 1 {
					return fmt.Errorf("%w: 已上传盖章件或已完成签署，不能登记寄出", apperrors.ErrStateConflict)
				}
			}
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
