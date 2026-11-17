package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
)

// recordE2EFixture 用途：在显式隔离库创建无项目关联的合成草稿及管理员。
// 参数：t 为测试上下文，连接仍受原 contract_test_* 回环检查保护。
// 返回值：真实仓储、服务、主体和合同，不启动远端项目、资金或审批服务。
func recordE2EFixture(t *testing.T) (*Repository, *application.Service, application.Principal, contract.Contract) {
	t.Helper()
	r, tenant := draftE2ERepository(t)
	actor := application.Principal{TenantID: tenant, UserID: ulid.Make().String(), IdentityID: ulid.Make().String(),
		Roles: []string{"admin"}, Permissions: map[string]bool{"contract.read": true, "contract.delete": true},
		PermissionScopes: map[string]contract.ScopeFilter{"contract.read": {AllowAll: true}, "contract.delete": {AllowAll: true}}}
	c := contract.Contract{ID: ulid.Make().String(), TenantID: tenant, Title: "合成删除验收合同",
		Number: "TEST-" + ulid.Make().String(), Type: "直签", ServiceType: "软件测试",
		OwnerUserID: actor.UserID, OwnerIdentityID: actor.IdentityID, OwnerOrgID: ulid.Make().String(),
		Currency: "CNY", AmountMinor: 10000, Content: "合成正文", Document: []byte("synthetic source"),
		Status: contract.StatusDraft, Version: 1}
	if err := r.CreateContract(context.Background(), c, actor.UserID); err != nil {
		t.Fatal(err)
	}
	if err := r.db.Create(&contractNumberRegistryRecord{ContractNumber: c.Number, TenantID: tenant,
		ContractID: c.ID, SeqYear: 2026, RuleKey: "record-test", IssuedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	return r, &application.Service{Repo: r}, actor, c
}

// recordE2ECount 用途：按本次租户与合同核对持久化副作用，禁止扫描或改写业务库。
// 参数：t 为测试上下文，r 为隔离仓储，model 为受控表，c 为合成合同。
// 返回值：本合同记录数；查询失败立即终止当前最小场景。
func recordE2ECount(t *testing.T, r *Repository, model any, c contract.Contract) int64 {
	t.Helper()
	var count int64
	if err := r.db.Model(model).Where("tenant_id=? AND contract_id=?", c.TenantID, c.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

// TestContractRecordDeleteMinimalMySQL 用途：验证一次草稿删除的隐藏、留痕和同目标幂等。
// 参数：t 为显式隔离 MySQL 场景，每次只创建一份合成合同。
// 返回值：无；身份、原件、业务状态、永久编号或事务证据不符时失败。
func TestContractRecordDeleteMinimalMySQL(t *testing.T) {
	r, service, actor, c := recordE2EFixture(t)
	ctx := context.Background()
	preview, err := service.PreviewContractDeletion(ctx, actor, c.ID)
	if err != nil || !preview.CanDelete {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	if _, err = service.DeleteContract(ctx, actor, c.ID, 99, "过期版本"); !errors.Is(err, apperrors.ErrVersionConflict) {
		t.Fatalf("stale version error=%v", err)
	}
	result, err := service.DeleteContract(ctx, actor, c.ID, 1, "误建合同")
	if err != nil || !result.Changed || result.Version != 2 || result.RecordState != contract.RecordDeleted {
		t.Fatalf("delete=%+v err=%v", result, err)
	}
	if _, err = r.GetContract(ctx, c.TenantID, c.ID); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("ordinary detail error=%v", err)
	}
	page, err := service.ListDeletedContractRecords(ctx, actor, contract.ContractListQuery{Limit: 20})
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].DeleteReason != "误建合同" {
		t.Fatalf("trash=%+v err=%v", page, err)
	}
	var row contractRecord
	if err = r.db.Unscoped().Where("id=?", c.ID).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != "draft" || row.Content != c.Content || string(row.RenderedDocument) != string(c.Document) || valueOrEmpty(row.ContractNumber) != c.Number {
		t.Fatal("logical delete changed original business content")
	}
	if registered, e := r.ContractNumberRegistryContains(ctx, c.Number); e != nil || !registered {
		t.Fatalf("permanent number lost: %v", e)
	}
	replay, err := service.DeleteContract(ctx, actor, c.ID, 1, "再次点击")
	if err != nil || replay.Changed || replay.Version != 2 {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	events, err := r.ListContractLifecycle(ctx, c.TenantID, c.ID)
	if err != nil || len(events) != 2 || events[1].EventType != "DELETE" {
		t.Fatalf("lifecycle=%+v err=%v", events, err)
	}
	var stream contractResponsibilityStream
	if err = r.db.Where("tenant_id=? AND contract_id=?", c.TenantID, c.ID).Take(&stream).Error; err != nil || stream.Active || stream.Version != 2 {
		t.Fatalf("responsibility=%+v err=%v", stream, err)
	}
	deliveries, err := r.ClaimContractResponsibilityDeliveries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	revocation := false
	for _, delivery := range deliveries {
		var payload struct {
			SourceContractID string `json:"source_contract_id"`
			Active           bool   `json:"active"`
		}
		if err = json.Unmarshal(delivery.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.SourceContractID == c.ID && !payload.Active {
			revocation = true
		}
	}
	if !revocation {
		t.Fatal("deleted parent swallowed inactive responsibility delivery")
	}
	if recordE2ECount(t, r, &projectDeliveryOutboxRecord{}, c) != 0 || recordE2ECount(t, r, &financialEffectiveOutboxRecord{}, c) != 0 {
		t.Fatal("delete derived business events")
	}
}

// TestContractRecordRestoreMinimalMySQL 用途：验证恢复保留草稿和原件，运行补建也不派生业务。
// 参数：t 为显式隔离 MySQL 场景，删除仅用于准备一份待恢复记录。
// 返回值：无；恢复增版、幂等或业务补建边界不符时失败。
func TestContractRecordRestoreMinimalMySQL(t *testing.T) {
	r, service, actor, c := recordE2EFixture(t)
	ctx := context.Background()
	if _, err := service.DeleteContract(ctx, actor, c.ID, 1, "准备恢复场景"); err != nil {
		t.Fatal(err)
	}
	result, err := service.RestoreContract(ctx, actor, c.ID, 2, "")
	if err != nil || !result.Changed || result.Version != 3 || result.RecordState != contract.RecordActive {
		t.Fatalf("restore=%+v err=%v", result, err)
	}
	current, err := r.GetContract(ctx, c.TenantID, c.ID)
	if err != nil || current.Status != contract.StatusDraft || current.Content != c.Content || string(current.Document) != string(c.Document) || current.Number != c.Number {
		t.Fatalf("restored original invalid err=%v", err)
	}
	if _, err = r.ReconcileProjectDeliveries(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = r.ReconcileFinancialEffectives(ctx); err != nil {
		t.Fatal(err)
	}
	if recordE2ECount(t, r, &projectDeliveryOutboxRecord{}, c) != 0 || recordE2ECount(t, r, &financialEffectiveOutboxRecord{}, c) != 0 {
		t.Fatal("restore triggered project or finance rebuild")
	}
	var stream contractResponsibilityStream
	if err = r.db.Where("tenant_id=? AND contract_id=?", c.TenantID, c.ID).Take(&stream).Error; err != nil || !stream.Active || stream.Version != 3 {
		t.Fatalf("responsibility=%+v err=%v", stream, err)
	}
	replay, err := service.RestoreContract(ctx, actor, c.ID, 2, "重复恢复")
	if err != nil || replay.Changed || replay.Version != 3 {
		t.Fatalf("restore replay=%+v err=%v", replay, err)
	}
}

// TestContractRecordScopeIntersectionMySQL 用途：验证管理权限不能扩大读取范围或租户。
// 参数：t 为显式隔离 MySQL 场景，一份合同分别核对错范围和错租户。
// 返回值：无；任一范围外记录暴露存在性或可写入时失败。
func TestContractRecordScopeIntersectionMySQL(t *testing.T) {
	_, service, actor, c := recordE2EFixture(t)
	actor.PermissionScopes["contract.delete"] = contract.ScopeFilter{OrganizationIDs: []string{ulid.Make().String()}}
	if _, err := service.PreviewContractDeletion(context.Background(), actor, c.ID); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("intersection error=%v", err)
	}
	actor.PermissionScopes["contract.delete"] = contract.ScopeFilter{AllowAll: true}
	actor.TenantID = ulid.Make().String()
	if _, err := service.DeleteContract(context.Background(), actor, c.ID, 1, "越界请求"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("tenant error=%v", err)
	}
}

// TestContractRecordConcurrentDeleteMySQL 用途：验证两个相同版本的并发删除只产生一次变更。
// 参数：t 为显式隔离 MySQL 场景，两个协程使用同一份管理员及合同基线。
// 返回值：无；版本重复递增、失败或重复生命周期事件时失败。
func TestContractRecordConcurrentDeleteMySQL(t *testing.T) {
	r, service, actor, c := recordE2EFixture(t)
	var wait sync.WaitGroup
	results := make(chan contract.RecordMutation, 2)
	failures := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := service.DeleteContract(context.Background(), actor, c.ID, 1, "并发删除")
			results <- result
			failures <- err
		}()
	}
	wait.Wait()
	close(results)
	close(failures)
	changed := 0
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	for result := range results {
		if result.Version != 2 {
			t.Fatalf("version=%d", result.Version)
		}
		if result.Changed {
			changed++
		}
	}
	if changed != 1 || recordE2ECount(t, r, &lifecycleEventRecord{}, c) != 2 {
		t.Fatal("concurrent delete duplicated transition")
	}
}

// TestContractRecordAuditFailureRollbackMySQL 用途：验证审计落库失败时删除整体回滚。
// 参数：t 为隔离 MySQL 场景，故障仅注入本仓储连接的审计写回调。
// 返回值：无；墓碑、版本、责任或生命周期残留部分提交时失败。
func TestContractRecordAuditFailureRollbackMySQL(t *testing.T) {
	r, service, actor, c := recordE2EFixture(t)
	want := errors.New("isolated audit failure")
	const callback = "record-test:audit-failure"
	if err := r.db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "con_audit_outbox" {
			tx.AddError(want)
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, err := service.DeleteContract(context.Background(), actor, c.ID, 1, "回滚场景")
	_ = r.db.Callback().Create().Remove(callback)
	if !errors.Is(err, want) {
		t.Fatalf("delete error=%v", err)
	}
	current, err := r.GetContract(context.Background(), c.TenantID, c.ID)
	if err != nil || current.Version != 1 || current.DeletedAt != nil {
		t.Fatalf("rollback version=%d err=%v", current.Version, err)
	}
	if recordE2ECount(t, r, &lifecycleEventRecord{}, c) != 1 {
		t.Fatal("failed delete left lifecycle event")
	}
}

// TestContractRecordBlocksUnappliedApprovalMySQL 用途：验证已终决议但未落账审批仍须先收口。
// 参数：t 为隔离 MySQL 场景，只创建一份 rejected 且 completion_applied=false 的真实实例。
// 返回值：无；误按已结束审批放行删除时失败。
func TestContractRecordBlocksUnappliedApprovalMySQL(t *testing.T) {
	r, service, actor, c := recordE2EFixture(t)
	now := time.Now().UTC()
	id := ulid.Make().String()
	if err := r.db.Create(&approvalInstanceRecord{ID: id, TenantID: c.TenantID, ContractID: c.ID, Kind: "contract_approval", Status: "rejected",
		ApplicantUserID: actor.UserID, FromStatus: "pending", TargetStatus: "approved", NodesJSON: []byte("[]"),
		TemporalWorkflowID: "record-" + id, TemporalRunID: id, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	_, err := service.DeleteContract(context.Background(), actor, c.ID, 1, "未收口场景")
	var blocked *application.ContractDeleteBlockedError
	if !errors.As(err, &blocked) || len(blocked.Blockers) == 0 || blocked.Blockers[0].Code != "APPROVAL_IN_FLIGHT" {
		t.Fatalf("blockers=%+v err=%v", blocked, err)
	}
}

// TestContractRecordHistoryMetadataMySQL 用途：验证删除后仍可授权冻结快照且无法回读当前正文。
// 参数：t 为隔离 MySQL 场景，审批实例为已落账拒绝历史。
// 返回值：无；有限元数据泄露正文或冻结快照不可读时失败。
func TestContractRecordHistoryMetadataMySQL(t *testing.T) {
	r, service, actor, c := recordE2EFixture(t)
	now := time.Now().UTC()
	id := ulid.Make().String()
	if err := r.db.Create(&approvalInstanceRecord{ID: id, TenantID: c.TenantID, ContractID: c.ID, Kind: "contract_approval", Status: "rejected", CompletionApplied: true,
		ApplicantUserID: actor.UserID, FromStatus: "pending", TargetStatus: "approved", NodesJSON: []byte("[]"), TemporalWorkflowID: "record-" + id, TemporalRunID: id, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(c)
	if err := r.db.Create(&approvalSnapshotRecord{ApprovalID: id, TenantID: c.TenantID, ContractID: c.ID, ContractVersion: 1, ContractJSON: body, RenderedDocument: c.Document, CreatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.DeleteContract(context.Background(), actor, c.ID, 1, "冻结历史场景"); err != nil {
		t.Fatal(err)
	}
	metadata, err := r.GetContractHistoryMetadata(context.Background(), contract.ScopeFilter{TenantID: c.TenantID, AllowAll: true}, c.ID)
	if err != nil || metadata.DeletedAt == nil || metadata.Content != "" || len(metadata.Document) > 0 {
		t.Fatalf("history metadata boundary err=%v", err)
	}
	snapshot, found, err := r.GetApprovalSnapshot(context.Background(), c.TenantID, id)
	if err != nil || !found || snapshot.Content != c.Content {
		t.Fatalf("frozen history found=%v err=%v", found, err)
	}
}

// TestContractRecordHistoricalReportIsNotBusinessBlockerMySQL 用途：核对真实获准历史来源，纯统计任务不阻断删除。
// 参数：t 为隔离 MySQL 场景，来源策略明确不派生项目与资金。
// 返回值：无；来源不一致被放行、统计被误阻断或恢复派生业务时失败。
func TestContractRecordHistoricalReportIsNotBusinessBlockerMySQL(t *testing.T) {
	r, service, actor, c := recordE2EFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	itemID := ulid.Make().String()
	policy := &contract.ImportPolicy{Version: 1, ProjectMode: "NONE", FinanceMode: "NONE", ReportMode: "ARCHIVE_SEPARATE", ResponsibilityMode: "INACTIVE", SigningMode: "DECLARED_ONLY"}
	policyJSON, _ := json.Marshal(policy)
	if err := r.db.Model(&contractRecord{}).Where("id=?", c.ID).Updates(map[string]any{
		"origin_kind": "IMPORTED", "import_mode": "ARCHIVE_ONLY", "import_policy_json": policyJSON,
		"origin_import_item_id": itemID, "import_source_revision": 1, "import_confirmed_at": now, "status": "archived",
		"import_report_release_version": 1, "import_report_required_channels": "API_ONLY"}).Error; err != nil {
		t.Fatal(err)
	}
	batch := contract.ImportBatch{ID: ulid.Make().String(), TenantID: c.TenantID, ActorID: actor.UserID, State: "TARGETS_PENDING", Version: 1, Revision: 1,
		SourceSystem: "record_test", Confirmation: &contract.ImportConfirmation{State: "APPROVED"}, Items: []contract.ImportItemState{{
			ID: itemID, State: "COMPLETE", ContractID: c.ID, SourceRevision: 1, Confirmed: true,
			Item: contract.ImportItem{Mode: contract.ImportArchiveOnly, Policy: policy, HistoryEvidence: &contract.ImportHistoryEvidence{
				TargetOwnerUserID: actor.UserID, HistoricalStatus: "archived", ReportRequiredChannels: "API_ONLY"}},
			Targets: []contract.ImportTarget{{Target: "REPORT", State: "RELEASE_REQUESTED", Phase: "RELEASE", Version: 1,
				ReleaseVersion: 1, CommandID: "0123456789abcdef0123456789abcdef", OccurredAt: now}},
		}}}
	if err := r.CreateImportBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.db.Where("tenant_id=?", c.TenantID).Delete(&importBatchRecord{}).Error })
	source, err := r.ImportReportSource(ctx, c.TenantID)
	if err != nil || len(source.Items) != 1 {
		t.Fatalf("initial report source items=%d err=%v", len(source.Items), err)
	}
	preview, err := service.PreviewContractDeletion(ctx, actor, c.ID)
	if err != nil || !preview.CanDelete {
		t.Fatalf("historical preview=%+v err=%v", preview, err)
	}
	if _, err = service.DeleteContract(ctx, actor, c.ID, 1, "历史误建"); err != nil {
		t.Fatal(err)
	}
	source, err = r.ImportReportSource(ctx, c.TenantID)
	if err != nil || len(source.Items) != 0 {
		t.Fatalf("deleted report source items=%d err=%v", len(source.Items), err)
	}
	if err = service.DispatchImportTargetOnce(ctx); err != nil {
		t.Fatal(err)
	}
	blocked, err := r.GetImportBatch(ctx, c.TenantID, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := blocked.Items[0].Targets[0]
	if before.State != "BLOCKED" || before.ErrorCode != "CONTRACT_RECORD_DELETED" || blocked.State != "PARTIAL" {
		t.Fatalf("deleted report was not finite terminal: %+v", before)
	}
	actor.Permissions["contract.import.release"] = true
	actor.PermissionScopes["contract.import.release"] = contract.ScopeFilter{AllowAll: true}
	if _, err = service.ReleaseImportItem(ctx, actor, itemID, "REPORT", before.Version, "RELEASE"); !errors.Is(err, application.ErrContractRecordDeleted) {
		t.Fatalf("recheck before restore error=%v", err)
	}
	if _, err = service.RestoreContract(ctx, actor, c.ID, 2, ""); err != nil {
		t.Fatal(err)
	}
	source, err = r.ImportReportSource(ctx, c.TenantID)
	if err != nil || len(source.Items) != 1 || source.Items[0].ReportReleaseVersion != 1 {
		t.Fatalf("restored report source items=%d err=%v", len(source.Items), err)
	}
	requeued, err := service.ReleaseImportItem(ctx, actor, itemID, "REPORT", before.Version, "RELEASE")
	if err != nil {
		t.Fatal(err)
	}
	queued := requeued.Items[0].Targets[0]
	if queued.ReleaseVersion != before.ReleaseVersion || queued.CommandID != before.CommandID || requeued.Items[0].SourceRevision != 1 {
		t.Fatal("manual report recheck changed frozen identity")
	}
	var calls atomic.Int32
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Method != http.MethodGet || request.URL.Query().Get("report_release_version") != "1" {
			t.Error("report recheck changed method or release version")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "OK", "data": map[string]any{
			"schema_version": 1, "tenant_id": c.TenantID, "item_id": itemID, "source_revision": 1, "report_release_version": 1,
			"required_channels": "API_ONLY", "applied_channels": []string{"API_APPLIED"}, "state": "APPLIED",
			"source_snapshot_id": strings.Repeat("b", 64), "aggregation_run_id": "isolated-report-run", "applied_at": now, "source_facts_connected": false}})
	}))
	defer receiver.Close()
	service.ImportReport = &application.ImportTargetTransport{BaseURL: receiver.URL, Client: receiver.Client(), TokenSource: func(context.Context) (string, error) { return "isolated-test-identity", nil }}
	if err = service.DispatchImportTargetOnce(ctx); err != nil {
		t.Fatal(err)
	}
	complete, err := r.GetImportBatch(ctx, c.TenantID, batch.ID)
	if err != nil || complete.State != "COMPLETED" || complete.Items[0].Targets[0].State != "COMPLETE" || calls.Load() != 1 {
		t.Fatalf("report recovery state=%s calls=%d err=%v", complete.State, calls.Load(), err)
	}
	if _, err = r.ReconcileFinancialEffectives(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = r.ReconcileProjectDeliveries(ctx); err != nil {
		t.Fatal(err)
	}
	if recordE2ECount(t, r, &projectDeliveryOutboxRecord{}, c) != 0 || recordE2ECount(t, r, &financialEffectiveOutboxRecord{}, c) != 0 {
		t.Fatal("historical restore derived business events")
	}
}
