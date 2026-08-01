package mysql

import (
	"testing"
	"time"
)

func TestFormatContractNumberUsesTemplateSnapshot(t *testing.T) {
	now := time.Date(2026, time.August, 1, 8, 0, 0, 0, time.UTC)
	got := formatContractNumber("CON-{YYYY}-{MM}{DD}-{ID8}", "01K000000000000000ABCDEF12", now)
	if got != "CON-2026-0801-ABCDEF12" {
		t.Fatalf("formatContractNumber() = %q", got)
	}
}
