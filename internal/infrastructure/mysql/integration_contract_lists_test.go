package mysql

import (
	"context"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"strings"
	"testing"
)

// TestIntegrationListsFilterLocalBeforeLimit 验证两个无游标集成查询在排序和LIMIT之前裁剪本地，旧NULL继续可见。
// 参数：t为测试上下文；DryRun只读取实际Gorm生成SQL及绑定值。
// 返回值：无；过滤放在读取后的Go数组或丢失原范围时失败。
func TestIntegrationListsFilterLocalBeforeLimit(t *testing.T) {
	for _, kind := range []string{"project", "settlement"} {
		t.Run(kind, func(t *testing.T) {
			db, record := newApprovedContractDryRun(t)
			repo := &Repository{db: db}
			filter := contract.ScopeFilter{TenantID: "tenant-a", IdentityID: "sales", AllowSelf: true}
			var err error
			if kind == "project" {
				_, err = repo.ListIntegrationApprovedContractsScoped(context.Background(), filter, 50)
			} else {
				_, err = repo.ListIntegrationCompletedContractsScoped(context.Background(), filter, "", 50)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, piece := range []string{"integration_policy IS NULL", "integration_policy <> ?", "counterparty_source", "category_source", "owner_identity_id", "tenant_id", "ORDER BY updated_at DESC LIMIT ?"} {
				if !strings.Contains(record.sql, piece) {
					t.Fatalf("SQL不在LIMIT前裁剪或丢范围: %s", record.sql)
				}
			}
			if strings.Index(record.sql, "integration_policy") > strings.Index(record.sql, "ORDER BY") {
				t.Fatal(record.sql)
			}
			// 50个更新较新的本地行在WHERE即被裁剪，原CRM NULL不属于被排除策略；LIMIT窗口留给旧CRM。
			if !strings.Contains(record.sql, "COALESCE(counterparty_source,'') <> 'LOCAL'") {
				t.Fatalf("50条本地会占用LIMIT窗口: %s", record.sql)
			}
		})
	}
}
