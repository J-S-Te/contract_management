package mysql

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/testfixture"
	"gorm.io/gorm"
)

type mysqlCatalogDirectory struct {
	orgs      []string
	user, org string
}

// ListCatalogTargets 提供显式隔离验收的平台目录端口替身，SQL仍执行真实42表。
// 参数：ctx/actor为当前测试主体。
// 返回值：当前有效销售与组织。
func (d *mysqlCatalogDirectory) ListCatalogTargets(ctx context.Context, actor application.Principal) ([]contract.LocalCatalogTarget, error) {
	return []contract.LocalCatalogTarget{{SubjectType: "USER", SubjectID: d.user, Name: "隔离销售"}, {SubjectType: "ORG", SubjectID: d.org, Name: "隔离组织"}}, nil
}

// CurrentCatalogOrganizations 提供测试期间当前实际会员结果，可模拟会员撤销。
// 参数：ctx/actor为当前测试主体。
// 返回值：当前有效组织副本。
func (d *mysqlCatalogDirectory) CurrentCatalogOrganizations(ctx context.Context, actor application.Principal) ([]string, error) {
	return append([]string(nil), d.orgs...), nil
}

// TestLocalCatalogGrantsMySQLLifecycle 验证真实42表的授权/撤销、SELF建单、组织会员、CAS并发和审计回滚。
// 参数：t为测试上下文，仅使用既有严格CONTRACT_TEST_DSN隔离门禁。
// 返回值：无；无DSN明确跳过，不探测或连接业务库。
func TestLocalCatalogGrantsMySQLLifecycle(t *testing.T) {
	repo, tenant := draftE2ERepository(t)
	t.Cleanup(func() {
		for _, table := range []string{"con_local_catalog_grant", "con_local_counterparty", "con_local_category"} {
			if err := repo.db.Exec("DELETE FROM "+table+" WHERE tenant_id=?", tenant).Error; err != nil {
				t.Error(err)
			}
		}
	})
	ctx := context.Background()
	adminID, salesID, orgID := newID(), newID(), newID()
	directory := &mysqlCatalogDirectory{orgs: []string{orgID}, user: salesID, org: orgID}
	s := &application.Service{Repo: repo, ExternalPDFEnabled: true, ContractSources: &application.ContractSourceOptions{CounterpartySource: "local", CategorySource: "local", Catalog: repo, Targets: directory}}
	admin := application.Principal{TenantID: tenant, UserID: adminID, IdentityID: adminID, Permissions: map[string]bool{"contract.catalog.manage": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.catalog.manage": {AllowAll: true}}}
	sales := application.Principal{TenantID: tenant, UserID: salesID, IdentityID: salesID, Roles: []string{"sales"}, Permissions: map[string]bool{"contract.create": true, "contract.read": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.create": {AllowSelf: true}, "contract.read": {AllowSelf: true}}}
	party, err := s.SaveLocalCounterparty(ctx, admin, contract.LocalCounterparty{Name: "隔离交易对方", Enabled: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	category, err := s.SaveLocalCategory(ctx, admin, contract.LocalCategory{Code: "TEST", Name: "隔离类别", Enabled: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListLocalCounterparties(ctx, sales, "", 20)
	if err != nil || len(list) != 0 {
		t.Fatalf("未授权销售可见: %+v %v", list, err)
	}
	grant := contract.LocalCatalogGrantInput{SubjectType: "USER", SubjectID: salesID, Enabled: true}
	if _, err = s.SaveLocalCatalogGrant(ctx, admin, "counterparties", party.ID, grant); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveLocalCatalogGrant(ctx, admin, "categories", category.ID, grant); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListLocalCounterparties(ctx, sales, "", 20)
	if err != nil || len(list) != 1 {
		t.Fatalf("USER授权不可见: %+v %v", list, err)
	}
	original := contract.Contract{Title: "隔离授权建单", Type: "直签", AmountMinor: 10000, Currency: "CNY", CounterpartySource: "LOCAL", LocalCounterpartyID: party.ID, CounterpartyVersion: party.Version, CategorySource: "LOCAL", SourceFilename: "grant.pdf", SourceMediaType: "application/pdf", Document: testfixture.PDF("isolated grant contract"), ServiceItems: []contract.ServiceItem{{ServiceType: "软件测试", Site: "隔离站点", Batch: "第一批", Category: "客户端显示", CategoryID: category.ID, CategoryVersion: category.Version, TestMode: "STANDARD"}}}
	created, err := s.CreateExternalContract(ctx, sales, original)
	if err != nil || created.LocalCounterpartyID != party.ID || created.CRMCustomerID != 0 {
		t.Fatalf("授权后真实建单: %+v %v", created, err)
	}
	grant.Enabled = false
	grant.ExpectedVersion = 1
	if _, err = s.SaveLocalCatalogGrant(ctx, admin, "counterparties", party.ID, grant); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListLocalCounterparties(ctx, sales, "", 20)
	if err != nil || len(list) != 0 {
		t.Fatalf("USER撤销未即时生效: %+v %v", list, err)
	}
	if _, err = s.CreateExternalContract(ctx, sales, original); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("撤销后仍建单: %v", err)
	}
	orgGrant := contract.LocalCatalogGrantInput{SubjectType: "ORG", SubjectID: orgID, Enabled: true}
	if _, err = s.SaveLocalCatalogGrant(ctx, admin, "counterparties", party.ID, orgGrant); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListLocalCounterparties(ctx, sales, "", 20)
	if err != nil || len(list) != 1 {
		t.Fatalf("ORG授权未按当前会员可见: %+v %v", list, err)
	}
	directory.orgs = nil
	list, err = s.ListLocalCounterparties(ctx, sales, "", 20)
	if err != nil || len(list) != 0 {
		t.Fatalf("已失效会员仍可见: %+v %v", list, err)
	}
	directory.orgs = []string{orgID}
	orgGrant.Enabled = false
	orgGrant.ExpectedVersion = 1
	if _, err = s.SaveLocalCatalogGrant(ctx, admin, "counterparties", party.ID, orgGrant); err != nil {
		t.Fatal(err)
	}
	owner, err := repo.ListVisibleLocalCounterparties(ctx, tenant, contract.LocalCatalogVisibility{IdentityID: adminID}, "", 20)
	if err != nil || len(owner) != 1 {
		t.Fatalf("原Owner不可见: %+v %v", owner, err)
	}
	foreign := admin
	foreign.TenantID = newID()
	if _, err = s.SaveLocalCatalogGrant(ctx, foreign, "counterparties", party.ID, contract.LocalCatalogGrantInput{SubjectType: "USER", SubjectID: salesID, Enabled: true}); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("跨租户grant: %v", err)
	}
	grant.Enabled = true
	grant.ExpectedVersion = 1
	if _, err = s.SaveLocalCatalogGrant(ctx, admin, "counterparties", party.ID, grant); !errors.Is(err, apperrors.ErrVersionConflict) {
		t.Fatalf("stale未拒绝: %v", err)
	}
	grant.ExpectedVersion = 2
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, e := s.SaveLocalCatalogGrant(ctx, admin, "counterparties", party.ID, grant); results <- e })
	}
	wg.Wait()
	close(results)
	successes := 0
	for e := range results {
		if e == nil {
			successes++
		} else if !errors.Is(e, apperrors.ErrVersionConflict) {
			t.Fatal(e)
		}
	}
	if successes != 1 {
		t.Fatalf("CAS并发成功=%d", successes)
	}
	var failAudit atomic.Bool
	denied := errors.New("isolated audit fault")
	callback := "local_catalog_grant_audit_fault"
	if err = repo.db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if failAudit.Load() && tx.Statement.Table == "con_audit_outbox" {
			tx.AddError(denied)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.db.Callback().Create().Remove(callback) })
	failAudit.Store(true)
	grant.Enabled = false
	grant.ExpectedVersion = 3
	if _, err = s.SaveLocalCatalogGrant(ctx, admin, "counterparties", party.ID, grant); !errors.Is(err, denied) {
		t.Fatalf("审计失败未回滚: %v", err)
	}
	failAudit.Store(false)
	rows, err := s.ListLocalCatalogGrants(ctx, admin, "counterparties", party.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.SubjectType == "USER" && (!row.Enabled || row.Version != 3) {
			t.Fatalf("审计回滚丢失: %+v", row)
		}
	}
	saved, err := repo.GetContract(ctx, tenant, created.ID)
	if err != nil || saved.LocalCounterpartyID != party.ID || saved.CustomerName != party.Name {
		t.Fatalf("授权变更重写旧快照: %+v %v", saved, err)
	}
}

// TestIntegrationListsKeepLegacyAfterFiftyLocalMySQL 验证真实SQL的50个本地+旧CRM反例及普通台账不变。
// 参数：t为测试上下文，仅显式隔离CONTRACT_TEST_DSN运行。
// 返回值：无；集成窗口被本地占用或台账误过滤时失败。
func TestIntegrationListsKeepLegacyAfterFiftyLocalMySQL(t *testing.T) {
	repo, tenant := draftE2ERepository(t)
	ctx := context.Background()
	owner := newID()
	now := time.Now().UTC()
	rows := make([]contractRecord, 0, 51)
	for range 50 {
		rows = append(rows, contractRecord{ID: newID(), TenantID: tenant, Title: "隔离本地", ContractType: "直签", OwnerUserID: owner, OwnerIdentityID: stringPtr(owner), AmountMinor: 100, Currency: "CNY", Status: string(contract.StatusCompleted), Version: 1, OriginKind: "NORMAL", CounterpartySource: "LOCAL", IntegrationPolicy: contract.IntegrationNotApplicable, CreatedAt: now, UpdatedAt: now})
	}
	old := contractRecord{ID: newID(), TenantID: tenant, Title: "隔离旧合同", ContractType: "直签", OwnerUserID: owner, OwnerIdentityID: stringPtr(owner), CRMCustomerID: uintPtr(42), AmountMinor: 100, Currency: "CNY", Status: string(contract.StatusCompleted), Version: 1, OriginKind: "NORMAL", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
	rows = append(rows, old)
	if err := repo.db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	filter := contract.ScopeFilter{TenantID: tenant, IdentityID: owner, AllowSelf: true}
	project, err := repo.ListIntegrationApprovedContractsScoped(ctx, filter, 50)
	if err != nil || len(project) != 1 || project[0].ID != old.ID {
		t.Fatalf("项目窗口隐藏旧合同: %+v %v", project, err)
	}
	settlement, err := repo.ListIntegrationCompletedContractsScoped(ctx, filter, "", 50)
	if err != nil || len(settlement) != 1 || settlement[0].ID != old.ID {
		t.Fatalf("结算窗口隐藏旧合同: %+v %v", settlement, err)
	}
	ordinary, err := repo.ListContractsScoped(ctx, filter, "", string(contract.StatusCompleted), 50)
	if err != nil || len(ordinary) != 50 {
		t.Fatalf("普通台账被裁剪: %d %v", len(ordinary), err)
	}
	for _, row := range ordinary {
		if row.CounterpartySource != "LOCAL" {
			t.Fatalf("普通窗口排序改变: %+v", row)
		}
	}
	filter.TenantID = newID()
	if found, err := repo.ListIntegrationApprovedContractsScoped(ctx, filter, 50); err != nil || len(found) != 0 {
		t.Fatalf("跨租户列表: %+v %v", found, err)
	}
}
