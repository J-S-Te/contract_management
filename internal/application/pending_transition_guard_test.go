package application

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// pendingGuardRepository 只替换直迁结果：在途审批计数是仓储层 SQL 判据，
// 应用层通过错误透传保证 409 语义不被改写、也不产生任何状态写入。
type pendingGuardRepository struct {
	*recordingRepository
	transitions   int
	transitionErr error
}

// TransitionDirect 记录直迁调用并返回预置的在途审批冲突。
// 参数：上下文、租户、合同、版本、目标状态、操作者、理由及幂等键为服务传入的迁移输入。
// 返回值：预置迁移错误，验证服务原样透传状态冲突。
func (r *pendingGuardRepository) TransitionDirect(context.Context, string, string, uint64, contract.Status, string, string, string) error {
	r.transitions++
	return r.transitionErr
}

// TestChangeStatusPendingToDraftSurfacesInFlightApprovalConflict 覆盖 M-6 应用层语义：合同审批在
// 途时把 pending 直迁回 draft 必须原样返回 ErrStateConflict（HTTP 409），且不进入任何 Temporal审
// 批启动路径。
// 参数：t 为测试上下文。
// 返回值：无；实际行为偏离断言时用例失败。
func TestChangeStatusPendingToDraftSurfacesInFlightApprovalConflict(t *testing.T) {
	repo := &pendingGuardRepository{
		recordingRepository: &recordingRepository{contract: statusAuthorizationContract("tenant-1", "sales-1", contract.StatusPending)},
		transitionErr:       apperrors.ErrStateConflict,
	}
	svc := &Service{Repo: repo}

	_, err := svc.ChangeStatus(context.Background(), statusAuthorizationActor("sales"), "contract-1", 3, contract.StatusDraft, "审批在途，直接撤回")
	if !errors.Is(err, apperrors.ErrStateConflict) {
		t.Fatalf("in-flight approval must yield ErrStateConflict, got %v", err)
	}
	if repo.transitions != 1 {
		t.Fatalf("transition calls = %d, want 1", repo.transitions)
	}
	if repo.contract.Status != contract.StatusPending || repo.contract.Version != 3 {
		t.Fatalf("conflict must not mutate contract: status=%s version=%d", repo.contract.Status, repo.contract.Version)
	}
}
