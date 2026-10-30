package application

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

type projectRetryRepository struct {
	recordingRepository
	batch       contract.ImportBatch
	beforeQueue func(*contract.ImportBatch)
	claim       bool
	saved       *contract.ImportTarget
}

// GetImportItemBatch 返回独立测试的同源批次。
// 参数：ctx为请求，tenant和item为查询身份。
// 返回值：测试批次，不访问真实数据库。
func (r *projectRetryRepository) GetImportItemBatch(ctx context.Context, tenant, item string) (contract.ImportBatch, error) {
	return r.batch, nil
}

// QueueImportTarget 模拟存储层版本围栏及持锁回调。
// 参数：ctx及身份参数限定目标，version为期望版本，change为实际业务修改。
// 返回值：版本冲突或业务回调错误；失败不提交目标。
func (r *projectRetryRepository) QueueImportTarget(ctx context.Context, tenant, batch, item string, version uint64, target string, change func(*contract.ImportBatch, *contract.ImportItemState, *contract.ImportTarget) error) error {
	if r.beforeQueue != nil {
		r.beforeQueue(&r.batch)
	}
	i := &r.batch.Items[0]
	task := i.Targets[0]
	if task.Version != version {
		return ErrImportConflict
	}
	if err := change(&r.batch, i, &task); err != nil {
		return err
	}
	task.Version++
	i.Targets[0] = task
	return nil
}

// ClaimImportTarget 禁止恢复请求同步执行远端命令。
// 参数：ctx为调度上下文，now为认领时间。
// 返回值：没有后台任务。
func (r *projectRetryRepository) ClaimImportTarget(ctx context.Context, now time.Time) (contract.ImportBatch, string, string, bool, error) {
	if r.claim {
		return r.batch, "item", "PROJECT", true, nil
	}
	return contract.ImportBatch{}, "", "", false, nil
}

// SaveImportTarget 拒绝测试中越过排队事务直接保存。
// 参数：ctx、tenant、batch、item及task为目标身份及状态。
// 返回值：固定错误，防止测试替身制造成功。
func (r *projectRetryRepository) SaveImportTarget(ctx context.Context, tenant, batch, item string, task contract.ImportTarget) error {
	if r.claim {
		r.saved = &task
		return nil
	}
	return errors.New("unexpected direct target save")
}

// ActivateImportResponsibility 禁止项目重试激活财务责任。
// 参数：ctx、tenant、item及version为责任围栏。
// 返回值：固定错误，确认没有跨目标副作用。
func (r *projectRetryRepository) ActivateImportResponsibility(ctx context.Context, tenant, item string, version uint64) (uint64, error) {
	return 0, errors.New("unexpected responsibility activation")
}

