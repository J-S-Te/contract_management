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
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	settlementintegration "github.com/j-s-te/contract-management/internal/integration/settlement"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 合同 → 结算的财务生效事件（contract.financial_effective.v1）。
//
// 断链根因：合同侧此前没有结构化付款计划，结算侧又强制要求总额匹配的
// payment_terms，因此无法产出合法事件。这里补齐三段：
//   1) con_contract_payment_term —— 人工维护的付款计划（选项 A）
//   2) con_financial_effective_outbox —— 财务生效时写入的事务性 Outbox
//   3) 认领/成功/失败/死信状态机，由 internal/integration/settlement 投递
// 失败关闭：付款计划缺失、合计不等于合同金额、缺少合同编号或客户信息时，
// **不写 Outbox、不发事件**，绝不为凑通链路而合成分期。

// ErrPaymentTermsInvalid 表示付款计划本身不合法（合计不符、日期非法、重复分期等）。
var ErrPaymentTermsInvalid = contract.ErrPaymentTermsInvalid

const financialEffectiveEventType = "contract.financial_effective.v1"

// PaymentTerm 复用领域类型：金额用十进制字符串，与结算侧一致。
type PaymentTerm = contract.PaymentTerm

type paymentTermRecord struct {
	ID            string `gorm:"primaryKey"`
	TenantID      string
	ContractID    string
	InstallmentNo uint
	DueDate       time.Time `gorm:"type:date"`
	AmountMinor   int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (paymentTermRecord) TableName() string { return "con_contract_payment_term" }

type financialEffectiveOutboxRecord struct {
	ID              string `gorm:"primaryKey"`
	TenantID        string
	ContractID      string
	ContractVersion uint64
	PayloadJSON     []byte `gorm:"type:json"`
	DeliveryStatus  string
	Attempts        uint
	NextAttemptAt   time.Time
	LockedAt        *time.Time
	DeliveredAt     *time.Time
	LastError       *string
	CreatedAt       time.Time
}

func (financialEffectiveOutboxRecord) TableName() string { return "con_financial_effective_outbox" }

// 结算侧 ContractEvent 的对应结构。project_id/project_name 非必填：
// 项目正是在本事件之后才由项目系统建立。
type financialEffectivePayload struct {
	EventID    string `json:"event_id"`
	EventType  string `json:"event_type"`
	TenantID   string `json:"tenant_id"`
	OccurredAt string `json:"occurred_at"`
	Aggregate  struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	} `json:"aggregate"`
	Contract struct {
		ID           string        `json:"id"`
		No           string        `json:"no"`
		Version      int           `json:"version"`
		CustomerID   string        `json:"customer_id"`
		CustomerName string        `json:"customer_name"`
		OwnerUserID  string        `json:"owner_user_id,omitempty"`
		ProjectID    string        `json:"project_id,omitempty"`
		ProjectName  string        `json:"project_name,omitempty"`
		Currency     string        `json:"currency"`
		Amount       string        `json:"amount"`
		PaymentTerms []PaymentTerm `json:"payment_terms"`
	} `json:"contract"`
}

// parseAmountMinor 把 "1234.56" 解析为分；只接受非负数与最多两位小数。
func parseAmountMinor(value, field string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("%w: %s is required", ErrPaymentTermsInvalid, field)
	}
	parts := strings.SplitN(trimmed, ".", 2)
	whole, frac := parts[0], ""
	if len(parts) == 2 {
		frac = parts[1]
	}
	if whole == "" || len(frac) > 2 {
		return 0, fmt.Errorf("%w: %s must be a decimal with at most two fractional digits", ErrPaymentTermsInvalid, field)
	}
	for _, char := range whole + frac {
		if char < '0' || char > '9' {
			return 0, fmt.Errorf("%w: %s must be a decimal with at most two fractional digits", ErrPaymentTermsInvalid, field)
		}
	}
	for len(frac) < 2 {
		frac += "0"
	}
	minor, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s is out of range", ErrPaymentTermsInvalid, field)
	}
	if minor <= 0 {
		return 0, fmt.Errorf("%w: %s must be positive", ErrPaymentTermsInvalid, field)
	}
	return minor, nil
}

func formatAmountMinor(minor int64) string {
	return fmt.Sprintf("%d.%02d", minor/100, minor%100)
}

