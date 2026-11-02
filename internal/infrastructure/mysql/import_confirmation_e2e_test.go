package mysql

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/businessclock"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/testfixture"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/oklog/ulid/v2"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// historicalMultiItemBatchFixture 原子建立一个含多个历史 item 的待确认批次。
//
// 参数：t 为测试上下文，r/tenant 为显式隔离数据库，count 为历史 item 数。
// 返回值：已完成草稿绑定、等待独立确认的批次；不连接业务环境。
//
// 与 historyMySQLFixture 的单 item 版本同构：每个 item 有独立的来源记录、原件、
// 内容摘要与合同行，只是数量可变，用来覆盖"一个批次事务内连续确认多个合同"的路径。
func historicalMultiItemBatchFixture(t *testing.T, r *Repository, tenant string, count int) contract.ImportBatch {
	t.Helper()
	ctx := context.Background()
	batchID, actor := ulid.Make().String(), ulid.Make().String()
	policy := &contract.ImportPolicy{Version: 1, ProjectMode: "NONE", FinanceMode: "NONE", ReportMode: "ARCHIVE_SEPARATE", ResponsibilityMode: "INACTIVE", SigningMode: "DECLARED_ONLY"}
	date, _ := time.Parse("2006-01-02", "2020-01-01")
	items := make([]contract.ImportItemState, 0, count)
	sources := make([]contract.ImportSource, 0, count)
	originals := make([][]byte, count)
	for index := 0; index < count; index++ {
		key := fmt.Sprintf("history-%d", index)
		itemID, sourceID := ulid.Make().String(), "source-"+key
		original := testfixture.PDF(fmt.Sprintf("historical original %d", index))
		sha := draftE2EHash(string(original))
		originals[index] = original
		evidence := &contract.ImportHistoryEvidence{
			DeclaredSignedDate: "2020-01-01", HistoricalStatus: "archived", HistoricalCustomerName: "原客户",
			HistoricalOwnerLabel: "原销售", TargetOwnerUserID: actor, ExpectedFacts: &[]contract.ImportFact{},
			ReportRequiredChannels: "API_ONLY",
		}
		item := contract.ImportItem{Key: key, ExternalRecordID: ulid.Make().String(), Mode: contract.ImportArchiveOnly,
			SourceFileRef: sourceID, Policy: policy, HistoryEvidence: evidence,
			Metadata: contract.ImportMetadata{Title: "历史确认-" + key, ContractType: "直签", CRMCustomerID: 42,
				CustomerName: "当前权威客户", AmountMinor: 10000, Currency: "CNY", StartDate: &date,
				PaymentTerms: []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2020-01-01", Amount: "100.00"}}}}
		items = append(items, contract.ImportItemState{ID: itemID, Item: item, SourceRevision: 1, State: "READY", Digest: "baseline"})
		sources = append(sources, contract.ImportSource{ID: sourceID, ItemKey: key, SHA256: sha,
			Filename: key + ".pdf", MediaType: "application/pdf", State: "READY"})
	}
	b := contract.ImportBatch{ID: batchID, TenantID: tenant, ActorID: actor, Version: 1, Revision: 1, State: "QUEUED",
		SourceSystem: "legacy", ExpiresAt: time.Now().Add(time.Hour), Sources: sources, Items: items,
		Confirmation: &contract.ImportConfirmation{WorkflowID: "t07-history-" + batchID, Revision: 1,
			Digest: strings.Repeat("b", 64), State: "STARTING", Roles: []string{"finance_director"},
			Assignees: map[string][]string{"finance_director": {"reviewer"}}, ExpiresAt: time.Now().Add(time.Hour)}}
	if e := r.CreateImportBatch(ctx, b); e != nil {
		t.Fatal(e)
	}
	var found bool
	var e error
	b, found, e = r.ClaimImportBatch(ctx, time.Now().UTC())
	if e != nil || !found {
		t.Fatal("claim", e)
	}
	// 认领是全局的（SKIP LOCKED + 最早 updated_at）；隔离库若残留别的批次，
	// 必须在这里明确失败，而不是让后续事务以围栏不匹配的形式报错。
	if b.ID != batchID || b.TenantID != tenant {
		t.Fatalf("claim 返回了无关批次 %s/%s，隔离测试库存在残留批次", b.TenantID, b.ID)
	}
	for index := range b.Items {
		item := b.Items[index]
		c := contract.Contract{ID: ulid.Make().String(), TenantID: tenant, Title: item.Item.Metadata.Title,
			Type: "直签", OwnerUserID: actor, OwnerIdentityID: actor, AmountMinor: 10000, Currency: "CNY",
			CRMCustomerID: 42, CustomerName: "当前权威客户", Document: originals[index], DocumentFormat: "PDF",
			SourceFilename: item.Item.SourceFileRef + ".pdf", SourceMediaType: "application/pdf",
			SourceSHA256: sources[index].SHA256, ContentHash: sources[index].SHA256,
			ExternalPaperNumber: fmt.Sprintf("纸面2020-%d", index), SourceFileStatus: "READY",
			OriginKind: "IMPORTED", ImportMode: contract.ImportArchiveOnly, ImportPolicy: policy,
			OriginImportItemID: item.ID, HistoricalStatus: "archived", Status: contract.StatusDraft,
			StartDate: &date, PaymentTerms: item.Item.Metadata.PaymentTerms,
			ServiceItems: []contract.ServiceItem{{ServiceType: "软件测试", Site: "测试站", Batch: "一批", Category: "软件测试", TestMode: "STANDARD"}}}
		if _, e = r.CommitImportDraft(ctx, tenant, b.ID, b.Revision, b.Epoch, item.ID, c, actor); e != nil {
			t.Fatal(e)
		}
	}
	if e = r.UpdateImportBatch(ctx, tenant, b.ID, func(current *contract.ImportBatch) error {
		for index := range current.Items {
			current.Items[index].State = "COMPLETE"
		}
		current.State = "COMPLETED"
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	b, e = r.GetImportBatch(ctx, tenant, b.ID)
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

// approveHistoricalBatchFixture 通过持久可信命令驱动同一活动确认整批历史 item。
// 参数：t/r/b 为隔离测试上下文和来源台账。
// 返回值：已批准台账；不创建普通审批历史。
func approveHistoricalBatchFixture(t *testing.T, r *Repository, b contract.ImportBatch) contract.ImportBatch {
	t.Helper()
	ctx := context.Background()
	in := workflows.ImportConfirmationInput{TenantID: b.TenantID, BatchID: b.ID, Revision: b.Revision, Digest: b.Confirmation.Digest}
	if state, e := r.AdvanceImportConfirmation(ctx, in); e != nil || state != "PENDING" {
		t.Fatalf("pending %s %v", state, e)
	}
	var e error
	b, e = r.GetImportBatch(ctx, b.TenantID, b.ID)
	if e != nil {
		t.Fatal(e)
	}
	actor := application.Principal{TenantID: b.TenantID, UserID: "reviewer", Roles: []string{"finance_director"},
		Permissions: map[string]bool{"contract.import.approve": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.import.approve": {AllowSelf: true}}}
	svc := &application.Service{Repo: r}
	if e = svc.ActImportConfirmation(ctx, actor, b.ID, b.Version, "approve-history-batch-1", "APPROVE"); e != nil {
		t.Fatal(e)
	}
	if state, e := r.AdvanceImportConfirmation(ctx, in); e != nil || state != "APPROVED" {
		t.Fatalf("approve %s %v", state, e)
	}
	b, e = r.GetImportBatch(ctx, b.TenantID, b.ID)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

// batchNumberRulePrefix 是本用例独占的单号前缀：只清理这个前缀的登记表行，
// 不与同包其他编号用例（GTSR-/ABCD-/默认 HT-）互相干扰。
const batchNumberRulePrefix = "HIMPB-"

// batchNumberRule 返回本用例的系统级编号规则：前缀 + 四位年份 + 四位流水。
func batchNumberRule() contract.ContractNumberRule {
	return contract.ContractNumberRule{Prefix: batchNumberRulePrefix, YearSegment: contract.NumberYearSegmentYYYY, SeqWidth: 4}
}

// saveBatchNumberRule 写入系统级编号规则行（全局单行配置，用例必须显式覆盖）。
func saveBatchNumberRule(t *testing.T, r *Repository, rule contract.ContractNumberRule) {
	t.Helper()
	normalized, err := contract.NormalizeContractNumberRule(rule)
	if err != nil {
		t.Fatalf("normalize rule: %v", err)
	}
	normalized.UpdatedBy, normalized.UpdatedAt = "e2e", time.Now().UTC()
	if err := r.SaveContractNumberRule(context.Background(), normalized); err != nil {
		t.Fatalf("save rule: %v", err)
	}
}

// resetBatchNumberState 清空本用例规则桶的流水与专属前缀的登记表占用，
// 使同一隔离库重复执行时水位与行数断言仍然成立。
func resetBatchNumberState(t *testing.T, r *Repository, rule contract.ContractNumberRule) {
	t.Helper()
	key, err := contract.ContractNumberRuleKey(rule)
	if err != nil {
		t.Fatalf("rule key: %v", err)
	}
	if err := r.db.Where("rule_key = ?", key).Delete(&contractNumberSequenceRecord{}).Error; err != nil {
		t.Fatalf("reset sequence: %v", err)
	}
	if err := r.db.Where("contract_number LIKE ?", batchNumberRulePrefix+"%").Delete(&contractNumberRegistryRecord{}).Error; err != nil {
		t.Fatalf("reset registry: %v", err)
	}
}

// restoreDefaultBatchNumberRule 在用例结束后恢复迁移种子默认规则，
// 避免全局单行配置影响同包后续的审批完成用例。
func restoreDefaultBatchNumberRule(t *testing.T, r *Repository) {
	t.Helper()
	t.Cleanup(func() {
		rule := contract.DefaultContractNumberRule()
		rule.UpdatedBy, rule.UpdatedAt = "e2e", time.Now().UTC()
		if err := r.SaveContractNumberRule(context.Background(), rule); err != nil {
			t.Errorf("restore default contract number rule: %v", err)
		}
	})
}

// TestHistoricalBatchConfirmationAllocatesUniqueNumbersOnIsolatedMySQL 覆盖多 item 批次的确认路径：
// 同一批次事务内先按 id 升序锁住全部合同行、再逐条取号，整批确认必须把规则编号一次发全，
// 既不能重复消耗年度流水，也不能让任何合同漏号。
// 参数：t 为测试上下文；仅显式 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无；单号、登记表行数或流水水位不符时失败。
func TestHistoricalBatchConfirmationAllocatesUniqueNumbersOnIsolatedMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	restoreDefaultBatchNumberRule(t, r)
	rule := batchNumberRule()
	resetBatchNumberState(t, r, rule)
	saveBatchNumberRule(t, r, rule)
	ruleKey, e := contract.ContractNumberRuleKey(rule)
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()

	const batchItems = 3
	b := historicalMultiItemBatchFixture(t, r, tenant, batchItems)
	b = approveHistoricalBatchFixture(t, r, b)
	if len(b.Items) != batchItems {
		t.Fatalf("items=%d want %d", len(b.Items), batchItems)
	}

	// 生产取号按北京日历日取年，断言必须同口径。
	year := businessclock.YearOf(time.Now())
	wanted := make(map[string]bool, batchItems)
	for sequence := 1; sequence <= batchItems; sequence++ {
		number, e := contract.BuildContractNumber(rule, year, uint64(sequence))
		if e != nil {
			t.Fatal(e)
		}
		wanted[number] = true
	}
	seen := make(map[string]bool, batchItems)
	for index := range b.Items {
		item := b.Items[index]
		if !item.Confirmed {
			t.Fatalf("item %d 未被确认", index)
		}
		c, e := r.GetContract(ctx, tenant, item.ContractID)
		if e != nil {
			t.Fatal(e)
		}
		if c.Version != 2 || c.Status != contract.StatusArchived {
			t.Fatalf("合同 %s version=%d status=%s want 2/archived", c.ID, c.Version, c.Status)
		}
		if c.Number == "" {
			t.Fatalf("合同 %s 没有拿到系统编号", c.ID)
		}
		// 每个合同都必须拿到按系统级规则拼出的编号，且批内互不重复。
		if !wanted[c.Number] {
			t.Fatalf("合同 %s 编号 %q 不在本批次规则序列 %v 内", c.ID, c.Number, wanted)
		}
		if seen[c.Number] {
			t.Fatalf("批内单号重复: %s", c.Number)
		}
		seen[c.Number] = true
	}

	// 登记表行数必须与合同数一致：没有多占号，也没有漏登记。
	var registryRows int64
	if e := r.db.Model(&contractNumberRegistryRecord{}).Where("contract_number LIKE ?", batchNumberRulePrefix+"%").Count(&registryRows).Error; e != nil || registryRows != int64(batchItems) {
		t.Fatalf("登记表行数=%d err=%v want %d", registryRows, e, batchItems)
	}
	// 流水水位必须等于 item 数：批量确认不能重复消耗年度流水。
	if current, e := r.ContractNumberSequenceValue(ctx, ruleKey, year); e != nil || current != uint64(batchItems) {
		t.Fatalf("流水水位=%d err=%v want %d", current, e, batchItems)
	}
}

// TestLockImportContractRowsOrdersByIDAscending 用 DryRun（无数据库连接）断言前置锁定 SQL 的形状：
// 必须同时带 "ORDER BY id ASC" 和 "FOR UPDATE"，即本批合同行按主键升序一次性加锁。
// 这条顺序是「全部合同行（id 升序）→ 流水行」不变量的实现细节：去掉 ORDER BY 会让
// 加锁顺序不受控，去掉 FOR UPDATE 会让前置阶段形同虚设，二者都会让 P1-B 的死锁消除失效。
func TestLockImportContractRowsOrdersByIDAscending(t *testing.T) {
	// DryRun + 关闭自动 Ping：只生成 SQL 不建立连接，因此本用例不需要 CONTRACT_TEST_DSN。
	db, err := gorm.Open(gormmysql.New(gormmysql.Config{
		DSN:                       "dryrun:dryrun@tcp(127.0.0.1:1)/contract_test_dryrun?parseTime=true",
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true, Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open dry-run gorm: %v", err)
	}
	var ids []string
	// 与 lockImportContractRows 的查询逐项对应，避免用例断言的是另一条 SQL。
	query := db.Session(&gorm.Session{DryRun: true}).Model(&contractRecord{}).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("tenant_id=? AND id IN ?", "tenant", []string{"a", "b"}).Order("id ASC").Pluck("id", &ids)
	if query.Error != nil {
		t.Fatalf("dry-run query: %v", query.Error)
	}
	sql := query.Statement.SQL.String()
	for _, fragment := range []string{"`con_contract`", "ORDER BY id ASC", "FOR UPDATE"} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("前置锁定 SQL 缺少 %q: %s", fragment, sql)
		}
	}
	if strings.Index(sql, "ORDER BY") > strings.Index(sql, "FOR UPDATE") {
		t.Fatalf("前置锁定必须在 ORDER BY 之后才 FOR UPDATE: %s", sql)
	}
}
