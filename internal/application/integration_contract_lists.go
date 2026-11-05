package application

import (
	"context"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"strings"
)

type ProjectConsumableRepository interface {
	ListIntegrationApprovedContractsScoped(context.Context, contract.ScopeFilter, int) ([]contract.Contract, error)
}
type SettlementConsumableRepository interface {
	ListIntegrationCompletedContractsScoped(context.Context, contract.ScopeFilter, string, int) ([]contract.Contract, error)
}

// ListProjectConsumableContracts 以原项目读取权限选择先裁剪来源的独立集成仓储路径。
// 参数：ctx/actor为可信主体，limit为原有数量上限。
// 返回值：可消费合同或权限/仓储错误，不在LIMIT之后伪装完整结果。
func (s *Service) ListProjectConsumableContracts(ctx context.Context, actor Principal, limit int) ([]contract.Contract, error) {
	filter, ok := s.contractScope(actor, "contract.approved.read")
	if !ok {
		return nil, ErrForbidden
	}
	repo, ok := s.Repo.(ProjectConsumableRepository)
	if !ok {
		return nil, ErrContractSourceConflict
	}
	return repo.ListIntegrationApprovedContractsScoped(ctx, filter, limit)
}

// ListSettlementConsumableContracts 以原合同读取权限返回先裁剪来源的completed集成列表。
// 参数：ctx/actor为可信主体，keyword/limit为原检索与数量边界。
// 返回值：可消费合同或权限/仓储错误，普通台账接口不变。
func (s *Service) ListSettlementConsumableContracts(ctx context.Context, actor Principal, keyword string, limit int) ([]contract.Contract, error) {
	filter, ok := s.contractScope(actor, "contract.read")
	if !ok {
		return nil, ErrForbidden
	}
	repo, ok := s.Repo.(SettlementConsumableRepository)
	if !ok {
		return nil, ErrContractSourceConflict
	}
	return repo.ListIntegrationCompletedContractsScoped(ctx, filter, strings.TrimSpace(keyword), limit)
}
