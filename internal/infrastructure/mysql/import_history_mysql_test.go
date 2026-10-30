package mysql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/testfixture"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/oklog/ulid/v2"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"gorm.io/gorm"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// historyMySQLFixture 原子建立合成历史原件草稿及稳定来源台账。
// 参数：t为测试上下文，r/tenant为显式隔离数据库，mode为历史策略。
// 返回值：待独立确认的批次与原PDF，不连接业务环境。
func historyMySQLFixture(t *testing.T, r *Repository, tenant string, mode contract.ImportMode) (contract.ImportBatch, []byte) {
	t.Helper()
	ctx := context.Background()
	id, item, actor := ulid.Make().String(), ulid.Make().String(), ulid.Make().String()
	policy := &contract.ImportPolicy{Version: 1, ProjectMode: "NONE", FinanceMode: "NONE", ReportMode: "ARCHIVE_SEPARATE", ResponsibilityMode: "INACTIVE", SigningMode: "DECLARED_ONLY"}
	status := "archived"
	if mode == contract.ImportContinueLive {
		policy.ProjectMode = "CREATE_NEW"
		policy.FinanceMode = "FULL_REPLAY"
		policy.ReportMode = "HISTORICAL_SEPARATE"
		policy.ResponsibilityMode = "ACTIVATE_ON_RELEASE"
		status = "active"
	}
	facts := []contract.ImportFact{}
	evidence := &contract.ImportHistoryEvidence{DeclaredSignedDate: "2020-01-01", HistoricalStatus: status, HistoricalCustomerName: "原客户", HistoricalOwnerLabel: "原销售", TargetOwnerUserID: actor, ExpectedFacts: &facts, ReportRequiredChannels: "API_ONLY"}
	if mode == contract.ImportContinueLive {
		evidence.Relationship = "PRIMARY"
	}
	original := testfixture.PDF("historical original unchanged")
	sha := draftE2EHash(string(original))
	date, _ := time.Parse("2006-01-02", "2020-01-01")
	m := contract.ImportItem{Key: "one", ExternalRecordID: id, Mode: mode, SourceFileRef: "source", Policy: policy, HistoryEvidence: evidence, Metadata: contract.ImportMetadata{Title: "历史确认", ContractType: "直签", CRMCustomerID: 42, CustomerName: "当前权威客户", AmountMinor: 10000, Currency: "CNY", StartDate: &date, PaymentTerms: []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2020-01-01", Amount: "100.00"}}}}
	b := contract.ImportBatch{ID: id, TenantID: tenant, ActorID: actor, Version: 1, Revision: 1, State: "QUEUED", SourceSystem: "legacy", ExpiresAt: time.Now().Add(time.Hour), Sources: []contract.ImportSource{{ID: "source", ItemKey: "one", SHA256: sha, Filename: "history.pdf", MediaType: "application/pdf", State: "READY"}}, Items: []contract.ImportItemState{{ID: item, Item: m, SourceRevision: 1, State: "READY", Digest: "baseline"}}, Confirmation: &contract.ImportConfirmation{WorkflowID: "t07-history-" + id, Revision: 1, Digest: strings.Repeat("b", 64), State: "STARTING", Roles: []string{"finance_director"}, Assignees: map[string][]string{"finance_director": {"reviewer"}}, ExpiresAt: time.Now().Add(time.Hour)}}
	if e := r.CreateImportBatch(ctx, b); e != nil {
		t.Fatal(e)
	}
	b, found, e := r.ClaimImportBatch(ctx, time.Now().UTC())
	if e != nil || !found {
		t.Fatal("claim", e)
	}
	c := contract.Contract{ID: ulid.Make().String(), TenantID: tenant, Title: m.Metadata.Title, Type: "直签", OwnerUserID: actor, OwnerIdentityID: actor, AmountMinor: 10000, Currency: "CNY", CRMCustomerID: 42, CustomerName: "当前权威客户", Document: original, DocumentFormat: "PDF", SourceFilename: "history.pdf", SourceMediaType: "application/pdf", SourceSHA256: sha, ContentHash: sha, ExternalPaperNumber: "纸面2020", SourceFileStatus: "READY", OriginKind: "IMPORTED", ImportMode: mode, ImportPolicy: policy, OriginImportItemID: item, HistoricalStatus: status, Status: contract.StatusDraft, StartDate: &date, PaymentTerms: m.Metadata.PaymentTerms, ServiceItems: []contract.ServiceItem{{ServiceType: "软件测试", Site: "测试站", Batch: "一批", Category: "软件测试", TestMode: "STANDARD"}}}
	if _, e = r.CommitImportDraft(ctx, tenant, b.ID, b.Revision, b.Epoch, item, c, actor); e != nil {
		t.Fatal(e)
	}
	if e = r.UpdateImportBatch(ctx, tenant, b.ID, func(current *contract.ImportBatch) error {
		current.Items[0].State = "COMPLETE"
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
	return b, original
}

// approveHistoryFixture 通过持久可信命令驱动相同Activity完成确认。
// 参数：t/r/b为隔离测试上下文和来源台账。
// 返回值：已批准台账；不创建普通审批历史。
func approveHistoryFixture(t *testing.T, r *Repository, b contract.ImportBatch) contract.ImportBatch {
	t.Helper()
	ctx := context.Background()
	in := workflows.ImportConfirmationInput{TenantID: b.TenantID, BatchID: b.ID, Revision: b.Revision, Digest: b.Confirmation.Digest}
	if state, e := r.AdvanceImportConfirmation(ctx, in); e != nil || state != "PENDING" {
		t.Fatalf("pending %s %v", state, e)
	}
	b, _ = r.GetImportBatch(ctx, b.TenantID, b.ID)
	before := b.Version
	if _, e := r.AdvanceImportConfirmation(ctx, in); e != nil {
		t.Fatal(e)
	}
	b, _ = r.GetImportBatch(ctx, b.TenantID, b.ID)
	if b.Version != before {
		t.Fatal("idle workflow poll changed optimistic batch version")
	}
	actor := application.Principal{TenantID: b.TenantID, UserID: "reviewer", Roles: []string{"finance_director"}, Permissions: map[string]bool{"contract.import.approve": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.import.approve": {AllowSelf: true}}}
	svc := &application.Service{Repo: r}
	if e := svc.ActImportConfirmation(ctx, actor, b.ID, b.Version, "approve-history-1", "APPROVE"); e != nil {
		t.Fatal(e)
	}
	for range 2 {
		if state, e := r.AdvanceImportConfirmation(ctx, in); e != nil || state != "APPROVED" {
			t.Fatalf("approve %s %v", state, e)
		}
	}
	b, _ = r.GetImportBatch(ctx, b.TenantID, b.ID)
	return b
}

// TestArchiveImportExcludedEveryPath 验证真实数据库内历史归档不会被普通派生或统计重新激活。
// 参数：t为专用回环测试库上下文。
// 返回值：无；任一来源门禁、幂等确认或PDF不变性丢失时失败。
func TestArchiveImportExcludedEveryPath(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	b, pdf := historyMySQLFixture(t, r, tenant, contract.ImportArchiveOnly)
	b = approveHistoryFixture(t, r, b)
	id := b.Items[0].ContractID
	c, e := r.GetContract(ctx, tenant, id)
	if e != nil || c.Status != contract.StatusArchived || c.Version != 2 || c.Number == "" || !bytes.Equal(pdf, c.Document) || c.SourceSHA256 != draftE2EHash(string(pdf)) {
		t.Fatalf("history source changed: %v", e)
	}
	if e = r.db.Transaction(func(tx *gorm.DB) error {
		if e := enqueueProjectActivation(tx, tenant, id); e != nil {
			return e
		}
		return enqueueFinancialEffective(tx, tenant, id)
	}); e != nil {
		t.Fatal(e)
	}
	if _, e = r.ReconcileProjectDeliveries(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = r.ReconcileFinancialEffectives(ctx); e != nil {
		t.Fatal(e)
	}
	for _, table := range []string{"con_project_delivery_outbox", "con_financial_effective_outbox", "con_approval_instance"} {
		var count int64
		if e = r.db.Table(table).Where("tenant_id=?", tenant).Count(&count).Error; e != nil || count != 0 {
			t.Fatalf("%s count=%d err=%v", table, count, e)
		}
	}
	for _, scope := range []contract.ScopeFilter{{TenantID: tenant, AllowAll: true}, {TenantID: tenant, IdentityID: c.OwnerIdentityID, AllowSelf: true}} {
		list, e := r.ListApprovedContractsScoped(ctx, scope, 50)
		if e != nil || len(list) != 0 {
			t.Fatalf("approved scope leaked %v", e)
		}
		refs, e := r.ListApprovedContractReferencesScoped(ctx, scope, "", 50)
		if e != nil || len(refs) != 0 {
			t.Fatalf("reference scope leaked %v", e)
		}
	}
	list, e := r.ListApprovedContracts(ctx, tenant, 50)
	if e != nil || len(list) != 0 {
		t.Fatal("approved leaked", e)
	}
	counts, e := r.CountSignedContractsByOpportunityIDs(ctx, tenant, []string{"42"})
	if e != nil || counts["42"] != 0 {
		t.Fatal("CRM count leaked", e)
	}
	dashboard, e := r.ContractDashboard(ctx, tenant, "", time.Now(), 50)
	if e != nil || dashboard.TotalContracts != 0 {
		t.Fatal("dashboard leaked", e)
	}
	var row contractRecord
	if e = r.db.Where("tenant_id=? AND id=?", tenant, id).Take(&row).Error; e != nil || responsibilityActive(row) {
		t.Fatal("archive responsibility active", e)
	}
	source, e := r.ImportReportSource(ctx, tenant)
	if e != nil || len(source.Items) != 0 {
		t.Fatal("unreleased report leaked", e)
	}
	actor := application.Principal{TenantID: tenant, UserID: b.ActorID, Permissions: map[string]bool{"contract.import.release": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.import.release": {AllowAll: true}}}
	svc := &application.Service{Repo: r}
	b, e = svc.ReleaseImportItem(ctx, actor, b.Items[0].ID, "REPORT", 1, "")
	if e != nil {
		t.Fatal(e)
	}
	if b.Items[0].Targets[2].State != "RELEASE_REQUESTED" {
		t.Fatal("source report pretended applied")
	}
	source, e = r.ImportReportSource(ctx, tenant)
	if e != nil || len(source.Items) != 1 || source.Items[0].StartDate == nil || *source.Items[0].StartDate != "2020-01-01" || source.Items[0].ReportReleaseVersion != 1 {
		t.Fatal("report release missing original date", e)
	}
	old := workflows.ImportConfirmationInput{TenantID: tenant, BatchID: b.ID, Revision: 2, Digest: b.Confirmation.Digest}
	if _, e = r.AdvanceImportConfirmation(ctx, old); !errors.Is(e, contract.ErrImportManifest) {
		t.Fatal("late revision accepted", e)
	}
}

// TestImportConfirmationRealTemporal 验证独立真实Temporal与隔离MySQL执行批准及稳定重放。
// 参数：t为测试上下文，仅接受显式本任务loopback地址和qa-external-pdf命名空间。
// 返回值：无；无配置跳过，不探测既有业务Temporal。
func TestImportConfirmationRealTemporal(t *testing.T) {
	address, namespace := os.Getenv("CONTRACT_IMPORT_TEMPORAL_ADDRESS"), os.Getenv("CONTRACT_IMPORT_TEMPORAL_NAMESPACE")
	if address == "" {
		t.Skip("dedicated Temporal is not configured")
	}
	if !strings.HasPrefix(address, "127.0.0.1:") || namespace != "qa-external-pdf" {
		t.Fatal("dedicated Temporal guard")
	}
	r, tenant := draftE2ERepository(t)
	b, pdf := historyMySQLFixture(t, r, tenant, contract.ImportArchiveOnly)
	temporalClient, e := client.Dial(client.Options{HostPort: address, Namespace: namespace})
	if e != nil {
		t.Fatal(e)
	}
	defer temporalClient.Close()
	queue := "t07-history-" + ulid.Make().String()
	w := worker.New(temporalClient, queue, worker.Options{})
	workflows.Register(w, &workflows.Activities{Store: r})
	if e = w.Start(); e != nil {
		t.Fatal(e)
	}
	defer w.Stop()
	svc := &application.Service{Repo: r, Temporal: temporalClient, TaskQueue: queue}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run, e := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: b.Confirmation.WorkflowID, TaskQueue: queue}, workflows.ImportConfirmationWorkflowName, workflows.ImportConfirmationInput{TenantID: tenant, BatchID: b.ID, Revision: 1, Digest: b.Confirmation.Digest})
	if e != nil {
		t.Fatal(e)
	}
	for {
		b, e = r.GetImportBatch(ctx, tenant, b.ID)
		if e != nil {
			t.Fatal(e)
		}
		if b.Confirmation.State == "PENDING" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("Temporal did not run confirmation activity")
		case <-time.After(100 * time.Millisecond):
		}
	}
	actor := application.Principal{TenantID: tenant, UserID: "reviewer", Roles: []string{"finance_director"}, Permissions: map[string]bool{"contract.import.approve": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.import.approve": {AllowSelf: true}}}
	if e = svc.ActImportConfirmation(ctx, actor, b.ID, b.Version, "real-temporal-approve", "APPROVE"); e != nil {
		t.Fatal(e)
	}
	var result string
	if e = run.Get(ctx, &result); e != nil || result != "APPROVED" {
		t.Fatalf("real Temporal %s %v", result, e)
	}
	c, e := r.GetContract(ctx, tenant, b.Items[0].ContractID)
	if e != nil || c.Version != 2 || !bytes.Equal(pdf, c.Document) {
		t.Fatal("real activity source changed", e)
	}
	t.Log("real isolated Temporal completed independent history workflow and actual MySQL activity")
}

// makeTargetDue 只调整本测试合成目标的退避时刻以触发下一次认领。
// 参数：t/r/b为测试上下文，target为本测试目标。
// 返回值：无；不改变业务版本或ACK。
func makeTargetDue(t *testing.T, r *Repository, b contract.ImportBatch, target string) {
	t.Helper()
	if e := r.UpdateImportBatch(context.Background(), b.TenantID, b.ID, func(current *contract.ImportBatch) error {
		for i := range current.Items {
			for j := range current.Items[i].Targets {
				task := &current.Items[i].Targets[j]
				if task.Target == target {
					task.NextAttemptAt = time.Now().Add(-time.Minute)
				}
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}

// TestContinueImportTargetOrderingMySQL 验证真实源台账的项目、HOLD、对账、责任ACK与放行先后。
// 参数：t为隔离MySQL上下文；HTTP接收方为明确协议替身，不冒充跨系统验收。
// 返回值：无；顺序、稳定命令或HOLD语义错误时失败。
func TestContinueImportTargetOrderingMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	b, _ := historyMySQLFixture(t, r, tenant, contract.ImportContinueLive)
	// 模拟撤回后只修改未建项引起的批次升代；既有单项来源代次必须保持1。
	if e := r.UpdateImportBatch(ctx, tenant, b.ID, func(current *contract.ImportBatch) error {
		current.Revision = 2
		current.Confirmation.Revision = 2
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	b, _ = r.GetImportBatch(ctx, tenant, b.ID)
	b = approveHistoryFixture(t, r, b)
	item := b.Items[0]
	owner := b.ActorID
	policyDigest, _ := contract.ImportDigest(item.Item.Policy)
	ready, ack := false, false
	var ownerVersion uint64
	releases, prepares, projects := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer isolated-test" {
			t.Error("missing machine token")
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if req.Method == http.MethodGet {
			state := "HOLD"
			if ready {
				state = "READY"
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"contract_snapshot_id": "snapshot", "policy_digest": policyDigest, "import_mode": item.Item.Mode, "source_contract_id": item.ContractID, "customer_id": "42", "currency": "CNY", "contract_amount": "100.00", "state": state, "migration_state": "HOLD", "origin_import_item_id": item.ID, "source_revision": 1, "release_version": 1, "snapshot_version": 3, "reconciliation_id": "local-finance-proof", "expected_owner_user_id": owner, "owner_user_id": owner, "owner_version": ownerVersion, "owner_active": ack, "owner_matches": ack}})
			return
		}
		body, _ := io.ReadAll(req.Body)
		var command map[string]any
		if e := json.Unmarshal(body, &command); e != nil {
			t.Error(e)
			return
		}
		if strings.Contains(req.URL.Path, "project-bindings") {
			projects++
			if command["source_revision"] != float64(1) {
				t.Error("project used batch revision instead of item revision")
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"schema_version": 1, "command_id": command["command_id"], "tenant_id": tenant, "origin_import_item_id": item.ID, "source_revision": 1, "policy_digest": command["policy_digest"], "release_version": 1, "contract_id": item.ContractID, "project_id": "actual-test-project", "project_version": 1, "status": "PROJECT_BOUND"}})
			return
		}
		if command["source_revision"] != float64(1) {
			t.Error("batch revision replaced immutable item source_revision")
		}
		phase := command["phase"]
		status, state := "FINANCE_PREPARED", "HOLD"
		if phase == "PREPARE" {
			prepares++
			facts, exists := command["expected_facts"]
			if !exists || facts == nil {
				t.Error("expected facts absent")
			}
			original := command["contract"].(map[string]any)
			if original["amount"] != "100.00" {
				t.Error("original amount changed")
			}
		} else {
			releases++
			status, state = "FINANCE_RELEASED", "RELEASED"
			if !ack || !ready {
				t.Error("release before actual ack or reconciliation")
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"event_id": command["event_id"], "contract_snapshot_id": "snapshot", "status": status, "migration_state": state}})
	}))
	defer server.Close()
	transport := &application.ImportTargetTransport{BaseURL: server.URL, TokenSource: func(context.Context) (string, error) { return "isolated-test", nil }}
	svc := &application.Service{Repo: r, ImportProject: transport, ImportFinance: transport, ImportOwners: historyOwnerDirectory{}}
	actor := application.Principal{TenantID: tenant, UserID: b.ActorID, Permissions: map[string]bool{"contract.import.release": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.import.release": {AllowAll: true}}}
	release := func(target, phase string) {
		t.Helper()
		b, _ = r.GetImportBatch(ctx, tenant, b.ID)
		var version uint64
		for _, task := range b.Items[0].Targets {
			if task.Target == target {
				version = task.Version
			}
		}
		var e error
		b, e = svc.ReleaseImportItem(ctx, actor, item.ID, target, version, phase)
		if e != nil {
			t.Fatal(e)
		}
	}
	dispatch := func() {
		t.Helper()
		if e := svc.DispatchImportTargetOnce(ctx); e != nil {
			t.Fatal(e)
		}
	}
	release("FINANCE", "PREPARE")
	dispatch()
	b, _ = r.GetImportBatch(ctx, tenant, b.ID)
	if b.Items[0].Targets[1].State != "PREPARED" || prepares != 1 || releases != 0 {
		t.Fatal("prepare not HOLD")
	}
	release("FINANCE", "RELEASE")
	dispatch()
	if releases != 0 {
		t.Fatal("release without reconciliation")
	}
	ready = true
	release("PROJECT", "")
	dispatch()
	if projects != 1 {
		t.Fatal("project target not completed")
	}
	makeTargetDue(t, r, b, "FINANCE")
	dispatch()
	var stream contractResponsibilityStream
	if e := r.db.Where("tenant_id=? AND contract_id=?", tenant, item.ContractID).Take(&stream).Error; e != nil || !stream.Active || releases != 0 {
		t.Fatal("responsibility activation/hold ordering", e)
	}
	ownerVersion, ack = stream.Version, true
	makeTargetDue(t, r, b, "FINANCE")
	dispatch()
	b, _ = r.GetImportBatch(ctx, tenant, b.ID)
	if b.Items[0].Targets[1].State != "COMPLETE" || releases != 1 {
		t.Fatalf("release missing %#v", b.Items[0].Targets[1])
	}
}

// TestImportReportRequiresActualSourceChannelMySQL 验证API_AND_SOURCE_FACTS不能降级API_ONLY成功。
// 参数：t为隔离MySQL上下文，报告HTTP为明确协议替身。
// 返回值：无；缺真实通道证明即完成时失败。
func TestImportReportRequiresActualSourceChannelMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	b, _ := historyMySQLFixture(t, r, tenant, contract.ImportArchiveOnly)
	if e := r.UpdateImportBatch(ctx, tenant, b.ID, func(current *contract.ImportBatch) error {
		current.Items[0].Item.HistoryEvidence.ReportRequiredChannels = "API_AND_SOURCE_FACTS"
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	b = approveHistoryFixture(t, r, b)
	item := b.Items[0]
	connected := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		channels := []string{"API_APPLIED"}
		if connected {
			channels = append(channels, "SOURCE_FACTS_APPLIED")
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"schema_version": 1, "tenant_id": tenant, "item_id": item.ID, "source_revision": 1, "report_release_version": 1, "required_channels": "API_AND_SOURCE_FACTS", "applied_channels": channels, "state": "APPLIED", "source_snapshot_id": strings.Repeat("a", 64), "aggregation_run_id": "actual-consumer-test-run", "applied_at": time.Now().UTC(), "source_facts_connected": connected}})
	}))
	defer server.Close()
	svc := &application.Service{Repo: r, ImportReport: &application.ImportTargetTransport{BaseURL: server.URL, TokenSource: func(context.Context) (string, error) { return "test", nil }}}
	actor := application.Principal{TenantID: tenant, UserID: b.ActorID, Permissions: map[string]bool{"contract.import.release": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.import.release": {AllowAll: true}}}
	if _, e := svc.ReleaseImportItem(ctx, actor, item.ID, "REPORT", 1, ""); e != nil {
		t.Fatal(e)
	}
	if e := svc.DispatchImportTargetOnce(ctx); e != nil {
		t.Fatal(e)
	}
	b, _ = r.GetImportBatch(ctx, tenant, b.ID)
	if b.Items[0].Targets[2].State == "COMPLETE" {
		t.Fatal("missing source facts treated applied")
	}
	connected = true
	makeTargetDue(t, r, b, "REPORT")
	if e := svc.DispatchImportTargetOnce(ctx); e != nil {
		t.Fatal(e)
	}
	b, _ = r.GetImportBatch(ctx, tenant, b.ID)
	if b.Items[0].Targets[2].State != "COMPLETE" {
		t.Fatal("complete proof not applied")
	}
	source, e := r.ImportReportSource(ctx, tenant)
	if e != nil || source.Items[0].RequiredChannels != "API_AND_SOURCE_FACTS" {
		t.Fatal("DSN requirement not persisted", e)
	}
}

type historyOwnerDirectory struct{}

// ResolveOwner 为独立协议测试返回当前有效的合成人员。
// 参数：ctx为上下文，tenant/user限定测试请求。
// 返回值：合成负责人，不调用业务人员目录。
func (historyOwnerDirectory) ResolveOwner(ctx context.Context, tenant, user string) (application.UserReference, error) {
	return application.UserReference{UserID: user, DisplayName: "当前合成负责人"}, nil
}

// TestHistoricalBaselineRejectsOrdinaryHandover 验证普通交接不能改写冻结历史负责人。
// 参数：t为显式隔离MySQL上下文。
// 返回值：无；历史责任被隐藏或历史基线被普通交接覆盖时失败。
func TestHistoricalBaselineRejectsOrdinaryHandover(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	b, _ := historyMySQLFixture(t, r, tenant, contract.ImportContinueLive)
	b = approveHistoryFixture(t, r, b)
	item := b.Items[0]
	responsibilities, e := r.ListOwnedResponsibilities(ctx, tenant, b.ActorID, "", 50)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, responsibility := range responsibilities {
		if responsibility.ResourceID == item.ContractID {
			found = true
		}
	}
	if !found {
		t.Fatal("historical responsibility hidden as zero workload")
	}
	_, e = r.TransferOwnership(ctx, application.HandoverTransferInput{TenantID: tenant, ResourceID: item.ContractID, FromUserID: b.ActorID, ToUserID: "other", ExpectedVersion: 2, IdempotencyKey: "historical-handover", BatchID: "test", ItemID: "test"}, time.Now().UTC())
	if !errors.Is(e, application.ErrImportCorrectionRequired) {
		t.Fatal("historical owner overwritten", e)
	}
}

// TestImportConfirmationWithdrawsBeforeDraftComplete 验证原件绑定未完成时仍可持久撤回并围栏旧worker。
// 参数：t为显式隔离MySQL上下文。
// 返回值：无；撤回依赖草稿先完成或未使旧执行代次失效时失败。
func TestImportConfirmationWithdrawsBeforeDraftComplete(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	b, _ := historyMySQLFixture(t, r, tenant, contract.ImportArchiveOnly)
	if e := r.UpdateImportBatch(ctx, tenant, b.ID, func(current *contract.ImportBatch) error {
		current.State = "PARTIAL"
		current.Items[0].State = "BLOCKED"
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	b, _ = r.GetImportBatch(ctx, tenant, b.ID)
	oldEpoch := b.Epoch
	actor := application.Principal{TenantID: tenant, UserID: b.ActorID, Permissions: map[string]bool{"contract.import.commit": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.import.commit": {AllowAll: true}}}
	svc := &application.Service{Repo: r}
	if e := svc.ActImportConfirmation(ctx, actor, b.ID, b.Version, "withdraw-before-complete", "WITHDRAW"); e != nil {
		t.Fatal(e)
	}
	state, e := r.AdvanceImportConfirmation(ctx, workflows.ImportConfirmationInput{TenantID: tenant, BatchID: b.ID, Revision: b.Revision, Digest: b.Confirmation.Digest})
	if e != nil || state != "WITHDRAWN" {
		t.Fatal("withdraw blocked by incomplete original", state, e)
	}
	b, _ = r.GetImportBatch(ctx, tenant, b.ID)
	if b.Epoch <= oldEpoch || b.State != "PAUSED" || b.Items[0].Confirmed {
		t.Fatal("withdraw did not fence draft worker")
	}
}
