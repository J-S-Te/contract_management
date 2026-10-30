package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/testfixture"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestImportTargetRejectsBare200 验证HTTP成功不等于合法业务回执。
// 参数：t为HTTP替身测试上下文。
// 返回值：无；裸200、缺失信封或跨主机重定向被接受时失败。
func TestImportTargetRejectsBare200(t *testing.T) {
	for _, body := range []string{"OK", `{}`, `{"data":null}`, `{"code":"ERROR","data":{}}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		transport := &ImportTargetTransport{BaseURL: server.URL, TokenSource: func(context.Context) (string, error) { return "test", nil }}
		var out map[string]any
		if e := transport.Call(context.Background(), "GET", "/test", nil, &out); !errors.Is(e, ErrImportTargetReceipt) {
			t.Fatalf("bare success accepted: %v", e)
		}
		server.Close()
	}
}

// TestImportConfirmationNoAssigneesAndScope 验证空角色成员与非受派主体不能确认历史。
// 参数：t为应用层测试上下文。
// 返回值：无；空成员、跨租户或失效角色被接受时失败。
func TestImportConfirmationNoAssigneesAndScope(t *testing.T) {
	b := contract.ImportBatch{ID: "batch", TenantID: "tenant", ActorID: "importer", Version: 1, Confirmation: &contract.ImportConfirmation{State: "PENDING", Roles: []string{"finance_director"}, Assignees: map[string][]string{"finance_director": {"reviewer"}}, ExpiresAt: time.Now().Add(time.Hour)}}
	actor := Principal{TenantID: "tenant", UserID: "reviewer", Roles: []string{"finance_director"}, Permissions: map[string]bool{"contract.import.approve": true}, PermissionScopes: allowAllScope("contract.import.approve")}
	if e := importConfirmationAccess(actor, b); e != nil {
		t.Fatal(e)
	}
	for _, change := range []string{"tenant", "role", "user", "permission"} {
		p := actor
		switch change {
		case "tenant":
			p.TenantID = "foreign"
		case "role":
			p.Roles = nil
		case "user":
			p.UserID = "unassigned"
		case "permission":
			p.Permissions = nil
		}
		if !errors.Is(importConfirmationAccess(p, b), ErrForbidden) {
			t.Fatal("unassigned confirmation allowed", change)
		}
	}
	repo := &importPreviewRepository{batch: b}
	svc := &Service{Repo: repo}
	if e := svc.ActImportConfirmation(context.Background(), actor, "batch", 1, "approve-key", "APPROVE"); e != nil {
		t.Fatal(e)
	}
	if e := svc.ActImportConfirmation(context.Background(), actor, "batch", 1, "approve-key", "APPROVE"); e != nil {
		t.Fatal("same command should replay", e)
	}
	if e := svc.ActImportConfirmation(context.Background(), actor, "batch", repo.batch.Version, "approve-key", "REJECT"); !errors.Is(e, ErrImportConflict) {
		t.Fatal("same key changed command accepted", e)
	}
}

// TestImportTargetDeterministicConflictBlocks 验证来源或策略冲突不进入无限重试。
// 参数：t为协议HTTP测试上下文。
// 返回值：无；确定性冲突被当作暂时等待时失败。
func TestImportTargetDeterministicConflictBlocks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		w.Write([]byte(`{"code":"IMPORT_IDEMPOTENCY_CONFLICT"}`))
	}))
	defer server.Close()
	transport := &ImportTargetTransport{BaseURL: server.URL, TokenSource: func(context.Context) (string, error) { return "test", nil }}
	var out map[string]any
	if e := transport.Call(context.Background(), "POST", "/test", nil, &out); !errors.Is(e, ErrImportTargetReceipt) {
		t.Fatal("deterministic conflict would retry forever", e)
	}
}

type importDocumentAuditRepository struct {
	importPreviewRepository
	auditCalls int
	auditErr   error
}

// RecordDocumentAccess 记录本测试的访问审计边界。
// 参数：ctx和身份参数限定本次调用。
// 返回值：注入的审计错误。
func (r *importDocumentAuditRepository) RecordDocumentAccess(ctx context.Context, tenant, id, kind, actor string) error {
	r.auditCalls++
	return r.auditErr
}

// TestImportAssignedReviewerReadsFrozenOriginal 验证受派人可经批次读取同一原件且失败审计不得泄露字节。
// 参数：t为应用层测试上下文。
// 返回值：无；越权、SHA漂移或审计失败被忽略时失败。
func TestImportAssignedReviewerReadsFrozenOriginal(t *testing.T) {
	pdf := testfixture.PDF("frozen historical contract")
	digest := fmt.Sprintf("%x", sha256.Sum256(pdf))
	repo := &importDocumentAuditRepository{}
	repo.batch = contract.ImportBatch{ID: "batch", TenantID: "tenant", ActorID: "importer", Sources: []contract.ImportSource{{ID: "file", SHA256: digest}}, Items: []contract.ImportItemState{{ID: "item", ContractID: "contract", Item: contract.ImportItem{SourceFileRef: "file"}}}, Confirmation: &contract.ImportConfirmation{Assignees: map[string][]string{"finance_director": {"reviewer"}}}}
	repo.contract = contract.Contract{ID: "contract", Document: pdf, DocumentFormat: "PDF", SourceSHA256: digest, SourceFilename: "history.pdf"}
	actor := Principal{TenantID: "tenant", UserID: "reviewer", Roles: []string{"finance_director"}, Permissions: map[string]bool{"contract.import.approve": true}, PermissionScopes: allowAllScope("contract.import.approve")}
	svc := &Service{Repo: repo}
	c, e := svc.GetImportSourceDocument(context.Background(), actor, "batch", "item")
	if e != nil || !bytes.Equal(c.Document, pdf) || repo.auditCalls != 1 {
		t.Fatal("assigned reviewer cannot read frozen original", e)
	}
	repo.auditErr = errors.New("audit unavailable")
	c, e = svc.GetImportSourceDocument(context.Background(), actor, "batch", "item")
	if e == nil || len(c.Document) != 0 {
		t.Fatal("audit failure leaked source")
	}
	repo.auditErr = nil
	repo.contract.Document = append([]byte(nil), pdf...)
	repo.contract.Document[0] = '!'
	if _, e = svc.GetImportSourceDocument(context.Background(), actor, "batch", "item"); !errors.Is(e, ErrImportIdentityConflict) {
		t.Fatal("source SHA drift accepted", e)
	}
	actor.Roles = nil
	if _, e = svc.GetImportSourceDocument(context.Background(), actor, "batch", "item"); !errors.Is(e, ErrForbidden) {
		t.Fatal("unassigned reviewer read source", e)
	}
}

// TestWithdrawnConfirmationPreservesItemRevisions 验证撤回后仅未建项升代且旧确认保留。
// 参数：t为应用层测试上下文。
// 返回值：无；旧基线代次被批次代次覆盖或旧确认丢失时失败。
func TestWithdrawnConfirmationPreservesItemRevisions(t *testing.T) {
	actor := Principal{TenantID: "tenant", UserID: "actor", Permissions: map[string]bool{"contract.import.preview": true}, PermissionScopes: allowAllScope("contract.import.preview")}
	first := contract.ImportItem{Key: "created", ExternalRecordID: "one", Mode: contract.ImportNormal, SourceFileRef: "first"}
	second := contract.ImportItem{Key: "pending", ExternalRecordID: "two", Mode: contract.ImportNormal, SourceFileRef: "second"}
	repo := &importPreviewRepository{batch: contract.ImportBatch{ID: "batch", TenantID: "tenant", ActorID: "actor", Version: 2, Revision: 1, State: "PAUSED", SourceSystem: "legacy", ExpiresAt: time.Now().Add(time.Hour), Sources: []contract.ImportSource{{ID: "first", ItemKey: "created", State: "READY"}, {ID: "second", ItemKey: "pending", State: "READY"}}, Items: []contract.ImportItemState{{ID: "created", ContractID: "contract", Item: first, SourceRevision: 1, State: "COMPLETE"}, {ID: "pending", Item: second, SourceRevision: 1, State: "BLOCKED"}}, Confirmation: &contract.ImportConfirmation{State: "WITHDRAWN", Revision: 1}}}
	second.Metadata.Title = "corrected pending"
	svc := &Service{Repo: repo, ContractImportEnabled: true}
	if e := svc.SetImportManifest(context.Background(), actor, "batch", 2, contract.ImportManifest{SchemaVersion: 1, SourceSystem: "legacy", Items: []contract.ImportItem{first, second}}); e != nil {
		t.Fatal(e)
	}
	if repo.batch.Revision != 2 || repo.batch.Items[0].SourceRevision != 1 || repo.batch.Items[1].SourceRevision != 2 || repo.batch.Confirmation != nil || len(repo.batch.ConfirmationHistory) != 1 {
		t.Fatal("withdraw revision separation failed")
	}
}

// TestImportFinanceRecoveryHistoryIsBounded 验证达到归档上限即拒绝恢复，不调用远端。
// 参数：t为应用边界测试上下文。
// 返回值：无；无限历史或绕过边界进入其他恢复分支时失败。
func TestImportFinanceRecoveryHistoryIsBounded(t *testing.T) {
	now := time.Now().UTC()
	task := contract.ImportTarget{Target: "FINANCE", State: "BLOCKED", Phase: "RELEASE", Version: 1, RecipientHTTPStatus: 409, RecipientCode: "SETTLEMENT_CONFLICT", RejectedAt: &now, RequestPayload: `{}`, CommandHistory: make([]contract.ImportTargetCommandHistory, 100)}
	b := contract.ImportBatch{Items: []contract.ImportItemState{{ID: "item", Targets: []contract.ImportTarget{task}}}}
	svc := &Service{}
	recovery, e := svc.prepareImportFinanceRecovery(context.Background(), nil, Principal{}, b, "item", "FINANCE", 1, "RELEASE")
	if !errors.Is(e, ErrImportUnsupported) || recovery != nil {
		t.Fatal("recovery history not bounded", e)
	}
}
