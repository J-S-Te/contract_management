package application

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/businessclock"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/oklog/ulid/v2"
)

type PricingRepository interface {
	ListStandardPrices(context.Context, contract.ScopeFilter, string, int, int) ([]contract.StandardPrice, error)
	GetStandardPrice(context.Context, string, string) (contract.StandardPrice, error)
	SaveStandardPrice(context.Context, contract.StandardPrice, uint64, string, string) (contract.StandardPrice, error)
}

// pricingRole 验证标准价职责角色，不允许管理员隐式取得发布职责。
// 参数：actor为可信主体，publish表示发布动作。
// 返回值：具备指定现有职责返回true。
func pricingRole(actor Principal, publish bool) bool {
	for _, role := range actor.Roles {
		if !publish && role == "admin" || publish && (role == "sales_director" || role == "finance_director") {
			return true
		}
	}
	return false
}

// priceInScope 判断目录记录的真实组织是否处于当前授权范围。
// 参数：filter为平台范围，p为目录记录；全租户目录只对全范围维护者开放写入。
// 返回值：有权操作返回true。
func priceInScope(filter contract.ScopeFilter, p contract.StandardPrice) bool {
	return filter.AllowAll || p.OrganizationID != "" && containsString(filter.OrganizationIDs, p.OrganizationID)
}

