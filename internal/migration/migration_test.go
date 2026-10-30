package migration

import (
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
	if len(items) != 35 ||
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
		items[34].version != 35 || items[34].name != "contract_import_history" {
		t.Fatalf("items = %#v", items)
	}
}
