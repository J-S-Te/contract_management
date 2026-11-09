package application

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/testfixture"
)

// autoApproveImportRepository 记录自动确认调用参数的测试仓储。
type autoApproveImportRepository struct {
	recordingRepository
	calls    int
	tenantID string
	batchID  string
	itemID   string
	roles    []string
	policy   ImportAutoApprovePolicy
	err      error
}

// AutoConfirmHistoricalImportItem 记录调用参数并返回预置结果。
// 参数：ctx 为上下文，tenantID/batchID/itemID 定位批次，roles 为确认角色，policy 为策略快照。
// 返回值：预置错误；默认成功。
func (r *autoApproveImportRepository) AutoConfirmHistoricalImportItem(_ context.Context, tenantID, batchID, itemID string, roles []string, policy ImportAutoApprovePolicy) error {
	r.calls++
	r.tenantID, r.batchID, r.itemID, r.roles, r.policy = tenantID, batchID, itemID, roles, policy
	return r.err
}

// TestAutoConfirmHistoricalImportItemPassesBatchIdentityAndRoles 验证服务层只做转发：
// 租户/批次取自已认领批次（不接受调用方传入），单项取目标行，角色取配置的会签模板。
// 参数：t 为测试上下文。
// 返回值：传参不符或错误未被原样返回时失败。
func TestAutoConfirmHistoricalImportItemPassesBatchIdentityAndRoles(t *testing.T) {
	repo := &autoApproveImportRepository{}
	service := &Service{Repo: repo, ContractImportConfirmationRoles: []string{"admin", "finance_director"}}
	b := contract.ImportBatch{ID: "batch-1", TenantID: "tenant-1", Items: []contract.ImportItemState{{ID: "item-1", Item: provisionHistoryItem("one", "old-1", "source-one", "客户", "销售", 10000)}}}
	_ = service.ConfigureCRMProvision(true, completeProvisionClients())

	policy := ImportAutoApprovePolicy{Auto: true, Version: 7}
	if err := service.autoConfirmHistoricalImportItem(context.Background(), b, "item-1", policy); err != nil {
		t.Fatal(err)
	}
	if repo.calls != 1 || repo.tenantID != "tenant-1" || repo.batchID != "batch-1" || repo.itemID != "item-1" {
		t.Fatalf("calls=%d tenant=%q batch=%q item=%q", repo.calls, repo.tenantID, repo.batchID, repo.itemID)
	}
	if !slices.Equal(repo.roles, []string{"admin", "finance_director"}) {
		t.Fatalf("roles=%v", repo.roles)
	}
	// 策略快照必须原样透传：仓储据此决定是否允许旧数据升级、写入哪个设置版本。
	if repo.policy != policy {
		t.Fatalf("policy=%+v, want %+v", repo.policy, policy)
	}

	// 仓储错误必须原样上抛：调度器据此写逐项 Issue，不得吞错变成"已完成"。
	repo.err = ErrImportConflict
	if err := service.autoConfirmHistoricalImportItem(context.Background(), b, "item-1", policy); !errors.Is(err, ErrImportConflict) {
		t.Fatalf("error=%v, want ErrImportConflict", err)
	}
}

// TestAutoConfirmHistoricalImportItemRequiresStoreCapability 验证仓储未实现自动确认能力时
// 返回 IMPORT_POLICY_UNSUPPORTED，而不是静默跳过导致"开立完成但永不归档"。
// 参数：t 为测试上下文。
// 返回值：错误码不符时失败。
func TestAutoConfirmHistoricalImportItemRequiresStoreCapability(t *testing.T) {
	service := &Service{Repo: &recordingRepository{}}
	err := service.autoConfirmHistoricalImportItem(context.Background(), contract.ImportBatch{ID: "batch-1", TenantID: "tenant-1"}, "item-1", ImportAutoApprovePolicy{})
	if !errors.Is(err, ErrImportUnsupported) {
		t.Fatalf("error=%v, want ErrImportUnsupported", err)
	}
}

