package mysql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/oklog/ulid/v2"
)

// seedSummaryContract 写入一张带 CRM 归属的普通合同。
func seedSummaryContract(t *testing.T, r *Repository, tenant string, updatedAt time.Time) (string, string) {
	t.Helper()
	id := ulid.Make().String()
	number := "HT-SUM-" + id[:10]
	opportunity := "7"
	customer := uint64(42)
	now := time.Now().UTC()
	record := contractRecord{
		ID: id, TenantID: tenant, ContractNumber: &number, Title: "摘要测试合同",
		ContractType: "sales", ServiceType: "integration", OwnerUserID: ulid.Make().String(),
		OpportunityID: &opportunity, CRMCustomerID: &customer,
		AmountMinor: 100, Currency: "CNY", Content: "{}", Status: string(contract.StatusDraft),
		Version: 1, CreatedAt: now, CreatedBy: "seed", UpdatedAt: updatedAt, UpdatedBy: "seed",
	}
	if err := r.db.Create(&record).Error; err != nil {
		t.Fatalf("seed summary contract: %v", err)
	}
	return id, number
}

// TestFindContractSummaryCandidateOnIsolatedMySQL 验证摘要候选的租户边界、
// ID/编号双路径匹配与 linked_at 证据优先级。
// 参数：t 为测试上下文；仅显式 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestFindContractSummaryCandidateOnIsolatedMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	updatedAt := time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	contractID, number := seedSummaryContract(t, r, tenant, updatedAt)
	t.Cleanup(func() {
		if err := r.db.Where("tenant_id = ?", tenant).Delete(&opportunityIntakeRecord{}).Error; err != nil {
			t.Error("contract summary test cleanup failed:", err)
		}
	})

	// 无商机接入审核记录时退回合同 updated_at。
	for _, ref := range []string{contractID, number} {
		candidate, err := r.FindContractSummaryCandidate(ctx, tenant, ref)
		if err != nil {
			t.Fatalf("ref=%s err=%v", ref, err)
		}
		if candidate.ContractID != contractID || candidate.ContractNumber != number ||
			candidate.CRMCustomerID != 42 || candidate.CRMOpportunityID != 7 || !candidate.LinkedAt.IsZero() {
			t.Fatalf("ref=%s candidate=%+v", ref, candidate)
		}
		if !candidate.UpdatedAt.Equal(updatedAt) {
			t.Fatalf("updated_at=%s want %s", candidate.UpdatedAt, updatedAt)
		}
	}
	// 跨租户与不存在都必须不可见。
	if _, err := r.FindContractSummaryCandidate(ctx, ulid.Make().String(), contractID); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("cross tenant err=%v", err)
	}
	if _, err := r.FindContractSummaryCandidate(ctx, tenant, "HT-MISSING"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("missing err=%v", err)
	}

	// 有 LINK_CONFIRMED 审核记录时必须使用 reviewed_at 作为可证明关联时间。
	now := time.Now().UTC()
	reviewedAt := updatedAt.Add(2 * time.Hour)
	intakeID := ulid.Make().String()
	ref := number
	intake := opportunityIntakeRecord{
		IntakeID: intakeID, TenantID: tenant, EventID: ulid.Make().String(), OpportunityID: 7,
		EventVersion: 1, OpportunityNo: "SJ-SUM", CustomerID: 42, ContractRef: ref,
		ContractID: &contractID, ContractNumber: &number, ExpectedAmount: "100.00", OccurredAt: now,
		Status: application.OpportunityIntakeLinkConfirmed, Version: 2,
		ReviewedAt: &reviewedAt, CreatedAt: now, UpdatedAt: now,
	}
	if err := r.db.Create(&intake).Error; err != nil {
		t.Fatal(err)
	}
	candidate, err := r.FindContractSummaryCandidate(ctx, tenant, contractID)
	if err != nil {
		t.Fatal(err)
	}
	if !candidate.LinkedAt.Equal(reviewedAt) {
		t.Fatalf("linked_at=%s want %s", candidate.LinkedAt, reviewedAt)
	}

	// 历史接入行没有回填 reviewed_at 时，用同单 LINK_CONFIRMED 审核流水的
	// created_at 回填“审核时间”，不得退化成近似 updated_at。
	historicalContractID, historicalNumber := seedSummaryContract(t, r, tenant, updatedAt.Add(-24*time.Hour))
	historicalIntakeID := ulid.Make().String()
	historical := opportunityIntakeRecord{
		IntakeID: historicalIntakeID, TenantID: tenant, EventID: ulid.Make().String(), OpportunityID: 7,
		EventVersion: 1, OpportunityNo: "SJ-HIST", CustomerID: 42, ContractRef: historicalNumber,
		ContractID: &historicalContractID, ContractNumber: &historicalNumber, ExpectedAmount: "100.00", OccurredAt: now,
		Status: application.OpportunityIntakeLinkConfirmed, Version: 2,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := r.db.Create(&historical).Error; err != nil {
		t.Fatal(err)
	}
	reviewedAt = now.Add(-3 * time.Hour)
	review := opportunityIntakeReviewRecord{
		ReviewID: ulid.Make().String(), TenantID: tenant, IntakeID: historicalIntakeID,
		IdempotencyKey: "summary-history-" + historicalIntakeID, RequestHash: "hash",
		Decision: application.OpportunityIntakeLinkConfirmed, Reason: "历史回填", Version: 1,
		ReviewerID: "reviewer", ResponseJSON: []byte(`{}`), CreatedAt: reviewedAt,
	}
	if err := r.db.Create(&review).Error; err != nil {
		t.Fatal(err)
	}
	candidate, err = r.FindContractSummaryCandidate(ctx, tenant, historicalContractID)
	if err != nil {
		t.Fatal(err)
	}
	if !candidate.LinkedAt.Equal(reviewedAt) {
		t.Fatalf("历史审核流水回填 linked_at=%s want %s", candidate.LinkedAt, reviewedAt)
	}

	// 既无 reviewed_at 也无审核流水/审计流水时，第 4 层退回接入单创建时间，
	// 并标注 INTAKE_CREATED（接入单创建早于确认动作，精度由应用层判为 approx）。
	fallbackContractID, fallbackNumber := seedSummaryContract(t, r, tenant, updatedAt)
	intakeCreatedAt := now.Add(-30 * time.Hour)
	fallback := opportunityIntakeRecord{
		IntakeID: ulid.Make().String(), TenantID: tenant, EventID: ulid.Make().String(), OpportunityID: 7,
		EventVersion: 1, OpportunityNo: "SJ-FALLBACK", CustomerID: 42, ContractRef: fallbackNumber,
		ContractID: &fallbackContractID, ContractNumber: &fallbackNumber, ExpectedAmount: "100.00", OccurredAt: now,
		Status: application.OpportunityIntakeLinkConfirmed, Version: 2,
		CreatedAt: intakeCreatedAt, UpdatedAt: now,
	}
	if err := r.db.Create(&fallback).Error; err != nil {
		t.Fatal(err)
	}
	candidate, err = r.FindContractSummaryCandidate(ctx, tenant, fallbackContractID)
	if err != nil {
		t.Fatal(err)
	}
	if !candidate.LinkedAt.Equal(intakeCreatedAt) || !candidate.FirstLinkedAt.Equal(intakeCreatedAt) ||
		candidate.LinkedAtSource != application.ContractSummaryLinkSourceIntakeCreated {
		t.Fatalf("仅有接入单创建时间时 candidate=%+v want INTAKE_CREATED=%s", candidate, intakeCreatedAt)
	}

	// 连接入单都没有（CRM 直接建合同）时不得伪造时间：三字段保持零值，
	// 由应用层回退 updated_at 并标注 APPROX_CONTRACT_UPDATED。
	bareContractID, _ := seedSummaryContract(t, r, tenant, updatedAt)
	candidate, err = r.FindContractSummaryCandidate(ctx, tenant, bareContractID)
	if err != nil {
		t.Fatal(err)
	}
	if !candidate.LinkedAt.IsZero() || !candidate.FirstLinkedAt.IsZero() || candidate.LinkedAtSource != "" {
		t.Fatalf("无任何证据时不应伪造 linked_at：%+v", candidate)
	}
}