// normalizePaymentTerms 校验并归一化人工录入的付款计划。
// 结算侧会逐条校验分期号、日期与金额，并在合计不等时拒绝；这里先失败关闭。
func normalizePaymentTerms(terms []PaymentTerm, contractAmountMinor int64) ([]paymentTermRecord, int64, error) {
	if len(terms) == 0 {
		return nil, 0, fmt.Errorf("%w: payment_terms are required", ErrPaymentTermsInvalid)
	}
	records := make([]paymentTermRecord, 0, len(terms))
	seen := make(map[int]struct{}, len(terms))
	var total int64
	for index, term := range terms {
		if term.InstallmentNo < 1 {
			return nil, 0, fmt.Errorf("%w: payment_terms[%d].installment_no must be positive", ErrPaymentTermsInvalid, index)
		}
		if _, duplicate := seen[term.InstallmentNo]; duplicate {
			return nil, 0, fmt.Errorf("%w: duplicate installment_no %d", ErrPaymentTermsInvalid, term.InstallmentNo)
		}
		seen[term.InstallmentNo] = struct{}{}
		dueDate, err := time.Parse("2006-01-02", strings.TrimSpace(term.DueDate))
		if err != nil {
			return nil, 0, fmt.Errorf("%w: payment_terms[%d].due_date must be YYYY-MM-DD", ErrPaymentTermsInvalid, index)
		}
		minor, err := parseAmountMinor(term.Amount, fmt.Sprintf("payment_terms[%d].amount", index))
		if err != nil {
			return nil, 0, err
		}
		if minor > contractAmountMinor-total {
			return nil, 0, fmt.Errorf("%w: payment terms exceed contract amount", ErrPaymentTermsInvalid)
		}
		total += minor
		records = append(records, paymentTermRecord{
			InstallmentNo: uint(term.InstallmentNo),
			DueDate:       dueDate,
			AmountMinor:   minor,
		})
	}
	if total != contractAmountMinor {
		return nil, 0, fmt.Errorf("%w: payment terms total %s does not equal contract amount %s",
			ErrPaymentTermsInvalid, formatAmountMinor(total), formatAmountMinor(contractAmountMinor))
	}
	return records, total, nil
}

// 此辅助方法仅在 CreateContract 或持锁草稿 CAS 内运行。有意
// 不提供独立的无版本写入路径，防止文档仍在审批时
// 修改审批冻结的财务条款。
func replaceContractPaymentTerms(tx *gorm.DB, c contract.Contract) error {
	terms, err := contract.NormalizePaymentTerms(c.PaymentTerms, c.AmountMinor, c.Currency)
	if err != nil {
		return fmt.Errorf("%w: %v", application.ErrValidation, err)
	}
	if err := tx.Where("tenant_id = ? AND contract_id = ?", c.TenantID, c.ID).Delete(&paymentTermRecord{}).Error; err != nil {
		return err
	}
	if len(terms) == 0 {
		return nil
	}
	records, _, err := normalizePaymentTerms(terms, c.AmountMinor)
	if err != nil {
		return fmt.Errorf("%w: %v", application.ErrValidation, err)
	}
	now := time.Now().UTC()
	for index := range records {
		records[index].ID, records[index].TenantID, records[index].ContractID = newID(), c.TenantID, c.ID
		records[index].CreatedAt, records[index].UpdatedAt = now, now
	}
	return tx.Create(&records).Error
}

// 批量补全仅限已授权合同标识及
// 相同租户；列表不引入财务表 N+1 查询。
func (r *Repository) attachPaymentTerms(ctx context.Context, tenantID string, contracts []contract.Contract) ([]contract.Contract, error) {
	if len(contracts) == 0 {
		return contracts, nil
	}
	ids := make([]string, len(contracts))
	positions := make(map[string]int, len(contracts))
	for index := range contracts {
		ids[index], positions[contracts[index].ID] = contracts[index].ID, index
		contracts[index].PaymentTerms = []contract.PaymentTerm{}
	}
	var records []paymentTermRecord
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND contract_id IN ?", tenantID, ids).Order("contract_id, installment_no").Find(&records).Error; err != nil {
		return nil, err
	}
	for _, record := range records {
		if index, ok := positions[record.ContractID]; ok {
			contracts[index].PaymentTerms = append(contracts[index].PaymentTerms, contract.PaymentTerm{InstallmentNo: int(record.InstallmentNo), DueDate: record.DueDate.Format("2006-01-02"), Amount: formatAmountMinor(record.AmountMinor)})
		}
	}
	return contracts, nil
}

