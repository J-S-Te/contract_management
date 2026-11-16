package application

import (
	"context"
	"io"
)

type DiscountMetricsStreamRepository interface {
	StreamDiscountMetrics(context.Context, string, io.Writer) error
}

// StreamDashboardMetrics 在原租户全范围机器权限下输出有界完整指标。
// 参数：ctx 为请求，actor 为验签主体，output 为协议输出。
// 返回值：缺权限、仓储能力或完整读取失败返回错误。
func (s *Service) StreamDashboardMetrics(ctx context.Context, actor Principal, output io.Writer) error {
	filter, ok := actor.Scope("contract.read")
	if !ok || !filter.AllowAll || actor.TenantID == "" {
		return ErrForbidden
	}
	repo, ok := s.Repo.(DiscountMetricsStreamRepository)
	if !ok {
		return ErrValidation
	}
	return repo.StreamDiscountMetrics(ctx, actor.TenantID, output)
}
