package application

import (
	"context"
	"strings"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

type catalogVisibilityCacheKey struct{}
type catalogVisibilityCache struct {
	tenant, identity string
	value            contract.LocalCatalogVisibility
}

type LocalCatalogTargetDirectory interface {
	ListCatalogTargets(context.Context, Principal) ([]contract.LocalCatalogTarget, error)
	CurrentCatalogOrganizations(context.Context, Principal) ([]string, error)
}

type LocalCatalogGrantRepository interface {
	ListLocalCatalogGrants(context.Context, contract.ScopeFilter, string, string) ([]contract.LocalCatalogGrant, error)
	SaveLocalCatalogGrant(context.Context, contract.ScopeFilter, string, string, contract.LocalCatalogGrant, uint64, string) (contract.LocalCatalogGrant, error)
}

type LocalCatalogVisibleRepository interface {
	ListVisibleLocalCounterparties(context.Context, string, contract.LocalCatalogVisibility, string, int) ([]contract.LocalCounterparty, error)
	GetVisibleLocalCounterparty(context.Context, string, contract.LocalCatalogVisibility, string) (contract.LocalCounterparty, error)
	ListVisibleLocalCategories(context.Context, string, contract.LocalCatalogVisibility, string, int) ([]contract.LocalCategory, error)
	GetVisibleLocalCategory(context.Context, string, contract.LocalCatalogVisibility, string) (contract.LocalCategory, error)
}

// catalogGrantManager 校验专有管理权限与条目种类，不以客户端目标替代主体范围。
// 参数：actor为当前主体，kind/id为目录条目。
// 返回值：可信管理范围与仓储，或权限及配置错误。
func (s *Service) catalogGrantManager(actor Principal, kind, id string) (contract.ScopeFilter, LocalCatalogGrantRepository, error) {
	filter, ok := actor.Scope("contract.catalog.manage")
	if !ok || actor.TenantID == "" || actor.IdentityID == "" || actor.UserID == "" {
		return filter, nil, ErrForbidden
	}
	if (kind != "counterparties" && kind != "categories") || id == "" || len(id) > 128 {
		return filter, nil, ErrValidation
	}
	if s.ContractSources == nil {
		return filter, nil, ErrLocalCatalogUnavailable
	}
	store, ok := s.ContractSources.Catalog.(LocalCatalogGrantRepository)
	if !ok {
		return filter, nil, ErrLocalCatalogUnavailable
	}
	return filter, store, nil
}

// ListLocalCatalogTargets 读取平台当前有效销售及其有效组织，供管理员显式选择。
// 参数：ctx/actor为真实租户与当前管理主体。
// 返回值：权威候选或权限/目录错误，不接受任意ID占位。
func (s *Service) ListLocalCatalogTargets(ctx context.Context, actor Principal) ([]contract.LocalCatalogTarget, error) {
	if _, ok := actor.Scope("contract.catalog.manage"); !ok || actor.TenantID == "" {
		return nil, ErrForbidden
	}
	if s.ContractSources == nil || s.ContractSources.Targets == nil {
		return nil, ErrPersonnelDirectoryUnavailable
	}
	return s.ContractSources.Targets.ListCatalogTargets(ctx, actor)
}

// ListLocalCatalogGrants 返回可管理条目的授权历史，包含撤销记录。
// 参数：ctx/actor为可信范围，kind/id为受管理条目。
// 返回值：版本化授权，越范围或依赖错误失败关闭。
func (s *Service) ListLocalCatalogGrants(ctx context.Context, actor Principal, kind, id string) ([]contract.LocalCatalogGrant, error) {
	filter, store, err := s.catalogGrantManager(actor, kind, id)
	if err != nil {
		return nil, err
	}
	return store.ListLocalCatalogGrants(ctx, filter, kind, id)
}

// SaveLocalCatalogGrant 对同租户有效销售或组织显式授权，停用旧授权不依赖失效目标仍在目录。
// 参数：ctx/actor为可信管理主体，kind/id为条目，in为稳定目标及预期版本。
// 返回值：新授权版本或真实权限、目标、并发及审计错误。
func (s *Service) SaveLocalCatalogGrant(ctx context.Context, actor Principal, kind, id string, in contract.LocalCatalogGrantInput) (contract.LocalCatalogGrant, error) {
	var value contract.LocalCatalogGrant
	if err := s.checkBusinessLicense(ctx); err != nil {
		return value, err
	}
	filter, store, err := s.catalogGrantManager(actor, kind, id)
	if err != nil {
		return value, err
	}
	in.SubjectID = strings.TrimSpace(in.SubjectID)
	if (in.SubjectType != "USER" && in.SubjectType != "ORG") || in.SubjectID == "" || len(in.SubjectID) > 128 || (!in.Enabled && in.ExpectedVersion == 0) {
		return value, ErrValidation
	}
	value = contract.LocalCatalogGrant{SubjectType: in.SubjectType, SubjectID: in.SubjectID, Enabled: in.Enabled}
	if in.Enabled {
		targets, e := s.ListLocalCatalogTargets(ctx, actor)
		if e != nil {
			return value, e
		}
		for _, target := range targets {
			if target.SubjectType == in.SubjectType && target.SubjectID == in.SubjectID {
				value.SubjectName = target.Name
				break
			}
		}
		if value.SubjectName == "" {
			return value, ErrLocalReferenceInvalid
		}
	}
	return store.SaveLocalCatalogGrant(ctx, filter, kind, id, value, in.ExpectedVersion, actor.UserID)
}

// localCatalogVisibility 读取目录授权需要的真实组织，不修改合同SELF/ORG授权范围。
// 参数：ctx/actor为当前主体。
// 返回值：主体身份及平台当前有效组织，目录失败则明确拒绝。
func (s *Service) localCatalogVisibility(ctx context.Context, actor Principal) (contract.LocalCatalogVisibility, error) {
	if cached, ok := ctx.Value(catalogVisibilityCacheKey{}).(catalogVisibilityCache); ok && cached.tenant == actor.TenantID && cached.identity == actor.IdentityID {
		return cached.value, nil
	}
	value := contract.LocalCatalogVisibility{IdentityID: actor.IdentityID}
	if actor.TenantID == "" || actor.IdentityID == "" {
		return value, ErrForbidden
	}
	if s.ContractSources.Targets != nil {
		orgs, err := s.ContractSources.Targets.CurrentCatalogOrganizations(ctx, actor)
		if err != nil {
			return value, err
		}
		value.OrganizationIDs = orgs
	}
	return value, nil
}

// localCounterpartyReference 在原权限核验后按独立目录授权解析本地交易对方。
// 参数：ctx/actor/filter为可信上下文，id为条目。
// 返回值：当前可见目录或错误，不扩大合同责任范围。
func (s *Service) localCounterpartyReference(ctx context.Context, actor Principal, filter contract.ScopeFilter, id string) (contract.LocalCounterparty, error) {
	if manage, ok := actor.Scope("contract.catalog.manage"); ok {
		return s.ContractSources.Catalog.GetLocalCounterparty(ctx, manage, id)
	}
	if reader, ok := s.ContractSources.Catalog.(LocalCatalogVisibleRepository); ok {
		visible, err := s.localCatalogVisibility(ctx, actor)
		if err != nil {
			return contract.LocalCounterparty{}, err
		}
		return reader.GetVisibleLocalCounterparty(ctx, filter.TenantID, visible, id)
	}
	return s.ContractSources.Catalog.GetLocalCounterparty(ctx, filter, id)
}

// localCategoryReference 在原权限核验后解析Owner或显式授权的启用类别。
// 参数：ctx/actor/filter为可信上下文，id为类别。
// 返回值：版本化类别或权限/目录错误。
func (s *Service) localCategoryReference(ctx context.Context, actor Principal, filter contract.ScopeFilter, id string) (contract.LocalCategory, error) {
	if manage, ok := actor.Scope("contract.catalog.manage"); ok {
		return s.ContractSources.Catalog.GetLocalCategory(ctx, manage, id)
	}
	if reader, ok := s.ContractSources.Catalog.(LocalCatalogVisibleRepository); ok {
		visible, err := s.localCatalogVisibility(ctx, actor)
		if err != nil {
			return contract.LocalCategory{}, err
		}
		return reader.GetVisibleLocalCategory(ctx, filter.TenantID, visible, id)
	}
	return s.ContractSources.Catalog.GetLocalCategory(ctx, filter, id)
}
