package migration

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/migrations"
)

// TestEmbeddedMigrationsAreContiguous 验证迁移连续且包含文档来源扩展。
// 参数：t 为测试上下文。
// 返回值：无。
func TestEmbeddedMigrationsAreContiguous(t *testing.T) {
	items, err := load(migrations.Files)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if len(items) != 39 ||
		items[0].version != 1 || items[0].name != "contract_workflow" ||
		items[1].version != 2 || items[1].name != "single_active_status_change" ||
		items[2].version != 3 || items[2].name != "username_snapshots" ||
		items[3].version != 4 || items[3].name != "display_name_snapshots" ||
		items[4].version != 5 || items[4].name != "contract_templates" ||
		items[5].version != 6 || items[5].name != "contract_start_date" ||
		items[6].version != 7 || items[6].name != "contract_customer_information" ||
		items[7].version != 8 || items[7].name != "contract_number_format" ||
		items[8].version != 9 || items[8].name != "contract_signing" ||
		items[9].version != 10 || items[9].name != "contract_signing_records" ||
		items[10].version != 11 || items[10].name != "contract_service_items" ||
		items[11].version != 12 || items[11].name != "contract_crm_customer" ||
		items[12].version != 13 || items[12].name != "project_delivery_outbox" ||
		items[13].version != 14 || items[13].name != "oidc_sessions" ||
		items[14].version != 15 || items[14].name != "contract_data_scope" ||
		items[15].version != 16 || items[15].name != "signing_recipient_phone" ||
		items[16].version != 17 || items[16].name != "opportunity_intakes" ||
		items[17].version != 18 || items[17].name != "opportunity_link_outbox" ||
		items[18].version != 19 || items[18].name != "notification_outbox_lock" ||
		items[19].version != 20 || items[19].name != "file_gateway_fields" ||
		items[20].version != 21 || items[20].name != "oidc_backchannel_logout" ||
		items[21].version != 22 || items[21].name != "source_file_gateway" ||
		items[22].version != 23 || items[22].name != "contract_payment_terms" ||
		items[23].version != 24 || items[23].name != "financial_effective_outbox" ||
		items[24].version != 25 || items[24].name != "handover_protocol" ||
		items[25].version != 26 || items[25].name != "contract_audit_outbox" ||
		items[26].version != 27 || items[26].name != "stamped_document_versions" ||
		items[27].version != 28 || items[27].name != "approval_snapshots" ||
		items[28].version != 29 || items[28].name != "template_version" ||
		items[29].version != 30 || items[29].name != "template_file_lifecycle" ||
		items[30].version != 31 || items[30].name != "template_file_upload_attempt" ||
		items[31].version != 32 || items[31].name != "contract_responsibility" ||
		items[32].version != 33 || items[32].name != "contract_document_source" ||
		items[33].version != 34 || items[33].name != "contract_import_batches" ||
		items[34].version != 35 || items[34].name != "contract_import_history" ||
		items[35].version != 36 || items[35].name != "contract_number_rule" ||
		items[36].version != 37 || items[36].name != "contract_number_registry" ||
		items[37].version != 38 || items[37].name != "oidc_session_idle" ||
		items[38].version != 39 || items[38].name != "con_audit_outbox_resource_index" {
		t.Fatalf("items = %#v", items)
	}
}

// TestSessionIdleMigrationDeclaresInteractiveAndSeenColumns 固定 000038 的列与存量回填语义。
// 空闲基准必须来自显式交互列（last_interactive_at），普通请求列（last_seen_at）只做观测；
// 存量行按 created_at 回填，避免升级后既有会话的空闲窗口因 NULL 语义被错误解释。
// 参数：t 为测试上下文。
// 返回值：无。
func TestSessionIdleMigrationDeclaresInteractiveAndSeenColumns(t *testing.T) {
	items, err := load(migrations.Files)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	var statement string
	for _, item := range items {
		if item.version == 38 {
			statement = item.sql
			break
		}
	}
	if statement == "" {
		t.Fatal("000038 oidc_session_idle 迁移不存在")
	}
	for _, fragment := range []string{
		"last_interactive_at DATETIME(3) NULL",
		"last_seen_at DATETIME(3) NULL",
		"SET last_interactive_at = created_at",
		"SET last_seen_at = created_at",
	} {
		if !strings.Contains(statement, fragment) {
			t.Fatalf("000038 迁移缺少 %q", fragment)
		}
	}
	if strings.Contains(statement, "DROP ") || strings.Contains(statement, "UPDATE con_oidc_session SET revoked_at") {
		t.Fatalf("000038 迁移不得删除结构或撤销既有会话: %s", statement)
	}
}

