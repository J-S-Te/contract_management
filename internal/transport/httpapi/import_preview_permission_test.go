package httpapi

import (
	"context"
	"encoding/json"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type importPreviewPermissionStore struct {
	application.Repository
	batch  contract.ImportBatch
	writes int
}

// CreateImportBatch 禁止本预检权限测试产生新批次。
// 参数：ctx及b为测试调用参数。
// 返回值：不支持错误。
func (r *importPreviewPermissionStore) CreateImportBatch(ctx context.Context, b contract.ImportBatch) error {
	return application.ErrImportUnsupported
}

// GetImportBatch 返回当前测试批次，不执行任何写入。
// 参数：ctx、tenant及id为当前读取身份。
// 返回值：测试批次。
func (r *importPreviewPermissionStore) GetImportBatch(ctx context.Context, tenant, id string) (contract.ImportBatch, error) {
	return r.batch, nil
}

// UpdateImportBatch 模拟原子保存并记录实际成功写入次数。
// 参数：ctx、tenant及id限定批次，change为受测保存回调。
// 返回值：回调错误或成功，不在失败时推进版本。
func (r *importPreviewPermissionStore) UpdateImportBatch(ctx context.Context, tenant, id string, change func(*contract.ImportBatch) error) error {
	b := r.batch
	if e := change(&b); e != nil {
		return e
	}
	b.Version++
	r.batch = b
	r.writes++
	return nil
}

// ClaimImportBatch 禁止权限测试启动后台建单。
// 参数：ctx及now为调度条件。
// 返回值：没有任务。
func (r *importPreviewPermissionStore) ClaimImportBatch(ctx context.Context, now time.Time) (contract.ImportBatch, bool, error) {
	return contract.ImportBatch{}, false, nil
}

// CommitImportDraft 禁止权限测试写入任何业务合同。
// 参数：ctx及全部身份参数为未使用的后台调用参数。
// 返回值：不支持错误。
func (r *importPreviewPermissionStore) CommitImportDraft(ctx context.Context, tenant, batch string, revision, epoch uint64, item string, c contract.Contract, actor string) (string, error) {
	return "", application.ErrImportUnsupported
}

// TestImportPreviewApproveOnlyCannotMutateTerminalBatch 验证受派只读审批人不能通过预检重置终态确认。
// 参数：t为真实HTTP及Service测试上下文，仓储边界为明确内存替身。
// 返回值：无；非403或批次状态、版本、epoch及确认被改变时失败。
func TestImportPreviewApproveOnlyCannotMutateTerminalBatch(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "http://example.com")
	for _, state := range []string{"WITHDRAWN", "REJECTED", "EXPIRED"} {
		t.Run(state, func(t *testing.T) {
			actor := application.Principal{TenantID: "tenant", UserID: "reviewer", Roles: []string{"finance_director"}, Permissions: map[string]bool{"contract.import.approve": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.import.approve": {AllowAll: true}}}
			repo := &importPreviewPermissionStore{batch: contract.ImportBatch{ID: "batch", TenantID: "tenant", ActorID: "importer", State: "PAUSED", Version: 7, Revision: 3, Epoch: 9, OperationID: "frozen-operation", ExpiresAt: time.Now().Add(time.Hour), Confirmation: &contract.ImportConfirmation{State: state, Revision: 3, Assignees: map[string][]string{"finance_director": {"reviewer"}}}, Items: []contract.ImportItemState{{ID: "item", ContractID: "created-contract", State: "COMPLETE", SourceRevision: 1}}}}
			before, _ := json.Marshal(repo.batch)
			svc := &application.Service{Repo: repo, ContractImportEnabled: true}
			router := NewRouter(svc, identityFunc(func(context.Context, *http.Request) (application.Principal, error) { return actor, nil }), nil)
			read := httptest.NewRecorder()
			router.ServeHTTP(read, httptest.NewRequest(http.MethodGet, "http://example.com/api/v1/contract-import-batches/batch", nil))
			if read.Code != 200 {
				t.Fatalf("assigned reader unexpectedly denied: %d %s", read.Code, read.Body.String())
			}
			req := httptest.NewRequest(http.MethodPost, "http://example.com/api/v1/contract-import-batches/batch/preview", strings.NewReader(`{"revision":3}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", "http://example.com")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			after, _ := json.Marshal(repo.batch)
			if response.Code != http.StatusForbidden || repo.writes != 0 || string(before) != string(after) {
				t.Fatalf("approve-only preview mutated terminal batch: status=%d writes=%d state=%s version=%d epoch=%d body=%s", response.Code, repo.writes, repo.batch.State, repo.batch.Version, repo.batch.Epoch, response.Body.String())
			}
		})
	}
}
