package mysql

import (
	"context"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

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