// TestContractSummaryMultipleIntakesOnIsolatedMySQL 验证多张接入单时
// linked_at 取最新一次、first_linked_at 取最早一次（不再是单点时间）。
// 参数：t 为测试上下文；仅显式 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestContractSummaryMultipleIntakesOnIsolatedMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	updatedAt := time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	contractID, number := seedSummaryContract(t, r, tenant, updatedAt)
	t.Cleanup(func() {
		if err := r.db.Where("tenant_id = ?", tenant).Delete(&opportunityIntakeRecord{}).Error; err != nil {
			t.Error("contract summary multi-intake cleanup failed:", err)
		}
	})
	now := time.Now().UTC()
	earliest := updatedAt.Add(3 * time.Hour)
	latest := updatedAt.Add(60 * time.Hour)
	for index, reviewedAt := range []time.Time{latest, earliest} {
		value := reviewedAt
		intake := opportunityIntakeRecord{
			IntakeID: ulid.Make().String(), TenantID: tenant, EventID: ulid.Make().String(), OpportunityID: 7,
			EventVersion: 1, OpportunityNo: "SJ-MULTI", CustomerID: 42, ContractRef: number,
			ContractID: &contractID, ContractNumber: &number, ExpectedAmount: "100.00", OccurredAt: now,
			Status: application.OpportunityIntakeLinkConfirmed, Version: uint64(index + 2),
			ReviewedAt: &value, CreatedAt: now, UpdatedAt: now,
		}
		if err := r.db.Create(&intake).Error; err != nil {
			t.Fatal(err)
		}
	}
	candidate, err := r.FindContractSummaryCandidate(ctx, tenant, contractID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.LinkedAtSource != application.ContractSummaryLinkSourceCRMReview {
		t.Fatalf("source=%q want CRM_REVIEW", candidate.LinkedAtSource)
	}
	if !candidate.LinkedAt.Equal(latest) {
		t.Fatalf("linked_at=%s want 最新一次 %s", candidate.LinkedAt, latest)
	}
	if !candidate.FirstLinkedAt.Equal(earliest) {
		t.Fatalf("first_linked_at=%s want 最早一次 %s", candidate.FirstLinkedAt, earliest)
	}
}

