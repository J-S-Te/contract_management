package application

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// TestImportBatchPreviewTokenOnlySerializedForCreator 验证批次台账读取按读取人收窄：
// 创建人保持现有 preview_token 字段，独立审批人拿到的台账不序列化该提交凭据。
// 参数：t为测试上下文。
// 返回值：无；审批人收到提交凭据或创建人字段被误删时失败。
func TestImportBatchPreviewTokenOnlySerializedForCreator(t *testing.T) {
	repository := &importPreviewRepository{batch: contract.ImportBatch{
		ID: "batch", TenantID: "tenant", ActorID: "creator", State: "PREVIEWED",
		Revision: 2, Version: 2, PreviewToken: "preview-secret",
		PreviewExpiresAt: time.Now().Add(time.Minute), ExpiresAt: time.Now().Add(time.Hour),
	}}
	service := &Service{Repo: repository, ContractImportEnabled: true}
	creator := Principal{TenantID: "tenant", UserID: "creator",
		Permissions:      map[string]bool{"contract.import.preview": true},
		PermissionScopes: map[string]contract.ScopeFilter{"contract.import.preview": {AllowSelf: true}},
	}

	batch, e := service.GetImportBatch(context.Background(), creator, "batch")
	if e != nil {
		t.Fatal(e)
	}
	payload, e := json.Marshal(batch)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(payload), `"preview_token":"preview-secret"`) {
		t.Fatalf("创建人未收到 preview_token: %s", payload)
	}

	// 冻结独立确认后，受派审批人只能读到台账事实，读不到创建人的提交凭据。
	repository.batch.Confirmation = &contract.ImportConfirmation{
		State: "PENDING", Assignees: map[string][]string{"finance_director": {"approver"}},
	}
	approver := Principal{TenantID: "tenant", UserID: "approver", Roles: []string{"finance_director"},
		Permissions:      map[string]bool{"contract.import.approve": true},
		PermissionScopes: map[string]contract.ScopeFilter{"contract.import.approve": {AllowAll: true}},
	}
	batch, e = service.GetImportBatch(context.Background(), approver, "batch")
	if e != nil {
		t.Fatal(e)
	}
	payload, e = json.Marshal(batch)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(payload), "preview_token") || strings.Contains(string(payload), "preview-secret") {
		t.Fatalf("审批人收到 preview_token: %s", payload)
	}

	// 同一台账对创建人仍保持原字段，不因审批人读取而变化。
	batch, e = service.GetImportBatch(context.Background(), creator, "batch")
	if e != nil {
		t.Fatal(e)
	}
	payload, e = json.Marshal(batch)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(payload), `"preview_token":"preview-secret"`) {
		t.Fatalf("创建人第二次读取丢失 preview_token: %s", payload)
	}
}
