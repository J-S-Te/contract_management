package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	projectintegration "github.com/j-s-te/contract-management/internal/integration/project"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type projectActivationPayload struct {
	ContractID              string                     `json:"contract_id"`
	ContractVersion         string                     `json:"contract_version"`
	ContractName            string                     `json:"contract_name"`
	Customer                string                     `json:"customer"`
	EffectiveAt             time.Time                  `json:"effective_at"`
	StampedContractUploaded bool                       `json:"stamped_contract_uploaded"`
	Services                []projectActivationService `json:"services"`
}
type projectActivationService struct {
	SourceID    string `json:"source_id"`
	Name        string `json:"name"`
	Site        string `json:"site"`
	Batch       string `json:"batch"`
	Category    string `json:"category"`
	System      string `json:"system"`
	Requirement string `json:"requirement"`
	TestMode    string `json:"test_mode"`
}

type projectDeliveryCandidate struct {
	TenantID   string
	ContractID string
}

type legacyProjectDeliveryCandidate struct {
	ID         string
	TenantID   string
	ContractID string
}

func projectDeliveryEligibleStatuses() []contract.Status {
	return []contract.Status{
		contract.StatusActive,
		contract.StatusInProgress,
		contract.StatusPendingPay,
		contract.StatusCompleted,
		contract.StatusTerminated,
		contract.StatusArchived,
	}
}

// ReconcileProjectDeliveries backfills contracts that reached an effective or
// terminal post-effective state before project integration was enabled. It
// also refreshes legacy deliveries that predate the stamped-contract flag;
// the same outbox row is reused so a second project cannot be created.
func (r *Repository) ReconcileProjectDeliveries(ctx context.Context) (int, error) {
	created := 0
	var legacyDeliveries []legacyProjectDeliveryCandidate
	err := r.db.WithContext(ctx).Table("con_project_delivery_outbox AS o").
		Select("o.id, o.tenant_id, o.contract_id").
		Joins("JOIN con_contract AS c ON c.id = o.contract_id AND c.tenant_id = o.tenant_id").
		Where("c.status IN ?", projectDeliveryEligibleStatuses()).
		Where("JSON_CONTAINS_PATH(o.payload_json, 'one', '$.stamped_contract_uploaded') = 0").
		Order("o.created_at ASC").Scan(&legacyDeliveries).Error
	if err != nil {
		return 0, err
	}
	for _, candidate := range legacyDeliveries {
		err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var record projectDeliveryOutboxRecord
			if lockErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", candidate.ID).Take(&record).Error; lockErr != nil {
				return lockErr
			}
			var stampedDocumentCount int64
			if countErr := tx.Model(&stampedDocumentRecord{}).Where("tenant_id = ? AND contract_id = ?", candidate.TenantID, candidate.ContractID).Count(&stampedDocumentCount).Error; countErr != nil {
				return countErr
			}
			return refreshProjectDeliveryStampStatus(tx, record, stampedDocumentCount > 0)
		})
		if err != nil {
			return created, err
		}
		created++
	}

	var candidates []projectDeliveryCandidate
	err = r.db.WithContext(ctx).Table("con_contract AS c").
		Select("c.tenant_id, c.id AS contract_id").
		Where("c.status IN ?", projectDeliveryEligibleStatuses()).
		Where("JSON_LENGTH(c.service_items_json) > 0").
		Where("NOT EXISTS (SELECT 1 FROM con_project_delivery_outbox AS o WHERE o.tenant_id = c.tenant_id AND o.contract_id = c.id)").
		Order("c.created_at ASC").
		Scan(&candidates).Error
	if err != nil {
		return created, err
	}

	for _, candidate := range candidates {
		inserted := false
		err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var existing int64
			if countErr := tx.Model(&projectDeliveryOutboxRecord{}).
				Where("tenant_id = ? AND contract_id = ?", candidate.TenantID, candidate.ContractID).
				Count(&existing).Error; countErr != nil {
				return countErr
			}
			if existing > 0 {
				return nil
			}
			if enqueueErr := enqueueProjectActivation(tx, candidate.TenantID, candidate.ContractID); enqueueErr != nil {
				return enqueueErr
			}
			inserted = true
			return nil
		})
		if err != nil {
			return created, err
		}
		if inserted {
			created++
		}
	}
	return created, nil
}

