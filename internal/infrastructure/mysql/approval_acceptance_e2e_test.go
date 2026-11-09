package mysql

import (
	"context"
	"encoding/json"
	"errors"
	core "github.com/J-S-Te/license-core"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/client"
	temporalmocks "go.temporal.io/sdk/mocks"
	"go.temporal.io/sdk/testsuite"
	"gorm.io/gorm"
)

type acceptancePersonnel struct{}

// ListEligibleUsers 返回本用例明确配置的有效管理员。
// 参数：ctx为目录查询上下文，actor为申请人，roles为受派角色。
// 返回值：单个管理员候选及空错误，不访问真实人员目录。
func (acceptancePersonnel) ListEligibleUsers(context.Context, application.Principal, []string) ([]application.UserReference, error) {
	return []application.UserReference{{UserID: "admin-1", Roles: []string{"admin"}}}, nil
}

type acceptanceRun struct {
	client.WorkflowRun
	id, run string
}

// GetID 返回已受理的真实启动参数所绑定的工作流身份。
// 参数：无。
// 返回值：测试启动调用收到的工作流ID。
func (r acceptanceRun) GetID() string { return r.id }

// GetRunID 返回本次模拟启动的执行身份。
// 参数：无。
// 返回值：绑定本次审批的执行ID。
func (r acceptanceRun) GetRunID() string { return r.run }

// acceptanceActor 构造仅对本测试合同负责的已授权销售。
// 参数：tenant为隔离租户，item为受测合同。
// 返回值：具有合法读取/编辑范围的业务身份。
func acceptanceActor(tenant string, item contract.Contract) application.Principal {
	return application.Principal{TenantID: tenant, UserID: item.OwnerUserID, IdentityID: item.OwnerIdentityID, Roles: []string{"sales"}, Permissions: map[string]bool{"contract.read": true, "contract.edit": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.read": {AllowAll: true}, "contract.edit": {AllowAll: true}}}
}

// acceptanceService 装配真实仓储并保留可观察的Temporal启动边界。
// 参数：t为测试上下文，repo为隔离MySQL仓储，startErr为启动故障。
// 返回值：真实应用服务、启动计数及实际启动输入。
func acceptanceService(t *testing.T, repo *Repository, startErr error) (*application.Service, *atomic.Int32, *workflows.StatusChangeInput) {
	t.Helper()
	calls := &atomic.Int32{}
	input := &workflows.StatusChangeInput{}
	var inputMu sync.Mutex
	temporal := temporalmocks.NewClient(t)
	temporal.On("ExecuteWorkflow", mock.Anything, mock.Anything, workflows.StatusChangeWorkflowName, mock.Anything).Return(func(_ context.Context, options client.StartWorkflowOptions, _ any, args ...any) (client.WorkflowRun, error) {
		calls.Add(1)
		current := args[0].(workflows.StatusChangeInput)
		inputMu.Lock()
		*input = current
		inputMu.Unlock()
		if startErr != nil {
			return nil, startErr
		}
		return acceptanceRun{id: options.ID, run: "run-" + current.ApprovalID}, nil
	}).Maybe()
	return &application.Service{Repo: repo, Personnel: acceptancePersonnel{}, Temporal: temporal}, calls, input
}

// TestStatusChangeAcceptancePersistsBeforeSuccessOnMySQL 验证受理回执对应可读取审批且重复同步冲突。
// 参数：t为测试上下文，仅连接CONTRACT_TEST_DSN隔离库，不运行真实Worker。
// 返回值：无；返回不存在的审批身份或让在途重复启动时失败。
func TestStatusChangeAcceptancePersistsBeforeSuccessOnMySQL(t *testing.T) {
	repo, tenant := draftE2ERepository(t)
	item := stampedE2EContract(t, repo, tenant, contract.StatusInProgress)
	service, calls, _ := acceptanceService(t, repo, nil)
	actor := acceptanceActor(tenant, item)
	first, err := service.ChangeStatus(context.Background(), actor, item.ID, item.Version, contract.StatusPendingPay, "申请收款")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := repo.GetApprovalMeta(context.Background(), tenant, first.ApprovalID)
	if err != nil || meta.WorkflowID != first.WorkflowID || meta.RunID != first.RunID {
		t.Fatalf("受理回执审批未持久化: result=%+v meta=%+v err=%v", first, meta, err)
	}
	_, err = service.ChangeStatus(context.Background(), actor, item.ID, item.Version, contract.StatusPendingPay, "再次申请")
	if !errors.Is(err, apperrors.ErrStateConflict) || calls.Load() != 1 {
		t.Fatalf("在途重复未同步拒绝: err=%v starts=%d", err, calls.Load())
	}
}

// TestStatusChangeConcurrentAcceptanceOnlyStartsOneOnMySQL 验证并发受理不靠先查防重。
// 参数：t为测试上下文，同一隔离合同行同时发起八个申请。
// 返回值：无；超过一个受理或返回任何不可读取审批时失败。
func TestStatusChangeConcurrentAcceptanceOnlyStartsOneOnMySQL(t *testing.T) {
	repo, tenant := draftE2ERepository(t)
	item := stampedE2EContract(t, repo, tenant, contract.StatusInProgress)
	service, calls, _ := acceptanceService(t, repo, nil)
	actor := acceptanceActor(tenant, item)
	gate := make(chan struct{})
	var wg sync.WaitGroup
	var accepted atomic.Int32
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			result, err := service.ChangeStatus(context.Background(), actor, item.ID, item.Version, contract.StatusPendingPay, "申请收款")
			if err == nil {
				accepted.Add(1)
				_, err = repo.GetApprovalMeta(context.Background(), tenant, result.ApprovalID)
			} else if errors.Is(err, apperrors.ErrStateConflict) {
				err = nil
			}
			errs <- err
		}()
	}
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("受理失败或返回不可读取身份: %v", err)
		}
	}
	if accepted.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("并发启动多轮: accepted=%d starts=%d", accepted.Load(), calls.Load())
	}
}

