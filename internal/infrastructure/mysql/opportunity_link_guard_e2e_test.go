package mysql

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/oklog/ulid/v2"
)

// seedOpportunityLinkContract 写入一张受测合同并返回其 ID/编号。
// 参数：status 为生命周期状态，customer 非零时写入 CRM 客户，origin 控制普通/导入来源。
// 返回值：合同 ID 与合同编号；直接插入避免业务创建路径改写来源字段。
func seedOpportunityLinkContract(t *testing.T, r *Repository, tenant string, status contract.Status, customer uint64, origin string) (string, string) {
	t.Helper()
	id := ulid.Make().String()
	number := "HT-LINK-" + id[:10]
	now := time.Now().UTC()
	record := contractRecord{
		ID: id, TenantID: tenant, ContractNumber: &number, Title: "商机核对测试合同",
		ContractType: "sales", ServiceType: "integration", OwnerUserID: ulid.Make().String(),
		AmountMinor: 100000, Currency: "CNY", Content: "{}", Status: string(status),
		Version: 1, CreatedAt: now, CreatedBy: "seed", UpdatedAt: now, UpdatedBy: "seed",
	}
	if customer != 0 {
		record.CRMCustomerID = &customer
	}
	if origin != "" {
		record.OriginKind = origin
		record.ImportMode = "BASELINE"
		record.ImportPolicyJSON = []byte(`{"mode":"BASELINE"}`)
	}
	if err := r.db.Create(&record).Error; err != nil {
		t.Fatalf("seed link contract: %v", err)
	}
	return id, number
}

// seedOpportunityIntake 写入一条待审核的商机接入记录（商机固定为 7）。
func seedOpportunityIntake(t *testing.T, r *Repository, tenant, ref string, customer uint64) string {
	t.Helper()
	return seedOpportunityIntakeForOpportunity(t, r, tenant, ref, customer, 7)
}