// provisionAutoApprove 模拟真实仓储的自动确认：把目标项置为已确认并落下
// 与人工审批同形状的 APPROVED 确认台账（真实实现另写合同终态与审计）。
// 参数：r 为测试仓储，batchID/itemID 定位单项，roles 为确认角色。
// 返回值：批次或单项不存在时返回冲突。
func (r *provisionImportRepository) provisionAutoApprove(batchID, itemID string, roles []string) error {
	if r.batch.ID != batchID {
		return ErrImportConflict
	}
	if r.batch.Confirmation == nil {
		now := time.Now().UTC()
		r.batch.Confirmation = &contract.ImportConfirmation{
			WorkflowID: "contract-import-auto-" + batchID, Revision: r.batch.Revision, State: "APPROVED",
			Roles: roles, ApprovedRoles: append([]string(nil), roles...),
			Assignees: map[string][]string{}, Commands: []contract.ImportConfirmationCommand{},
			SubmittedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	}
	for i := range r.batch.Items {
		if r.batch.Items[i].ID == itemID {
			r.batch.Items[i].Confirmed = true
			return nil
		}
	}
	return ErrImportConflict
}

// AutoConfirmHistoricalImportItem 让测试仓储满足 ContractImportAutoApproveStore。
// 参数：ctx 为上下文，tenantID/batchID 定位批次，itemID 为单项，roles 为确认角色，
// policy 为策略快照（仅 Auto 为真时才允许合成/确认，模拟真实仓储的冲突语义）。
// 返回值：租户或批次不匹配返回冲突。
func (r *provisionImportRepository) AutoConfirmHistoricalImportItem(_ context.Context, tenantID, batchID, itemID string, roles []string, policy ImportAutoApprovePolicy) error {
	if tenantID != r.batch.TenantID {
		return ErrImportConflict
	}
	r.autoApproveCalls = append(r.autoApproveCalls, policy)
	if !policy.Auto {
		// 真实仓储在这里会返回 ErrImportConflict：非 AUTO 策略不得自动确认。
		return ErrImportConflict
	}
	return r.provisionAutoApprove(batchID, itemID, roles)
}

// GetImportAutoApproveSetting 让测试仓储满足 ContractImportAutoApproveSettingStore，
// 返回测试预置的租户级运行开关（零值即「未开启」）。
// 参数：ctx 为上下文，tenantID 为租户。
// 返回值：预置设置。
func (r *provisionImportRepository) GetImportAutoApproveSetting(_ context.Context, tenantID string) (contract.ImportAutoApproveSetting, error) {
	setting := r.autoApproveSetting
	setting.TenantID = tenantID
	return setting, nil
}

// SetImportAutoApproveSetting 以测试预置值推进版本，模拟真实仓储的乐观写入。
// 参数：ctx 为上下文，tenantID 为租户，enabled 为目标状态，expectedVersion 为客户端版本，
// actorID 为操作者。
// 返回值：写入后的设置；版本不符返回冲突。
func (r *provisionImportRepository) SetImportAutoApproveSetting(_ context.Context, tenantID string, enabled bool, expectedVersion uint64, actorID string) (contract.ImportAutoApproveSetting, error) {
	if expectedVersion != r.autoApproveSetting.Version {
		return contract.ImportAutoApproveSetting{}, ErrImportConflict
	}
	r.autoApproveSetting = contract.ImportAutoApproveSetting{
		TenantID: tenantID, Enabled: enabled, Version: expectedVersion + 1,
		UpdatedBy: actorID, UpdatedAt: time.Now().UTC(),
	}
	return r.autoApproveSetting, nil
}

// TestImportProvisionExecutionAutoApprovesOnlyWhenEnabled 验证双层开关型行为：
// env 能力关闭时即使租户运行开关开启也不触发（逐字节保持既有行为）；
// env 开启但租户开关关闭时仍等待人工会签；两者都开启才在开立成功后自动确认，
// 并按声明状态收敛到 TARGETS_PENDING（等目标投递）。
// 参数：t 为测试上下文。
// 返回值：批次终态、确认台账或单项确认标记不符时失败。
func TestImportProvisionExecutionAutoApprovesOnlyWhenEnabled(t *testing.T) {
	for _, testCase := range []struct {
		name             string
		capability       bool
		tenantEnabled    bool
		wantState        string
		wantConfirmation bool
	}{
		{name: "capability-off-ignores-tenant-setting", capability: false, tenantEnabled: true, wantState: "COMPLETED", wantConfirmation: false},
		{name: "tenant-switch-off-keeps-manual-confirmation", capability: true, tenantEnabled: false, wantState: "COMPLETED", wantConfirmation: false},
		{name: "both-enabled-auto-approves", capability: true, tenantEnabled: true, wantState: "TARGETS_PENDING", wantConfirmation: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			pdf := testfixture.PDF("historical contract")
			actor := provisionImportActor()
			_, customer, opportunity, owners := newProvisionStubs()
			item := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
			repo := newProvisionImportRepository(provisionImportBatch([]contract.ImportItem{item}, provisionTestSource("one", pdf)))
			repo.autoApproveSetting = contract.ImportAutoApproveSetting{Enabled: testCase.tenantEnabled, Version: 1}
			service := provisionTestService(repo, pdf, customer, opportunity, owners)
			service.ContractImportAutoApproveHistorical = testCase.capability
			previewProvisionBatch(t, service, repo, actor)

			dispatchUntilSettled(t, service)

			if repo.batch.Items[0].State != "COMPLETE" {
				t.Fatalf("item=%+v", repo.batch.Items[0])
			}
			if repo.batch.State != testCase.wantState {
				t.Fatalf("batch state=%s, want %s", repo.batch.State, testCase.wantState)
			}
			if got := repo.batch.Confirmation != nil; got != testCase.wantConfirmation {
				t.Fatalf("confirmation=%+v, want present=%t", repo.batch.Confirmation, testCase.wantConfirmation)
			}
			if got := repo.batch.Items[0].Confirmed; got != testCase.wantConfirmation {
				t.Fatalf("item confirmed=%t, want %t", got, testCase.wantConfirmation)
			}
			if testCase.wantConfirmation {
				confirmation := repo.batch.Confirmation
				if confirmation.State != "APPROVED" || confirmation.Revision != repo.batch.Revision {
					t.Fatalf("confirmation=%+v", confirmation)
				}
				// 自动审批不改变合同本身：归档终态仍由历史确认写入，编排只负责触发。
				if repo.created.Status != contract.StatusDraft {
					t.Fatalf("provision wrote contract status=%s", repo.created.Status)
				}
			}
		})
	}
}

