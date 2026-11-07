package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/testfixture"
	"github.com/j-s-te/contract-management/internal/workflows"
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

	if err := r.AutoConfirmHistoricalImportItem(ctx, tenant, b.ID, itemID, roles, contractAutoPolicy(1)); err != nil {
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
	// 自动审批的确认主体必须是 SYSTEM：人工会签路径写批次创建人，两者不能混。
	if record.UpdatedBy != "SYSTEM" {
		t.Fatalf("updated_by=%q, want SYSTEM", record.UpdatedBy)
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
		len(after.Confirmation.ApprovedRoles) != len(roles) ||
		after.Confirmation.PolicyDecision != contract.ImportPolicyDecisionAuto ||
		after.Confirmation.PolicyVersion != 1 {
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
	if err = r.AutoConfirmHistoricalImportItem(ctx, tenant, b.ID, itemID, roles, contractAutoPolicy(1)); err != nil {
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
			SubmittedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour),
			PolicyDecision: contract.ImportPolicyDecisionManual, PolicyVersion: 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	err := r.AutoConfirmHistoricalImportItem(ctx, tenant, b.ID, itemID, []string{"admin"}, application.ImportAutoApprovePolicy{})
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

// contractAutoPolicy 构造租户开关版本对应的自动审批策略快照。
// 参数：version 为设置版本。
// 返回值：Auto=true 的策略。
func contractAutoPolicy(version uint64) application.ImportAutoApprovePolicy {
	return application.ImportAutoApprovePolicy{Auto: true, Version: version}
}

// pinAutoApproveConfirmation 在已认领批次上写入一份指定形状的确认台账。
// 参数：t 为测试上下文，r 为隔离库仓储，b 为已认领批次，cf 为确认台账。
// 返回值：重新读取的批次。
func pinAutoApproveConfirmation(t *testing.T, r *Repository, b contract.ImportBatch, cf contract.ImportConfirmation) contract.ImportBatch {
	t.Helper()
	ctx := context.Background()
	if err := r.UpdateImportBatch(ctx, b.TenantID, b.ID, func(current *contract.ImportBatch) error {
		confirmation := cf
		current.Confirmation = &confirmation
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pinned, err := r.GetImportBatch(ctx, b.TenantID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	return pinned
}

// autoConfirmPayload 读取某合同上指定动作的首条审计载荷并解析为键值。
// 用解析而不是字符串包含断言，避免受 json.Marshal 的键序与空格影响。
// 参数：r 为隔离库仓储，tenant/action/resourceID 为定位条件。
// 返回值：载荷键值或查询/解析错误。
func autoConfirmPayload(t *testing.T, r *Repository, tenant, action, resourceID string) map[string]any {
	t.Helper()
	var payloads []string
	if err := r.db.Model(&auditOutboxRecord{}).Where("tenant_id=? AND action=? AND resource_id=?", tenant, action, resourceID).
		Order("id").Pluck("payload_json", &payloads).Error; err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 1 {
		t.Fatalf("%s 审计条数=%d, want 1", action, len(payloads))
	}
	decoded := map[string]any{}
	if err := json.Unmarshal([]byte(payloads[0]), &decoded); err != nil {
		t.Fatalf("decode %s payload: %v", action, err)
	}
	return decoded
}

// TestAutoConfirmHistoricalImportItemRequiresApprovedPinnedConfirmation 是 P0 回归：
// 生产提交链路冻结的确认不再是「无确认台账」，因此自动审批必须认可「APPROVED + AUTO」
// 的已固化确认并完成归档，同时把「MANUAL + STARTING」判为冲突、绝不劫持人工会签。
// 参数：t 为测试上下文；仅 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：合同版本/终态/声明状态/系统号/确认时间/单项确认/目标数量或冲突语义不符时失败。
func TestAutoConfirmHistoricalImportItemRequiresApprovedPinnedConfirmation(t *testing.T) {
	ctx := context.Background()
	roles := []string{"admin", "finance_director"}

	t.Run("auto-approved-archives-declared-state", func(t *testing.T) {
		r, tenant := draftE2ERepository(t)
		b := autoApproveHistoryFixture(t, r, tenant)
		itemID, contractID := b.Items[0].ID, b.Items[0].ContractID
		b = pinAutoApproveConfirmation(t, r, b, contract.ImportConfirmation{
			WorkflowID: "contract-import-auto-" + b.ID, Revision: b.Revision, Digest: strings.Repeat("a", 64),
			State: "APPROVED", Roles: roles, ApprovedRoles: append([]string(nil), roles...),
			Assignees: map[string][]string{}, Commands: []contract.ImportConfirmationCommand{},
			SubmittedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(72 * time.Hour),
			PolicyDecision: contract.ImportPolicyDecisionAuto, PolicyVersion: 1})

		if err := r.AutoConfirmHistoricalImportItem(ctx, tenant, b.ID, itemID, roles, contractAutoPolicy(1)); err != nil {
			t.Fatalf("auto confirm error: %v", err)
		}

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
		if record.UpdatedBy != "SYSTEM" {
			t.Fatalf("updated_by=%q, want SYSTEM", record.UpdatedBy)
		}

		after, err := r.GetImportBatch(ctx, tenant, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !after.Items[0].Confirmed || len(after.Items[0].Targets) != 3 {
			t.Fatalf("item confirmed=%t targets=%+v", after.Items[0].Confirmed, after.Items[0].Targets)
		}
		// 已固化台账不得被自动确认改写策略字段或状态。
		if after.Confirmation == nil || after.Confirmation.State != "APPROVED" ||
			after.Confirmation.PolicyDecision != contract.ImportPolicyDecisionAuto || after.Confirmation.PolicyVersion != 1 {
			t.Fatalf("confirmation=%+v", after.Confirmation)
		}

		// 审计：自动审批带策略来源，确认审计带 policy=auto 且不是升级。
		autoPayload := autoConfirmPayload(t, r, tenant, "CONTRACT_MANAGEMENT:IMPORT_AUTO_APPROVED", contractID)
		if autoPayload["policy"] != "auto" || autoPayload["policy_source"] != "tenant_setting" ||
			autoPayload["setting_version"] != float64(1) {
			t.Fatalf("IMPORT_AUTO_APPROVED 载荷缺少策略来源: %+v", autoPayload)
		}
		confirmedPayload := autoConfirmPayload(t, r, tenant, "CONTRACT_MANAGEMENT:IMPORT_CONFIRMED", contractID)
		if confirmedPayload["policy"] != "auto" {
			t.Fatalf("IMPORT_CONFIRMED 载荷缺少 policy=auto: %+v", confirmedPayload)
		}
		if _, upgraded := confirmedPayload["policy_upgraded"]; upgraded {
			t.Fatalf("已固化确认不应标记升级: %+v", confirmedPayload)
		}
		var actors []string
		if err = r.db.Model(&auditOutboxRecord{}).
			Where("tenant_id=? AND action=? AND resource_id=?", tenant, "CONTRACT_MANAGEMENT:IMPORT_CONFIRMED", contractID).
			Pluck("actor_id", &actors).Error; err != nil {
			t.Fatal(err)
		}
		if len(actors) != 1 || actors[0] != "SYSTEM" {
			t.Fatalf("IMPORT_CONFIRMED actor=%v, want SYSTEM", actors)
		}
	})

	t.Run("manual-starting-conflicts-and-leaves-draft", func(t *testing.T) {
		r, tenant := draftE2ERepository(t)
		b := autoApproveHistoryFixture(t, r, tenant)
		itemID, contractID := b.Items[0].ID, b.Items[0].ContractID
		b = pinAutoApproveConfirmation(t, r, b, contract.ImportConfirmation{
			WorkflowID: "contract-import-confirmation-" + b.ID + "-1", Revision: b.Revision,
			Digest: strings.Repeat("b", 64), State: "STARTING", Roles: roles,
			Assignees: map[string][]string{"admin": {"reviewer"}}, ApprovedRoles: []string{},
			Commands: []contract.ImportConfirmationCommand{}, SubmittedAt: time.Now().UTC(),
			ExpiresAt:      time.Now().Add(72 * time.Hour),
			PolicyDecision: contract.ImportPolicyDecisionManual, PolicyVersion: 1})

		err := r.AutoConfirmHistoricalImportItem(ctx, tenant, b.ID, itemID, roles, application.ImportAutoApprovePolicy{Version: 1})
		if !errors.Is(err, application.ErrImportConflict) {
			t.Fatalf("error=%v, want ErrImportConflict", err)
		}
		c, getErr := r.GetContract(ctx, tenant, contractID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if c.Status != contract.StatusDraft || c.Version != 1 {
			t.Fatalf("人工会签在途时被自动归档: status=%s version=%d", c.Status, c.Version)
		}
		after, getErr := r.GetImportBatch(ctx, tenant, b.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if after.Items[0].Confirmed || after.Confirmation.State != "STARTING" {
			t.Fatalf("冲突后仍改写了台账: item=%+v confirmation=%+v", after.Items[0], after.Confirmation)
		}
	})
}

// TestAutoConfirmHistoricalImportItemUpgradesLegacyConfirmation 验证旧数据升级的严格护栏：
// 只有「未固化策略 + STARTING/EXPIRED + 无命令 + 无已批角色 + 未过期」的就地升级路径，
// 任何已经留下人工动作的确认都必须冲突并保持导入草稿。
// 参数：t 为测试上下文；仅 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：升级结果、策略字段、审计标记或冲突语义不符时失败。
func TestAutoConfirmHistoricalImportItemUpgradesLegacyConfirmation(t *testing.T) {
	ctx := context.Background()
	roles := []string{"admin"}

	t.Run("no-commands-upgrades-in-place", func(t *testing.T) {
		r, tenant := draftE2ERepository(t)
		b := autoApproveHistoryFixture(t, r, tenant)
		itemID, contractID := b.Items[0].ID, b.Items[0].ContractID
		b = pinAutoApproveConfirmation(t, r, b, contract.ImportConfirmation{
			WorkflowID: "contract-import-confirmation-" + b.ID + "-1", Revision: b.Revision,
			Digest: strings.Repeat("c", 64), State: "STARTING", Roles: roles,
			Assignees: map[string][]string{"admin": {"reviewer"}}, ApprovedRoles: []string{},
			Commands: []contract.ImportConfirmationCommand{}, SubmittedAt: time.Now().UTC(),
			ExpiresAt: time.Now().Add(72 * time.Hour)})

		policy := application.ImportAutoApprovePolicy{Auto: true, Version: 3, AllowLegacyUpgrade: true}
		if err := r.AutoConfirmHistoricalImportItem(ctx, tenant, b.ID, itemID, roles, policy); err != nil {
			t.Fatalf("legacy upgrade error: %v", err)
		}
		after, err := r.GetImportBatch(ctx, tenant, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Confirmation.State != "APPROVED" ||
			after.Confirmation.PolicyDecision != contract.ImportPolicyDecisionAuto ||
			after.Confirmation.PolicyVersion != 3 ||
			len(after.Confirmation.ApprovedRoles) != len(roles) {
			t.Fatalf("confirmation=%+v", after.Confirmation)
		}
		c, err := r.GetContract(ctx, tenant, contractID)
		if err != nil {
			t.Fatal(err)
		}
		if c.Version != 2 || c.Status != "archived" {
			t.Fatalf("contract version=%d status=%s", c.Version, c.Status)
		}
		payload := autoConfirmPayload(t, r, tenant, "CONTRACT_MANAGEMENT:IMPORT_CONFIRMED", contractID)
		if payload["policy_upgraded"] != true || payload["policy_version"] != float64(3) || payload["policy"] != "auto" {
			t.Fatalf("升级确认审计缺少标记: %+v", payload)
		}
	})

	t.Run("approve-command-blocks-upgrade", func(t *testing.T) {
		r, tenant := draftE2ERepository(t)
		b := autoApproveHistoryFixture(t, r, tenant)
		itemID, contractID := b.Items[0].ID, b.Items[0].ContractID
		b = pinAutoApproveConfirmation(t, r, b, contract.ImportConfirmation{
			WorkflowID: "contract-import-confirmation-" + b.ID + "-1", Revision: b.Revision,
			Digest: strings.Repeat("d", 64), State: "STARTING", Roles: roles,
			Assignees: map[string][]string{"admin": {"reviewer"}}, ApprovedRoles: []string{},
			Commands: []contract.ImportConfirmationCommand{{Key: "approve-key-1", ActorID: "reviewer",
				Action: "APPROVE", Roles: roles, At: time.Now().UTC()}},
			SubmittedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(72 * time.Hour)})

		policy := application.ImportAutoApprovePolicy{Auto: true, Version: 3, AllowLegacyUpgrade: true}
		err := r.AutoConfirmHistoricalImportItem(ctx, tenant, b.ID, itemID, roles, policy)
		if !errors.Is(err, application.ErrImportConflict) {
			t.Fatalf("error=%v, want ErrImportConflict", err)
		}
		c, getErr := r.GetContract(ctx, tenant, contractID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if c.Version != 1 || c.Status != contract.StatusDraft {
			t.Fatalf("有命令的旧确认被升级: version=%d status=%s", c.Version, c.Status)
		}
	})
}

// TestAdvanceImportConfirmationNoOpOnApproved 验证 APPROVED 终态早退：
// 自动审批已冻结并完成的确认不允许工作流活动再改写台账或重复确认合同。
// 参数：t 为测试上下文；仅 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：状态、批次版本或合同版本被改写时失败。
func TestAdvanceImportConfirmationNoOpOnApproved(t *testing.T) {
	ctx := context.Background()
	r, tenant := draftE2ERepository(t)
	b := autoApproveHistoryFixture(t, r, tenant)
	contractID := b.Items[0].ContractID
	b = pinAutoApproveConfirmation(t, r, b, contract.ImportConfirmation{
		WorkflowID: "contract-import-auto-" + b.ID, Revision: b.Revision, Digest: strings.Repeat("e", 64),
		State: "APPROVED", Roles: []string{"admin"}, ApprovedRoles: []string{"admin"},
		Assignees: map[string][]string{}, Commands: []contract.ImportConfirmationCommand{},
		SubmittedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(72 * time.Hour),
		PolicyDecision: contract.ImportPolicyDecisionAuto, PolicyVersion: 1})

	in := workflows.ImportConfirmationInput{TenantID: tenant, BatchID: b.ID, Revision: b.Revision, Digest: b.Confirmation.Digest}
	state, err := r.AdvanceImportConfirmation(ctx, in)
	if err != nil || state != "APPROVED" {
		t.Fatalf("advance state=%s err=%v", state, err)
	}
	after, err := r.GetImportBatch(ctx, tenant, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != b.Version || after.State != b.State || after.Items[0].Confirmed {
		t.Fatalf("APPROVED 终态被改写: version=%d→%d state=%s confirmed=%t",
			b.Version, after.Version, after.State, after.Items[0].Confirmed)
	}
	c, err := r.GetContract(ctx, tenant, contractID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != 1 || c.Status != contract.StatusDraft {
		t.Fatalf("APPROVED 早退后仍确认了合同: version=%d status=%s", c.Version, c.Status)
	}
}
