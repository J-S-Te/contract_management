package mysql

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/testutil/scriptdb"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// grantSQLRepository 构造执行真实Gorm路径的脚本SQL连接，不连接MySQL。
// 参数：t为测试上下文，steps为预期事务和SQL。
// 返回值：仓储；SQL列映射或事务边界不符时测试失败。
func grantSQLRepository(t *testing.T, steps ...scriptdb.Step) *Repository {
	db := scriptdb.Open(t, steps...)
	gormDB, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: db, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	return &Repository{db: gormDB}
}

// TestCatalogGrantSQLVersionAndAuditRollback 验证真实事务按稳定目标及版本更新，审计失败回滚。
// 参数：t为测试上下文。
// 返回值：无；跨租户、版本或审计边界不符时失败。
func TestCatalogGrantSQLVersionAndAuditRollback(t *testing.T) {
	for _, scenario := range []string{"success", "stale", "audit-failure"} {
		t.Run(scenario, func(t *testing.T) {
			var args []driver.NamedValue
			steps := []scriptdb.Step{{Kind: "begin"}, {Kind: "query", SQL: "con_local_counterparty", Names: []string{"tenant_id", "id", "owner_identity_id"}, Rows: [][]driver.Value{{"tenant-a", "entry", "admin"}}},
				{Kind: "query", SQL: "con_local_catalog_grant", Names: []string{"tenant_id", "entry_kind", "entry_id", "subject_type", "subject_id", "subject_name", "enabled", "version"}, Rows: [][]driver.Value{{"tenant-a", "counterparties", "entry", "USER", "sales", "有效销售", true, uint64(2)}}}}
			want := error(nil)
			if scenario == "stale" {
				want = apperrors.ErrVersionConflict
				steps = append(steps, scriptdb.Step{Kind: "rollback"})
			} else {
				audit := scriptdb.Step{Kind: "exec", SQL: "con_audit_outbox"}
				if scenario == "audit-failure" {
					want = errors.New("audit unavailable")
					audit.Err = want
				}
				steps = append(steps, scriptdb.Step{Kind: "exec", SQL: "UPDATE `con_local_catalog_grant`", Args: &args}, audit)
				if want == nil {
					steps = append(steps, scriptdb.Step{Kind: "commit"})
				} else {
					steps = append(steps, scriptdb.Step{Kind: "rollback"})
				}
			}
			repo := grantSQLRepository(t, steps...)
			expected := uint64(2)
			if scenario == "stale" {
				expected = 1
			}
			value, err := repo.SaveLocalCatalogGrant(context.Background(), contract.ScopeFilter{TenantID: "tenant-a", IdentityID: "admin", AllowAll: true}, "counterparties", "entry", contract.LocalCatalogGrant{SubjectType: "USER", SubjectID: "sales", Enabled: false}, expected, "admin")
			if !errors.Is(err, want) {
				t.Fatalf("value=%+v err=%v want=%v", value, err, want)
			}
			if want == nil && value.Version != 3 {
				t.Fatalf("版本未递增: %+v", value)
			}
		})
	}
}

// TestVisibleCatalogSQLRequiresCurrentGrant 验证目录授权SQL保留租户、启用、本人及真实组织且不扩大合同scope。
// 参数：t为测试上下文。
// 返回值：无；任意缺少授权谓词或使用客户端租户时失败。
func TestVisibleCatalogSQLRequiresCurrentGrant(t *testing.T) {
	db, recorder := newApprovedContractDryRun(t)
	repo := &Repository{db: db}
	_, err := repo.ListVisibleLocalCounterparties(context.Background(), "tenant-a", contract.LocalCatalogVisibility{IdentityID: "sales", OrganizationIDs: []string{"org-valid"}}, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"tenant_id", "enabled", "owner_identity_id", "con_local_catalog_grant", "subject_type", "subject_id"} {
		if !strings.Contains(recorder.sql, part) {
			t.Fatalf("缺少范围: %s", recorder.sql)
		}
	}
	found := false
	for _, arg := range recorder.params {
		if arg == "org-valid" {
			found = true
		}
	}
	if !found {
		t.Fatalf("未绑定平台当前组织: %+v", recorder.params)
	}
}
