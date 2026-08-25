package mysql

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
	return result, nil
}

func (r *Repository) SaveStampedDocument(ctx context.Context, tenantID string, document contract.StampedDocument) error {
	digest := fmt.Sprintf("%x", sha256.Sum256(document.Document))
	record := stampedDocumentRecord{ContractID: document.ContractID, TenantID: tenantID, OriginalFilename: document.OriginalFilename, ContentSHA256: digest, Document: document.Document, UploadedAt: document.UploadedAt, UploadedBy: document.UploadedBy}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "contract_id"}}, DoUpdates: clause.AssignmentColumns([]string{"original_filename", "content_sha256", "document", "uploaded_at", "uploaded_by"})}).Create(&record).Error; err != nil {
			return err
		}
		signing := signingRecord{ContractID: document.ContractID, TenantID: tenantID, Method: "paper", Status: string(contract.SigningPendingReview), Version: 1, UpdatedAt: document.UploadedAt, UpdatedBy: document.UploadedBy}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "contract_id"}}, DoUpdates: clause.Assignments(map[string]any{"status": contract.SigningPendingReview, "version": gorm.Expr("version + 1"), "updated_at": document.UploadedAt, "updated_by": document.UploadedBy})}).Create(&signing).Error; err != nil {
			return err
		}
		return markProjectDeliveryStamped(tx, tenantID, document.ContractID)
	})
}

func (r *Repository) GetStampedDocument(ctx context.Context, tenantID, contractID string) (contract.StampedDocument, error) {
	var record stampedDocumentRecord
	err := r.db.WithContext(ctx).Where("tenant_id = ? AND contract_id = ?", tenantID, contractID).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.StampedDocument{}, apperrors.ErrNotFound
	}
	if err != nil {
		return contract.StampedDocument{}, err
	}
	return contract.StampedDocument{ContractID: record.ContractID, OriginalFilename: record.OriginalFilename, Document: record.Document, UploadedAt: record.UploadedAt, UploadedBy: record.UploadedBy}, nil
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
	if err := r.db.WithContext(ctx).Select("contract_id", "original_filename", "uploaded_at").Where("tenant_id = ? AND contract_id IN ?", tenantID, ids).Find(&documents).Error; err != nil {
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

func (r *Repository) ListSigningRecordsScoped(ctx context.Context, filter contract.ScopeFilter, limit int) ([]contract.SigningRecord, error) {
	contracts, err := r.ListApprovedContractsScoped(ctx, filter, limit)
	if err != nil || len(contracts) == 0 {
		return nil, err
	}
	return r.signingRecordsForContracts(ctx, filter.TenantID, contracts)
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
	if err := r.db.WithContext(ctx).Select("contract_id", "original_filename", "uploaded_at").Where("tenant_id = ? AND contract_id IN ?", tenantID, ids).Find(&documents).Error; err != nil {
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
	err = r.db.WithContext(ctx).Select("contract_id", "original_filename", "uploaded_at").Where("tenant_id = ? AND contract_id = ?", tenantID, contractID).Take(&document).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.SigningRecord{}, err
	}
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
	err = r.db.WithContext(ctx).Select("contract_id", "original_filename", "uploaded_at").Where("tenant_id = ? AND contract_id = ?", filter.TenantID, contractID).Take(&document).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.SigningRecord{}, err
	}
	return signingFromRecords(item, signing, document), nil
}

func signingFromRecords(item contract.Contract, row signingRecord, document stampedDocumentRecord) contract.SigningRecord {
	status := contract.SigningPendingShipment
	method := "paper"
	version := uint64(0)
	if row.ContractID != "" {
		status, method, version = contract.SigningStatus(row.Status), row.Method, row.Version
	}
	return contract.SigningRecord{Contract: item, Method: method, Status: status, CourierNumber: valueOrEmpty(row.CourierNumber), RecipientName: valueOrEmpty(row.RecipientName), RecipientPhone: valueOrEmpty(row.RecipientPhone), RecipientAddress: valueOrEmpty(row.RecipientAddress), MailedAt: row.MailedAt, CustomerReceivedAt: row.CustomerReceivedAt, ReturnedDocumentName: document.OriginalFilename, ReturnedAt: timePtr(document.UploadedAt), SealVerified: row.SealVerified, SignatureVerified: row.SignatureVerified, SignedAt: row.SignedAt, ConfirmedAt: row.ConfirmedAt, ReminderCount: row.ReminderCount, LastRemindedAt: row.LastRemindedAt, Version: version}
}

func (r *Repository) SaveSigningShipment(ctx context.Context, tenantID, contractID, actor string, shipment contract.SigningShipment) error {
	now := time.Now().UTC()
	record := signingRecord{ContractID: contractID, TenantID: tenantID, Method: "paper", Status: string(contract.SigningInReturn), CourierNumber: stringPtr(shipment.CourierNumber), RecipientName: stringPtr(shipment.RecipientName), RecipientPhone: stringPtr(shipment.RecipientPhone), RecipientAddress: stringPtr(shipment.RecipientAddress), MailedAt: &shipment.MailedAt, Version: 1, UpdatedAt: now, UpdatedBy: actor}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "contract_id"}}, DoUpdates: clause.Assignments(map[string]any{"status": contract.SigningInReturn, "courier_number": shipment.CourierNumber, "recipient_name": shipment.RecipientName, "recipient_phone": shipment.RecipientPhone, "recipient_address": shipment.RecipientAddress, "mailed_at": shipment.MailedAt, "version": gorm.Expr("version + 1"), "updated_at": now, "updated_by": actor})}).Create(&record).Error; err != nil {
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
		result := tx.Model(&signingRecord{}).Where("tenant_id = ? AND contract_id = ? AND status = ?", tenantID, contractID, contract.SigningPendingReview).Updates(map[string]any{"status": contract.SigningCompleted, "seal_verified": confirmation.SealVerified, "signature_verified": confirmation.SignatureVerified, "signed_at": confirmation.SignedAt, "confirmed_at": now, "version": gorm.Expr("version + 1"), "updated_at": now, "updated_by": actor})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return apperrors.ErrStateConflict
		}
		return insertSigningNotification(tx, tenantID, contractID, "signing_confirmed", "合同签署已确认", "合同签署文件已完成核验，签署流程已完成", signingNotificationKey(contractID, "confirmed"))
	})
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
		Status: string(c.Status), StartDate: c.StartDate, EndDate: c.EndDate, ContentHash: stringPtr(c.ContentHash), Version: 1,
		CreatedAt: now, CreatedBy: actorUserID, UpdatedAt: now, UpdatedBy: actorUserID,
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
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
