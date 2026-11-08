package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/testfixture"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/client"
	temporalmocks "go.temporal.io/sdk/mocks"
)

// confirmationFreezeRepository 是提交冻结用例的内存仓储：
// 同时实现批次存储、冻结能力、运行开关读取与自动确认写入，用来覆盖
// 「策略判定与确认写入同一事务」的服务层语义（真实锁序由 MySQL 用例验证）。
type confirmationFreezeRepository struct {
	recordingRepository
	batch       contract.ImportBatch
	setting     contract.ImportAutoApproveSetting
	freezeCalls int
	getCalls    int
	freezeErr   error
	autoCalls   []ImportAutoApprovePolicy
}

// CreateImportBatch 保存测试批次。
// 参数：ctx 为上下文，b 为批次。
// 返回值：无错误。
func (r *confirmationFreezeRepository) CreateImportBatch(_ context.Context, b contract.ImportBatch) error {
	r.batch = b
	return nil
}

// GetImportBatch 返回测试批次。
// 参数：ctx/tenant/id 为调用身份。
// 返回值：批次及无错误。
func (r *confirmationFreezeRepository) GetImportBatch(_ context.Context, _, _ string) (contract.ImportBatch, error) {
	return r.batch, nil
}

// UpdateImportBatch 执行测试元数据回调并推进版本。
// 参数：ctx/tenant/id 为调用身份，change 为待执行修改。
// 返回值：回调错误。
func (r *confirmationFreezeRepository) UpdateImportBatch(_ context.Context, _, _ string, change func(*contract.ImportBatch) error) error {
	if err := change(&r.batch); err != nil {
		return err
	}
	r.batch.Version++
	return nil
}

// ClaimImportBatch 禁止后台调度干扰本用例。
// 参数：ctx/now 为调度条件。
// 返回值：没有任务。
func (r *confirmationFreezeRepository) ClaimImportBatch(context.Context, time.Time) (contract.ImportBatch, bool, error) {
	return contract.ImportBatch{}, false, nil
}

// CommitImportDraft 禁止本用例创建合同。
// 参数：其余参数为建单身份。
// 返回值：固定错误，出现即代表用例走错路径。
func (r *confirmationFreezeRepository) CommitImportDraft(context.Context, string, string, uint64, uint64, string, contract.Contract, string) (string, error) {
	return "", errors.New("confirmation test attempted contract creation")
}

// FreezeImportConfirmation 模拟真实仓储：先锁批次行再读设置，回调后保存。
// 参数：ctx 为上下文，tenantID/batchID 定位台账，change 为冻结回调。
// 返回值：注入错误、回调错误或成功。
func (r *confirmationFreezeRepository) FreezeImportConfirmation(_ context.Context, _, _ string, change func(*contract.ImportBatch, contract.ImportAutoApproveSetting) error) error {
	r.freezeCalls++
	if r.freezeErr != nil {
		return r.freezeErr
	}
	if err := change(&r.batch, r.setting); err != nil {
		return err
	}
	r.batch.Version++
	return nil
}

// GetImportAutoApproveSetting 记录读取次数并返回预置设置。
// 参数：ctx/tenantID 为读取身份。
// 返回值：预置设置。
func (r *confirmationFreezeRepository) GetImportAutoApproveSetting(_ context.Context, tenantID string) (contract.ImportAutoApproveSetting, error) {
	r.getCalls++
	setting := r.setting
	setting.TenantID = tenantID
	return setting, nil
}

// SetImportAutoApproveSetting 让测试仓储满足设置接口（本用例不写入）。
// 参数：其余参数为写入契约。
// 返回值：版本推进后的设置。
func (r *confirmationFreezeRepository) SetImportAutoApproveSetting(_ context.Context, tenantID string, enabled bool, expectedVersion uint64, actorID string) (contract.ImportAutoApproveSetting, error) {
	r.setting = contract.ImportAutoApproveSetting{TenantID: tenantID, Enabled: enabled,
		Version: expectedVersion + 1, UpdatedBy: actorID, UpdatedAt: time.Now().UTC()}
	return r.setting, nil
}

// AutoConfirmHistoricalImportItem 记录策略快照，供执行期口径断言使用。
// 参数：policy 为执行期策略快照。
// 返回值：无错误。
func (r *confirmationFreezeRepository) AutoConfirmHistoricalImportItem(_ context.Context, _, _, _ string, _ []string, policy ImportAutoApprovePolicy) error {
	r.autoCalls = append(r.autoCalls, policy)
	return nil
}

