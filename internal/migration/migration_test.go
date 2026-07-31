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
	if len(items) != 2 ||
		items[0].version != 1 || items[0].name != "contract_workflow" ||
		items[1].version != 2 || items[1].name != "single_active_status_change" {
		t.Fatalf("items = %#v", items)
	}
}
