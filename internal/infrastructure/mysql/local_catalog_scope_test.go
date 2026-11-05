package mysql

import (
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// TestLocalCatalogScopeSQL 验证生产查询真实生成租户和范围条件且无范围不全量。
// 参数：t为上下文；DryRun禁用自动连接与版本探测，不访问数据库。
// 返回值：无。
func TestLocalCatalogScopeSQL(t *testing.T) {
	db, err := gorm.Open(mysqldriver.New(mysqldriver.Config{DSN: "test:unused@tcp(127.0.0.1:1)/contract_test_dry", SkipInitializeWithVersion: true}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		filter contract.ScopeFilter
		want   string
	}{
		{contract.ScopeFilter{TenantID: "tenant", IdentityID: "identity", AllowSelf: true}, "owner_identity_id"},
		{contract.ScopeFilter{TenantID: "tenant", OrganizationIDs: []string{"org"}}, "owner_org_id"},
		{contract.ScopeFilter{TenantID: "tenant", ProjectIDs: []string{"project"}}, "owner_project_id"},
		{contract.ScopeFilter{TenantID: "tenant"}, "1=0"},
		{contract.ScopeFilter{AllowAll: true}, "1=0"},
	} {
		query := localCatalogScope(db.Model(&localCounterpartyRecord{}), tc.filter).Find(&[]localCounterpartyRecord{})
		if query.Error != nil {
			t.Fatal(query.Error)
		}
		sql := query.Statement.SQL.String()
		if !strings.Contains(sql, "tenant_id") || !strings.Contains(sql, tc.want) {
			t.Fatalf("scope missing: %s", sql)
		}
		if query.Statement.Schema.LookUpField("TenantID") == nil || query.Statement.Schema.LookUpField("OwnerIdentityID") == nil {
			t.Fatal("embedded ownership fields not persisted")
		}
	}
}
