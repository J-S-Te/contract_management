package httpapi

import "testing"

func TestStampedUploadRequiresExplicitExpectedVersion(t *testing.T) {
	for _, raw := range []string{"", "  ", "abc", "-1", "1.5"} {
		if _, err := parseStampedExpectedVersion(raw); err == nil {
			t.Fatalf("expected_version=%q accepted, want validation error", raw)
		}
	}
	for raw, want := range map[string]uint64{"0": 0, "1": 1, " 42 ": 42} {
		got, err := parseStampedExpectedVersion(raw)
		if err != nil || got != want {
			t.Fatalf("expected_version=%q got=%d err=%v, want %d", raw, got, err, want)
		}
	}
}