// rejectedProjectFixture 构造与现场422协议形态相同的冻结测试数据。
// 参数：t为测试上下文。
// 返回值：独立存储替身、应用服务及具有既有放行权限的主体。
func rejectedProjectFixture(t *testing.T) (*projectRetryRepository, *Service, Principal) {
	t.Helper()
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	facts := []contract.ImportFact{}
	policy := &contract.ImportPolicy{Version: 1, ProjectMode: "CREATE_NEW", FinanceMode: "FULL_REPLAY", ReportMode: "HISTORICAL_SEPARATE", ResponsibilityMode: "ACTIVATE_ON_RELEASE", SigningMode: "DECLARED_ONLY"}
	item := contract.ImportItemState{ID: "item", ContractID: "contract", SourceRevision: 4, Confirmed: true,
		Item: contract.ImportItem{Mode: contract.ImportContinueLive, Policy: policy,
			Metadata:        contract.ImportMetadata{StartDate: &start, PaymentTerms: []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2026-12-31", Amount: "300.00"}}},
			HistoryEvidence: &contract.ImportHistoryEvidence{TargetOwnerUserID: "owner", Relationship: "PRIMARY", HistoricalStatus: "active", ExpectedFacts: &facts, ReportRequiredChannels: "API_AND_SOURCE_FACTS"}}}
	c := contract.Contract{ID: "contract", TenantID: "tenant", Number: "HT-QA", Version: 2, Title: "synthetic QA", CRMCustomerID: 10, CustomerName: "synthetic customer", OwnerUserID: "owner", StartDate: &start, OriginKind: "IMPORTED", OriginImportItemID: "item", ImportMode: contract.ImportContinueLive, ImportPolicy: policy,
		ServiceItems: []contract.ServiceItem{{Name: "QA", Site: "QA site", Batch: "one", Category: "software", TestMode: "STANDARD"}}}
	digest, _ := contract.ImportDigest(policy)
	commandID := stableImportCommand(item.ID, 4, "PROJECT", "RELEASE")
	// 独立协议样本明确列出接收方字段，避免用待修复的构造函数生成期望。
	payload := map[string]any{"schema_version": 1, "command_id": commandID, "tenant_id": "tenant", "contract_id": "contract", "contract_version": "2", "origin_import_item_id": "item", "source_revision": 4, "policy_digest": digest, "release_version": 1, "import_mode": "CONTINUE_LIVE", "project_mode": "CREATE_NEW", "relationship": "PRIMARY", "target_owner_user_id": "owner",
		"contract": map[string]any{"contract_id": "contract", "contract_number": "HT-QA", "contract_version": "2", "contract_name": "synthetic QA", "customer": "synthetic customer", "customer_id": "10", "effective_at": "2025-01-01T00:00:00Z", "services": []any{map[string]any{"source_id": "contract-01", "name": "QA", "site": "QA site", "batch": "one", "category": "software", "system": "", "system_level": "", "requirement": "", "test_mode": "STANDARD"}}}}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	rejected := time.Now().UTC().Add(-time.Minute)
	item.Targets = []contract.ImportTarget{{Target: "PROJECT", Phase: "RELEASE", State: "BLOCKED", Version: 5, ReleaseVersion: 1, CommandID: commandID, Epoch: 2, Attempts: 1, RecipientHTTPStatus: 422, RecipientCode: "IMPORT_VALIDATION_FAILED", RejectedAt: &rejected, RequestPayload: string(raw), OccurredAt: rejected}}
	r := &projectRetryRepository{recordingRepository: recordingRepository{contract: c}, batch: contract.ImportBatch{ID: "batch", TenantID: "tenant", ActorID: "importer", Items: []contract.ImportItemState{item}, Confirmation: &contract.ImportConfirmation{State: "APPROVED"}}}
	actor := Principal{TenantID: "tenant", UserID: "operator", Permissions: map[string]bool{"contract.import.release": true}, PermissionScopes: allowAllScope("contract.import.release")}
	return r, &Service{Repo: r}, actor
}

// TestRejectedProjectCanRetryIdenticalCommand 验证修正外部依赖后显式重排同一命令。
// 参数：t为测试上下文。
// 返回值：无；载荷漂移、提前成功、丢失真实拒绝或代次不围栏时失败。
func TestRejectedProjectCanRetryIdenticalCommand(t *testing.T) {
	r, s, actor := rejectedProjectFixture(t)
	old := r.batch.Items[0].Targets[0]
	result, err := s.ReleaseImportItem(context.Background(), actor, "item", "PROJECT", 5, "RELEASE")
	if err != nil {
		t.Fatalf("explicit same-command retry rejected: %v", err)
	}
	got := result.Items[0].Targets[0]
	if got.State != "QUEUED" || got.Epoch != old.Epoch+1 || got.Version != old.Version+1 || got.CommandID != old.CommandID || got.RequestPayload != old.RequestPayload || got.ReleaseVersion != old.ReleaseVersion || got.CommandGeneration != old.CommandGeneration || !got.OccurredAt.Equal(old.OccurredAt) {
		t.Fatalf("frozen command changed or completed early: %#v", got)
	}
	if len(got.CommandHistory) != 1 || got.CommandHistory[0].HTTPStatus != 422 || got.CommandHistory[0].RecipientCode != "IMPORT_VALIDATION_FAILED" || got.CommandHistory[0].RequestPayload != old.RequestPayload || got.RecoveryRequestedBy != actor.UserID {
		t.Fatal("lost rejection evidence or operator identity")
	}
}

// TestRejectedProjectRetryRemainsFailClosed 验证恢复入口不放宽冻结基线与授权边界。
// 参数：t为表驱动测试上下文。
// 返回值：无；任一危险形态可被重排时失败。
func TestRejectedProjectRetryRemainsFailClosed(t *testing.T) {
	changes := map[string]func(*projectRetryRepository, *Principal){
		"permission":        func(r *projectRetryRepository, p *Principal) { p.Permissions = nil },
		"tenant":            func(r *projectRetryRepository, p *Principal) { p.TenantID = "foreign" },
		"unconfirmed":       func(r *projectRetryRepository, p *Principal) { r.batch.Confirmation.State = "PENDING" },
		"unknown-result":    func(r *projectRetryRepository, p *Principal) { r.batch.Items[0].Targets[0].RecipientHTTPStatus = 0 },
		"identity-conflict": func(r *projectRetryRepository, p *Principal) { r.batch.Items[0].Targets[0].RecipientHTTPStatus = 409 },
		"other-code": func(r *projectRetryRepository, p *Principal) {
			r.batch.Items[0].Targets[0].RecipientCode = "IMPORT_POLICY_UNSUPPORTED"
		},
		"missing-rejection": func(r *projectRetryRepository, p *Principal) { r.batch.Items[0].Targets[0].RejectedAt = nil },
		"active-lease": func(r *projectRetryRepository, p *Principal) {
			r.batch.Items[0].Targets[0].LeaseUntil = time.Now().Add(time.Minute)
		},
		"changed-owner":    func(r *projectRetryRepository, p *Principal) { r.contract.OwnerUserID = "other" },
		"changed-contract": func(r *projectRetryRepository, p *Principal) { r.contract.Version++ },
		"changed-payload":  func(r *projectRetryRepository, p *Principal) { r.batch.Items[0].Targets[0].RequestPayload = `{}` },
		"history-limit": func(r *projectRetryRepository, p *Principal) {
			r.batch.Items[0].Targets[0].CommandHistory = make([]contract.ImportTargetCommandHistory, 100)
		},
		"stale-version": func(r *projectRetryRepository, p *Principal) { r.batch.Items[0].Targets[0].Version++ },
		"late-generation": func(r *projectRetryRepository, p *Principal) {
			r.beforeQueue = func(b *contract.ImportBatch) { b.Items[0].Targets[0].Epoch++ }
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			r, s, actor := rejectedProjectFixture(t)
			change(r, &actor)
			_, err := s.ReleaseImportItem(context.Background(), actor, "item", "PROJECT", 5, "RELEASE")
			if err == nil || r.batch.Items[0].Targets[0].State != "BLOCKED" {
				t.Fatal("unsafe retry accepted", err)
			}
		})
	}
}

