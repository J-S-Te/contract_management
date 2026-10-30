package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/auditctx"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	projectintegration "github.com/j-s-te/contract-management/internal/integration/project"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type projectActivationPayload struct {
	ContractID      string `json:"contract_id"`
	ContractNumber  string `json:"contract_number"`
	ContractVersion string `json:"contract_version"`
	ContractName    string `json:"contract_name"`
	Customer        string `json:"customer"`
	// CustomerID 是合同上的 CRM 客户标识；项目系统据此按客户聚合与对账，
	// 不再依赖客户名称的字符串匹配。未从商机带出客户时为空。
	CustomerID              string                        `json:"customer_id,omitempty"`
	EffectiveAt             time.Time                     `json:"effective_at"`
	StampedContractUploaded bool                          `json:"stamped_contract_uploaded"`
	Services                []contract.ProjectServiceItem `json:"services"`
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

// ReconcileProjectDeliveries 补建或刷新普通生效及后续终态合同的原项目出站记录。
// 参数：ctx为扫描上下文。
// 返回值：实际补建数量或数据库错误，复用原出站身份避免重复项目。
func (r *Repository) ReconcileProjectDeliveries(ctx context.Context) (int, error) {
	created := 0
	var legacyDeliveries []legacyProjectDeliveryCandidate
	err := ordinaryContractScope(r.db.WithContext(ctx).Table("con_project_delivery_outbox AS o"), "c").
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
	err = ordinaryContractScope(r.db.WithContext(ctx).Table("con_contract AS c"), "c").
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

// enqueueProjectActivation 为符合普通来源策略的合同登记幂等项目激活事件。
// 参数：tx为当前事务，tenantID/contractID为可信合同身份。
// 返回值：登记结果错误；不适用的历史合同不进入普通事件流。
func enqueueProjectActivation(tx *gorm.DB, tenantID, contractID string) error {
	var row contractRecord
	if err := tx.Where("tenant_id = ? AND id = ?", tenantID, contractID).Take(&row).Error; err != nil {
		return err
	}
	if !ordinaryContractRecord(row) {
		return nil
	}
	var items []contract.ServiceItem
	if err := json.Unmarshal(row.ServiceItemsJSON, &items); err != nil {
		return fmt.Errorf("decode service items for project delivery: %w", err)
	}
	services := contract.ProjectServiceItems(contractID, items)
	effectiveAt := time.Now().UTC()
	if row.StartDate != nil {
		effectiveAt = row.StartDate.UTC()
	}
	var stampedDocumentCount int64
	if err := tx.Model(&stampedDocumentRecord{}).Where("tenant_id = ? AND contract_id = ?", tenantID, contractID).Count(&stampedDocumentCount).Error; err != nil {
		return err
	}
	payload := newProjectActivationPayload(row, effectiveAt, stampedDocumentCount > 0, services)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	record := projectDeliveryOutboxRecord{ID: newID(), TenantID: tenantID, ContractID: row.ID, ContractVersion: row.Version, PayloadJSON: encoded, DeliveryStatus: "pending", NextAttemptAt: now, CreatedAt: now}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&record).Error; err != nil {
		return err
	}
	// 交接给项目是关键跨系统动作：与投递行同事务写审计，便于回答"合同何时、由谁交接"。
	auditPayload, err := json.Marshal(map[string]any{
		"contract_id": row.ID, "contract_version": row.Version, "service_item_count": len(services), "target": "PROJECT_MANAGEMENT",
	})
	if err != nil {
		return err
	}
	meta := auditctx.FromContext(tx.Statement.Context)
	actor := meta.ActorID
	if actor == "" {
		actor = "SYSTEM"
	}
	return insertAuditOutbox(tx, tenantID, newID(), AuditOutboxEntry{
		ActorID: actor, Action: "CONTRACT_MANAGEMENT:HANDED_OFF_TO_PROJECT",
		ResourceType: "contract", ResourceID: row.ID, Payload: auditPayload,
		LoginIP: meta.LoginIP, ClientIP: meta.ClientIP, IPSource: meta.IPSource, RequestID: meta.RequestID,
	})
}

func newProjectActivationPayload(row contractRecord, effectiveAt time.Time, stamped bool, services []contract.ProjectServiceItem) projectActivationPayload {
	return projectActivationPayload{
		ContractID: row.ID, ContractNumber: valueOrEmpty(row.ContractNumber), ContractVersion: fmt.Sprintf("%d", row.Version),
		ContractName: row.Title, Customer: firstProjectValue(valueOrEmpty(row.CustomerName), "未指定客户"),
		CustomerID: customerIDOf(row.CRMCustomerID), EffectiveAt: effectiveAt,
		StampedContractUploaded: stamped, Services: services,
	}
}

// markProjectDeliveryStamped 在上传盖章合同后，
// 刷新既有生效投递。复用原 Outbox 行，保留
// 合同版本幂等键，同时允许项目管理
// 清除不阻塞业务的上传警告。
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

// customerIDOf 把合同上的 CRM 客户标识转成项目系统使用的稳定标识；未关联商机时为空。
func customerIDOf(crmCustomerID *uint64) string {
	if crmCustomerID == nil || *crmCustomerID == 0 {
		return ""
	}
	return strconv.FormatUint(*crmCustomerID, 10)
}
