package mysql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/testfixture"
	"github.com/oklog/ulid/v2"
)

// autoApproveHistoryFixture 原子建立一个「CRM 开立刚完成、等待自动审批」的历史批次。
//
// 与 historyMySQLFixture 的差异只有两点：Confirmation 为空（自动审批需自行合成
// 确认台账），批次保持 RUNNING（开立编排持有租约）。合同行按真实开立结果写成
// version=1、SourceFileStatus=READY、OriginKind=IMPORTED，即 confirmHistoricalItem
// 要求的前置基线。
//
// 参数：t 为测试上下文，r/tenant 为显式隔离数据库。
// 返回值：已建单待自动确认的批次；不连接业务环境。
func autoApproveHistoryFixture(t *testing.T, r *Repository, tenant string) contract.ImportBatch {
	t.Helper()
	ctx := context.Background()
	batchID, itemID, actor := ulid.Make().String(), ulid.Make().String(), ulid.Make().String()
	policy := &contract.ImportPolicy{Version: 1, ProjectMode: "NONE", FinanceMode: "NONE",
		ReportMode: "ARCHIVE_SEPARATE", ResponsibilityMode: "INACTIVE", SigningMode: "DECLARED_ONLY"}
	facts := []contract.ImportFact{}
	evidence := &contract.ImportHistoryEvidence{DeclaredSignedDate: "2020-01-01", HistoricalStatus: "archived",
		HistoricalCustomerName: "原客户", HistoricalOwnerLabel: "原销售", TargetOwnerUserID: actor,
		ExpectedFacts: &facts, ReportRequiredChannels: "API_ONLY"}
	original := testfixture.PDF("auto approve historical original")
	sha := draftE2EHash(string(original))
	date, _ := time.Parse("2006-01-02", "2020-01-01")
	item := contract.ImportItem{Key: "one", ExternalRecordID: batchID, Mode: contract.ImportArchiveOnly,
		SourceFileRef: "source", Policy: policy, HistoryEvidence: evidence,
		Metadata: contract.ImportMetadata{Title: "自动审批历史合同", ContractType: "直签", CRMCustomerID: 42,
			CustomerName: "当前权威客户", AmountMinor: 10000, Currency: "CNY", StartDate: &date,
			PaymentTerms: []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2020-01-01", Amount: "100.00"}}}}
	b := contract.ImportBatch{ID: batchID, TenantID: tenant, ActorID: actor, Version: 1, Revision: 1,
		State: "QUEUED", SourceSystem: "legacy", ExpiresAt: time.Now().Add(time.Hour),
		Sources: []contract.ImportSource{{ID: "source", ItemKey: "one", SHA256: sha,
			Filename: "history.pdf", MediaType: "application/pdf", State: "READY"}},
		Items: []contract.ImportItemState{{ID: itemID, Item: item, SourceRevision: 1, State: "READY",
			Digest: "baseline"}}}
	if e := r.CreateImportBatch(ctx, b); e != nil {
		t.Fatal(e)
	}
	claimed, found, e := r.ClaimImportBatch(ctx, time.Now().UTC())
	if e != nil || !found {
		t.Fatal("claim", e)
	}
	// 认领是全局的（SKIP LOCKED + 最早 updated_at）；隔离库若残留别的批次，
	// 必须在这里明确失败，而不是让后续事务以围栏不匹配的形式报错。
	if claimed.ID != batchID || claimed.TenantID != tenant {
		t.Fatalf("claim 返回了无关批次 %s/%s，隔离测试库存在残留批次", claimed.TenantID, claimed.ID)
	}
	c := contract.Contract{ID: ulid.Make().String(), TenantID: tenant, Title: item.Metadata.Title,
		Type: "直签", OwnerUserID: actor, OwnerIdentityID: actor, AmountMinor: 10000, Currency: "CNY",
		CRMCustomerID: 42, CustomerName: "当前权威客户", Document: original, DocumentFormat: "PDF",
		SourceFilename: "history.pdf", SourceMediaType: "application/pdf", SourceSHA256: sha, ContentHash: sha,
		ExternalPaperNumber: "纸面2020", SourceFileStatus: "READY", OriginKind: "IMPORTED",
		ImportMode: contract.ImportArchiveOnly, ImportPolicy: policy, OriginImportItemID: itemID,
		HistoricalStatus: "archived", Status: contract.StatusDraft, StartDate: &date,
		PaymentTerms: item.Metadata.PaymentTerms,
		ServiceItems: []contract.ServiceItem{{ServiceType: "软件测试", Site: "测试站", Batch: "一批",
			Category: "软件测试", TestMode: "STANDARD"}}}
	if _, e = r.CommitImportDraft(ctx, tenant, claimed.ID, claimed.Revision, claimed.Epoch, itemID, c, actor); e != nil {
		t.Fatal(e)
	}
	// 开立编排此刻已完成建单（item 状态 CREATED），下一步就是自动审批。
	b, e = r.GetImportBatch(ctx, tenant, batchID)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		for _, table := range []string{"con_import_identity", "con_import_batch", "con_audit_outbox"} {
			if e := r.db.Exec("DELETE FROM "+table+" WHERE tenant_id=?", tenant).Error; e != nil {
				t.Error(e)
			}
		}
	})
	return b
}