// confirmationSubmitBatch 建立一份已预检、等待提交的历史批次。
// 参数：t 为测试上下文，mode 为目标历史模式。
// 返回值：PREVIEWED 批次。
func confirmationSubmitBatch(t *testing.T, mode contract.ImportMode) contract.ImportBatch {
	t.Helper()
	pdf := testfixture.PDF("historical confirmation source")
	item := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
	// 直接声明 CRM 客户，避免依赖开立通道的查重前置。
	item.Metadata.CRMCustomerID = 42
	item.Mode = mode
	b := provisionImportBatch([]contract.ImportItem{item}, provisionTestSource("one", pdf))
	b.State = "PREVIEWED"
	b.PreviewToken = "preview-token"
	b.PreviewExpiresAt = time.Now().Add(time.Hour)
	b.Items[0].Digest = "item-digest"
	return b
}

// confirmationSubmitService 组装带冻结能力与受派人目录的服务。
// 参数：repo 为测试仓储，temporal 为工作流客户端替身。
// 返回值：服务实例。
func confirmationSubmitService(repo *confirmationFreezeRepository, temporal client.Client) *Service {
	return &Service{
		Repo: repo, ContractImportEnabled: true, ContractImportHistoryEnabled: true,
		ContractImportConfirmationRoles:     []string{"ROLE_IMPORT_APPROVER"},
		ContractImportAutoApproveHistorical: true,
		ImportOwners:                        provisionOwnerResolver{},
		Personnel: personnelStub{users: []UserReference{
			{UserID: "approver-1", Roles: []string{"ROLE_IMPORT_APPROVER"}}}},
		Temporal: temporal,
	}
}

// confirmationSubmitSession 返回固定的 32 字节会话哈希。
// 参数：无。
// 返回值：会话引用副本。
func confirmationSubmitSession() []byte {
	sum := sha256.Sum256([]byte("confirmation-submit-session"))
	return sum[:]
}

