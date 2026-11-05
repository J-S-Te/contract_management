package application

import (
	"context"
	"fmt"
	"strings"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// prepareContractReferences 按显式来源解析真实引用，保留旧CRM及项目目录分支。
// 参数：ctx为请求，actor为可信主体，c为合同，external区分外部原件，permission限定范围。
// 返回值：权威字段规范化后的合同，或来源、范围、版本和依赖错误。
func (s *Service) prepareContractReferences(ctx context.Context, actor Principal, c contract.Contract, external bool, permission string) (contract.Contract, error) {
	filter, ok := s.contractScope(actor, permission)
	if !ok {
		return c, ErrForbidden
	}
	if err := validateContractReferenceTypes(c); err != nil {
		return c, err
	}
	if c.CounterpartySource == contract.CounterpartyLocal || c.CategorySource == contract.CategoryLocal {
		if err := validateLocalSourceOptions(c, s.ContractSources); err != nil {
			return c, err
		}
		if _, reader := s.ContractSources.Catalog.(LocalCatalogVisibleRepository); reader {
			if _, manage := actor.Scope("contract.catalog.manage"); !manage {
				visible, err := s.localCatalogVisibility(ctx, actor)
				if err != nil {
					return c, err
				}
				ctx = context.WithValue(ctx, catalogVisibilityCacheKey{}, catalogVisibilityCache{tenant: actor.TenantID, identity: actor.IdentityID, value: visible})
			}
		}
		c.IntegrationPolicy = contract.IntegrationNotApplicable
	}
	var err error
	c, err = s.resolveCounterpartyReference(ctx, actor, c, external, filter)
	if err != nil {
		return c, err
	}
	return s.resolveContractCategories(ctx, actor, c, external, filter)
}

// validateContractReferenceTypes 校验来源命名空间，LOCAL不借用CRM数字身份。
// 参数：c为尚未解析的合同引用。
// 返回值：字段合法返回nil，否则返回校验错误。
func validateContractReferenceTypes(c contract.Contract) error {
	local := c.CounterpartySource == contract.CounterpartyLocal
	if c.CounterpartySource != "" && c.CounterpartySource != contract.CounterpartyCRM && !local {
		return ErrValidation
	}
	if c.CategorySource != "" && c.CategorySource != contract.CategoryProject && c.CategorySource != contract.CategoryLocal {
		return ErrValidation
	}
	if c.CounterpartySource == contract.CounterpartyCRM && c.CRMCustomerID == 0 {
		return ErrValidation
	}
	if !local && (c.LocalCounterpartyID != "" || c.CounterpartyVersion != 0) {
		return ErrValidation
	}
	if local && (c.CRMCustomerID != 0 || c.OpportunityID != "" || c.OpportunityName != "") {
		return ErrValidation
	}
	return nil
}

// validateLocalSourceOptions 复核明确启用的本地目录与不兼容出站能力。
// 参数：c为明确本地来源合同，options为可信运行配置。
// 返回值：配置冲突或目录不可用错误，不自动降级来源。
func validateLocalSourceOptions(c contract.Contract, options *ContractSourceOptions) error {
	if options == nil || (c.CounterpartySource == contract.CounterpartyLocal && options.CounterpartySource != "local") || (c.CategorySource == contract.CategoryLocal && options.CategorySource != "local") {
		return ErrContractSourceConflict
	}
	if options.ProjectIntegrationEnabled || options.SettlementIntegrationEnabled || options.CRMLinkEnabled {
		return ErrContractSourceConflict
	}
	if options.Catalog == nil {
		return ErrLocalCatalogUnavailable
	}
	return nil
}

// resolveCounterpartyReference 解析本地版本化交易对方，或执行旧CRM权威回读。
// 参数：ctx为请求，actor为可信主体，c为合同，external为原件模式，filter为真实范围。
// 返回值：权威名称及引用合同，或原分类错误。
func (s *Service) resolveCounterpartyReference(ctx context.Context, actor Principal, c contract.Contract, external bool, filter contract.ScopeFilter) (contract.Contract, error) {
	if c.CounterpartySource == contract.CounterpartyLocal {
		if c.LocalCounterpartyID == "" || c.CounterpartyVersion == 0 {
			return c, ErrValidation
		}
		value, err := s.localCounterpartyReference(ctx, actor, filter, c.LocalCounterpartyID)
		if err != nil {
			return c, err
		}
		if value.TenantID != actor.TenantID || value.ID != c.LocalCounterpartyID || !value.Enabled || value.Version != c.CounterpartyVersion || strings.TrimSpace(value.Name) == "" {
			return c, ErrLocalReferenceInvalid
		}
		c.CustomerName = value.Name
		return c, nil
	}
	if !external && c.CRMCustomerID == 0 && c.OpportunityID == "" && len(c.PaymentTerms) == 0 {
		return c, nil
	}
	if external && (c.CRMCustomerID == 0 || c.CustomerName == "" || (c.OpportunityID == "") != (c.OpportunityName == "") || !validOptionalCRMID(c.OpportunityID)) {
		return c, ErrValidation
	}
	if c.CRMCustomerID == 0 || !validOptionalCRMID(c.OpportunityID) {
		return c, fmt.Errorf("%w: 付款计划或商机引用须关联有效的CRM客户", ErrValidation)
	}
	reference, err := s.validateExternalCRMReference(ctx, c.CRMCustomerID, c.OpportunityID, actor.IdentityID)
	if err != nil {
		return c, err
	}
	c.CustomerName = strings.TrimSpace(reference.Customer.Name)
	c.OpportunityName = ""
	if reference.Opportunity != nil {
		c.OpportunityName = strings.TrimSpace(reference.Opportunity.Name)
	}
	return c, nil
}

// resolveContractCategories 校验独立本地类别版本或保留原外部合同项目目录校验。
// 参数：ctx/actor为可信主体，c为合同，external区分原件，filter为合同范围。
// 返回值：规范类别快照或校验及依赖错误，不修改服务项SourceID。
func (s *Service) resolveContractCategories(ctx context.Context, actor Principal, c contract.Contract, external bool, filter contract.ScopeFilter) (contract.Contract, error) {
	if c.CategorySource == contract.CategoryLocal {
		for i := range c.ServiceItems {
			item := &c.ServiceItems[i]
			if item.CategoryID == "" || item.CategoryVersion == 0 {
				return c, ErrValidation
			}
			value, err := s.localCategoryReference(ctx, actor, filter, item.CategoryID)
			if err != nil {
				return c, err
			}
			if value.TenantID != filter.TenantID || value.ID != item.CategoryID || !value.Enabled || value.Version != item.CategoryVersion {
				return c, ErrLocalReferenceInvalid
			}
			item.Category = value.Name
		}
		return c, nil
	}
	categories := make([]string, 0, len(c.ServiceItems))
	for _, item := range c.ServiceItems {
		if item.CategoryID != "" || item.CategoryVersion != 0 {
			return c, ErrValidation
		}
		categories = append(categories, item.Category)
	}
	if external {
		if err := s.validateExternalDetectionCategories(ctx, categories); err != nil {
			return c, err
		}
	}
	return c, nil
}
