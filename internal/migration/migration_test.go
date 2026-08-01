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
	if len(items) != 6 ||
		items[0].version != 1 || items[0].name != "contract_workflow" ||
		items[1].version != 2 || items[1].name != "single_active_status_change" ||
		items[2].version != 3 || items[2].name != "username_snapshots" ||
		items[3].version != 4 || items[3].name != "display_name_snapshots" ||
		items[4].version != 5 || items[4].name != "contract_templates" ||
		items[5].version != 6 || items[5].name != "contract_start_date" {
		t.Fatalf("items = %#v", items)
	}
}