// TestSubmitImportConfirmationFreezesPinnedPolicy 验证提交事务内固化的策略判定：
// AUTO 在完全未配置 Temporal 时也能受理并落 APPROVED 台账（不再出现 submit 成功、
// 执行必冲突的 P0），MANUAL 保持既有 STARTING + 一次工作流启动。
// 参数：t 为测试上下文。
// 返回值：状态、策略字段、工作流调用次数不符时失败。
func TestSubmitImportConfirmationFreezesPinnedPolicy(t *testing.T) {
	ctx := context.Background()
	actor := provisionImportActor()

	t.Run("auto-without-temporal", func(t *testing.T) {
		repo := &confirmationFreezeRepository{
			batch:   confirmationSubmitBatch(t, contract.ImportArchiveOnly),
			setting: contract.ImportAutoApproveSetting{Enabled: true, Version: 1},
		}
		repo.batch.Items[0].Item.Metadata.CRMCustomerID = 0
		// 刻意不注入 Temporal：AUTO 路径不得依赖它。
		service := confirmationSubmitService(repo, nil)
		_ = service.ConfigureCRMProvision(true, completeProvisionClients())

		b, err := service.SubmitImportConfirmation(ctx, actor, "batch", 1, "preview-token", "submit-key-auto", confirmationSubmitSession())
		if err != nil {
			t.Fatalf("auto submit error: %v", err)
		}
		if repo.freezeCalls != 1 {
			t.Fatalf("freezeCalls=%d, want 1（提交必须走冻结能力）", repo.freezeCalls)
		}
		confirmation := b.Confirmation
		if confirmation == nil || confirmation.State != "APPROVED" || !confirmation.AutoApproved() {
			t.Fatalf("confirmation=%+v", confirmation)
		}
		if confirmation.PolicyDecision != contract.ImportPolicyDecisionAuto || confirmation.PolicyVersion != 1 {
			t.Fatalf("policy decision=%q version=%d", confirmation.PolicyDecision, confirmation.PolicyVersion)
		}
		if confirmation.WorkflowID != "contract-import-auto-batch" {
			t.Fatalf("workflow id=%q", confirmation.WorkflowID)
		}
		if len(confirmation.ApprovedRoles) != 1 || confirmation.ApprovedRoles[0] != "ROLE_IMPORT_APPROVER" {
			t.Fatalf("approved roles=%v", confirmation.ApprovedRoles)
		}
		if len(confirmation.Assignees) != 0 {
			t.Fatalf("auto assignees=%v, want empty", confirmation.Assignees)
		}
		if b.State != "QUEUED" || b.OperationID != "history-batch" || b.CommittedRevision != 1 {
			t.Fatalf("batch state=%s operation=%q revision=%d", b.State, b.OperationID, b.CommittedRevision)
		}

		// 同键重放：AUTO 已冻结的确认没有工作流，重放必须成功且不再依赖 Temporal。
		replayed, err := service.SubmitImportConfirmation(ctx, actor, "batch", 1, "preview-token", "submit-key-auto", confirmationSubmitSession())
		if err != nil || replayed.Confirmation == nil || !replayed.Confirmation.AutoApproved() {
			t.Fatalf("replay error=%v confirmation=%+v", err, replayed.Confirmation)
		}
	})

	t.Run("manual-keeps-starting-and-workflow", func(t *testing.T) {
		repo := &confirmationFreezeRepository{
			batch:   confirmationSubmitBatch(t, contract.ImportArchiveOnly),
			setting: contract.ImportAutoApproveSetting{Enabled: false, Version: 4},
		}
		temporal := temporalmocks.NewClient(t)
		run := temporalmocks.NewWorkflowRun(t)
		run.On("GetID").Return("workflow-1").Maybe()
		run.On("GetRunID").Return("run-1").Maybe()
		temporal.On("ExecuteWorkflow", mock.Anything, mock.Anything, workflows.ImportConfirmationWorkflowName, mock.Anything).Return(run, nil)
		service := confirmationSubmitService(repo, temporal)

		b, err := service.SubmitImportConfirmation(ctx, actor, "batch", 1, "preview-token", "submit-key-manual", confirmationSubmitSession())
		if err != nil {
			t.Fatalf("manual submit error: %v", err)
		}
		confirmation := b.Confirmation
		if confirmation == nil || confirmation.State != "STARTING" || confirmation.AutoApproved() {
			t.Fatalf("confirmation=%+v", confirmation)
		}
		if confirmation.PolicyDecision != contract.ImportPolicyDecisionManual || confirmation.PolicyVersion != 4 {
			t.Fatalf("policy decision=%q version=%d", confirmation.PolicyDecision, confirmation.PolicyVersion)
		}
		if confirmation.WorkflowID != "contract-import-confirmation-batch-1" {
			t.Fatalf("workflow id=%q", confirmation.WorkflowID)
		}
		if len(confirmation.Assignees["ROLE_IMPORT_APPROVER"]) != 1 || len(confirmation.ApprovedRoles) != 0 {
			t.Fatalf("assignees=%v approved=%v", confirmation.Assignees, confirmation.ApprovedRoles)
		}
		if len(temporal.Calls) != 1 {
			t.Fatalf("temporal calls=%d, want 1", len(temporal.Calls))
		}
	})

	t.Run("manual-requires-temporal", func(t *testing.T) {
		repo := &confirmationFreezeRepository{
			batch:   confirmationSubmitBatch(t, contract.ImportArchiveOnly),
			setting: contract.ImportAutoApproveSetting{Enabled: false, Version: 1},
		}
		service := confirmationSubmitService(repo, nil)

		_, err := service.SubmitImportConfirmation(ctx, actor, "batch", 1, "preview-token", "submit-key-manual-none", confirmationSubmitSession())
		if !errors.Is(err, ErrImportUnsupported) {
			t.Fatalf("error=%v, want ErrImportUnsupported", err)
		}
		if repo.freezeCalls != 0 {
			t.Fatalf("人工路径缺 Temporal 仍写入了确认: freezeCalls=%d", repo.freezeCalls)
		}
	})

	t.Run("capability-off-keeps-manual", func(t *testing.T) {
		repo := &confirmationFreezeRepository{
			batch:   confirmationSubmitBatch(t, contract.ImportArchiveOnly),
			setting: contract.ImportAutoApproveSetting{Enabled: true, Version: 2},
		}
		temporal := temporalmocks.NewClient(t)
		run := temporalmocks.NewWorkflowRun(t)
		run.On("GetID").Return("workflow-1").Maybe()
		run.On("GetRunID").Return("run-1").Maybe()
		temporal.On("ExecuteWorkflow", mock.Anything, mock.Anything, workflows.ImportConfirmationWorkflowName, mock.Anything).Return(run, nil)
		service := confirmationSubmitService(repo, temporal)
		// env 能力关闭时租户开关再开也只能走人工会签（逐字节保持既有行为）。
		service.ContractImportAutoApproveHistorical = false

		b, err := service.SubmitImportConfirmation(ctx, actor, "batch", 1, "preview-token", "submit-key-cap-off", confirmationSubmitSession())
		if err != nil {
			t.Fatalf("submit error: %v", err)
		}
		if b.Confirmation == nil || b.Confirmation.State != "STARTING" || b.Confirmation.AutoApproved() {
			t.Fatalf("confirmation=%+v", b.Confirmation)
		}
	})
}