// TestAuditOutboxResourceIndexMigrationIsGuarded 固定 000039 的守卫式幂等语义：
// 用 information_schema.statistics 判存在性，只加键、不改数据、不删任何结构。
// 参数：t 为测试上下文。
// 返回值：无。
func TestAuditOutboxResourceIndexMigrationIsGuarded(t *testing.T) {
	items, err := load(migrations.Files)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	var statement string
	for _, item := range items {
		if item.version == 39 {
			statement = item.sql
			break
		}
	}
	if statement == "" {
		t.Fatal("000039 con_audit_outbox_resource_index 迁移不存在")
	}
	for _, fragment := range []string{
		"information_schema.statistics",
		"index_name = 'idx_con_audit_outbox_resource'",
		"ADD KEY idx_con_audit_outbox_resource (tenant_id, resource_type, resource_id, action, created_at)",
		"IF index_count = 0 THEN",
	} {
		if !strings.Contains(statement, fragment) {
			t.Fatalf("000039 迁移缺少 %q", fragment)
		}
	}
	for _, forbidden := range []string{"DROP TABLE", "UPDATE con_audit_outbox", "DELETE FROM", "DROP COLUMN", "DROP INDEX"} {
		if strings.Contains(statement, forbidden) {
			t.Fatalf("000039 迁移不得删除结构或改写数据: %q", forbidden)
		}
	}
}

// TestPreflightsReferenceEmbeddedMigrations 固定「预检按版本号注册」的对应关系：
// 注册表里每个版本都必须真实存在于嵌入迁移中，且当前只注册了 37 号（000037 登记表）。
//
// 这条断言替代了原先按迁移名注册的隐患：迁移名拼错、改名或删文件后忘记同步时，
// 按名字查表只会静默跳过预检、没有任何测试会红；现在 Run 会通过 verifyPreflights
// 直接失败，本用例保证这条失败路径不会被“顺手改成 skip”。
//
// 参数：t 为测试上下文。
// 返回值：无。
func TestPreflightsReferenceEmbeddedMigrations(t *testing.T) {
	items, err := load(migrations.Files)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if err := verifyPreflights(preflights, items); err != nil {
		t.Fatalf("预检注册表必须与嵌入迁移一一对应: %v", err)
	}
	for version, preflight := range preflights {
		found := false
		for _, migration := range items {
			if migration.version == version {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("预检注册的版本 %d 在嵌入迁移里不存在", version)
		}
		if preflight == nil {
			t.Fatalf("预检注册的版本 %d 没有处理函数", version)
		}
	}
	if _, exists := preflights[37]; !exists {
		t.Fatal("000037 必须注册存量编号预检")
	}
}

// TestVerifyPreflightsRejectsUnknownVersion 验证注册了不存在的版本号时直接报错，
// 而不是静默跳过预检。
// 参数：t 为测试上下文。
// 返回值：无。
func TestVerifyPreflightsRejectsUnknownVersion(t *testing.T) {
	items, err := load(migrations.Files)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	noop := func(context.Context, *sql.DB) error { return nil }
	registered := map[uint64]func(context.Context, *sql.DB) error{
		37: noop,
		99: noop,
	}
	err = verifyPreflights(registered, items)
	if !errors.Is(err, errUnknownPreflightVersion) {
		t.Fatalf("err = %v want errUnknownPreflightVersion", err)
	}
	if !strings.Contains(err.Error(), "99") {
		t.Fatalf("错误必须指出非法版本号 99: %v", err)
	}
	if err := verifyPreflights(map[uint64]func(context.Context, *sql.DB) error{}, items); err != nil {
		t.Fatalf("空注册表必须通过: %v", err)
	}
}
