package application

import (
	"context"
	"errors"
	"strings"
)

var ErrSignedContractCounterUnavailable = errors.New("signed contract counter is unavailable")

// OpportunitySignedContractCounter 由可按 CRM 商机引用
// 聚合审批通过合同的仓储实现。
type OpportunitySignedContractCounter interface {
	CountSignedContractsByOpportunityIDs(context.Context, string, []string) (map[string]uint64, error)
}

func (s *Service) CountSignedContractsByOpportunityIDs(ctx context.Context, tenantID string, opportunityIDs []string) (map[string]uint64, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" || len(opportunityIDs) == 0 || len(opportunityIDs) > 1000 {
		return nil, ErrValidation
	}
	counter, ok := s.Repo.(OpportunitySignedContractCounter)
	if !ok {
		return nil, ErrSignedContractCounterUnavailable
	}
	return counter.CountSignedContractsByOpportunityIDs(ctx, tenantID, opportunityIDs)
}
