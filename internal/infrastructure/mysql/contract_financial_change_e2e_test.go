package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
)

// 本用例需要隔离测试库（CONTRACT_TEST_DSN，contract_test_* 回环库）；未配置时跳过。
// 覆盖 BIZ-XEVT2-05 合同侧：per-version 补建、受控变更 version+1 与新事件、
// 无计划=不发送，以及“旧版本重复提交不得产生第二份事件”。

// financialChangeE2EContract 造一份已审批生效的普通合同夹具（含付款计划与审批证据）。
// 参数：t 为测试上下文，r 为仓储，tenant 为隔离租户。
// 返回值：合同记录与其版本。
func financialChangeE2EContract(t *testing.T, r *Repository, tenant string) contractRecord {
	t.Helper()
	ctx := context.Background()
	original := draftE2EContract(t, r, tenant)
	schedule := []contract.Contract{
		{
			ID: original.ID, TenantID: tenant, AmountMinor: 1000000, Currency: "CNY",
			PaymentTerms: []contract.PaymentTerm{
				{InstallmentNo: 1, DueDate: "2026-12-01", Amount: "4000.00"},
				{InstallmentNo: 2, DueDate: "2027-01-01", Amount: "6000.00"},
			},
		},
	}
	if err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return replaceContractPaymentTerms(tx, schedule[0])
	}); err != nil {
		t.Fatal(err)
	}
	// 模拟既有审批通过后的生效合同：状态生效、版本+1，并保留一次已通过的审批实例。
	number := "HT-FIN-" + ulid.Make().String()[:12]
	if err := r.db.WithContext(ctx).Model(&contractRecord{}).Where("tenant_id = ? AND id = ?", tenant, original.ID).Updates(map[string]any{
		"status": string(contract.StatusActive), "version": gorm.Expr("version + ?", 1),
		"contract_number": number, "crm_customer_id": uint64(42), "customer_name": "客户甲",
	}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	instance := approvalInstanceRecord{
		ID: ulid.Make().String(), TenantID: tenant, ContractID: original.ID,
		Kind: string(approval.KindContract), Status: string(approval.StatusApproved),
		ApplicantUserID: original.OwnerUserID, NodesJSON: []byte("[]"),
		TemporalWorkflowID: "financial-change-" + original.ID, TemporalRunID: ulid.Make().String(),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := r.db.WithContext(ctx).Create(&instance).Error; err != nil {
		t.Fatal(err)
	}
	var row contractRecord
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenant, original.ID).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func financialEventRows(t *testing.T, r *Repository, tenant, contractID string) []financialEffectiveOutboxRecord {
	t.Helper()
	var rows []financialEffectiveOutboxRecord
	if err := r.db.Where("tenant_id = ? AND contract_id = ?", tenant, contractID).Order("contract_version ASC").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestContractFinancialChangeVersionedEventE2E 验证合同侧受控财务变更的完整语义。
// 参数：t 为测试上下文（需 CONTRACT_TEST_DSN）。
// 返回值：无。
func TestContractFinancialChangeVersionedEventE2E(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	row := financialChangeE2EContract(t, r, tenant)

	// 1) 审批生效后补建当前版本事件：只允许一条。
	if _, err := r.ReconcileFinancialEffectives(ctx); err != nil {
		t.Fatal(err)
	}
	events := financialEventRows(t, r, tenant, row.ID)
	if len(events) != 1 || events[0].ContractVersion != row.Version {
		t.Fatalf("首版事件异常：%+v row.Version=%d", events, row.Version)
	}
	if _, err := r.ReconcileFinancialEffectives(ctx); err != nil {
		t.Fatal(err)
	}
	if again := financialEventRows(t, r, tenant, row.ID); len(again) != 1 {
		t.Fatalf("同一版本重复补建：%d", len(again))
	}

	// 2) 版本升级（例如状态流转）后必须补建新版本事件：per-version 语义。
	if err := r.db.WithContext(ctx).Model(&contractRecord{}).Where("tenant_id = ? AND id = ?", tenant, row.ID).
		Update("version", gorm.Expr("version + ?", 1)).Error; err != nil {
		t.Fatal(err)
	}
	if created, err := r.ReconcileFinancialEffectives(ctx); err != nil || created != 1 {
		t.Fatalf("版本升级未补建：created=%d err=%v", created, err)
	}
	events = financialEventRows(t, r, tenant, row.ID)
	if len(events) != 2 || events[1].ContractVersion != row.Version+1 {
		t.Fatalf("per-version 补建异常：%+v", events)
	}
	current := row.Version + 1

	// 3) 受控财务变更：version+1、替换计划、生成新版本事件、审计留痕。
	change := application.ContractFinancialChange{
		ContractID: row.ID, ExpectedVersion: current, AmountMinor: 1200000, Currency: "CNY",
		PaymentTerms: []contract.PaymentTerm{
			{InstallmentNo: 1, DueDate: "2026-12-20", Amount: "5000.00"},
			{InstallmentNo: 2, DueDate: "2027-01-20", Amount: "7000.00"},
		},
		Reason: "客户追加范围，财务复核后调整总额",
	}
	updated, err := r.ApplyContractFinancialChange(ctx, tenant, change, "actor-1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != current+1 || updated.AmountMinor != 1200000 || len(updated.PaymentTerms) != 2 || updated.PaymentTerms[1].Amount != "7000.00" {
		t.Fatalf("变更结果异常：%+v", updated)
	}
	stored, err := r.ListContractPaymentTerms(ctx, tenant, row.ID)
	if err != nil || len(stored) != 2 || stored[0].Amount != "5000.00" {
		t.Fatalf("付款计划未在同一事务替换：%+v err=%v", stored, err)
	}
	events = financialEventRows(t, r, tenant, row.ID)
	if len(events) != 3 || events[2].ContractVersion != current+1 {
		t.Fatalf("新版本事件未生成：%+v", events)
	}
	var payload financialEffectivePayload
	if err := json.Unmarshal(events[2].PayloadJSON, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Contract.Version != int(current+1) || payload.Contract.Amount != "12000.00" || len(payload.Contract.PaymentTerms) != 2 {
		t.Fatalf("事件载荷未反映新版本：%+v", payload.Contract)
	}
	var lifecycle lifecycleEventRecord
	if err := r.db.Where("tenant_id = ? AND contract_id = ? AND idempotency_key = ?", tenant, row.ID, fmt.Sprintf("%s:financial:v%d", row.ID, current+1)).Take(&lifecycle).Error; err != nil {
		t.Fatalf("缺少财务变更生命周期留痕：%v", err)
	}
	var audit auditOutboxRecord
	if err := r.db.Where("tenant_id = ? AND resource_id = ? AND action = ?", tenant, row.ID, "CONTRACT_MANAGEMENT:FINANCIAL_TERMS_CHANGED").Take(&audit).Error; err != nil {
		t.Fatalf("缺少财务变更审计：%v", err)
	}
	if !strings.Contains(string(audit.PayloadJSON), `"to_amount_minor":1200000`) {
		t.Fatalf("审计载荷缺少金额跃迁：%s", audit.PayloadJSON)
	}

	// 4) 旧版本重复提交：版本冲突且不产生任何新事件或计划回退。
	if _, err := r.ApplyContractFinancialChange(ctx, tenant, change, "actor-1"); !errors.Is(err, apperrors.ErrVersionConflict) {
		t.Fatalf("旧版本变更必须冲突：%v", err)
	}
	if after := financialEventRows(t, r, tenant, row.ID); len(after) != 3 {
		t.Fatalf("冲突请求产生了事件：%d", len(after))
	}
	if created, err := r.ReconcileFinancialEffectives(ctx); err != nil || created != 0 {
		t.Fatalf("变更后补建不应重复：created=%d err=%v", created, err)
	}

	// 5) 无付款计划=不发送：新合同即使生效也不得合成事件。
	noPlan := draftE2EContract(t, r, tenant)
	if err := r.db.WithContext(ctx).Model(&contractRecord{}).Where("tenant_id = ? AND id = ?", tenant, noPlan.ID).Updates(map[string]any{
		"status": string(contract.StatusActive), "version": gorm.Expr("version + ?", 1),
		"contract_number": "HT-NOPLAN-" + ulid.Make().String()[:8], "crm_customer_id": uint64(42), "customer_name": "客户乙",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReconcileFinancialEffectives(ctx); err != nil {
		t.Fatal(err)
	}
	if rows := financialEventRows(t, r, tenant, noPlan.ID); len(rows) != 0 {
		t.Fatalf("无计划合同不得产生财务事件：%d", len(rows))
	}
}

// TestContractFinancialChangeRequiresApprovedEvidenceE2E 验证缺少审批证据或存在
// 在途审批时受控变更失败关闭，合同金额/版本/计划都不被改写。
// 参数：t 为测试上下文（需 CONTRACT_TEST_DSN）。
// 返回值：无。
func TestContractFinancialChangeRequiresApprovedEvidenceE2E(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	row := financialChangeE2EContract(t, r, tenant)
	change := application.ContractFinancialChange{
		ContractID: row.ID, ExpectedVersion: row.Version, AmountMinor: 1200000, Currency: "CNY",
		PaymentTerms: []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2026-12-20", Amount: "12000.00"}},
		Reason:       "审批证据缺失必须拒绝",
	}
	// 1) 在途审批：不得改额。
	running := approvalInstanceRecord{
		ID: ulid.Make().String(), TenantID: tenant, ContractID: row.ID,
		Kind: string(approval.KindStatusChange), Status: string(approval.StatusRunning),
		ApplicantUserID: "actor-1", NodesJSON: []byte("[]"),
		TemporalWorkflowID: "financial-change-running-" + row.ID, TemporalRunID: ulid.Make().String(),
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := r.db.WithContext(ctx).Create(&running).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := r.ApplyContractFinancialChange(ctx, tenant, change, "actor-1"); !errors.Is(err, apperrors.ErrStateConflict) {
		t.Fatalf("在途审批未拒绝：%v", err)
	}
	if err := r.db.WithContext(ctx).Where("id = ?", running.ID).Delete(&approvalInstanceRecord{}).Error; err != nil {
		t.Fatal(err)
	}
	// 2) 没有任何已通过审批实例：不得改额。
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND contract_id = ? AND kind = ?", tenant, row.ID, string(approval.KindContract)).Delete(&approvalInstanceRecord{}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := r.ApplyContractFinancialChange(ctx, tenant, change, "actor-1"); !errors.Is(err, application.ErrFinancialChangeApprovalRequired) {
		t.Fatalf("缺失审批证据未拒绝：%v", err)
	}
	stored, err := r.GetContract(ctx, tenant, row.ID)
	if err != nil || stored.Version != row.Version || stored.AmountMinor != row.AmountMinor {
		t.Fatalf("拒绝后合同被改写：%+v err=%v", stored, err)
	}
	if len(financialEventRows(t, r, tenant, row.ID)) != 0 {
		t.Fatal("拒绝后不得产生财务事件")
	}
}
