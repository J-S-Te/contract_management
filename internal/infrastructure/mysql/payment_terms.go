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
	"github.com/j-s-te/contract-management/internal/auditctx"
	"github.com/j-s-te/contract-management/internal/domain/approval"
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

// 财务事件入队的显式跳过原因码。审批时若付款计划缺失或合计不符，业务上绝不能
// 合成事件（失败关闭），但“什么都没发生”必须是可解释、可统计的状态，
// 而不是静默分支：原因码写入入队结果并用于补建/告警，避免“已生效却永久无事件”无人知晓。
const (
	financialSkipNotOrdinary     = "not_ordinary_contract"
	financialSkipMissingIdentity = "missing_contract_number_or_customer"
	financialSkipInvalidTerms    = "invalid_currency_amount_or_version"
	financialSkipNoPaymentTerms  = "payment_terms_missing"
	financialSkipTotalMismatch   = "payment_terms_total_mismatch"
)

// financialEffectiveSkipReason 判断合同能否产生合法财务事件并给出具体原因。
// 参数：row 为权威合同行，terms 为人工维护的付款计划。
// 返回值：空串表示满足投递条件；否则为稳定的跳过原因码。
func financialEffectiveSkipReason(row *contractRecord, terms []paymentTermRecord) string {
	if row == nil {
		return financialSkipNotOrdinary
	}
	if !ordinaryContractRecord(*row) {
		return financialSkipNotOrdinary
	}
	if valueOrEmpty(row.ContractNumber) == "" || customerIDOf(row.CRMCustomerID) == "" || valueOrEmpty(row.CustomerName) == "" {
		return financialSkipMissingIdentity
	}
	if len(strings.TrimSpace(row.Currency)) != 3 || row.AmountMinor <= 0 || row.Version == 0 {
		return financialSkipInvalidTerms
	}
	if len(terms) == 0 {
		return financialSkipNoPaymentTerms
	}
	var total int64
	for _, term := range terms {
		total += term.AmountMinor
	}
	if total != row.AmountMinor {
		return financialSkipTotalMismatch
	}
	return ""
}

// financialEffectiveEligible 判断普通合同能否产生合法财务生效事件，不具备条件时不造事件。
// 参数：tx为当前事务，tenantID/contractID为可信合同身份。
// 返回值：合同、原付款计划、是否满足投递条件及数据库错误。
func financialEffectiveEligible(tx *gorm.DB, tenantID, contractID string) (*contractRecord, []paymentTermRecord, bool, error) {
	var row contractRecord
	if err := tx.Where("tenant_id = ? AND id = ?", tenantID, contractID).Take(&row).Error; err != nil {
		return nil, nil, false, err
	}
	var terms []paymentTermRecord
	if err := tx.Where("tenant_id = ? AND contract_id = ?", tenantID, contractID).
		Order("installment_no ASC").Find(&terms).Error; err != nil {
		return &row, nil, false, err
	}
	return &row, terms, financialEffectiveSkipReason(&row, terms) == "", nil
}

// financialEffectiveEnqueue 描述一次财务事件入队的可观测结果。
// 参数/返回值：由 enqueueFinancialEffective 填充。
type financialEffectiveEnqueue struct {
	// Enqueued 表示该版本已有可投递的 outbox 行（本次写入或幂等命中）。
	Enqueued bool
	// SkipReason 非空表示按业务规则不产生事件（例如未维护付款计划）。
	// 调用方不得把它当成功，也不得据此回滚业务事务；由周期补建和缺口告警观测。
	SkipReason string
}

// enqueueFinancialEffective 在合同生效事务内写入财务事件 Outbox。
// 必须与 approve 事务同源，否则会出现"合同已生效但事件永久丢失"。
// 参数：tx 为业务事务，tenantID/contractID 为可信合同身份。
// 返回值：入队结果（含显式跳过原因）与真实存储错误。存储错误必须由调用方回滚事务，
// 不允许静默吞掉；“不满足条件”不是存储失败，返回 SkipReason 供补建/告警。
func enqueueFinancialEffective(tx *gorm.DB, tenantID, contractID string) (financialEffectiveEnqueue, error) {
	row, terms, eligible, err := financialEffectiveEligible(tx, tenantID, contractID)
	if err != nil {
		return financialEffectiveEnqueue{}, err
	}
	if !eligible {
		return financialEffectiveEnqueue{SkipReason: financialEffectiveSkipReason(row, terms)}, nil
	}
	record := financialEffectiveOutboxRecord{
		ID: newID(), TenantID: tenantID, ContractID: row.ID, ContractVersion: row.Version,
		DeliveryStatus: "pending", NextAttemptAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}
	payload, err := newFinancialEffectivePayload(record.ID, row, terms, time.Now().UTC())
	if err != nil {
		return financialEffectiveEnqueue{}, err
	}
	record.PayloadJSON = payload
	// 幂等键为 (tenant, contract, version)：同一版本重复触发只保留一条事件，
	// 结算侧再按 event_id 去重。
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&record).Error; err != nil {
		return financialEffectiveEnqueue{}, err
	}
	return financialEffectiveEnqueue{Enqueued: true}, nil
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