// TestSubmitImportConfirmationRequiresFreezerCapability 验证仓储缺冻结能力时
// 失败关闭：不允许退化成「普通更新 + 执行期再读开关」的两事务写法。
// 参数：t 为测试上下文。
// 返回值：未被拒绝或写了确认时失败。
func TestSubmitImportConfirmationRequiresFreezerCapability(t *testing.T) {
	pdf := testfixture.PDF("historical confirmation source")
	item := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
	item.Metadata.CRMCustomerID = 42
	batch := provisionImportBatch([]contract.ImportItem{item}, provisionTestSource("one", pdf))
	batch.State = "PREVIEWED"
	batch.PreviewToken = "preview-token"
	batch.PreviewExpiresAt = time.Now().Add(time.Hour)
	batch.Items[0].Digest = "item-digest"
	repo := &importPreviewRepository{batch: batch}
	temporal := temporalmocks.NewClient(t)
	service := &Service{
		Repo: repo, ContractImportEnabled: true, ContractImportHistoryEnabled: true,
		ContractImportConfirmationRoles: []string{"ROLE_IMPORT_APPROVER"},
		ImportOwners:                    provisionOwnerResolver{},
		Temporal:                        temporal,
	}

	_, err := service.SubmitImportConfirmation(context.Background(), provisionImportActor(), "batch", 1, "preview-token", "submit-key-no-freezer", confirmationSubmitSession())
	if !errors.Is(err, ErrImportUnsupported) {
		t.Fatalf("error=%v, want ErrImportUnsupported", err)
	}
	if repo.batch.Confirmation != nil {
		t.Fatalf("缺能力仍然写入确认: %+v", repo.batch.Confirmation)
	}
}

// TestImportAutoApprovePolicyPinsFrozenDecision 验证执行期策略解析：
// 已固化策略只认 pin（不读 env/库），无确认台账才跟随 live，
// 未固化的旧数据跟随 live 但仅在这种形态下允许严格护栏内的升级。
// 参数：t 为测试上下文。
// 返回值：Auto/Version/AllowLegacyUpgrade 或读取次数不符时失败。
func TestImportAutoApprovePolicyPinsFrozenDecision(t *testing.T) {
	ctx := context.Background()
	for _, testCase := range []struct {
		name          string
		capability    bool
		setting       contract.ImportAutoApproveSetting
		confirmation  *contract.ImportConfirmation
		wantAuto      bool
		wantVersion   uint64
		wantUpgrade   bool
		wantReadCalls int
	}{
		{
			name: "pinned-auto-ignores-switch-off", capability: false,
			setting:      contract.ImportAutoApproveSetting{Enabled: false, Version: 9},
			confirmation: &contract.ImportConfirmation{State: "APPROVED", PolicyDecision: contract.ImportPolicyDecisionAuto, PolicyVersion: 3},
			wantAuto:     true, wantVersion: 3, wantReadCalls: 0,
		},
		{
			name: "pinned-manual-ignores-switch-on", capability: true,
			setting:      contract.ImportAutoApproveSetting{Enabled: true, Version: 9},
			confirmation: &contract.ImportConfirmation{State: "PENDING", PolicyDecision: contract.ImportPolicyDecisionManual, PolicyVersion: 2},
			wantAuto:     false, wantVersion: 2, wantReadCalls: 0,
		},
		{
			name: "no-confirmation-follows-live", capability: true,
			setting:  contract.ImportAutoApproveSetting{Enabled: true, Version: 5},
			wantAuto: true, wantVersion: 5, wantReadCalls: 1,
		},
		{
			name: "no-confirmation-live-off", capability: true,
			setting:  contract.ImportAutoApproveSetting{Enabled: false, Version: 6},
			wantAuto: false, wantVersion: 6, wantReadCalls: 1,
		},
		{
			name: "legacy-confirmation-allows-upgrade-when-live-on", capability: true,
			setting:      contract.ImportAutoApproveSetting{Enabled: true, Version: 7},
			confirmation: &contract.ImportConfirmation{State: "STARTING"},
			wantAuto:     true, wantVersion: 7, wantUpgrade: true, wantReadCalls: 1,
		},
		{
			name: "legacy-confirmation-live-off-blocks-upgrade", capability: true,
			setting:      contract.ImportAutoApproveSetting{Enabled: false, Version: 8},
			confirmation: &contract.ImportConfirmation{State: "STARTING"},
			wantAuto:     false, wantVersion: 8, wantUpgrade: false, wantReadCalls: 1,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repo := &confirmationFreezeRepository{setting: testCase.setting}
			service := &Service{Repo: repo, ContractImportAutoApproveHistorical: testCase.capability}
			b := confirmationSubmitBatch(t, contract.ImportArchiveOnly)
			b.Items[0].Item.Metadata.CRMCustomerID = 0
			b.Confirmation = testCase.confirmation
			_ = service.ConfigureCRMProvision(true, completeProvisionClients())

			policy, err := service.importAutoApprovePolicy(ctx, b)
			if err != nil {
				t.Fatalf("policy error: %v", err)
			}
			if policy.Auto != testCase.wantAuto || policy.Version != testCase.wantVersion ||
				policy.AllowLegacyUpgrade != testCase.wantUpgrade {
				t.Fatalf("policy=%+v, want auto=%t version=%d upgrade=%t", policy, testCase.wantAuto, testCase.wantVersion, testCase.wantUpgrade)
			}
			if repo.getCalls != testCase.wantReadCalls {
				t.Fatalf("setting reads=%d, want %d", repo.getCalls, testCase.wantReadCalls)
			}
		})
	}
}