// seedOpportunityIntakeForOpportunity 写入指定商机号的待审核接入记录。
// 参数：ref 为合同 ID 或编号，customer 为 CRM 客户，opportunityID 为商机 ID。
// 返回值：接入记录主键。
func seedOpportunityIntakeForOpportunity(t *testing.T, r *Repository, tenant, ref string, customer, opportunityID uint64) string {
	t.Helper()
	now := time.Now().UTC()
	intakeID := ulid.Make().String()
	row := opportunityIntakeRecord{
		IntakeID: intakeID, TenantID: tenant, EventID: ulid.Make().String(), OpportunityID: opportunityID,
		EventVersion: 1, OpportunityNo: "SJ-LINK", CustomerID: customer, ContractRef: ref,
		ExpectedAmount: "100.00", OccurredAt: now, Status: application.OpportunityIntakeAccepted,
		Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := r.db.Create(&row).Error; err != nil {
		t.Fatalf("seed opportunity intake: %v", err)
	}
	return intakeID
}

// pinContractOpportunity 把受测合同的商机引用固定为指定值，模拟“已关联商机 A”的合同。
// 参数：contractID 为合同主键，opportunityID 为已关联商机；仅用于测试数据准备。
// 返回值：无。
func pinContractOpportunity(t *testing.T, r *Repository, tenant, contractID string, opportunityID uint64) {
	t.Helper()
	linked := strconv.FormatUint(opportunityID, 10)
	if err := r.db.Model(&contractRecord{}).
		Where("tenant_id = ? AND id = ?", tenant, contractID).
		Update("opportunity_id", linked).Error; err != nil {
		t.Fatalf("pin contract opportunity: %v", err)
	}
}

func reviewOpportunityLink(t *testing.T, r *Repository, tenant, intakeID, key string) (application.OpportunityIntake, error) {
	t.Helper()
	return r.ReviewOpportunityIntake(context.Background(), tenant, intakeID, "reviewer-1",
		application.OpportunityIntakeReview{Decision: application.OpportunityIntakeLinkConfirmed, Reason: "核对一致", Version: 1},
		key, "hash-"+key, "审核员")
}

// TestOpportunityLinkGuardAndAuditOnIsolatedMySQL 验证商机核对关联的来源/状态/客户门禁、
// 同事务审计、幂等重放与 ID/编号冲突拒绝。
// 参数：t 为测试上下文；仅显式 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestOpportunityLinkGuardAndAuditOnIsolatedMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	t.Cleanup(func() {
		for _, model := range []any{&opportunityLinkOutboxRecord{}, &opportunityIntakeReviewRecord{}, &opportunityIntakeRecord{}, &auditOutboxRecord{}} {
			if err := r.db.Where("tenant_id = ?", tenant).Delete(model).Error; err != nil {
				t.Error("opportunity link test cleanup failed:", err)
			}
		}
	})

	t.Run("draft and pending first association writes audit", func(t *testing.T) {
		for _, status := range []contract.Status{contract.StatusDraft, contract.StatusPending} {
			contractID, number := seedOpportunityLinkContract(t, r, tenant, status, 0, "")
			intakeID := seedOpportunityIntake(t, r, tenant, number, 42)
			result, err := reviewOpportunityLink(t, r, tenant, intakeID, "idem-"+intakeID)
			if err != nil {
				t.Fatalf("%s review: %v", status, err)
			}
			if result.ContractID != contractID || result.ContractNumber != number {
				t.Fatalf("%s result=%+v", status, result)
			}
			var linked contractRecord
			if err := r.db.Where("tenant_id = ? AND id = ?", tenant, contractID).Take(&linked).Error; err != nil {
				t.Fatal(err)
			}
			if uint64Value(linked.CRMCustomerID) != 42 || stringValue(linked.OpportunityID) != "7" {
				t.Fatalf("%s contract refs=%+v", status, linked)
			}
			var audits []auditOutboxRecord
			if err := r.db.Where("tenant_id = ? AND action = ? AND resource_id = ?", tenant, "CONTRACT_MANAGEMENT:OPPORTUNITY_LINK_CONFIRMED", contractID).Find(&audits).Error; err != nil {
				t.Fatal(err)
			}
			if len(audits) != 1 || !strings.Contains(string(audits[0].PayloadJSON), intakeID) ||
				!strings.Contains(string(audits[0].PayloadJSON), `"before"`) || !strings.Contains(string(audits[0].PayloadJSON), `"after"`) {
				t.Fatalf("%s audit=%+v payload=%s", status, audits, audits[0].PayloadJSON)
			}
			var outbox int64
			if err := r.db.Model(&opportunityLinkOutboxRecord{}).Where("tenant_id = ? AND intake_id = ?", tenant, intakeID).Count(&outbox).Error; err != nil || outbox != 1 {
				t.Fatalf("%s link outbox count=%d err=%v", status, outbox, err)
			}
			// 同一幂等键重放必须返回同一结果且不重复写审计/Outbox。
			replayed, err := reviewOpportunityLink(t, r, tenant, intakeID, "idem-"+intakeID)
			if err != nil || replayed.ContractID != contractID {
				t.Fatalf("%s replay=%+v err=%v", status, replayed, err)
			}
			var auditCount int64
			if err := r.db.Model(&auditOutboxRecord{}).Where("tenant_id = ? AND action = ? AND resource_id = ?", tenant, "CONTRACT_MANAGEMENT:OPPORTUNITY_LINK_CONFIRMED", contractID).Count(&auditCount).Error; err != nil || auditCount != 1 {
				t.Fatalf("%s replay audit=%d err=%v", status, auditCount, err)
			}
		}
	})

	rejectCases := []struct {
		name     string
		status   contract.Status
		customer uint64
		origin   string
	}{
		{name: "imported baseline", status: contract.StatusDraft, origin: "IMPORTED"},
		{name: "archived", status: contract.StatusArchived},
		{name: "terminated", status: contract.StatusTerminated},
		{name: "customer mismatch", status: contract.StatusDraft, customer: 99},
		{name: "approved first association", status: contract.StatusApproved},
	}
	for _, testCase := range rejectCases {
		t.Run("reject "+testCase.name, func(t *testing.T) {
			_, number := seedOpportunityLinkContract(t, r, tenant, testCase.status, testCase.customer, testCase.origin)
			intakeID := seedOpportunityIntake(t, r, tenant, number, 42)
			_, err := reviewOpportunityLink(t, r, tenant, intakeID, "idem-"+intakeID)
			if !errors.Is(err, application.ErrOpportunityLinkRejected) {
				t.Fatalf("err=%v want ErrOpportunityLinkRejected", err)
			}
			// 拒绝必须整体回滚：接入记录仍为 ACCEPTED，未写关联 Outbox。
			var intake opportunityIntakeRecord
			if err := r.db.Where("tenant_id = ? AND intake_id = ?", tenant, intakeID).Take(&intake).Error; err != nil {
				t.Fatal(err)
			}
			if intake.Status != application.OpportunityIntakeAccepted {
				t.Fatalf("rejected intake status=%s", intake.Status)
			}
			var outbox int64
			if err := r.db.Model(&opportunityLinkOutboxRecord{}).Where("tenant_id = ? AND intake_id = ?", tenant, intakeID).Count(&outbox).Error; err != nil || outbox != 0 {
				t.Fatalf("rejected outbox=%d err=%v", outbox, err)
			}
		})
	}

	t.Run("same customer other opportunity is rejected without rewriting contract", func(t *testing.T) {
		// 已审批合同且客户一致，但已关联商机 7：改写为同客户商机 8 必须整体拒绝。
		contractID, number := seedOpportunityLinkContract(t, r, tenant, contract.StatusApproved, 42, "")
		pinContractOpportunity(t, r, tenant, contractID, 7)
		otherIntake := seedOpportunityIntakeForOpportunity(t, r, tenant, number, 42, 8)
		_, err := reviewOpportunityLink(t, r, tenant, otherIntake, "idem-"+otherIntake)
		if !errors.Is(err, application.ErrOpportunityLinkRejected) {
			t.Fatalf("err=%v want ErrOpportunityLinkRejected", err)
		}
		var unchanged contractRecord
		if err := r.db.Where("tenant_id = ? AND id = ?", tenant, contractID).Take(&unchanged).Error; err != nil {
			t.Fatal(err)
		}
		if stringValue(unchanged.OpportunityID) != "7" || uint64Value(unchanged.CRMCustomerID) != 42 {
			t.Fatalf("contract refs rewritten: %+v", unchanged)
		}
		var auditCount int64
		if err := r.db.Model(&auditOutboxRecord{}).Where("tenant_id = ? AND action = ? AND resource_id = ?", tenant, "CONTRACT_MANAGEMENT:OPPORTUNITY_LINK_CONFIRMED", contractID).Count(&auditCount).Error; err != nil || auditCount != 0 {
			t.Fatalf("rejected audit=%d err=%v", auditCount, err)
		}
		var linkedOutbox int64
		if err := r.db.Model(&opportunityLinkOutboxRecord{}).Where("tenant_id = ? AND intake_id = ?", tenant, otherIntake).Count(&linkedOutbox).Error; err != nil || linkedOutbox != 0 {
			t.Fatalf("rejected outbox=%d err=%v", linkedOutbox, err)
		}
		// 同一商机 7 的重复确认保持幂等成功，引用不变。
		sameIntake := seedOpportunityIntakeForOpportunity(t, r, tenant, number, 42, 7)
		if _, err := reviewOpportunityLink(t, r, tenant, sameIntake, "idem-"+sameIntake); err != nil {
			t.Fatalf("same opportunity confirm err=%v", err)
		}
		var replay contractRecord
		if err := r.db.Where("tenant_id = ? AND id = ?", tenant, contractID).Take(&replay).Error; err != nil {
			t.Fatal(err)
		}
		if stringValue(replay.OpportunityID) != "7" || uint64Value(replay.CRMCustomerID) != 42 {
			t.Fatalf("same opportunity replay refs=%+v", replay)
		}
	})

	t.Run("contract ref matching both id and number of different contracts is rejected", func(t *testing.T) {
		id, _ := seedOpportunityLinkContract(t, r, tenant, contract.StatusDraft, 0, "")
		otherID := ulid.Make().String()
		now := time.Now().UTC()
		conflict := contractRecord{
			ID: otherID, TenantID: tenant, ContractNumber: &id, Title: "编号撞 ID",
			ContractType: "sales", ServiceType: "integration", OwnerUserID: ulid.Make().String(),
			AmountMinor: 100, Currency: "CNY", Content: "{}", Status: string(contract.StatusDraft),
			Version: 1, CreatedAt: now, CreatedBy: "seed", UpdatedAt: now, UpdatedBy: "seed",
		}
		if err := r.db.Create(&conflict).Error; err != nil {
			t.Fatal(err)
		}
		intakeID := seedOpportunityIntake(t, r, tenant, id, 42)
		_, err := reviewOpportunityLink(t, r, tenant, intakeID, "idem-"+intakeID)
		if !errors.Is(err, application.ErrOpportunityLinkRejected) {
			t.Fatalf("err=%v want ErrOpportunityLinkRejected", err)
		}
	})

	t.Run("unknown contract ref is not found", func(t *testing.T) {
		intakeID := seedOpportunityIntake(t, r, tenant, "HT-MISSING-"+ulid.Make().String()[:8], 42)
		_, err := reviewOpportunityLink(t, r, tenant, intakeID, "idem-"+intakeID)
		if !errors.Is(err, apperrors.ErrNotFound) {
			t.Fatalf("err=%v want ErrNotFound", err)
		}
	})
}