// 财务事件按“合同版本”幂等的判定子句（BIZ-XEVT2-05）。
//
// 旧实现只判断“该合同是否已有任意财务事件”，因此合同改版（version+1）后
// 永远不会补建新事件，结算侧只能停在同一合同第一笔事件上。这里改为
// per-version 判定：同一 (tenant, contract, contract_version) 只允许一条事件，
// 版本升级后允许补建新的事件；结算侧再按 event_id 与版本语义处理修订。
//
// 注意：该子句只替换“已有事件”的判定，不改变补建的前置条件——补建仍要求
// 合同已维护合法付款计划（EXISTS con_contract_payment_term），
// 即“无计划=不发送”的既有语义保持不变。
const financialEventVersionClause = "o.tenant_id = c.tenant_id AND o.contract_id = c.id AND o.contract_version = c.version"

// ReconcileFinancialEffectives 补建满足条件且“当前版本”尚无财务出站事件的普通合同。
// 参数：ctx为扫描上下文。
// 返回值：实际新增数量或数据库错误；暂不具备条件的合同留待后续扫描。
// 该方法是幂等的：当前版本已存在 outbox 行的合同不再选中，重复调用不会重复入队；
// 合同 version 升级（审批后状态流转或受控财务变更）会为其新版本补建事件。
func (r *Repository) ReconcileFinancialEffectives(ctx context.Context) (int, error) {
	var candidates []projectDeliveryCandidate
	err := ordinaryContractScope(r.db.WithContext(ctx).Table("con_contract AS c"), "c").
		Select("c.tenant_id, c.id AS contract_id").
		Where("c.status IN ?", projectDeliveryEligibleStatuses()).
		Where("EXISTS (SELECT 1 FROM con_contract_payment_term AS t WHERE t.tenant_id = c.tenant_id AND t.contract_id = c.id)").
		Where("NOT EXISTS (SELECT 1 FROM con_financial_effective_outbox AS o WHERE " + financialEventVersionClause + ")").
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
			result, enqueueErr := enqueueFinancialEffective(tx, row.TenantID, row.ID)
			if enqueueErr != nil {
				return enqueueErr
			}
			inserted = result.Enqueued
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

// FinancialEffectiveBacklog 统计“已生效但当前版本没有任何财务出站事件”的普通合同数量。
// 参数：ctx 为只读查询上下文。
// 返回值：缺口合同数与查询错误。
// 该计数用于把“审批时未维护付款计划所以不产生事件”和“版本升级后事件缺失”
// 的静默缺口暴露为周期告警；它只读、不写、不合成任何事件，也不改变事件顺序。
// 判定与补建保持一致（按 contract_version），因此“无计划=不发送”的前提下，
// 缺计划的合同仍计入缺口而不是被算作已投递。
func (r *Repository) FinancialEffectiveBacklog(ctx context.Context) (int, error) {
	var count int64
	err := ordinaryContractScope(r.db.WithContext(ctx).Table("con_contract AS c"), "c").
		Where("c.status IN ?", projectDeliveryEligibleStatuses()).
		Where("NOT EXISTS (SELECT 1 FROM con_financial_effective_outbox AS o WHERE " + financialEventVersionClause + ")").
		Count(&count).Error
	if err != nil {
		return 0, err
	}
	return int(count), nil
}

// ApplyContractFinancialChange 在单个事务内执行受控合同财务变更（BIZ-XEVT2-05）。
//
// 参数：ctx 携带审计来源信息；tenantID/actorUserID 为可信租户与操作者；
// change 为应用层已校验的新金额、付款计划、并发版本与原因。
// 返回值：变更后的合同（已含新付款计划）或失败关闭错误。
//
// 事务内顺序固定：锁定合同行 → 复核版本/状态/来源 → 复核审批前置
// （无在途审批且存在已通过实例）→ version+1 更新金额 → 替换付款计划 →
// 入队新版本财务事件 → 写生命周期与审计。任一步失败整体回滚，
// 不会出现“合同已改额但结算收不到事件”或“事件已入队但合同未改”的半状态。
func (r *Repository) ApplyContractFinancialChange(ctx context.Context, tenantID string, change application.ContractFinancialChange, actorUserID string) (contract.Contract, error) {
	var out contract.Contract
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(actorUserID) == "" || strings.TrimSpace(change.ContractID) == "" || change.ExpectedVersion == 0 {
		return out, apperrors.ErrNotFound
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row contractRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND id = ?", tenantID, change.ContractID).Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apperrors.ErrNotFound
			}
			return err
		}
		// 纵深防御：行上的租户必须与可信租户一致，跨租户请求一律按不存在处理。
		if row.TenantID != tenantID {
			return apperrors.ErrNotFound
		}
		if row.Version != change.ExpectedVersion {
			return apperrors.ErrVersionConflict
		}
		if !application.FinancialChangeStatusEligible(contract.Status(row.Status)) {
			return apperrors.ErrStateConflict
		}
		// 历史导入基线由导入更正流程管理，不允许在此改写金额或计划。
		if !ordinaryContractRecord(row) {
			return apperrors.ErrStateConflict
		}
		// 审批前置（失败关闭）：存在在途审批时不得改额；且必须有一次已通过的
		// 合同审批（或状态变更审批）作为“已审批”的可证明证据。
		var running int64
		if err := tx.Model(&approvalInstanceRecord{}).
			Where("tenant_id = ? AND contract_id = ? AND status = ?", tenantID, row.ID, string(approval.StatusRunning)).
			Count(&running).Error; err != nil {
			return err
		}
		if running > 0 {
			return apperrors.ErrStateConflict
		}
		var approved int64
		if err := tx.Model(&approvalInstanceRecord{}).
			Where("tenant_id = ? AND contract_id = ? AND status = ? AND kind IN ?",
				tenantID, row.ID, string(approval.StatusApproved), []string{string(approval.KindContract), string(approval.KindStatusChange)}).
			Count(&approved).Error; err != nil {
			return err
		}
		if approved == 0 {
			return application.ErrFinancialChangeApprovalRequired
		}
		// 币种变更不在本入口范围内（属于换合同语义）。
		if !strings.EqualFold(strings.TrimSpace(change.Currency), strings.TrimSpace(row.Currency)) {
			return application.ErrFinancialChangeCurrencyLocked
		}
		if change.AmountMinor <= 0 {
			return fmt.Errorf("%w: 变更后的合同金额必须大于零", ErrPaymentTermsInvalid)
		}
		if len(change.PaymentTerms) == 0 {
			return fmt.Errorf("%w: 财务变更必须提供完整付款计划", ErrPaymentTermsInvalid)
		}
		terms, normalizeErr := contract.NormalizePaymentTerms(change.PaymentTerms, change.AmountMinor, row.Currency)
		if normalizeErr != nil {
			return fmt.Errorf("%w: %v", application.ErrValidation, normalizeErr)
		}
		now := time.Now().UTC()
		result := tx.Model(&contractRecord{}).
			Where("tenant_id = ? AND id = ? AND version = ? AND status = ?", tenantID, row.ID, change.ExpectedVersion, row.Status).
			Updates(map[string]any{
				"amount_minor": change.AmountMinor, "version": gorm.Expr("version + ?", 1),
				"updated_at": now, "updated_by": actorUserID,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return apperrors.ErrVersionConflict
		}
		updated := contractFromRecord(row)
		updated.AmountMinor, updated.Currency, updated.PaymentTerms = change.AmountMinor, row.Currency, terms
		updated.Version, updated.UpdatedAt = change.ExpectedVersion+1, now
		if err := replaceContractPaymentTerms(tx, updated); err != nil {
			return err
		}
		// 幂等键天然按版本：version 已 +1，因此这是该合同新版本的第一条事件。
		outcome, enqueueErr := enqueueFinancialEffective(tx, tenantID, row.ID)
		if enqueueErr != nil {
			return enqueueErr
		}
		if !outcome.Enqueued || outcome.SkipReason != "" {
			// 变更后仍无法生成合法事件是确定性异常，必须回滚而不是留下两套口径。
			return fmt.Errorf("%w: %s", application.ErrFinancialChangeEventNotQueued, outcome.SkipReason)
		}
		lifecycle := lifecycleEventRecord{
			ID: newID(), TenantID: tenantID, ContractID: row.ID, FromStatus: row.Status, ToStatus: row.Status,
			ActorUserID: stringPtr(actorUserID), Reason: stringPtr(change.Reason),
			IdempotencyKey: fmt.Sprintf("%s:financial:v%d", row.ID, updated.Version), OccurredAt: now,
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&lifecycle).Error; err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]any{
			"contract_id": row.ID, "from_version": change.ExpectedVersion, "to_version": updated.Version,
			"from_amount_minor": row.AmountMinor, "to_amount_minor": change.AmountMinor,
			"currency": strings.ToUpper(strings.TrimSpace(row.Currency)), "installment_count": len(terms),
			"reason": change.Reason, "approval_evidence": "APPROVED_APPROVAL_INSTANCE",
		})
		if err != nil {
			return err
		}
		meta := auditctx.FromContext(ctx)
		if err := insertAuditOutbox(tx, tenantID, newID(), AuditOutboxEntry{
			ActorID: actorUserID, Action: "CONTRACT_MANAGEMENT:FINANCIAL_TERMS_CHANGED",
			ResourceType: "contract", ResourceID: row.ID, Payload: payload,
			LoginIP: meta.LoginIP, ClientIP: meta.ClientIP, IPSource: meta.IPSource, RequestID: meta.RequestID,
		}); err != nil {
			return err
		}
		out = updated
		return nil
	})
	return out, err
}

