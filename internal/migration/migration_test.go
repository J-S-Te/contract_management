package migration

import (
	"testing"

	"github.com/j-s-te/contract-management/migrations"
)

func TestEmbeddedMigrationsAreContiguous(t *testing.T) {
	items, err := load(migrations.Files)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if len(items) != 17 ||
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
		items[15].version != 16 || items[15].name != "opportunity_intakes" ||
		items[16].version != 17 || items[16].name != "opportunity_link_outbox" {
		t.Fatalf("items = %#v", items)
	}
}