// ListContractPaymentTerms 返回按分期号排序的付款计划。
func (r *Repository) ListContractPaymentTerms(ctx context.Context, tenantID, contractID string) ([]PaymentTerm, error) {
	var records []paymentTermRecord
	if err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND contract_id = ?", tenantID, contractID).
		Order("installment_no ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	terms := make([]PaymentTerm, 0, len(records))
	for _, record := range records {
		terms = append(terms, PaymentTerm{
			InstallmentNo: int(record.InstallmentNo),
			DueDate:       record.DueDate.Format("2006-01-02"),
			Amount:        formatAmountMinor(record.AmountMinor),
		})
	}
	return terms, nil
}

// financialEffectiveEligible 判断当前合同能否产出合法的财务生效事件。
// 返回 ok=false 表示"暂不具备条件"，调用方应保持静默（由 Reconcile 后续重试），
// 而不是插入一条伪造事件。
func financialEffectiveEligible(tx *gorm.DB, tenantID, contractID string) (*contractRecord, []paymentTermRecord, bool, error) {
	var row contractRecord
	if err := tx.Where("tenant_id = ? AND id = ?", tenantID, contractID).Take(&row).Error; err != nil {
		return nil, nil, false, err
	}
	contractNumber := valueOrEmpty(row.ContractNumber)
	customerID := customerIDOf(row.CRMCustomerID)
	customerName := valueOrEmpty(row.CustomerName)
	if contractNumber == "" || customerID == "" || customerName == "" {
		return &row, nil, false, nil
	}
	if len(strings.TrimSpace(row.Currency)) != 3 || row.AmountMinor <= 0 || row.Version == 0 {
		return &row, nil, false, nil
	}
	var terms []paymentTermRecord
	if err := tx.Where("tenant_id = ? AND contract_id = ?", tenantID, contractID).
		Order("installment_no ASC").Find(&terms).Error; err != nil {
		return &row, nil, false, err
	}
	if len(terms) == 0 {
		return &row, nil, false, nil
	}
	var total int64
	for _, term := range terms {
		total += term.AmountMinor
	}
	if total != row.AmountMinor {
		return &row, nil, false, nil
	}
	return &row, terms, true, nil
}

// enqueueFinancialEffective 在合同生效事务内写入财务事件 Outbox。
// 必须与 approve 事务同源，否则会出现"合同已生效但事件永久丢失"。
func enqueueFinancialEffective(tx *gorm.DB, tenantID, contractID string) error {
	row, terms, eligible, err := financialEffectiveEligible(tx, tenantID, contractID)
	if err != nil || !eligible {
		return err
	}
	record := financialEffectiveOutboxRecord{
		ID: newID(), TenantID: tenantID, ContractID: row.ID, ContractVersion: row.Version,
		DeliveryStatus: "pending", NextAttemptAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}
	payload, err := newFinancialEffectivePayload(record.ID, row, terms, time.Now().UTC())
	if err != nil {
		return err
	}
	record.PayloadJSON = payload
	// 幂等键为 (tenant, contract, version)：同一版本重复触发只保留一条事件，
	// 结算侧再按 event_id 去重。
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&record).Error
}

