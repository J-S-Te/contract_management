package application

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

var ErrLocalCatalogUnavailable = errors.New("合同本地目录暂不可用")
var localCategoryCode = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_.-]{0,63}$`)

var ErrContractSourceConflict = errors.New("合同本地来源与当前启用的联动能力不兼容")
var ErrLocalReferenceInvalid = errors.New("本地目录引用不存在、已停用或版本已变化")

// ContractSourceOptions 保留独立目录选择及现有不兼容出站能力的显式边界。
type ContractSourceOptions struct {
	CounterpartySource           string
	CategorySource               string
	Catalog                      LocalCatalogRepository
	Targets                      LocalCatalogTargetDirectory
	ProjectIntegrationEnabled    bool
	SettlementIntegrationEnabled bool
	CRMLinkEnabled               bool
}

// LocalCatalogRepository 只允许通过显式可信租户与数据范围访问合同本地目录。
type LocalCatalogRepository interface {
	ListLocalCounterparties(context.Context, contract.ScopeFilter, string, int) ([]contract.LocalCounterparty, error)
	GetLocalCounterparty(context.Context, contract.ScopeFilter, string) (contract.LocalCounterparty, error)
	SaveLocalCounterparty(context.Context, contract.ScopeFilter, contract.LocalCounterparty, uint64, string) (contract.LocalCounterparty, error)
	ListLocalCategories(context.Context, contract.ScopeFilter, string, int) ([]contract.LocalCategory, error)
	GetLocalCategory(context.Context, contract.ScopeFilter, string) (contract.LocalCategory, error)
	SaveLocalCategory(context.Context, contract.ScopeFilter, contract.LocalCategory, uint64, string) (contract.LocalCategory, error)
}

// localCatalogReadScope 只接受管理或合同录入真实授权范围，不从角色名推定范围。
// 参数：actor为当前可信会话主体。
// 返回值：可读目录范围或权限错误。
func (s *Service) localCatalogReadScope(actor Principal) (contract.ScopeFilter, error) {
	for _, permission := range []string{"contract.catalog.manage", "contract.create", "contract.edit"} {
		if filter, ok := s.contractScope(actor, permission); ok {
			return filter, nil
		}
	}
	return contract.ScopeFilter{}, ErrForbidden
}

// localCatalogStore 返回明确配置的本地目录，不回退外部业务目录。
// 参数：无。
// 返回值：本地仓储或来源配置错误。
func (s *Service) localCatalogStore() (LocalCatalogRepository, error) {
	if s.ContractSources == nil || s.ContractSources.Catalog == nil {
		return nil, ErrLocalCatalogUnavailable
	}
	return s.ContractSources.Catalog, nil
}

// ListLocalCounterparties 按当前主体真实范围读取本地交易对方。
// 参数：ctx为请求，actor为可信主体，keyword为检索词，limit为受限数量。
// 返回值：真实范围内目录或错误，不伪装空目录。
func (s *Service) ListLocalCounterparties(ctx context.Context, actor Principal, keyword string, limit int) ([]contract.LocalCounterparty, error) {
	filter, err := s.localCatalogReadScope(actor)
	if err != nil {
		return nil, err
	}
	store, err := s.localCatalogStore()
	if err != nil {
		return nil, err
	}
	keyword = strings.TrimSpace(keyword)
	if len([]rune(keyword)) > 100 {
		return nil, ErrValidation
	}
	if _, manage := actor.Scope("contract.catalog.manage"); !manage {
		if reader, ok := store.(LocalCatalogVisibleRepository); ok {
			visible, e := s.localCatalogVisibility(ctx, actor)
			if e != nil {
				return nil, e
			}
			return reader.ListVisibleLocalCounterparties(ctx, filter.TenantID, visible, keyword, limit)
		}
	}
	return store.ListLocalCounterparties(ctx, filter, keyword, limit)
}

// SaveLocalCounterparty 以专有管理权限保存本地目录，不接受客户端租户或归属。
// 参数：ctx为请求，actor为可信主体，value为白名单字段，expected为已读版本。
// 返回值：已审计的新版本或授权、长度、版本及存储错误。
func (s *Service) SaveLocalCounterparty(ctx context.Context, actor Principal, value contract.LocalCounterparty, expected uint64) (contract.LocalCounterparty, error) {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return value, err
	}
	filter, ok := actor.Scope("contract.catalog.manage")
	if !ok || actor.TenantID == "" || actor.UserID == "" || actor.IdentityID == "" {
		return value, ErrForbidden
	}
	value.Name = strings.TrimSpace(value.Name)
	if value.Name == "" || len([]rune(value.Name)) > 255 {
		return value, ErrValidation
	}
	store, err := s.localCatalogStore()
	if err != nil {
		return value, err
	}
	return store.SaveLocalCounterparty(ctx, filter, value, expected, actor.UserID)
}

// ListLocalCategories 仅返回当前授权范围的本地类别和版本。
// 参数：ctx为请求，actor为可信主体，keyword为编码或名称检索，limit为受限数量。
// 返回值：真实目录或错误。
func (s *Service) ListLocalCategories(ctx context.Context, actor Principal, keyword string, limit int) ([]contract.LocalCategory, error) {
	filter, err := s.localCatalogReadScope(actor)
	if err != nil {
		return nil, err
	}
	store, err := s.localCatalogStore()
	if err != nil {
		return nil, err
	}
	keyword = strings.TrimSpace(keyword)
	if len([]rune(keyword)) > 100 {
		return nil, ErrValidation
	}
	if _, manage := actor.Scope("contract.catalog.manage"); !manage {
		if reader, ok := store.(LocalCatalogVisibleRepository); ok {
			visible, e := s.localCatalogVisibility(ctx, actor)
			if e != nil {
				return nil, e
			}
			return reader.ListVisibleLocalCategories(ctx, filter.TenantID, visible, keyword, limit)
		}
	}
	return store.ListLocalCategories(ctx, filter, keyword, limit)
}

// SaveLocalCategory 以专有目录管理权限保存或停用类别，已有编码由仓储保持不可变。
// 参数：ctx为请求，actor为可信主体，value为白名单字段，expected为已读版本。
// 返回值：已审计新版本或校验、授权、并发及存储错误。
func (s *Service) SaveLocalCategory(ctx context.Context, actor Principal, value contract.LocalCategory, expected uint64) (contract.LocalCategory, error) {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return value, err
	}
	filter, ok := actor.Scope("contract.catalog.manage")
	if !ok || actor.TenantID == "" || actor.UserID == "" || actor.IdentityID == "" {
		return value, ErrForbidden
	}
	value.Code, value.Name = strings.TrimSpace(value.Code), strings.TrimSpace(value.Name)
	if !localCategoryCode.MatchString(value.Code) || value.Name == "" || len([]rune(value.Name)) > 120 {
		return value, ErrValidation
	}
	store, err := s.localCatalogStore()
	if err != nil {
		return value, err
	}
	return store.SaveLocalCategory(ctx, filter, value, expected, actor.UserID)
}
