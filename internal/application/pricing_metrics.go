package application

import (
	"context"
	"github.com/j-s-te/contract-management/internal/businessclock"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

type DiscountMetricRow struct {
	Kind           string `json:"kind"`
	RecordID       string `json:"record_id"`
	OrganizationID string `json:"organization_id"`
	IdentityID     string `json:"identity_id"`
	ProjectID      string `json:"project_id"`
	Period         string `json:"period"`
	Status         string `json:"status"`
	Currency       string `json:"currency,omitempty"`
	AmountMinor    int64  `json:"amount_minor"`
	StandardMinor  int64  `json:"standard_minor,omitempty"`
	Bucket         string `json:"bucket,omitempty"`
	UnknownReason  string `json:"unknown_reason,omitempty"`
}
type DiscountMetrics struct {
	SchemaVersion       int                 `json:"schema_version"`
	TenantID            string              `json:"tenant_id"`
	SourceCode          string              `json:"source_code"`
	SnapshotAt          string              `json:"snapshot_at"`
	SourceUpdatedAt     string              `json:"source_updated_at,omitempty"`
	CollectionStartedAt string              `json:"collection_started_at,omitempty"`
	Complete            bool                `json:"complete"`
	Basis               string              `json:"basis"`
	Rows                []DiscountMetricRow `json:"rows"`
}
type DiscountMetricsRepository interface {
	DiscountMetrics(context.Context, string) (DiscountMetrics, error)
}

// DashboardMetrics 读取完整合同折扣机器事实，始终要求租户全范围机器授权。
// 参数：ctx为请求，actor为已绑定tenant的dashboard主体。
// 返回值：事务快照；缺少范围或来源能力时失败关闭。
func (s *Service) DashboardMetrics(ctx context.Context, actor Principal) (DiscountMetrics, error) {
	filter, ok := actor.Scope("contract.read")
	if !ok || !filter.AllowAll {
		return DiscountMetrics{}, ErrForbidden
	}
	repo, ok := s.Repo.(DiscountMetricsRepository)
	if !ok {
		return DiscountMetrics{}, ErrValidation
	}
	return repo.DiscountMetrics(ctx, actor.TenantID)
}

// DiscountRow 从当前有效合同冻结定价生成单一统计事实。
// 参数：c为权威NORMAL合同。
// 返回值：已知精确分桶或有固定原因的未知行。
func DiscountRow(c contract.Contract) DiscountMetricRow {
	row := DiscountMetricRow{Kind: "discount", RecordID: c.ID, OrganizationID: c.OwnerOrgID, IdentityID: c.OwnerIdentityID, ProjectID: c.ProjectID, Status: "UNKNOWN", Currency: c.Currency, AmountMinor: c.AmountMinor, UnknownReason: "MISSING_PRICING"}
	if c.StartDate != nil {
		row.Period = c.StartDate.In(businessclock.Location()).Format("2006-01")
	}
	if c.Pricing == nil {
		return row
	}
	total, err := contract.PricingTotal(c)
	if err != nil {
		row.UnknownReason = "INCOMPLETE_PRICING"
		return row
	}
	if row.Period == "" {
		row.UnknownReason = "MISSING_EFFECTIVE_DATE"
		return row
	}
	row.Status = "KNOWN"
	row.UnknownReason = ""
	row.StandardMinor = total
	row.Bucket = contract.DiscountBucket(c.AmountMinor, total)
	return row
}