// ListStandardPrices 读取当前租户发布价或有权管理的草稿分页。
// 参数：ctx与actor为请求及主体，status为状态，offset/limit为分页。
// 返回值：有界目录；无明确读取范围或存储能力时失败关闭。
func (s *Service) ListStandardPrices(ctx context.Context, actor Principal, status string, offset, limit int) ([]contract.StandardPrice, error) {
	filter, ok := actor.Scope("contract.read")
	if !ok {
		return nil, ErrForbidden
	}
	if status != "" && status != "DRAFT" && status != "PUBLISHED" && status != "DISABLED" {
		return nil, ErrValidation
	}
	if status != "PUBLISHED" && !pricingRole(actor, false) && !pricingRole(actor, true) {
		status = "PUBLISHED"
	}
	if offset < 0 || offset > 1000000 {
		return nil, ErrValidation
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	repo, ok := s.Repo.(PricingRepository)
	if !ok {
		return nil, apperrors.ErrStateConflict
	}
	rows, err := repo.ListStandardPrices(ctx, filter, status, offset, limit)
	if err != nil {
		return nil, err
	}
	out := make([]contract.StandardPrice, 0, len(rows))
	for _, p := range rows {
		if p.OrganizationID == "" || priceInScope(filter, p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// SaveStandardPrice 执行草稿维护、独立发布或停用，发布字段保持不可变。
// 参数：ctx/actor为请求主体，p为输入，expected为并发版本，action为SAVE/PUBLISH/DISABLE。
// 返回值：已保存记录；越权、自审、状态和并发不符返回错误。
func (s *Service) SaveStandardPrice(ctx context.Context, actor Principal, p contract.StandardPrice, expected uint64, action string) (contract.StandardPrice, error) {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return p, err
	}
	permission := "contract.pricing.manage"
	if action == "PUBLISH" {
		permission = "contract.pricing.publish"
	}
	filter, ok := actor.Scope(permission)
	if !ok || !pricingRole(actor, action == "PUBLISH") || actor.UserID == "" {
		return p, ErrForbidden
	}
	repo, ok := s.Repo.(PricingRepository)
	if !ok {
		return p, apperrors.ErrStateConflict
	}
	p.TenantID = actor.TenantID
	if p.ID != "" {
		current, err := repo.GetStandardPrice(ctx, actor.TenantID, p.ID)
		if err != nil {
			return p, err
		}
		if !priceInScope(filter, current) {
			return p, ErrForbidden
		}
		if action != "SAVE" {
			p = current
		}
		if action == "PUBLISH" && (current.CreatedBy == actor.UserID || current.UpdatedBy == actor.UserID) {
			return p, ErrForbidden
		}
	}
	if !priceInScope(filter, p) {
		return p, ErrForbidden
	}
	p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
	if err := contract.ValidateStandardPrice(p); err != nil {
		return p, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if p.ID == "" {
		if action != "SAVE" || expected != 0 {
			return p, ErrValidation
		}
		p.ID = ulid.Make().String()
	}
	return repo.SaveStandardPrice(ctx, p, expected, actor.UserID, action)
}

// preparePricing 服务端选择已发布基准或延续原冻结明细，拒绝客户端提供标准额。
// 参数：ctx/actor为请求主体，c为已校验合同，previous为旧冻结基准。
// 返回值：完整服务端冻结合同；缺项、日期、币税和金额不匹配返回校验错误。
func (s *Service) preparePricing(ctx context.Context, actor Principal, c contract.Contract, previous *contract.Pricing) (contract.Contract, error) {
	if c.Pricing == nil {
		return c, nil
	}
	if len(c.Pricing.Lines) == 0 || len(c.Pricing.Lines) > 20 {
		return c, ErrValidation
	}
	raw, _ := json.Marshal(c.Pricing)
	p := new(contract.Pricing)
	if err := json.Unmarshal(raw, p); err != nil {
		return c, err
	}
	today := time.Now().In(businessclock.Location()).Format("2006-01-02")
	if p.BasisDate == "" {
		if previous != nil {
			p.BasisDate = previous.BasisDate
		} else {
			p.BasisDate = today
		}
	}
	if _, err := time.Parse("2006-01-02", p.BasisDate); err != nil || p.BasisDate > today {
		return c, ErrValidation
	}
	if p.Reselect && (strings.TrimSpace(p.Reason) == "" || len([]rune(p.Reason)) > 500 || previous != nil && p.BasisDate == previous.BasisDate) {
		return c, ErrValidation
	}
	if previous != nil && !p.Reselect && (c.Currency != previous.Currency || p.BasisDate != previous.BasisDate || p.TaxBasis != previous.TaxBasis || p.TaxRateBPS != previous.TaxRateBPS) {
		return c, ErrValidation
	}
	c.ServiceItems = append([]contract.ServiceItem(nil), c.ServiceItems...)
	for i := range c.ServiceItems {
		if c.ServiceItems[i].SourceID == "" {
			c.ServiceItems[i].SourceID = ulid.Make().String()
		}
	}
	old := map[string]contract.PricingLine{}
	if previous != nil {
		for _, l := range previous.Lines {
			old[l.ServiceID] = l
		}
	}
	for i := range p.Lines {
		l := &p.Lines[i]
		if l.ServiceIndex < 0 || l.ServiceIndex >= len(c.ServiceItems) {
			return c, ErrValidation
		}
		item := c.ServiceItems[l.ServiceIndex]
		prior, exists := old[item.SourceID]
		if previous != nil && !p.Reselect && exists {
			if l.PriceID != prior.PriceID || l.Quantity != prior.Quantity || l.Unit != "" && l.Unit != prior.Unit {
				return c, ErrValidation
			}
			amount := l.AmountMinor
			serviceIndex := l.ServiceIndex
			*l = prior
			l.ServiceIndex = serviceIndex
			l.AmountMinor = amount
		} else {
			if previous != nil && !p.Reselect {
				return c, fmt.Errorf("%w: 新服务项须显式选择新基准日", ErrValidation)
			}
			repo, ok := s.Repo.(PricingRepository)
			if !ok {
				return c, apperrors.ErrStateConflict
			}
			price, err := repo.GetStandardPrice(ctx, actor.TenantID, l.PriceID)
			if err != nil {
				return c, err
			}
			category := item.CategoryID
			if category == "" {
				category = item.Category
			}
			if category == "" {
				category = item.ServiceType
			}
			if price.CategoryID != category || item.CategoryID != "" && price.CategoryVersion != item.CategoryVersion {
				return c, ErrValidation
			}
			if price.Status != "PUBLISHED" || price.Currency != c.Currency || price.TaxBasis != p.TaxBasis || price.TaxRateBPS != p.TaxRateBPS || price.EffectiveFrom > p.BasisDate || price.EffectiveTo != "" && price.EffectiveTo < p.BasisDate || price.OrganizationID != "" && price.OrganizationID != c.OwnerOrgID || price.ServiceType != item.ServiceType || item.CategoryID != "" && price.CategoryID != item.CategoryID {
				return c, ErrValidation
			}
			standard, err := contract.QuantityAmount(l.Quantity, price.UnitPriceMinor)
			if err != nil {
				return c, ErrValidation
			}
			l.LineID = ulid.Make().String()
			l.ServiceID = item.SourceID
			l.PriceVersion = price.Version
			l.StandardMinor = standard
			l.Unit = price.Unit
			l.UnitPriceMinor = price.UnitPriceMinor
		}
	}
	p.Currency = c.Currency
	p.FrozenAt = time.Now().UTC()
	if previous != nil && !p.Reselect {
		p.FrozenAt = previous.FrozenAt
	}
	p.Reselect = false
	c.Pricing = p
	if _, err := contract.PricingTotal(c); err != nil {
		return c, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return c, nil
}

// preservePricingAmounts 延续原标准额并按原成交占比分配合法改额，尾差落最后一行。
// 参数：p为旧冻结基准，amount为新总额，oldAmount为旧合同总额。
// 返回值：独立副本；零总额时按冻结标准额比例分配。
func preservePricingAmounts(p *contract.Pricing, amount, oldAmount int64) *contract.Pricing {
	if p == nil {
		return nil
	}
	data, _ := json.Marshal(p)
	out := new(contract.Pricing)
	_ = json.Unmarshal(data, out)
	denominator := oldAmount
	if denominator <= 0 {
		for _, l := range p.Lines {
			denominator += l.StandardMinor
		}
	}
	var assigned int64
	for i := range out.Lines {
		weight := p.Lines[i].AmountMinor
		if oldAmount <= 0 {
			weight = p.Lines[i].StandardMinor
		}
		n := int64(0)
		if denominator > 0 {
			value := new(big.Int).Mul(big.NewInt(amount), big.NewInt(weight))
			value.Quo(value, big.NewInt(denominator))
			n = value.Int64()
		}
		if i == len(out.Lines)-1 {
			n = amount - assigned
		}
		out.Lines[i].AmountMinor = n
		assigned += n
	}
	return out
}

// validatePricingContinuation 校验草稿延续基准时币种和稳定服务的计价语义未改变。
// 参数：current为原合同不可变输入，next为已解析权威来源的新草稿。
// 返回值：币种、服务类型或类别身份/版本改变却未显式重选时返回校验错误。
func validatePricingContinuation(current, next contract.Contract) error {
	if current.Pricing == nil || next.Pricing != nil && next.Pricing.Reselect {
		return nil
	}
	if next.Currency != current.Pricing.Currency {
		return ErrValidation
	}
	old := map[string]contract.ServiceItem{}
	for _, item := range current.ServiceItems {
		if item.SourceID != "" {
			old[item.SourceID] = item
		}
	}
	for _, item := range next.ServiceItems {
		prior, found := old[item.SourceID]
		if !found {
			continue
		}
		if item.ServiceType != prior.ServiceType || item.Category != prior.Category || item.CategoryID != prior.CategoryID || item.CategoryVersion != prior.CategoryVersion {
			return ErrValidation
		}
	}
	return nil
}

// resolveNewPricingOrganization 通过已配置的平台本人组织目录冻结组织价的可信合同归属。
// 参数：ctx/actor为请求主体，c为尚未规范化的新建合同；不接受客户端自由组织归属。
// 返回值：冻结真实归属的合同；非本人有效组织、混合组织或当前动作无范围时失败关闭。
func (s *Service) resolveNewPricingOrganization(ctx context.Context, actor Principal, c contract.Contract) (contract.Contract, error) {
	if c.Pricing == nil {
		return c, nil
	}
	if len(c.Pricing.Lines) == 0 || len(c.Pricing.Lines) > 20 {
		return c, ErrValidation
	}
	filter, ok := actor.Scope("contract.create")
	if !ok {
		return c, ErrForbidden
	}
	repo, ok := s.Repo.(PricingRepository)
	if !ok {
		return c, apperrors.ErrStateConflict
	}
	organization := ""
	for _, line := range c.Pricing.Lines {
		price, err := repo.GetStandardPrice(ctx, actor.TenantID, line.PriceID)
		if err != nil {
			return c, err
		}
		if price.OrganizationID == "" {
			continue
		}
		if organization != "" && organization != price.OrganizationID {
			return c, ErrValidation
		}
		organization = price.OrganizationID
	}
	if organization == "" {
		return c, nil
	}
	if s.ContractSources == nil || s.ContractSources.Targets == nil {
		return c, ErrPersonnelDirectoryUnavailable
	}
	orgs, err := s.ContractSources.Targets.CurrentCatalogOrganizations(ctx, actor)
	if err != nil {
		return c, err
	}
	if !containsString(orgs, organization) {
		return c, ErrForbidden
	}
	if !filter.AllowAll && !filter.AllowSelf && !containsString(filter.OrganizationIDs, organization) {
		return c, ErrForbidden
	}
	if c.OwnerOrgID != "" && c.OwnerOrgID != organization {
		return c, ErrForbidden
	}
	c.OwnerOrgID = organization
	c.PricingOrganizationVerified = true
	// SELF仅写本人合同的可信组织标签，不给主体新增ORG权限或他人合同访问能力。
	if filter.AllowSelf {
		c.OwnerIdentityID = actor.IdentityID
	}
	return c, nil
}