// TestStatusChangeStartFailureLeavesNoReservationOnMySQL 验证启动失败回滚同步预占。
// 参数：t为测试上下文，注入启动RPC故障后使用同版本重试。
// 返回值：无；启动失败留下在途孤儿或阻塞重新申请时失败。
func TestStatusChangeStartFailureLeavesNoReservationOnMySQL(t *testing.T) {
	repo, tenant := draftE2ERepository(t)
	item := stampedE2EContract(t, repo, tenant, contract.StatusInProgress)
	failed, _, input := acceptanceService(t, repo, errors.New("temporal start unavailable"))
	actor := acceptanceActor(tenant, item)
	if _, err := failed.ChangeStatus(context.Background(), actor, item.ID, item.Version, contract.StatusPendingPay, "申请收款"); err == nil {
		t.Fatal("启动失败未透传")
	}
	var count int64
	if err := repo.db.Model(&approvalInstanceRecord{}).Where("tenant_id=? AND contract_id=?", tenant, item.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("启动失败遗留审批: count=%d", count)
	}
	assertAcceptanceRows(t, repo, tenant, item.ID, 0)
	if input.ApprovalID == "" {
		t.Fatal("未到达真实启动边界")
	}
	service, _, _ := acceptanceService(t, repo, nil)
	retry, err := service.ChangeStatus(context.Background(), actor, item.ID, item.Version, contract.StatusPendingPay, "申请收款")
	if err != nil {
		t.Fatalf("失败后不能新申请: %v", err)
	}
	if _, err = repo.GetApprovalMeta(context.Background(), tenant, retry.ApprovalID); err != nil {
		t.Fatalf("重试受理未持久化: %v", err)
	}
}

