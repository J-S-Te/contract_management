package application

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type CRMCustomerReference struct {
	ID     uint64 `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type CRMOpportunityReference struct {
	ID         uint64 `json:"id"`
	Name       string `json:"name"`
	CustomerID uint64 `json:"customer_id"`
	Status     string `json:"status"`
}

type CRMContractReference struct {
	Customer    CRMCustomerReference     `json:"customer"`
	Opportunity *CRMOpportunityReference `json:"opportunity,omitempty"`
}

type CRMContractReferenceDirectory interface {
	Resolve(context.Context, uint64, string, string) (CRMContractReference, error)
}

var (
	ErrCRMReferenceInvalid              = errors.New("CRM contract reference is invalid")
	ErrCRMReferenceDirectoryUnavailable = errors.New("CRM contract reference directory is unavailable")
)

// validateExternalCRMReference 校验客户/商机引用并保留可恢复的授权上下文分类。
// 参数：ctx限定调用，客户/商机ID定位引用，actorIdentityID限定真实操作者范围。
// 返回值：最小权威引用；授权缺失、字段无效和依赖失败分别保留错误语义。
func (service *Service) validateExternalCRMReference(ctx context.Context, customerID uint64, opportunityID, actorIdentityID string) (CRMContractReference, error) {
	if service.CRMContractReferences == nil {
		return CRMContractReference{}, fmt.Errorf("%w: CRM 只读目录未配置", ErrCRMReferenceDirectoryUnavailable)
	}
	result, err := service.CRMContractReferences.Resolve(ctx, customerID, opportunityID, actorIdentityID)
	if err != nil {
		if errors.Is(err, ErrCRMActorContextRequired) {
			return CRMContractReference{}, ErrCRMActorContextRequired
		}
		if errors.Is(err, ErrCRMReferenceInvalid) {
			return CRMContractReference{}, fmt.Errorf("%w: CRM 客户或商机无效", ErrValidation)
		}
		return CRMContractReference{}, fmt.Errorf("%w: %v", ErrCRMReferenceDirectoryUnavailable, err)
	}
	if result.Customer.ID != customerID || result.Customer.Status != "ACTIVE" || strings.TrimSpace(result.Customer.Name) == "" {
		return CRMContractReference{}, fmt.Errorf("%w: CRM 客户无效", ErrValidation)
	}
	if opportunityID == "" {
		if result.Opportunity != nil {
			return CRMContractReference{}, fmt.Errorf("%w: CRM 商机响应不匹配", ErrValidation)
		}
		return result, nil
	}
	expectedOpportunityID, parseErr := strconv.ParseUint(opportunityID, 10, 64)
	if parseErr != nil || expectedOpportunityID == 0 || result.Opportunity == nil || result.Opportunity.ID != expectedOpportunityID || result.Opportunity.CustomerID != customerID || strings.TrimSpace(result.Opportunity.Name) == "" || result.Opportunity.Status == "VOID" {
		return CRMContractReference{}, fmt.Errorf("%w: CRM 商机无效或不属于所选客户", ErrValidation)
	}
	return result, nil
}
