package mysql

import "testing"

func TestSigningNotificationKeyIsStablePerContractEvent(t *testing.T) {
	contractID := "contract-1"
	if got, want := signingNotificationKey(contractID, "shipped"), "contract-1:signing:shipped"; got != want {
		t.Fatalf("shipped key = %q, want %q", got, want)
	}
	if got := signingNotificationKey(contractID, "shipped"); got != signingNotificationKey(contractID, "shipped") {
		t.Fatalf("repeating the same signing event changed its dedupe key")
	}
	if got := signingNotificationKey(contractID, "received"); got == signingNotificationKey(contractID, "confirmed") {
		t.Fatalf("different signing events must not share a dedupe key")
	}
}