// TestControlImportResumeHonorsPinnedPolicy 验证暂停/恢复的确认门禁：
// AUTO 冻结的 APPROVED 必须可恢复（该状态没有工作流，恢复只是重排待投递目标），
// 人工 PENDING/人工 APPROVED 与 STARTING 之外的终态仍必须拒绝。
// 参数：t 为测试上下文。
// 返回值：放行/拒绝结果不符时失败。
func TestControlImportResumeHonorsPinnedPolicy(t *testing.T) {
	ctx := context.Background()
	session := confirmationSubmitSession()
	for _, testCase := range []struct {
		name         string
		confirmation *contract.ImportConfirmation
		wantConflict bool
	}{
		{
			name: "auto-approved-resumable",
			confirmation: &contract.ImportConfirmation{State: "APPROVED",
				PolicyDecision: contract.ImportPolicyDecisionAuto, PolicyVersion: 1},
		},
		{
			name: "manual-starting-resumable",
			confirmation: &contract.ImportConfirmation{State: "STARTING",
				PolicyDecision: contract.ImportPolicyDecisionManual,
				Assignees:      map[string][]string{"ROLE_IMPORT_APPROVER": {"approver-1"}}},
		},
		{
			name: "manual-pending-rejected",
			confirmation: &contract.ImportConfirmation{State: "PENDING",
				PolicyDecision: contract.ImportPolicyDecisionManual},
			wantConflict: true,
		},
		{
			name: "manual-approved-rejected",
			confirmation: &contract.ImportConfirmation{State: "APPROVED",
				PolicyDecision: contract.ImportPolicyDecisionManual,
				ApprovedRoles:  []string{"ROLE_IMPORT_APPROVER"}},
			wantConflict: true,
		},
		{
			name:         "rejected-rejected",
			confirmation: &contract.ImportConfirmation{State: "REJECTED"},
			wantConflict: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			batch := confirmationSubmitBatch(t, contract.ImportArchiveOnly)
			batch.Confirmation = testCase.confirmation
			batch.OperationID = "history-batch"
			batch.CommittedRevision = batch.Revision
			batch.Version = 1
			repo := &confirmationFreezeRepository{batch: batch}
			service := &Service{Repo: repo, ContractImportConfirmationRoles: []string{"ROLE_IMPORT_APPROVER"}}
			if testCase.name == "auto-approved-resumable" {
				repo.batch.Items[0].Item.Metadata.CRMCustomerID = 0
				_ = service.ConfigureCRMProvision(true, completeProvisionClients())
			}

			err := service.ControlImport(ctx, provisionImportActor(), "batch", 1, true, session)
			if testCase.wantConflict {
				if !errors.Is(err, ErrImportConflict) {
					t.Fatalf("error=%v, want ErrImportConflict", err)
				}
				if repo.batch.State != batch.State {
					t.Fatalf("被拒绝的恢复仍改写了批次: %s", repo.batch.State)
				}
				return
			}
			if err != nil {
				t.Fatalf("resume error: %v", err)
			}
			if repo.batch.State != "QUEUED" {
				t.Fatalf("state=%s, want QUEUED", repo.batch.State)
			}
		})
	}
}

