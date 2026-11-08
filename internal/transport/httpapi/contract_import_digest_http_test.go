package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// digestSubmitStore 是提交确认用例的最小内存仓储：
// 只实现提交路径真正用到的批次读取、冻结与租户开关读取，其余方法一律拒绝，
// 这样误走其他写入时用例会立刻失败而不是静默通过。
type digestSubmitStore struct {
	application.Repository
	batch   contract.ImportBatch
	setting contract.ImportAutoApproveSetting
}

// GetImportBatch 返回当前内存批次。
// 参数：ctx/tenant/id 为读取身份（本用例只有一个批次）。
// 返回值：批次与无错误。
func (s *digestSubmitStore) GetImportBatch(_ context.Context, _, _ string) (contract.ImportBatch, error) {
	return s.batch, nil
}

// CreateImportBatch 拒绝本用例建立新批次：提交只针对预置的已预检批次。
// 参数：ctx/b 为创建契约。
// 返回值：固定不支持错误。
func (s *digestSubmitStore) CreateImportBatch(context.Context, contract.ImportBatch) error {
	return application.ErrImportUnsupported
}

// ClaimImportBatch 禁止后台调度干扰本用例。
// 参数：ctx/now 为调度条件。
// 返回值：没有任务。
func (s *digestSubmitStore) ClaimImportBatch(context.Context, time.Time) (contract.ImportBatch, bool, error) {
	return contract.ImportBatch{}, false, nil
}

// CommitImportDraft 拒绝本用例创建合同（提交阶段不得建单）。
// 参数：其余参数为建单身份。
// 返回值：固定不支持错误。
func (s *digestSubmitStore) CommitImportDraft(context.Context, string, string, uint64, uint64, string, contract.Contract, string) (string, error) {
	return "", application.ErrImportUnsupported
}

// UpdateImportBatch 拒绝非冻结写路径：提交必须走 FreezeImportConfirmation。
// 参数：其余参数为写入契约。
// 返回值：固定不支持错误。
func (s *digestSubmitStore) UpdateImportBatch(context.Context, string, string, func(*contract.ImportBatch) error) error {
	return application.ErrImportUnsupported
}

// FreezeImportConfirmation 模拟真实仓储的冻结语义：回调成功后推进批次版本。
// 参数：ctx/tenantID/batchID 为定位身份，change 为冻结回调。
// 返回值：回调错误或成功。
func (s *digestSubmitStore) FreezeImportConfirmation(_ context.Context, _, _ string, change func(*contract.ImportBatch, contract.ImportAutoApproveSetting) error) error {
	if err := change(&s.batch, s.setting); err != nil {
		return err
	}
	s.batch.Version++
	return nil
}

// GetImportAutoApproveSetting 返回用例预置的租户运行开关。
// 参数：ctx/tenantID 为读取身份。
// 返回值：预置设置。
func (s *digestSubmitStore) GetImportAutoApproveSetting(context.Context, string) (contract.ImportAutoApproveSetting, error) {
	return s.setting, nil
}

// SetImportAutoApproveSetting 让仓储满足开关接口（本用例不写开关）。
// 参数：其余参数为写入契约。
// 返回值：固定不支持错误。
func (s *digestSubmitStore) SetImportAutoApproveSetting(context.Context, string, bool, uint64, string) (contract.ImportAutoApproveSetting, error) {
	return contract.ImportAutoApproveSetting{}, application.ErrImportUnsupported
}

// digestHTTPBatch 构造一份已预检、等待提交的历史批次。
// 参数：tenant/actorID 为测试租户与创建人。
// 返回值：PREVIEWED 批次；单项来自真实历史清单形状（含责任人证据）。
func digestHTTPBatch(tenant, actorID string) contract.ImportBatch {
	manifest := historicalManifest("source-file", actorID)
	item := manifest.Items[0]
	// 合法历史CRM开立清单不声明已有引用，摘要测试止于冻结，不执行开立。
	item.Metadata.CustomerContact = "测试联系人"
	item.Metadata.CustomerPhone = "13800138000"
	return contract.ImportBatch{
		ID: "batch-digest", TenantID: tenant, ActorID: actorID, Version: 1, Revision: 1,
		State: "PREVIEWED", SourceSystem: "isolated-history", PreviewToken: "preview-token-digest",
		PreviewExpiresAt: time.Now().Add(time.Hour), ExpiresAt: time.Now().Add(time.Hour),
		Sources: []contract.ImportSource{{ID: "source-file", ItemKey: "history-one",
			SHA256: strings.Repeat("a", 64), Filename: "history.pdf", MediaType: "application/pdf", State: "READY"}},
		Items: []contract.ImportItemState{{ID: "item-digest", Item: item, SourceRevision: 1,
			State: "READY", Digest: "item-digest"}},
	}
}