// TestAutoConfirmHistoricalImportItemArchivesDeclaredState 验证自动审批的完整效果：
// 合同按清单声明归档（ARCHIVE_ONLY → archived、version=2、分配系统号、写入确认时间），
// 单项标记已确认并生成 PROJECT/FINANCE/REPORT 目标，批次合成 APPROVED 确认台账，
// 同事务写出 IMPORT_AUTO_APPROVED 审计；重复调用幂等。
// 参数：t 为测试上下文；仅 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：终态、审计或幂等性不符时失败。
func TestAutoConfirmHistoricalImportItemArchivesDeclaredState(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	b := autoApproveHistoryFixture(t, r, tenant)
	itemID, contractID := b.Items[0].ID, b.Items[0].ContractID
	roles := []string{"admin", "finance_director"}

	if err := r.AutoConfirmHistoricalImportItem(ctx, tenant, b.ID, itemID, roles); err != nil {
		t.Fatal(err)
	}

	// 合同：声明状态归档、version=2、系统号已分配、确认时间已写。
	c, err := r.GetContract(ctx, tenant, contractID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != "archived" || c.Version != 2 || strings.TrimSpace(c.Number) == "" {
		t.Fatalf("contract status=%s version=%d number=%q", c.Status, c.Version, c.Number)
	}
	var record contractRecord
	if err = r.db.Where("tenant_id=? AND id=?", tenant, contractID).Take(&record).Error; err != nil {
		t.Fatal(err)
	}
	if record.ImportConfirmedAt == nil {
		t.Fatal("import_confirmed_at 未写入")
	}

	// 批次：单项已确认并生成三个目标；合成确认台账为 APPROVED 且可核对。
	after, err := r.GetImportBatch(ctx, tenant, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Items[0].Confirmed || len(after.Items[0].Targets) != 3 {
		t.Fatalf("item confirmed=%t targets=%+v", after.Items[0].Confirmed, after.Items[0].Targets)
	}
	if after.Confirmation == nil || after.Confirmation.State != "APPROVED" ||
		after.Confirmation.WorkflowID != "contract-import-auto-"+b.ID ||
		after.Confirmation.Digest == "" || after.Confirmation.Roles == nil ||
		len(after.Confirmation.ApprovedRoles) != len(roles) {
		t.Fatalf("confirmation=%+v", after.Confirmation)
	}
	// 状态收敛交给调度器：这里不得自行把批次改成 TARGETS_PENDING/COMPLETED。
	if after.State != "RUNNING" {
		t.Fatalf("batch state=%s, want RUNNING", after.State)
	}

	// 审计：自动审批与既有历史确认各一条，且不重复。
	autoAudits, err := countAuditOutbox(r, tenant, "CONTRACT_MANAGEMENT:IMPORT_AUTO_APPROVED", contractID)
	if err != nil {
		t.Fatal(err)
	}
	confirmedAudits, err := countAuditOutbox(r, tenant, "CONTRACT_MANAGEMENT:IMPORT_CONFIRMED", contractID)
	if err != nil {
		t.Fatal(err)
	}
	if autoAudits != 1 || confirmedAudits != 1 {
		t.Fatalf("audits auto=%d confirmed=%d", autoAudits, confirmedAudits)
	}

	// 幂等：重复调用不重复取号、不重复写审计、不改动合同版本。
	if err = r.AutoConfirmHistoricalImportItem(ctx, tenant, b.ID, itemID, roles); err != nil {
		t.Fatal(err)
	}
	repeated, err := r.GetImportBatch(ctx, tenant, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Version != after.Version {
		t.Fatalf("重复调用改写了批次版本 %d → %d", after.Version, repeated.Version)
	}
	reconfirmed, err := r.GetContract(ctx, tenant, contractID)
	if err != nil {
		t.Fatal(err)
	}
	if reconfirmed.Version != 2 || reconfirmed.Number != c.Number {
		t.Fatalf("重复调用改写了合同 version=%d number=%q", reconfirmed.Version, reconfirmed.Number)
	}
	if autoAudits, err = countAuditOutbox(r, tenant, "CONTRACT_MANAGEMENT:IMPORT_AUTO_APPROVED", contractID); err != nil {
		t.Fatal(err)
	}
	if autoAudits != 1 {
		t.Fatalf("重复调用写入了 %d 条自动审批审计", autoAudits)
	}
}

// TestAutoConfirmHistoricalImportItemLeavesManualConfirmationInProgress 验证不劫持人工流程：
// 批次已有未获批的确认台账（PENDING）时返回围栏冲突且不写任何终态。
// 参数：t 为测试上下文；仅 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：冲突未被拒绝或合同被提前归档时失败。
func TestAutoConfirmHistoricalImportItemLeavesManualConfirmationInProgress(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	b := autoApproveHistoryFixture(t, r, tenant)
	itemID, contractID := b.Items[0].ID, b.Items[0].ContractID
	if err := r.UpdateImportBatch(ctx, tenant, b.ID, func(current *contract.ImportBatch) error {
		current.Confirmation = &contract.ImportConfirmation{WorkflowID: "contract-import-confirmation-" + b.ID,
			Revision: current.Revision, Digest: strings.Repeat("b", 64), State: "PENDING",
			Roles: []string{"admin"}, Assignees: map[string][]string{"admin": {"reviewer"}},
			ApprovedRoles: []string{}, Commands: []contract.ImportConfirmationCommand{},
			SubmittedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	err := r.AutoConfirmHistoricalImportItem(ctx, tenant, b.ID, itemID, []string{"admin"})
	if !errors.Is(err, application.ErrImportConflict) {
		t.Fatalf("error=%v, want ErrImportConflict", err)
	}
	c, getErr := r.GetContract(ctx, tenant, contractID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if c.Status != contract.StatusDraft || c.Version != 1 {
		t.Fatalf("人工确认在途时被自动归档: status=%s version=%d", c.Status, c.Version)
	}
}

// countAuditOutbox 统计某合同上指定动作的审计行数。
// 参数：r 为隔离库仓储，tenant 为测试租户，action/resourceID 为审计定位条件。
// 返回值：行数或查询错误。
func countAuditOutbox(r *Repository, tenant, action, resourceID string) (int64, error) {
	var count int64
	err := r.db.Model(&auditOutboxRecord{}).Where("tenant_id=? AND action=? AND resource_id=?", tenant, action, resourceID).Count(&count).Error
	return count, err
}