// TestHistoricalAutoRejectsIneligibleBatches 验证开关开启仍须整批满足历史开立条件。
// 参数：t为测试上下文。
// 返回值：接续履约、已有引用或混合批次跳过人工时失败。
func TestHistoricalAutoRejectsIneligibleBatches(t *testing.T) {
	for _, name := range []string{"continue-live", "existing-customer", "mixed"} {
		t.Run(name, func(t *testing.T) {
			b := confirmationSubmitBatch(t, contract.ImportArchiveOnly)
			b.Items[0].Item.Metadata.CRMCustomerID = 0
			switch name {
			case "continue-live":
				b.Items[0].Item.Mode = contract.ImportContinueLive
				b.Items[0].Item.Metadata.CRMCustomerID = 42
				b.Items[0].Item.HistoryEvidence.HistoricalStatus = "active"
				b.Items[0].Item.Policy.ResponsibilityMode = "ACTIVATE_ON_RELEASE"
				b.Items[0].Item.Policy.ProjectMode = "CREATE_NEW"
				b.Items[0].Item.Policy.FinanceMode = "FULL_REPLAY"
				b.Items[0].Item.HistoryEvidence.Relationship = "PRIMARY"
				facts := []contract.ImportFact{}
				b.Items[0].Item.HistoryEvidence.ExpectedFacts = &facts
				b.Items[0].Item.Metadata.PaymentTerms = []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2024-03-05", Amount: "1234.56"}}
			case "existing-customer":
				b.Items[0].Item.Metadata.CRMCustomerID = 42
			case "mixed":
				other := b.Items[0]
				other.ID = "item-other"
				other.Item.Metadata.CRMCustomerID = 42
				b.Items = append(b.Items, other)
			}
			repo := &confirmationFreezeRepository{batch: b, setting: contract.ImportAutoApproveSetting{Enabled: true, Version: 1}}
			service := confirmationSubmitService(repo, nil)
			clients := completeProvisionClients()
			_ = service.ConfigureCRMProvision(true, clients)
			_, err := service.SubmitImportConfirmation(context.Background(), provisionImportActor(), "batch", 1, "preview-token", "submit-ineligible", confirmationSubmitSession())
			if !errors.Is(err, ErrImportUnsupported) || repo.batch.Confirmation != nil {
				t.Fatalf("非自动资格批次跳过人工: err=%v confirmation=%+v", err, repo.batch.Confirmation)
			}
			if err := contract.ValidateHistoryEvidence(b.Items[0].Item); err != nil {
				t.Fatalf("非法测试清单: %v", err)
			}
			temporal := temporalmocks.NewClient(t)
			run := temporalmocks.NewWorkflowRun(t)
			temporal.On("ExecuteWorkflow", mock.Anything, mock.Anything, workflows.ImportConfirmationWorkflowName, mock.Anything).Return(run, nil)
			service.Temporal = temporal
			accepted, submitErr := service.SubmitImportConfirmation(context.Background(), provisionImportActor(), "batch", 1, "preview-token", "submit-manual-valid", confirmationSubmitSession())
			if submitErr != nil || accepted.Confirmation == nil || accepted.Confirmation.PolicyDecision != contract.ImportPolicyDecisionManual || accepted.Confirmation.State != "STARTING" {
				t.Fatalf("非自动资格未冻结人工: b=%+v err=%v", accepted, submitErr)
			}
			b.Confirmation = &contract.ImportConfirmation{State: "APPROVED", PolicyDecision: contract.ImportPolicyDecisionAuto}
			if _, err = service.importAutoApprovePolicy(context.Background(), b); !errors.Is(err, ErrImportConflict) {
				t.Fatalf("旧AUTO恢复未被阻断: %v", err)
			}
		})
	}
}
