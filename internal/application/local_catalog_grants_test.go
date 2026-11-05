package application

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

type grantStoreFixture struct {
	LocalCatalogRepository
	saved []contract.LocalCatalogGrant
}

// ListLocalCatalogGrants 返回测试中的已受权条目授权历史。
// 参数：ctx/filter为范围，kind/id为条目。
// 返回值：历史授权。
func (f *grantStoreFixture) ListLocalCatalogGrants(ctx context.Context, filter contract.ScopeFilter, kind, id string) ([]contract.LocalCatalogGrant, error) {
	return f.saved, nil
}

// SaveLocalCatalogGrant 捕获经真实应用校验后的授权写入。
// 参数：ctx/filter为范围，kind/id为条目，value为规范授权，expected为版本，actor为操作者。
// 返回值：递增版本授权。
func (f *grantStoreFixture) SaveLocalCatalogGrant(ctx context.Context, filter contract.ScopeFilter, kind, id string, value contract.LocalCatalogGrant, expected uint64, actor string) (contract.LocalCatalogGrant, error) {
	value.Version = expected + 1
	f.saved = append(f.saved, value)
	return value, nil
}

type grantDirectoryFixture struct {
	calls    int
	orgCalls int
}

// ListCatalogTargets 返回已核实同租户有效销售及组织。
// 参数：ctx/actor为可信范围。
// 返回值：明确有效的候选，不含伪造ID。
func (f *grantDirectoryFixture) ListCatalogTargets(ctx context.Context, actor Principal) ([]contract.LocalCatalogTarget, error) {
	f.calls++
	return []contract.LocalCatalogTarget{{SubjectType: "USER", SubjectID: "sales-valid", Name: "有效销售"}, {SubjectType: "ORG", SubjectID: "org-valid", Name: "有效组织"}}, nil
}

// CurrentCatalogOrganizations 返回当前主体真实组织而非其合同权限范围。
// 参数：ctx/actor为可信主体。
// 返回值：当前有效组织。
func (f *grantDirectoryFixture) CurrentCatalogOrganizations(ctx context.Context, actor Principal) ([]string, error) {
	f.orgCalls++
	return []string{"org-valid"}, nil
}

