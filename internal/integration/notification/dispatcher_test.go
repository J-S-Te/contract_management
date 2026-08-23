package notification

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
			ID: "n-1", TenantID: "t-1", RecipientUserID: strPtr("u-1"),
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
	if received.EventType != "approved" || received.NotificationScope != "CROSS_SYSTEM" ||
		received.EventID != "a-1:approved" || received.IdempotencyKey != "a-1:approved" ||
		len(received.Recipients) != 1 || received.Recipients[0] != "u-1" ||
		received.TargetURL != "/contract_management/contracts/c-1" || received.ReferenceType != "CONTRACT" {
		t.Fatalf("unexpected payload: %+v", received)
	}
	if !strings.Contains(received.Title, "审批") {
		t.Fatalf("title not carried: %q", received.Title)
	}
}