// TestImportConfirmationDigestExposedOverHTTP 验证提交与读取均暴露冻结清单摘要。
// 参数：t为测试上下文；合法历史CRM开立AUTO资格，内存仓储与真实TCP路由。
// 返回值：无；字段缺失、长度不符或两次读取不一致时失败。
func TestImportConfirmationDigestExposedOverHTTP(t *testing.T) {
	tenant, actorID := "tenant-digest", "actor-digest"
	batch := digestHTTPBatch(tenant, actorID)
	store := &digestSubmitStore{
		batch:   batch,
		setting: contract.ImportAutoApproveSetting{Enabled: true, Version: 1},
	}
	service := &application.Service{
		Repo: store, ContractImportEnabled: true, ContractImportHistoryEnabled: true,
		ContractImportAutoApproveHistorical: true,
		ContractImportConfirmationRoles:     []string{"admin"},
		ImportOwners:                        historicalImportOwners{},
		CRMProvisionCustomerClient:          historicalProvisionCustomer{},
		CRMProvisionOpportunityClient:       historicalProvisionOpportunity{},
		CRMProvisionOwnerDirectory:          historicalProvisionOwners{},
	}
	// AUTO 路径刻意不注入 Temporal：摘要与策略固化都不依赖工作流。
	client := historicalServer(t, service, tenant, actorID)

	// 期望值与提交侧同一口径独立算出：{revision, items, sources} 的 SHA256。
	expected, err := contract.ImportDigest(struct {
		Revision uint64
		Items    []contract.ImportItemState
		Sources  []contract.ImportSource
	}{batch.Revision, batch.Items, batch.Sources})
	if err != nil {
		t.Fatalf("计算期望摘要失败: %v", err)
	}

	path := "/api/v1/contract-import-batches/" + batch.ID
	body, _ := json.Marshal(map[string]any{"revision": batch.Revision, "preview_token": batch.PreviewToken})
	status, data, err := client.request(http.MethodPost, path+"/submit-confirmation", body, "application/json", "digest-submit-key", false)
	if err != nil || status != http.StatusAccepted {
		t.Fatalf("提交状态=%d err=%v response=%s", status, err, data)
	}
	submitted := importBatchConfirmationDigest(t, data)
	if submitted != expected {
		t.Fatalf("提交响应 manifest_digest=%q, want %q", submitted, expected)
	}

	// GET 批次必须返回同一摘要：确认范围不因读取路径（重新序列化）而变化。
	status, data, err = client.request(http.MethodGet, path, nil, "application/json", "", false)
	if err != nil || status != http.StatusOK {
		t.Fatalf("读取状态=%d err=%v response=%s", status, err, data)
	}
	if read := importBatchConfirmationDigest(t, data); read != expected {
		t.Fatalf("批次读取 manifest_digest=%q, want %q", read, expected)
	}
}

// importBatchConfirmationDigest 从 HTTP 信封中取出 confirmation.manifest_digest。
// 参数：t 为测试上下文，body 为原始响应字节。
// 返回值：摘要；字段缺失、类型不符或长度不为 64 时直接失败。
func importBatchConfirmationDigest(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		Code string `json:"code"`
		Data struct {
			Confirmation map[string]any `json:"confirmation"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Code != "OK" {
		t.Fatalf("响应不是成功信封: %v %s", err, body)
	}
	raw, ok := envelope.Data.Confirmation["manifest_digest"]
	if !ok {
		t.Fatalf("响应缺少 confirmation.manifest_digest: %s", body)
	}
	digest, ok := raw.(string)
	if !ok || len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		t.Fatalf("manifest_digest=%v 不是 64 位十六进制摘要", raw)
	}
	return digest
}