// TestLocalCatalogGrantValidatesRealTargetAndPreservesRevocation 验证新授权须权威有效，旧目标失效后仍可撤销。
// 参数：t为测试上下文。
// 返回值：无；无权限、伪ID、未登记目标被写入时失败。
func TestLocalCatalogGrantValidatesRealTargetAndPreservesRevocation(t *testing.T) {
	store, directory := &grantStoreFixture{}, &grantDirectoryFixture{}
	s := &Service{ContractSources: &ContractSourceOptions{Catalog: store, Targets: directory}}
	actor := Principal{TenantID: "tenant", UserID: "admin", IdentityID: "admin", Permissions: map[string]bool{"contract.catalog.manage": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.catalog.manage": {AllowAll: true}}}
	for _, kind := range []string{"USER", "ORG"} {
		id := map[string]string{"USER": "sales-valid", "ORG": "org-valid"}[kind]
		result, err := s.SaveLocalCatalogGrant(context.Background(), actor, "counterparties", "entry", contract.LocalCatalogGrantInput{SubjectType: kind, SubjectID: id, Enabled: true})
		if err != nil || result.SubjectID != id || result.Version != 1 {
			t.Fatalf("grant=%+v err=%v", result, err)
		}
	}
	for _, id := range []string{"foreign-sales", "disabled-sales", "arbitrary-id"} {
		before := len(store.saved)
		if _, err := s.SaveLocalCatalogGrant(context.Background(), actor, "categories", "entry", contract.LocalCatalogGrantInput{SubjectType: "USER", SubjectID: id, Enabled: true}); !errors.Is(err, ErrLocalReferenceInvalid) || len(store.saved) != before {
			t.Fatalf("target=%s err=%v", id, err)
		}
	}
	calls := directory.calls
	if _, err := s.SaveLocalCatalogGrant(context.Background(), actor, "categories", "entry", contract.LocalCatalogGrantInput{SubjectType: "USER", SubjectID: "disabled-sales", ExpectedVersion: 2, Enabled: false}); err != nil || directory.calls != calls {
		t.Fatalf("撤销被目录失效阻断: %v", err)
	}
	actor.Permissions = map[string]bool{"contract.create": true}
	if _, err := s.SaveLocalCatalogGrant(context.Background(), actor, "categories", "entry", contract.LocalCatalogGrantInput{SubjectType: "USER", SubjectID: "sales-valid", Enabled: true}); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
}

type visibleGrantFixture struct{ LocalCatalogRepository }

// ListVisibleLocalCounterparties 返回授权层的目录边界替身。
// 参数：ctx/tenant/visibility为可信读取范围，keyword/limit为列表边界。
// 返回值：空目录，仅用于接口完整性。
func (f *visibleGrantFixture) ListVisibleLocalCounterparties(ctx context.Context, tenant string, visibility contract.LocalCatalogVisibility, keyword string, limit int) ([]contract.LocalCounterparty, error) {
	return nil, nil
}

// GetVisibleLocalCounterparty 返回当前被授权的权威交易对方。
// 参数：ctx/tenant/visibility为可信读取范围，id为条目。
// 返回值：同租户启用快照。
func (f *visibleGrantFixture) GetVisibleLocalCounterparty(ctx context.Context, tenant string, visibility contract.LocalCatalogVisibility, id string) (contract.LocalCounterparty, error) {
	return contract.LocalCounterparty{ID: id, TenantID: tenant, Name: "权威对方", Version: 1, Enabled: true}, nil
}

// ListVisibleLocalCategories 返回授权层类别边界替身。
// 参数：ctx/tenant/visibility为可信读取范围，keyword/limit为列表边界。
// 返回值：空目录，仅用于接口完整性。
func (f *visibleGrantFixture) ListVisibleLocalCategories(ctx context.Context, tenant string, visibility contract.LocalCatalogVisibility, keyword string, limit int) ([]contract.LocalCategory, error) {
	return nil, nil
}

// GetVisibleLocalCategory 返回当前被授权的权威类别。
// 参数：ctx/tenant/visibility为可信读取范围，id为条目。
// 返回值：同租户启用快照。
func (f *visibleGrantFixture) GetVisibleLocalCategory(ctx context.Context, tenant string, visibility contract.LocalCatalogVisibility, id string) (contract.LocalCategory, error) {
	return contract.LocalCategory{ID: id, TenantID: tenant, Name: "权威类别", Version: 1, Enabled: true}, nil
}

// TestLocalCatalogReferencesReadMembershipOnce 验证多服务项引用只读取一次当前真实组织且不扩大合同scope。
// 参数：t为测试上下文。
// 返回值：无；为每个类别重复请求平台或修改合同范围时失败。
func TestLocalCatalogReferencesReadMembershipOnce(t *testing.T) {
	directory := &grantDirectoryFixture{}
	s := &Service{ContractSources: &ContractSourceOptions{CounterpartySource: "local", CategorySource: "local", Catalog: &visibleGrantFixture{}, Targets: directory}}
	actor := Principal{TenantID: "tenant", UserID: "sales", IdentityID: "sales", Permissions: map[string]bool{"contract.create": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.create": {AllowSelf: true}}}
	c := contract.Contract{CounterpartySource: "LOCAL", LocalCounterpartyID: "party", CounterpartyVersion: 1, CategorySource: "LOCAL", ServiceItems: []contract.ServiceItem{{CategoryID: "a", CategoryVersion: 1}, {CategoryID: "b", CategoryVersion: 1}}}
	if _, err := s.prepareContractReferences(context.Background(), actor, c, true, "contract.create"); err != nil {
		t.Fatal(err)
	}
	if directory.orgCalls != 1 || actor.PermissionScopes["contract.create"].AllowAll || len(actor.PermissionScopes["contract.create"].OrganizationIDs) != 0 {
		t.Fatalf("membership calls=%d actor=%+v", directory.orgCalls, actor)
	}
}