// TestContractSummaryAuditOutboxEvidenceOnIsolatedMySQL 验证第 3 层证据：
// 只存在 con_audit_outbox 的关联确认流水（接入单已被清理/迁移的历史合同）时，
// 仍能从权威审计痕迹恢复 linked_at，并区分最新与最早两次关联。
// 参数：t 为测试上下文；仅显式 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestContractSummaryAuditOutboxEvidenceOnIsolatedMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	updatedAt := time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	contractID, _ := seedSummaryContract(t, r, tenant, updatedAt)
	t.Cleanup(func() {
		if err := r.db.Where("tenant_id = ?", tenant).Delete(&auditOutboxRecord{}).Error; err != nil {
			t.Error("contract summary audit cleanup failed:", err)
		}
	})
	firstLinkedAt := updatedAt.Add(2 * time.Hour)
	latestLinkedAt := updatedAt.Add(50 * time.Hour)
	for index, linkedAt := range []time.Time{firstLinkedAt, latestLinkedAt} {
		row := auditOutboxRecord{
			ID: newID(), TenantID: tenant, EventID: ulid.Make().String(),
			Action:       "CONTRACT_MANAGEMENT:OPPORTUNITY_LINK_CONFIRMED",
			ResourceType: "contract", ResourceID: contractID, IPSource: "UNKNOWN", Result: "SUCCESS",
			DeliveryStatus: "pending", NextAttemptAt: linkedAt, CreatedAt: linkedAt,
		}
		if index == 1 {
			// 非 SUCCESS 的失败尝试不得成为证据。
			failed := row
			failed.ID = newID()
			failed.EventID = ulid.Make().String()
			failed.Result = "FAILURE"
			failed.CreatedAt = latestLinkedAt.Add(time.Hour)
			if err := r.db.Create(&failed).Error; err != nil {
				t.Fatal(err)
			}
		}
		if err := r.db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	candidate, err := r.FindContractSummaryCandidate(ctx, tenant, contractID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.LinkedAtSource != application.ContractSummaryLinkSourceAuditLog {
		t.Fatalf("source=%q want AUDIT_LOG", candidate.LinkedAtSource)
	}
	if !candidate.LinkedAt.Equal(latestLinkedAt) {
		t.Fatalf("linked_at=%s want 最新一次 %s", candidate.LinkedAt, latestLinkedAt)
	}
	if !candidate.FirstLinkedAt.Equal(firstLinkedAt) {
		t.Fatalf("first_linked_at=%s want 最早一次 %s", candidate.FirstLinkedAt, firstLinkedAt)
	}
	// 跨租户审计流水不能成为证据：换租户查同一合同 ID 必须不可见。
	otherTenant := ulid.Make().String()
	if _, err := r.FindContractSummaryCandidate(ctx, otherTenant, contractID); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("cross tenant err=%v", err)
	}
}

// TestContractSummaryResourceIndexOnIsolatedMySQL 断言第 3 层证据查询所需的索引
// 真实存在且定义与迁移一致：缺索引时摘要接口会扫整张审计 Outbox。
// 参数：t 为测试上下文；仅显式 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestContractSummaryResourceIndexOnIsolatedMySQL(t *testing.T) {
	r, _ := draftE2ERepository(t)
	var columns []string
	if err := r.db.Raw(`SELECT column_name FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = 'con_audit_outbox'
		  AND index_name = 'idx_con_audit_outbox_resource'
		ORDER BY seq_in_index`).Scan(&columns).Error; err != nil {
		t.Fatal(err)
	}
	want := []string{"tenant_id", "resource_type", "resource_id", "action", "created_at"}
	if strings.Join(columns, ",") != strings.Join(want, ",") {
		t.Fatalf("idx_con_audit_outbox_resource 列序=%v want %v", columns, want)
	}
}