func enqueueProjectActivation(tx *gorm.DB, tenantID, contractID string) error {
	var row contractRecord
	if err := tx.Where("tenant_id = ? AND id = ?", tenantID, contractID).Take(&row).Error; err != nil {
		return err
	}
	var items []contract.ServiceItem
	if err := json.Unmarshal(row.ServiceItemsJSON, &items); err != nil {
		return fmt.Errorf("decode service items for project delivery: %w", err)
	}
	services := make([]projectActivationService, 0)
	for itemIndex, item := range items {
		systems := item.Systems
		if len(systems) == 0 {
			systems = []contract.SystemInfo{{}}
		}
		for systemIndex, system := range systems {
			sourceID := strings.TrimSpace(item.SourceID)
			if sourceID == "" {
				sourceID = fmt.Sprintf("%s-%02d", contractID, itemIndex+1)
			}
			if len(systems) > 1 {
				sourceID = fmt.Sprintf("%s-%02d", sourceID, systemIndex+1)
			}
			mode := strings.ToUpper(strings.TrimSpace(item.TestMode))
			if mode == "" {
				mode = "STANDARD"
				if strings.Contains(item.ServiceType, "渗透") || strings.Contains(strings.ToLower(item.ServiceType), "penetration") {
					mode = "PENETRATION"
				}
			}
			services = append(services, projectActivationService{SourceID: sourceID, Name: firstProjectValue(item.Name, item.ServiceType), Site: firstProjectValue(item.Site, "默认场所"), Batch: firstProjectValue(item.Batch, "默认批次"), Category: firstProjectValue(item.Category, item.ServiceType), System: system.Name, Requirement: item.Requirement, TestMode: mode})
		}
	}
	effectiveAt := time.Now().UTC()
	if row.StartDate != nil {
		effectiveAt = row.StartDate.UTC()
	}
	var stampedDocumentCount int64
	if err := tx.Model(&stampedDocumentRecord{}).Where("tenant_id = ? AND contract_id = ?", tenantID, contractID).Count(&stampedDocumentCount).Error; err != nil {
		return err
	}
	payload := projectActivationPayload{ContractID: row.ID, ContractVersion: fmt.Sprintf("%d", row.Version), ContractName: row.Title, Customer: firstProjectValue(valueOrEmpty(row.CustomerName), "未指定客户"), EffectiveAt: effectiveAt, StampedContractUploaded: stampedDocumentCount > 0, Services: services}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	record := projectDeliveryOutboxRecord{ID: newID(), TenantID: tenantID, ContractID: row.ID, ContractVersion: row.Version, PayloadJSON: encoded, DeliveryStatus: "pending", NextAttemptAt: now, CreatedAt: now}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&record).Error
}

// markProjectDeliveryStamped refreshes an existing activation delivery after
// the stamped contract is uploaded. Reusing the original outbox row preserves
// the contract-version idempotency key while allowing project management to
// clear its non-blocking upload warning.
func markProjectDeliveryStamped(tx *gorm.DB, tenantID, contractID string) error {
	var record projectDeliveryOutboxRecord
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("tenant_id = ? AND contract_id = ?", tenantID, contractID).
		Order("created_at DESC").Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return refreshProjectDeliveryStampStatus(tx, record, true)
}

func refreshProjectDeliveryStampStatus(tx *gorm.DB, record projectDeliveryOutboxRecord, uploaded bool) error {
	var payload projectActivationPayload
	if err := json.Unmarshal(record.PayloadJSON, &payload); err != nil {
		return fmt.Errorf("decode project delivery for stamped contract: %w", err)
	}
	payload.StampedContractUploaded = uploaded
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return tx.Model(&projectDeliveryOutboxRecord{}).Where("id = ?", record.ID).Updates(map[string]any{
		"payload_json":    encoded,
		"delivery_status": "pending",
		"attempts":        0,
		"next_attempt_at": time.Now().UTC(),
		"locked_at":       nil,
		"delivered_at":    nil,
		"last_error":      nil,
	}).Error
}
func firstProjectValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (r *Repository) ClaimProjectDelivery(ctx context.Context) (projectintegration.Delivery, bool, error) {
	var delivery projectintegration.Delivery
	found := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row projectDeliveryOutboxRecord
		now := time.Now().UTC()
		err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("(delivery_status = ? AND next_attempt_at <= ?) OR (delivery_status = ? AND locked_at < ?)", "pending", now, "processing", now.Add(-5*time.Minute)).Order("next_attempt_at ASC, created_at ASC").Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		attempts := row.Attempts + 1
		if err := tx.Model(&projectDeliveryOutboxRecord{}).Where("id = ?", row.ID).Updates(map[string]any{"delivery_status": "processing", "attempts": attempts, "locked_at": now}).Error; err != nil {
			return err
		}
		delivery = projectintegration.Delivery{ID: row.ID, TenantID: row.TenantID, Payload: row.PayloadJSON, Attempts: attempts}
		found = true
		return nil
	})
	return delivery, found, err
}
func (r *Repository) MarkProjectDeliveryDelivered(ctx context.Context, id string) error {
	now := time.Now().UTC()
	result := r.db.WithContext(ctx).Model(&projectDeliveryOutboxRecord{}).Where("id = ? AND delivery_status = ?", id, "processing").Updates(map[string]any{"delivery_status": "delivered", "delivered_at": now, "locked_at": nil, "last_error": nil})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return apperrors.ErrNotFound
	}
	return nil
}
func (r *Repository) MarkProjectDeliveryFailed(ctx context.Context, id, message string, attempts uint, dead bool) error {
	status := "pending"
	if dead {
		status = "dead"
	}
	delay := time.Second
	for count := uint(1); count < attempts && delay < 10*time.Minute; count++ {
		delay *= 2
	}
	if delay > 10*time.Minute {
		delay = 10 * time.Minute
	}
	if len(message) > 1000 {
		message = message[:1000]
	}
	return r.db.WithContext(ctx).Model(&projectDeliveryOutboxRecord{}).Where("id = ? AND delivery_status = ?", id, "processing").Updates(map[string]any{"delivery_status": status, "next_attempt_at": time.Now().UTC().Add(delay), "locked_at": nil, "last_error": message}).Error
}
