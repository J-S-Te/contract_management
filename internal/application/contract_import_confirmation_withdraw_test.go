package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// withdrawAutoBatch 构造一份等待撤回判定的批次。
//
// 初始版本固定为 3、执行代次固定为 4，便于断言「撤回是否升代」；单项是否已确认、
// 确认有效性、批次状态与策略判定由调用方按用例注入。
//
// 参数：state 为批次状态，confirmed 为单项是否已确认，expiresAt 为确认有效期。
// 返回值：条件字段已填充的批次；租户与创建人取自 provisionImportActor。
func withdrawAutoBatch(state string, confirmed bool, expiresAt time.Time) contract.ImportBatch {
	return contract.ImportBatch{
		ID: "batch", TenantID: provisionTestTenant, ActorID: "actor",
		Version: 3, Revision: 1, State: state, Epoch: 4,
		Items: []contract.ImportItemState{{ID: "item", State: "CREATED", Confirmed: confirmed}},
		Confirmation: &contract.ImportConfirmation{
			WorkflowID: "contract-import-auto-batch", Revision: 1, Digest: "manifest-digest",
			State: "APPROVED", Roles: []string{"ROLE_IMPORT_APPROVER"},
			ApprovedRoles: []string{"ROLE_IMPORT_APPROVER"}, Assignees: map[string][]string{},
			Commands: []contract.ImportConfirmationCommand{}, SubmittedAt: time.Now().UTC(),
			ExpiresAt:      expiresAt,
			PolicyDecision: contract.ImportPolicyDecisionAuto, PolicyVersion: 1,
		},
	}
}

// TestActImportConfirmationWithdrawsAutoApprovedBatch 验证 AUTO 确认的撤回逃生口：
// AUTO 在冻结时就是 APPROVED 且没有工作流，提交错了只能由本事务直接落
// WITHDRAWN + PAUSED + 升代，否则错误提交永久无法撤回。
// 参数：t 为测试上下文。
// 返回值：状态迁移、命令留痕或护栏不符时失败。
func TestActImportConfirmationWithdrawsAutoApprovedBatch(t *testing.T) {
	ctx := context.Background()
	actor := provisionImportActor()

	t.Run("auto-approved-zero-confirmed-withdraws", func(t *testing.T) {
		repo := &confirmationFreezeRepository{batch: withdrawAutoBatch("RUNNING", false, time.Now().Add(time.Hour))}
		if e := (&Service{Repo: repo}).ActImportConfirmation(ctx, actor, "batch", 3, "withdraw-auto-key", "WITHDRAW"); e != nil {
			t.Fatalf("AUTO 撤回被拒绝: %v", e)
		}
		after := repo.batch
		if after.Confirmation == nil || after.Confirmation.State != "WITHDRAWN" {
			t.Fatalf("确认状态=%+v, want WITHDRAWN", after.Confirmation)
		}
		if after.State != "PAUSED" || after.Epoch != 5 || !after.LeaseUntil.IsZero() {
			t.Fatalf("批次 state=%s epoch=%d lease=%v, want PAUSED/5/zero", after.State, after.Epoch, after.LeaseUntil)
		}
		commands := after.Confirmation.Commands
		if len(commands) != 1 {
			t.Fatalf("撤回命令=%+v, want 1 条留痕", commands)
		}
		command := commands[0]
		if command.Key != "withdraw-auto-key" || command.ActorID != "actor" || command.Action != "WITHDRAW" || command.At.IsZero() {
			t.Fatalf("撤回命令字段=%+v", command)
		}
		// 同键重放：命令已存在，必须幂等成功且不再追加第二条。
		if e := (&Service{Repo: repo}).ActImportConfirmation(ctx, actor, "batch", after.Version, "withdraw-auto-key", "WITHDRAW"); e != nil {
			t.Fatalf("同键重放失败: %v", e)
		}
		if len(repo.batch.Confirmation.Commands) != 1 || repo.batch.Confirmation.State != "WITHDRAWN" {
			t.Fatalf("重放改写了撤回结果: %+v", repo.batch.Confirmation)
		}
	})

	// AUTO 没有能走到 EXPIRED 的收敛路径，有效期过了也必须仍可撤回，
	// 否则错误提交会在 72 小时后永久锁死。
	t.Run("auto-expired-confirmation-still-withdraws", func(t *testing.T) {
		repo := &confirmationFreezeRepository{batch: withdrawAutoBatch("RUNNING", false, time.Now().Add(-time.Hour))}
		if e := (&Service{Repo: repo}).ActImportConfirmation(ctx, actor, "batch", 3, "withdraw-auto-expired", "WITHDRAW"); e != nil {
			t.Fatalf("AUTO 撤回受确认有效期拦截: %v", e)
		}
		if repo.batch.Confirmation.State != "WITHDRAWN" || repo.batch.State != "PAUSED" || repo.batch.Epoch != 5 {
			t.Fatalf("batch state=%s epoch=%d confirmation=%s", repo.batch.State, repo.batch.Epoch, repo.batch.Confirmation.State)
		}
	})

	t.Run("confirmed-item-conflicts-without-change", func(t *testing.T) {
		repo := &confirmationFreezeRepository{batch: withdrawAutoBatch("RUNNING", true, time.Now().Add(time.Hour))}
		if e := (&Service{Repo: repo}).ActImportConfirmation(ctx, actor, "batch", 3, "withdraw-auto-confirmed", "WITHDRAW"); !errors.Is(e, ErrImportConflict) {
			t.Fatalf("已确认项仍可撤回: %v", e)
		}
		unchanged := repo.batch
		if unchanged.Confirmation.State != "APPROVED" || unchanged.State != "RUNNING" || unchanged.Epoch != 4 ||
			len(unchanged.Confirmation.Commands) != 0 {
			t.Fatalf("冲突路径改写了批次: state=%s epoch=%d confirmation=%+v", unchanged.State, unchanged.Epoch, unchanged.Confirmation)
		}
	})

	t.Run("completed-batch-conflicts", func(t *testing.T) {
		repo := &confirmationFreezeRepository{batch: withdrawAutoBatch("COMPLETED", false, time.Now().Add(time.Hour))}
		if e := (&Service{Repo: repo}).ActImportConfirmation(ctx, actor, "batch", 3, "withdraw-auto-completed", "WITHDRAW"); !errors.Is(e, ErrImportConflict) {
			t.Fatalf("已完成批次仍可撤回: %v", e)
		}
		if repo.batch.Confirmation.State != "APPROVED" || repo.batch.State != "COMPLETED" {
			t.Fatalf("冲突路径改写了批次: %+v", repo.batch)
		}
	})

	t.Run("stale-version-conflicts", func(t *testing.T) {
		repo := &confirmationFreezeRepository{batch: withdrawAutoBatch("RUNNING", false, time.Now().Add(time.Hour))}
		if e := (&Service{Repo: repo}).ActImportConfirmation(ctx, actor, "batch", 2, "withdraw-auto-stale", "WITHDRAW"); !errors.Is(e, ErrImportConflict) {
			t.Fatalf("旧版本仍可撤回: %v", e)
		}
		if repo.batch.Confirmation.State != "APPROVED" || repo.batch.Epoch != 4 {
			t.Fatalf("冲突路径改写了批次: %+v", repo.batch)
		}
	})
}

