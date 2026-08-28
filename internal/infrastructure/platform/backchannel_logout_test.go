package platform

import (
	"encoding/json"
	"testing"
	"time"
)

func TestValidBackchannelClaims(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	claims := backchannelLogoutClaims{
		Subject: "subject-1", JTI: "jti-1", Issued: now.Unix() - 10, Expires: now.Unix() + 60,
		Audience: "contract_management", Events: map[string]json.RawMessage{backchannelLogoutEvent: json.RawMessage(`{}`)},
	}
	if !validBackchannelClaims(claims, "contract_management", now) {
		t.Fatal("valid logout claims were rejected")
	}
	claims.Events[backchannelLogoutEvent] = json.RawMessage(`{"sid":"unexpected"}`)
	if validBackchannelClaims(claims, "contract_management", now) {
		t.Fatal("logout event with event properties was accepted")
	}
}

func TestBackchannelClaimsRejectsLongTTLAndMissingSubject(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	base := backchannelLogoutClaims{Subject: "s", JTI: "j", Issued: now.Unix() - 1, Expires: now.Unix() + 301, Audience: "c", Events: map[string]json.RawMessage{backchannelLogoutEvent: json.RawMessage(`{}`)}}
	if validBackchannelClaims(base, "c", now) {
		t.Fatal("long-lived logout token was accepted")
	}
	base.Expires = now.Unix() + 10
	base.Subject = ""
	if validBackchannelClaims(base, "c", now) {
		t.Fatal("logout token without subject was accepted")
	}
}
