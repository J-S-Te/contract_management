package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/stretchr/testify/mock"
	temporalmocks "go.temporal.io/sdk/mocks"
)

type deletedHistoryRepository struct{ *recordingRepository }

// GetContractHistoryMetadata 用途：为服务测试提供已授权的墓碑摘要，不提供当前正文。
// 参数：ctx/filter/id 为真实服务传入的读取上下文、范围与合同身份。
// 返回值：合成当前归属元数据或越界错误，不模拟实际数据库证明。
func (r *deletedHistoryRepository) GetContractHistoryMetadata(ctx context.Context, filter contract.ScopeFilter, id string) (contract.Contract, error) {
	row := r.contract
	if row.ID != id || !canAccessContract(filter, row) {
		return contract.Contract{}, apperrors.ErrNotFound
	}
	row.Content = ""
	row.Document = nil
	return row, nil
}

// TestDeletedApprovalHistoryRequiresFrozenSnapshot 用途：验证墓碑只展示不可变审批快照。
// 参数：t 为服务层测试上下文，分别核对有快照和无快照分支。
// 返回值：无；无快照回读现行正文或有快照时丢失历史时失败。
func TestDeletedApprovalHistoryRequiresFrozenSnapshot(t *testing.T) {
	for _, found := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_snapshot", true: "frozen_snapshot"}[found], func(t *testing.T) {
			now := time.Now().UTC()
			r := &deletedHistoryRepository{&recordingRepository{
				contract:            contract.Contract{ID: "C", TenantID: "T", DeletedAt: &now, Content: "不得回读的当前正文"},
				approvalMeta:        approval.Meta{ID: "A", TenantID: "T", ContractID: "C", WorkflowID: "workflow", RunID: "run", ApplicantUserID: "owner"},
				hasApprovalSnapshot: found, approvalSnapshot: contract.Contract{ID: "C", TenantID: "T", Content: "冻结正文", Version: 1},
			}}
			service := &Service{Repo: r}
			if found {
				temporal := temporalmocks.NewClient(t)
				temporal.On("QueryWorkflow", mock.Anything, "workflow", "run", mock.Anything).Return(nil, errors.New("historical workflow expired"))
				service.Temporal = temporal
			}
			actor := Principal{TenantID: "T", UserID: "viewer", Permissions: map[string]bool{"contract.read": true, "approval.view": true}, PermissionScopes: allowAllScope("contract.read")}
			detail, err := service.GetApprovalDetail(context.Background(), actor, "A")
			if !found {
				if !errors.Is(err, apperrors.ErrNotFound) {
					t.Fatalf("no snapshot error=%v", err)
				}
				return
			}
			if err != nil || !detail.SnapshotAvailable || !detail.StateUnavailable || detail.Contract.Content != "冻结正文" {
				t.Fatalf("history=%+v err=%v", detail, err)
			}
		})
	}
}