// TestRejectedProjectRetryPreservesEveryOperator 验证连续恢复不覆盖先前操作者。
// 参数：t为同一冻结命令的两次显式恢复测试。
// 返回值：无；任一次恢复缺少实际操作者和时间时失败。
func TestRejectedProjectRetryPreservesEveryOperator(t *testing.T) {
	r, s, actor := rejectedProjectFixture(t)
	if _, err := s.ReleaseImportItem(context.Background(), actor, "item", "PROJECT", 5, "RELEASE"); err != nil {
		t.Fatal(err)
	}
	task := &r.batch.Items[0].Targets[0]
	rejected := time.Now().UTC()
	task.State, task.RecipientHTTPStatus, task.RecipientCode = "BLOCKED", 422, "IMPORT_VALIDATION_FAILED"
	task.RejectedAt = &rejected
	task.Attempts++
	actor.UserID = "second-operator"
	if _, err := s.ReleaseImportItem(context.Background(), actor, "item", "PROJECT", task.Version, "RELEASE"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(r.batch.Items[0].Targets[0].CommandHistory)
	var history []map[string]any
	if err := json.Unmarshal(raw, &history); err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0]["recovery_requested_by"] != "operator" || history[1]["recovery_requested_by"] != "second-operator" || history[0]["recovery_requested_at"] == nil || history[1]["recovery_requested_at"] == nil {
		t.Fatalf("per-attempt operator evidence lost: %s", raw)
	}
}

// TestProjectTargetCompletionClearsPreviousWait 验证真实合法回执清除上次等待提示。
// 参数：t为真实本地HTTP连接与后台派发测试上下文。
// 返回值：无；当前投影漂移仍发送旧命令或成功后保留旧等待错误时失败。
func TestProjectTargetCompletionClearsPreviousWait(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-baseline", true: "changed-baseline"}[changed], func(t *testing.T) {
			r, s, _ := rejectedProjectFixture(t)
			r.claim = true
			task := &r.batch.Items[0].Targets[0]
			task.State, task.ErrorCode = "WAITING", "IMPORT_TARGET_WAITING"
			task.RecipientHTTPStatus, task.RecipientCode, task.RejectedAt = 0, "", nil
			if changed {
				r.contract.Version++
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				raw, _ := io.ReadAll(req.Body)
				if string(raw) != task.RequestPayload {
					t.Error("frozen bytes changed")
				}
				var reply map[string]any
				if err := json.Unmarshal(raw, &reply); err != nil {
					t.Error(err)
					return
				}
				reply["project_id"], reply["project_version"], reply["status"] = "one-project", 1, "PROJECT_BOUND"
				_ = json.NewEncoder(w).Encode(map[string]any{"data": reply})
			}))
			defer server.Close()
			s.ImportProject = &ImportTargetTransport{BaseURL: server.URL, TokenSource: func(context.Context) (string, error) { return "unit-machine-token", nil }, Client: server.Client()}
			if err := s.DispatchImportTargetOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if r.saved == nil {
				t.Fatal("no persisted result")
			}
			if changed {
				if calls != 0 || r.saved.State != "BLOCKED" {
					t.Fatal("changed baseline reached recipient", calls, r.saved.State)
				}
			} else if calls != 1 || r.saved.State != "COMPLETE" || r.saved.ErrorCode != "" || r.saved.ReceiptID != "one-project" {
				t.Fatal("success retained previous wait", r.saved.State, r.saved.ErrorCode)
			}
		})
	}
}
