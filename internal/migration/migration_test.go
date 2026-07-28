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
	if len(items) != 1 || items[0].version != 1 || items[0].name != "contract_workflow" {
		t.Fatalf("items = %#v", items)
	}
}