// TestAcceptedStatusChangeRoundAfterRejectionOnMySQL 验证持久化受理仍允许同版本新轮次。
// 参数：t为测试上下文，经真实CompleteApproval关闭第一轮后重申请。
// 返回值：无；第二轮被旧工作流身份阻塞或在途重复未拒时失败。
func TestAcceptedStatusChangeRoundAfterRejectionOnMySQL(t *testing.T) {
	repo, tenant := draftE2ERepository(t)
	item := stampedE2EContract(t, repo, tenant, contract.StatusInProgress)
	service, calls, _ := acceptanceService(t, repo, nil)
	actor := acceptanceActor(tenant, item)
	first, err := service.ChangeStatus(context.Background(), actor, item.ID, item.Version, contract.StatusPendingPay, "首轮申请")
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.CompleteApproval(context.Background(), workflows.CompleteApprovalActivityInput{ApprovalID: first.ApprovalID, TenantID: tenant, ContractID: item.ID, Status: approval.StatusRejected}); err != nil {
		t.Fatal(err)
	}
	second, err := service.ChangeStatus(context.Background(), actor, item.ID, item.Version, contract.StatusPendingPay, "修改原因重新申请")
	if err != nil || second.ApprovalID == first.ApprovalID || second.WorkflowID == first.WorkflowID || calls.Load() != 2 {
		t.Fatalf("新轮次受理错误: first=%+v second=%+v err=%v starts=%d", first, second, err, calls.Load())
	}
}

// TestRolledBackAcceptanceCannotBeRebuiltByWorkerOnMySQL 验证已启动但本地未受理不能由Worker补建。
// 参数：t为测试上下文，以受理缺失的RequireExisting活动模拟启动后本地回滚。
// 返回值：无；Worker补建了未受理审批或其他副作用时失败。
func TestRolledBackAcceptanceCannotBeRebuiltByWorkerOnMySQL(t *testing.T) {
	repo, tenant := draftE2ERepository(t)
	item := stampedE2EContract(t, repo, tenant, contract.StatusInProgress)
	input := recoveryFlowStart(tenant, item, approval.KindStatusChange)
	if err := json.Unmarshal([]byte(`{"require_existing":true}`), &input); err != nil {
		t.Fatal(err)
	}
	if err := repo.StartApproval(context.Background(), input); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("未受理活动竟自行建单: err=%v", err)
	}
	assertAcceptanceRows(t, repo, tenant, item.ID, 0)
}

// assertAcceptanceRows 核对审批受理事务的全部核心副作用数量。
// 参数：t为测试上下文，repo为隔离仓储，tenant/contractID定位受测数据，want为预期数量。
// 返回值：无；审批、任务或快照有残留时失败。
func assertAcceptanceRows(t *testing.T, repo *Repository, tenant, contractID string, want int64) {
	t.Helper()
	checks := []struct {
		query string
		args  []any
	}{
		{"SELECT COUNT(*) FROM con_approval_instance WHERE tenant_id=? AND contract_id=?", []any{tenant, contractID}},
		{"SELECT COUNT(*) FROM con_approval_snapshot WHERE tenant_id=? AND contract_id=?", []any{tenant, contractID}},
		{"SELECT COUNT(*) FROM con_approval_task WHERE approval_id IN (SELECT id FROM con_approval_instance WHERE tenant_id=? AND contract_id=?)", []any{tenant, contractID}},
	}
	for _, check := range checks {
		var count int64
		if err := repo.db.Raw(check.query, check.args...).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Fatalf("受理副作用数量=%d want=%d query=%s", count, want, check.query)
		}
	}
}

// TestAcceptedWorkflowCannotRebuildAfterLocalRollbackOnMySQL 验证远端已接受而本地写入失败的围栏。
// 参数：t为测试上下文，在真实RunID落库位置注入错误并用Temporal测试环境继续已启动输入。
// 返回值：无；源事务残留或新Worker绕过受理事实补建时失败。
func TestAcceptedWorkflowCannotRebuildAfterLocalRollbackOnMySQL(t *testing.T) {
	repo, tenant := draftE2ERepository(t)
	item := stampedE2EContract(t, repo, tenant, contract.StatusInProgress)
	service, calls, input := acceptanceService(t, repo, nil)
	localErr := errors.New("local run binding failed")
	var faultEnabled atomic.Bool
	faultEnabled.Store(true)
	callback := "qa_status_acceptance_rollback"
	if err := repo.db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if faultEnabled.Load() && tx.Statement.Table == "con_approval_instance" {
			tx.AddError(localErr)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.db.Callback().Update().Remove(callback) })
	_, err := service.ChangeStatus(context.Background(), acceptanceActor(tenant, item), item.ID, item.Version, contract.StatusPendingPay, "申请收款")
	if !errors.Is(err, localErr) || calls.Load() != 1 || !input.RequireExisting {
		t.Fatalf("未命中已接受后本地失败边界: err=%v calls=%d input=%+v", err, calls.Load(), input)
	}
	assertAcceptanceRows(t, repo, tenant, item.ID, 0)
	faultEnabled.Store(false)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&workflows.Activities{Store: repo})
	env.ExecuteWorkflow(workflows.StatusChangeWorkflow, *input)
	if env.GetWorkflowError() == nil {
		t.Fatal("本地回滚后工作流未拒绝未受理输入")
	}
	assertAcceptanceRows(t, repo, tenant, item.ID, 0)
}

