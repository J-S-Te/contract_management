package application

import (
	"context"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"testing"
)

type notificationListRepository struct {
	*recordingRepository
	tenant, applicant string
	filters           []approval.ListFilter
}

// ListApprovals 记录通知读取是否仍绑定租户与业务过滤。
// 参数：ctx为上下文，tenant为租户，applicant为申请人，上限及filters为读取约束。
// 返回值：空摘要集合且无错误。
func (r *notificationListRepository) ListApprovals(_ context.Context, tenant, applicant string, _ int, filters ...approval.ListFilter) ([]approval.Summary, error) {
	r.tenant, r.applicant, r.filters = tenant, applicant, filters
	return nil, nil
}

// TestNotificationApprovalListScopes 验证合同通知查询不能绕过合同可见范围。
// 参数：t 为隔离测试上下文。
// 返回值：无；无权合同被读取或普通列表扩大申请人范围时失败。
func TestNotificationApprovalListScopes(t *testing.T) {
	repo := &notificationListRepository{recordingRepository: &recordingRepository{
		contract: contract.Contract{ID: "contract", TenantID: "tenant", OwnerUserID: "owner"}}}
	service := &Service{Repo: repo}
	actor := Principal{TenantID: "tenant", UserID: "reviewer", Permissions: map[string]bool{
		"contract.read": true, "approval.view": true}, PermissionScopes: allowAllScope("contract.read")}
	if _, err := service.ListMyApprovals(context.Background(), actor, 50); err != nil {
		t.Fatal(err)
	}
	if repo.applicant != "reviewer" {
		t.Fatal("ordinary list widened applicant scope")
	}
	if _, err := service.ListMyApprovals(context.Background(), actor, 50, approval.ListFilter{ContractID: "contract"}); err != nil {
		t.Fatal(err)
	}
	if repo.tenant != "tenant" || repo.applicant != "" || len(repo.filters) != 1 || repo.filters[0].ContractID != "contract" {
		t.Fatalf("unexpected scope: %+v", repo)
	}
	actor.PermissionScopes = allowSelfScope("contract.read")
	repo.filters = nil
	if _, err := service.ListMyApprovals(context.Background(), actor, 50, approval.ListFilter{ContractID: "contract"}); err == nil {
		t.Fatal("invisible contract must be rejected")
	}
	if len(repo.filters) != 0 {
		t.Fatal("invisible contract reached approval query")
	}
}
