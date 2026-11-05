package httpapi

import (
	"context"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// ListIntegrationCompletedContractsScoped 适配原付款期测试仓储的新集成查询路径，保留原真实响应断言。
// 参数：ctx/filter为测试可信范围，keyword/limit为原列表参数。
// 返回值：原夹具中的已保存付款期，不吞掉集成错误。
func (r completedPaymentTermsRepository) ListIntegrationCompletedContractsScoped(ctx context.Context, filter contract.ScopeFilter, keyword string, limit int) ([]contract.Contract, error) {
	return r.ListContracts(ctx, filter.TenantID, keyword, "", string(contract.StatusCompleted), limit)
}