// TestAcceptedInitializationBindsRunIDIdempotentlyOnMySQL 验证初始化只补绑定而不重复建任务快照。
// 参数：t为测试上下文，真实预占行先无RunID，再由可信Worker填入执行身份。
// 返回值：无；重复初始化产生副作用或另一个RunID覆盖已绑定身份时失败。
func TestAcceptedInitializationBindsRunIDIdempotentlyOnMySQL(t *testing.T) {
	repo, tenant := draftE2ERepository(t)
	item := stampedE2EContract(t, repo, tenant, contract.StatusInProgress)
	in := recoveryFlowStart(tenant, item, approval.KindStatusChange)
	in.RunID = ""
	if err := repo.StartApproval(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	in.RequireExisting = true
	in.RunID = "real-worker-run"
	for i := 0; i < 2; i++ {
		if err := repo.StartApproval(context.Background(), in); err != nil {
			t.Fatalf("初始化或重放失败: %v", err)
		}
	}
	meta, err := repo.GetApprovalMeta(context.Background(), tenant, in.ApprovalID)
	if err != nil || meta.RunID != "real-worker-run" {
		t.Fatalf("执行身份未绑定: meta=%+v err=%v", meta, err)
	}
	assertAcceptanceRows(t, repo, tenant, item.ID, 1)
	in.RunID = "another-worker-run"
	if err := repo.StartApproval(context.Background(), in); !errors.Is(err, apperrors.ErrStateConflict) {
		t.Fatalf("其他执行覆盖身份: %v", err)
	}
}

type acceptanceLicenseGate struct{ expired *atomic.Bool }

// Check 在受理写入后模拟商业许可失效。
// 参数：ctx为检查上下文，op为业务写操作，expired由测试事务写快照时触发。
// 返回值：失效后返回拒绝；失效前允许，不修改真实许可实现。
func (g acceptanceLicenseGate) Check(context.Context, core.Operation) error {
	if g.expired.Load() {
		return core.ErrDenied
	}
	return nil
}

// TestStatusChangeLicenseExpiryInsideAcceptanceRollsBackOnMySQL 验证许可检查紧邻事务内启动RPC。
// 参数：t为测试上下文，在审批快照已经写入、工作流尚未启动时模拟许可失效。
// 返回值：无；失效仍启动工作流或留下受理副作用时失败。
func TestStatusChangeLicenseExpiryInsideAcceptanceRollsBackOnMySQL(t *testing.T) {
	repo, tenant := draftE2ERepository(t)
	item := stampedE2EContract(t, repo, tenant, contract.StatusInProgress)
	service, calls, _ := acceptanceService(t, repo, nil)
	var expired atomic.Bool
	service.LicenseGate = acceptanceLicenseGate{expired: &expired}
	callback := "qa_status_acceptance_license"
	if err := repo.db.Callback().Create().After("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "con_approval_snapshot" {
			expired.Store(true)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.db.Callback().Create().Remove(callback) })
	_, err := service.ChangeStatus(context.Background(), acceptanceActor(tenant, item), item.ID, item.Version, contract.StatusPendingPay, "申请收款")
	if !expired.Load() || !errors.Is(err, application.ErrCommercialLicense) || calls.Load() != 0 {
		t.Fatalf("事务内许可失效未阻止启动: expired=%v err=%v starts=%d", expired.Load(), err, calls.Load())
	}
	assertAcceptanceRows(t, repo, tenant, item.ID, 0)
}