// ClaimFinancialEffectiveDelivery 认领一条到期财务事件（兼容单条调用方）。
// 参数：ctx 控制认领事务。
// 返回值：认领载荷、是否存在和存储错误。
func (r *Repository) ClaimFinancialEffectiveDelivery(ctx context.Context) (settlementintegration.Delivery, bool, error) {
	deliveries, err := r.ClaimFinancialEffectiveDeliveries(ctx, 1)
	if err != nil || len(deliveries) == 0 {
		return settlementintegration.Delivery{}, false, err
	}
	return deliveries[0], true, nil
}

// ClaimFinancialEffectiveDeliveries 在单个事务内按积压顺序批量认领到期财务事件。
// 参数：ctx 控制事务，limit 为本轮上限（<=0 或 >200 时取 100）。
// 返回值：认领行及存储错误。每行仍各自写 processing + locked_at 租约 + attempts+1，
// 批量只放大单轮吞吐，不改变逐事件幂等、租约回收与失败退避语义。
func (r *Repository) ClaimFinancialEffectiveDeliveries(ctx context.Context, limit int) ([]settlementintegration.Delivery, error) {
	limit = boundedDeliveryClaimLimit(limit)
	deliveries := make([]settlementintegration.Delivery, 0, limit)
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []financialEffectiveOutboxRecord
		now := time.Now().UTC()
		err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("(delivery_status = ? AND next_attempt_at <= ?) OR (delivery_status = ? AND locked_at < ?)", "pending", now, "processing", now.Add(-5*time.Minute)).
			Order("next_attempt_at ASC, created_at ASC").Limit(limit).Find(&rows).Error
		if err != nil {
			return err
		}
		for index := range rows {
			attempts := rows[index].Attempts + 1
			if err := tx.Model(&financialEffectiveOutboxRecord{}).Where("id = ?", rows[index].ID).
				Updates(map[string]any{"delivery_status": "processing", "attempts": attempts, "locked_at": now}).Error; err != nil {
				return err
			}
			deliveries = append(deliveries, settlementintegration.Delivery{
				ID: rows[index].ID, TenantID: rows[index].TenantID, Payload: rows[index].PayloadJSON, Attempts: attempts,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return deliveries, nil
}

// boundedDeliveryClaimLimit 把批量认领上限收敛到有界区间，避免调用方传入 0/超大值。
// 参数：limit 为请求上限。
// 返回值：1~200 之间的有效上限。
func boundedDeliveryClaimLimit(limit int) int {
	if limit <= 0 || limit > 200 {
		return 100
	}
	return limit
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