// TestActImportConfirmationWithdrawGuardsNonCreator 验证 AUTO 撤回仍只授予批次创建人：
// 撤回是「撤销自己的错误提交」，不是审批权限的延伸，因此越权必须是 Forbidden。
// 参数：t 为测试上下文。
// 返回值：非创建人被放行或状态被改写时失败。
func TestActImportConfirmationWithdrawGuardsNonCreator(t *testing.T) {
	ctx := context.Background()
	other := provisionImportActor()
	other.UserID, other.IdentityID = "other", "identity-other"
	repo := &confirmationFreezeRepository{batch: withdrawAutoBatch("RUNNING", false, time.Now().Add(time.Hour))}

	if e := (&Service{Repo: repo}).ActImportConfirmation(ctx, other, "batch", 3, "withdraw-auto-other", "WITHDRAW"); !errors.Is(e, ErrForbidden) {
		t.Fatalf("非创建人撤回错误=%v, want ErrForbidden", e)
	}
	if repo.batch.Confirmation.State != "APPROVED" || repo.batch.State != "RUNNING" ||
		repo.batch.Epoch != 4 || len(repo.batch.Confirmation.Commands) != 0 {
		t.Fatalf("越权路径改写了批次: %+v", repo.batch)
	}
}

// TestActImportConfirmationKeepsManualWithdrawSemantics 验证人工会签的撤回归属不变：
// PENDING/STARTING 只追加命令交工作流收敛（不迁移状态），MANUAL/APPROVED 仍拒绝，
// 也就是 AUTO 分支没有放宽人工确认的既有护栏。
// 参数：t 为测试上下文。
// 返回值：人工路径被 AUTO 分支影响时失败。
func TestActImportConfirmationKeepsManualWithdrawSemantics(t *testing.T) {
	ctx := context.Background()
	actor := provisionImportActor()

	for _, testCase := range []struct {
		name         string
		state        string
		wantConflict bool
	}{
		{name: "manual-pending-appends-command", state: "PENDING"},
		{name: "manual-starting-appends-command", state: "STARTING"},
		{name: "manual-approved-conflicts", state: "APPROVED", wantConflict: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			b := withdrawAutoBatch("RUNNING", false, time.Now().Add(time.Hour))
			b.Confirmation.PolicyDecision = contract.ImportPolicyDecisionManual
			b.Confirmation.State = testCase.state
			b.Confirmation.Assignees = map[string][]string{"ROLE_IMPORT_APPROVER": {"approver-1"}}
			repo := &confirmationFreezeRepository{batch: b}

			err := (&Service{Repo: repo}).ActImportConfirmation(ctx, actor, "batch", 3, "withdraw-manual-key", "WITHDRAW")
			if testCase.wantConflict {
				if !errors.Is(err, ErrImportConflict) {
					t.Fatalf("人工已批准确认仍可撤回: %v", err)
				}
				if repo.batch.Confirmation.State != testCase.state || len(repo.batch.Confirmation.Commands) != 0 {
					t.Fatalf("冲突路径改写了确认: %+v", repo.batch.Confirmation)
				}
				return
			}
			if err != nil {
				t.Fatalf("人工撤回命令被拒绝: %v", err)
			}
			// 人工路径的收敛仍归工作流：这里只能追加命令，不得就地迁移状态。
			if repo.batch.Confirmation.State != testCase.state || repo.batch.State != "RUNNING" || repo.batch.Epoch != 4 {
				t.Fatalf("人工撤回就地迁移了状态: state=%s batch=%s epoch=%d",
					repo.batch.Confirmation.State, repo.batch.State, repo.batch.Epoch)
			}
			if len(repo.batch.Confirmation.Commands) != 1 || repo.batch.Confirmation.Commands[0].Action != "WITHDRAW" {
				t.Fatalf("人工撤回命令未留痕: %+v", repo.batch.Confirmation.Commands)
			}
		})
	}
}
