package notification

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func strPtr(s string) *string { return &s }

func TestPriorityFor(t *testing.T) {
	cases := map[string]string{
		"signing_pending":  "HIGH",
		"expired":          "HIGH",
		"pending_approval": "NORMAL",
		"approved":         "NORMAL",
		"status_change":    "NORMAL",
	}
	for in, want := range cases {
		if got := priorityFor(in); got != want {
			t.Fatalf("priorityFor(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestTargetAndReferenceMapping(t *testing.T) {
	contractID := strPtr("c-1")
	approvalID := strPtr("a-1")

	withContract := Delivery{ContractID: contractID, ApprovalID: approvalID}
	if got := targetURLFor(withContract); got != "/contract_management/contracts/c-1" {
		t.Fatalf("targetURLFor=%q", got)
	}
	if got := referenceTypeFor(withContract); got != "CONTRACT" {
		t.Fatalf("referenceTypeFor=%q", got)
	}
	if got := referenceIDFor(withContract); got != "c-1" {
		t.Fatalf("referenceIDFor=%q", got)
	}

	approvalOnly := Delivery{ApprovalID: approvalID}
	if got := targetURLFor(approvalOnly); got != "/contract_management/approvals/a-1" {
		t.Fatalf("targetURLFor(approval)=%q", got)
	}
	if got := referenceTypeFor(approvalOnly); got != "APPROVAL" {
		t.Fatalf("referenceTypeFor(approval)=%q", got)
	}
}

func TestResolveRecipients(t *testing.T) {
	d := Dispatcher{}
	userID := strPtr("u-1")
	if got, err := d.resolveRecipients(context.Background(), Delivery{RecipientUserID: userID}); err != nil || len(got) != 1 || got[0] != "u-1" {
		t.Fatalf("user recipient got=%v err=%v", got, err)
	}
	role := strPtr("contract_specialist")
	d.ResolveRoleRecipients = func(_ context.Context, _ string, codes []string) ([]string, error) {
		if len(codes) != 1 || codes[0] != "contract_specialist" {
			t.Fatalf("role codes=%v", codes)
		}
		return []string{"u-1", "u-2"}, nil
	}
	if got, err := d.resolveRecipients(context.Background(), Delivery{RecipientRoleCode: role}); err != nil || len(got) != 2 {
		t.Fatalf("role recipient got=%v err=%v", got, err)
	}
	missing := Dispatcher{}
	if _, err := missing.resolveRecipients(context.Background(), Delivery{RecipientRoleCode: role}); err == nil {
		// 无 resolver 时 role 收件人必须报错。
		t.Fatalf("expected error when role resolver missing")
	}
}

type fakeStore struct {
	delivery  Delivery
	found     bool
	delivered string
	failed    string
}

func (f *fakeStore) ClaimNotificationDelivery(context.Context) (Delivery, bool, error) {
	return f.delivery, f.found, nil
}
func (f *fakeStore) MarkNotificationDelivered(_ context.Context, id string) error {
	f.delivered = id
	return nil
}
func (f *fakeStore) MarkNotificationFailed(_ context.Context, id, _ string, _ uint, _ bool) error {
	f.failed = id
	return errors.New("should not fail")
}

func TestDispatchOnePostsIngestionEvent(t *testing.T) {
	var received ingestionEventPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/notifications/events" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Fatalf("missing bearer")
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Fatalf("bad payload: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	store := &fakeStore{
		found: true,
		delivery: Delivery{
			ID: "n-1", TenantID: "t-1", RecipientKey: "user:u-1", RecipientUserID: strPtr("u-1"),
			NotificationType: "approved", Title: "合同审批已通过", Content: "合同已批准并生效",
			ContractID: strPtr("c-1"), DedupeKey: "a-1:approved", Attempts: 1,
			CreatedAt: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		},
	}
	d := &Dispatcher{
		Store: store, BaseURL: server.URL, MaxAttempts: 20,
		TokenSource: func(context.Context) (string, error) { return "tok", nil },
	}
	if err := d.dispatchOne(context.Background()); err != nil {
		t.Fatalf("dispatchOne: %v", err)
	}
	if store.delivered != "n-1" {
		t.Fatalf("expected delivered n-1, got %q", store.delivered)
	}
	identity, err := notificationDeliveryIdentity(store.delivery)
	if err != nil {
		t.Fatal(err)
	}
	if received.EventType != "approved" || received.NotificationScope != "CROSS_SYSTEM" ||
		received.EventID != identity || received.IdempotencyKey != identity ||
		len(received.Recipients) != 1 || received.Recipients[0] != "u-1" ||
		received.TargetURL != "/contract_management/contracts/c-1" || received.ReferenceType != "CONTRACT" {
		t.Fatalf("unexpected payload: %+v", received)
	}
	if !strings.Contains(received.Title, "审批") {
		t.Fatalf("title not carried: %q", received.Title)
	}
}

func TestNotificationIdentityMatchesIndependentPlatformContract(t *testing.T) {
	// Primary platform application/template.go contract, independent of the
	// implementation's hash helper: event codes start with a letter and the
	// application/environment-prefixed storage key must fit 128 bytes.
	platformCode := regexp.MustCompile(`^[A-Z][A-Z0-9_:-]{0,127}$`)
	for _, recipient := range []string{"user:01M4HVMMS8VPV95ZV6ZNGDME9R", "role:contract_specialist"} {
		for _, key := range []string{"01M4JCXT8BHT6WTV28EFAA8S5N:approved", strings.Repeat("很长:业务:key\x00", 1000)} {
			delivery := Delivery{TenantID: "owned", DedupeKey: key, RecipientKey: recipient, Attempts: 1}
			identity, err := notificationDeliveryIdentity(delivery)
			if err != nil {
				t.Fatal(err)
			}
			if !platformCode.MatchString(identity) || len(identity) > 128 || len("contract_management:prod:"+identity) > 128 {
				t.Fatalf("identity violates platform contract: %q", identity)
			}
			delivery.Attempts++
			retry, err := notificationDeliveryIdentity(delivery)
			if err != nil || retry != identity {
				t.Fatal("retry changed notification identity")
			}
		}
	}
}

func TestNotificationIdentitySeparatesRecipientAndFramedTuple(t *testing.T) {
	seen := map[string]bool{}
	for _, delivery := range []Delivery{
		{TenantID: "owned", DedupeKey: "same:event", RecipientKey: "user:one"},
		{TenantID: "owned", DedupeKey: "same:event", RecipientKey: "user:two"},
		{TenantID: "owned", DedupeKey: "same:event", RecipientKey: "role:contract_specialist"},
		{TenantID: "owned", DedupeKey: "a:b", RecipientKey: "c"},
		{TenantID: "owned", DedupeKey: "a", RecipientKey: "b:c"},
		{TenantID: "other", DedupeKey: "same:event", RecipientKey: "user:one"},
	} {
		identity, err := notificationDeliveryIdentity(delivery)
		if err != nil {
			t.Fatal(err)
		}
		if seen[identity] {
			t.Fatal("distinct recipient or framed tuple collided")
		}
		seen[identity] = true
	}
}

func TestNonSuccessfulIngestionRemainsRetryFailure(t *testing.T) {
	for _, status := range []int{422, 409, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			store := &fakeStore{found: true, delivery: Delivery{ID: "owned", TenantID: "tenant", DedupeKey: "event", RecipientKey: "user:one", RecipientUserID: strPtr("one")}}
			d := Dispatcher{Store: store, BaseURL: server.URL, MaxAttempts: 20}
			if err := d.dispatchOne(context.Background()); err == nil || store.failed != "owned" || store.delivered != "" {
				t.Fatal("non-successful ingestion did not remain a retry failure")
			}
		})
	}
}

func TestIncompleteIdentityFailsBeforeSending(t *testing.T) {
	for _, field := range []string{"tenant", "dedupe", "recipient"} {
		delivery := Delivery{TenantID: "owned", DedupeKey: "event", RecipientKey: "user:one"}
		switch field {
		case "tenant":
			delivery.TenantID = ""
		case "dedupe":
			delivery.DedupeKey = ""
		case "recipient":
			delivery.RecipientKey = ""
		}
		if _, err := notificationDeliveryIdentity(delivery); err == nil {
			t.Fatal("incomplete identity accepted")
		}
	}
}

func TestIngestionContractRetriedRowsAndSeparateRecipients(t *testing.T) {
	platformCode := regexp.MustCompile(`^[A-Z][A-Z0-9_:-]{0,127}$`)
	accepted := map[string]string{}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload ingestionEventPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || !platformCode.MatchString(payload.EventID) || len("contract_management:prod:"+payload.IdempotencyKey) > 128 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		if len(payload.Recipients) != 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		if prior, ok := accepted[payload.IdempotencyKey]; ok && prior != payload.Recipients[0] {
			w.WriteHeader(http.StatusConflict)
			return
		}
		accepted[payload.IdempotencyKey] = payload.Recipients[0]
		requests++
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	store := &fakeStore{found: true, delivery: Delivery{ID: "row-one", TenantID: "owned", DedupeKey: "01M4JCXT8BHT6WTV28EFAA8S5N:approved", RecipientKey: "user:one", RecipientUserID: strPtr("one")}}
	d := Dispatcher{Store: store, BaseURL: server.URL, MaxAttempts: 20}
	for attempt := 1; attempt <= 2; attempt++ {
		store.delivery.Attempts = uint(attempt)
		if err := d.dispatchOne(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	store.delivery.ID, store.delivery.RecipientKey, store.delivery.RecipientUserID = "row-two", "user:two", strPtr("two")
	if err := d.dispatchOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests != 3 || len(accepted) != 2 || store.delivered != "row-two" {
		t.Fatal("retry dedupe or multi-recipient ingestion contract violated")
	}
}