// flakyAutoApproveRepository 让前 N 次自动确认失败，用于覆盖「开立已完成但确认中断」
// 的恢复路径；其余调用原样委托给真实测试仓储。
type flakyAutoApproveRepository struct {
	*provisionImportRepository
	failures int
	calls    int
}

// AutoConfirmHistoricalImportItem 按剩余失败次数注入冲突。
// 参数：ctx/tenantID/batchID/itemID/roles/policy 与真实仓储一致。
// 返回值：注入期内返回围栏冲突，之后委托真实实现。
func (r *flakyAutoApproveRepository) AutoConfirmHistoricalImportItem(ctx context.Context, tenantID, batchID, itemID string, roles []string, policy ImportAutoApprovePolicy) error {
	r.calls++
	if r.failures > 0 {
		r.failures--
		return ErrImportConflict
	}
	return r.provisionImportRepository.AutoConfirmHistoricalImportItem(ctx, tenantID, batchID, itemID, roles, policy)
}

// TestImportProvisionAutoApproveRetriesAfterInterruption 验证自动审批只在确认真正落地后
// 才收口单项：第一次确认失败时该行保持 BLOCKED 且不留半套确认台账；操作者恢复后重排
// 必须重放确认（而不是只把台账项标记 COMPLETE），最终收敛到 TARGETS_PENDING。
// 参数：t 为测试上下文。
// 返回值：失败尝试被当成完成、或恢复后未补齐确认时失败。
func TestImportProvisionAutoApproveRetriesAfterInterruption(t *testing.T) {
	pdf := testfixture.PDF("historical contract")
	actor := provisionImportActor()
	_, customer, opportunity, owners := newProvisionStubs()
	item := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
	repo := newProvisionImportRepository(provisionImportBatch([]contract.ImportItem{item}, provisionTestSource("one", pdf)))
	service := provisionTestService(repo, pdf, customer, opportunity, owners)
	service.ContractImportAutoApproveHistorical = true
	repo.autoApproveSetting = contract.ImportAutoApproveSetting{Enabled: true, Version: 1}
	previewProvisionBatch(t, service, repo, actor)
	flaky := &flakyAutoApproveRepository{provisionImportRepository: repo, failures: 1}
	service.Repo = flaky

	dispatchUntilSettled(t, service)

	if repo.batch.State != "PARTIAL" || repo.batch.Items[0].State != "BLOCKED" {
		t.Fatalf("首次失败后 batch state=%s item=%+v", repo.batch.State, repo.batch.Items[0])
	}
	if repo.batch.Confirmation != nil || repo.batch.Items[0].Confirmed {
		t.Fatalf("失败尝试留下了确认写入: confirmation=%+v confirmed=%t",
			repo.batch.Confirmation, repo.batch.Items[0].Confirmed)
	}

	// 模拟操作者恢复（resume）：批次重新排队后调度器会领到同一行。
	repo.batch.State = "QUEUED"
	dispatchUntilSettled(t, service)

	if flaky.calls < 2 {
		t.Fatalf("恢复后没有重放自动确认: calls=%d", flaky.calls)
	}
	if repo.batch.Confirmation == nil || repo.batch.Confirmation.State != "APPROVED" {
		t.Fatalf("恢复后确认未落地: %+v", repo.batch.Confirmation)
	}
	if !repo.batch.Items[0].Confirmed || repo.batch.Items[0].State != "COMPLETE" {
		t.Fatalf("恢复后单项未收口: %+v", repo.batch.Items[0])
	}
	if repo.batch.State != "TARGETS_PENDING" {
		t.Fatalf("恢复后 batch state=%s, want TARGETS_PENDING", repo.batch.State)
	}
}