// newFinancialEffectivePayload 冻结财务条款并携带已有真实负责人和项目引用。
// 参数：eventID 为事件号，row 为权威合同，terms 为合法分期，occurredAt 为发生时间。
// 返回值：财务事件JSON及序列化错误。
func newFinancialEffectivePayload(eventID string, row *contractRecord, terms []paymentTermRecord, occurredAt time.Time) ([]byte, error) {
	paymentTerms := make([]PaymentTerm, 0, len(terms))
	for _, term := range terms {
		paymentTerms = append(paymentTerms, PaymentTerm{
			InstallmentNo: int(term.InstallmentNo),
			DueDate:       term.DueDate.Format("2006-01-02"),
			Amount:        formatAmountMinor(term.AmountMinor),
		})
	}
	payload := financialEffectivePayload{
		EventID: eventID, EventType: financialEffectiveEventType, TenantID: row.TenantID,
		OccurredAt: occurredAt.UTC().Format(time.RFC3339),
	}
	payload.Aggregate.ID = row.ID
	payload.Aggregate.Version = int(row.Version)
	payload.Contract.ID = row.ID
	payload.Contract.No = valueOrEmpty(row.ContractNumber)
	payload.Contract.Version = int(row.Version)
	payload.Contract.CustomerID = customerIDOf(row.CRMCustomerID)
	payload.Contract.CustomerName = valueOrEmpty(row.CustomerName)
	payload.Contract.OwnerUserID = row.OwnerUserID
	payload.Contract.ProjectID = valueOrEmpty(row.ProjectID)
	payload.Contract.Currency = strings.ToUpper(strings.TrimSpace(row.Currency))
	payload.Contract.Amount = formatAmountMinor(row.AmountMinor)
	payload.Contract.PaymentTerms = paymentTerms
	return json.Marshal(payload)
}

// ReconcileFinancialEffectives 为"已生效、已有付款计划、但尚无 Outbox 行"的合同补建事件。
// 用于集成启用前已生效的存量合同，以及付款计划后补录的场景。不具备条件的合同保持静默。
func (r *Repository) ReconcileFinancialEffectives(ctx context.Context) (int, error) {
	var candidates []projectDeliveryCandidate
	err := r.db.WithContext(ctx).Table("con_contract AS c").
		Select("c.tenant_id, c.id AS contract_id").
		Where("c.status IN ?", projectDeliveryEligibleStatuses()).
		Where("EXISTS (SELECT 1 FROM con_contract_payment_term AS t WHERE t.tenant_id = c.tenant_id AND t.contract_id = c.id)").
		Where("NOT EXISTS (SELECT 1 FROM con_financial_effective_outbox AS o WHERE o.tenant_id = c.tenant_id AND o.contract_id = c.id)").
		Order("c.created_at ASC").
		Scan(&candidates).Error
	if err != nil {
		return 0, err
	}
	created := 0
	for _, candidate := range candidates {
		inserted := false
		err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			row, _, eligible, eligibleErr := financialEffectiveEligible(tx, candidate.TenantID, candidate.ContractID)
			if eligibleErr != nil || !eligible {
				return eligibleErr
			}
			if enqueueErr := enqueueFinancialEffective(tx, row.TenantID, row.ID); enqueueErr != nil {
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

func (r *Repository) ClaimFinancialEffectiveDelivery(ctx context.Context) (settlementintegration.Delivery, bool, error) {
	var delivery settlementintegration.Delivery
	found := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row financialEffectiveOutboxRecord
		now := time.Now().UTC()
		err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("(delivery_status = ? AND next_attempt_at <= ?) OR (delivery_status = ? AND locked_at < ?)", "pending", now, "processing", now.Add(-5*time.Minute)).
			Order("next_attempt_at ASC, created_at ASC").Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		attempts := row.Attempts + 1
		if err := tx.Model(&financialEffectiveOutboxRecord{}).Where("id = ?", row.ID).
			Updates(map[string]any{"delivery_status": "processing", "attempts": attempts, "locked_at": now}).Error; err != nil {
			return err
		}
		delivery = settlementintegration.Delivery{ID: row.ID, TenantID: row.TenantID, Payload: row.PayloadJSON, Attempts: attempts}
		found = true
		return nil
	})
	return delivery, found, err
}

func (r *Repository) MarkFinancialEffectiveDelivered(ctx context.Context, id string) error {
	now := time.Now().UTC()
	result := r.db.WithContext(ctx).Model(&financialEffectiveOutboxRecord{}).
		Where("id = ? AND delivery_status = ?", id, "processing").
		Updates(map[string]any{"delivery_status": "delivered", "delivered_at": now, "locked_at": nil, "last_error": nil})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return apperrors.ErrNotFound
	}
	return nil
}

func (r *Repository) MarkFinancialEffectiveFailed(ctx context.Context, id, message string, attempts uint, dead bool) error {
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
	return r.db.WithContext(ctx).Model(&financialEffectiveOutboxRecord{}).
		Where("id = ? AND delivery_status = ?", id, "processing").
		Updates(map[string]any{"delivery_status": status, "next_attempt_at": time.Now().UTC().Add(delay), "locked_at": nil, "last_error": message}).Error
}
