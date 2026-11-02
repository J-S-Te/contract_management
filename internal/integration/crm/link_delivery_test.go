package crm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
)

const crmLinkTestNonce = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

type linkStoreStub struct {
	delivery  LinkDelivery
	claimed   bool
	delivered string
	failure   string
	attempts  uint
	dead      bool
	claimErr  error
}

func (s *linkStoreStub) ClaimOpportunityLink(context.Context) (LinkDelivery, bool, error) {
	if s.claimErr != nil {
		return LinkDelivery{}, false, s.claimErr
	}
	if !s.claimed {
		return LinkDelivery{}, false, nil
	}
	s.claimed = false
	return s.delivery, true, nil
}

func (s *linkStoreStub) MarkOpportunityLinkDelivered(_ context.Context, id string) error {
	s.delivered = id
	return nil
}

func (s *linkStoreStub) MarkOpportunityLinkFailed(_ context.Context, id, reason string, attempts uint, dead bool) error {
	s.failure = reason
	s.attempts = attempts
	s.dead = dead
	return nil
}

func linkPayload(t *testing.T, opportunityID uint64, eventID string) []byte {
	t.Helper()
	payload, err := json.Marshal(OpportunityLinkCallback{EventID: eventID, IntakeID: "intake-1", OpportunityID: opportunityID, Status: application.OpportunityIntakeLinkConfirmed})
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	return payload
}

func TestDispatcherUsesClientCredentialsAndIntegrationHeaders(t *testing.T) {
	fixedNow := time.Date(2026, 10, 30, 6, 7, 8, 123456789, time.UTC)
	tokenRequestSeen := false
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenRequestSeen = true
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token request form: %v", err)
		} else {
			if user, password, ok := r.BasicAuth(); !ok || user != "contract_management-prod-crm-contract-link" || password != "test-only-secret" {
				t.Errorf("token request basic auth = %q/%q/%v", user, password, ok)
			}
			if got := r.Form.Get("grant_type"); got != "client_credentials" {
				t.Errorf("token grant_type = %q", got)
			}
			if got := r.Form.Get("scope"); got != "opportunity.signed.write" {
				t.Errorf("token scope = %q", got)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"machine-token","token_type":"Bearer","expires_in":3600}`)
	}))
	defer tokenServer.Close()

	var crmRequest *http.Request
	crmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		crmRequest = r
		w.WriteHeader(http.StatusAccepted)
	}))
	defer crmServer.Close()

	store := &linkStoreStub{claimed: true, delivery: LinkDelivery{ID: "delivery-1", TenantID: "tenant-1", Payload: linkPayload(t, 42, "event-1")}}
	dispatcher := &Dispatcher{
		Store: store, BaseURL: crmServer.URL + "/customer-opportunity", MaxAttempts: 20,
		TokenSource: NewClientCredentialsTokenSource(context.Background(), tokenServer.URL, "contract_management-prod-crm-contract-link", "test-only-secret", "opportunity.signed.write"),
		Now:         func() time.Time { return fixedNow },
		Nonce:       func() (string, error) { return crmLinkTestNonce, nil },
	}
	if err := dispatcher.dispatchOne(context.Background()); err != nil {
		t.Fatalf("dispatchOne() error = %v", err)
	}
	if store.delivered != "delivery-1" || store.failure != "" {
		t.Fatalf("delivered = %q, failure = %q", store.delivered, store.failure)
	}

	if !tokenRequestSeen {
		t.Fatal("token endpoint was not called")
	}

	if crmRequest == nil {
		t.Fatal("CRM endpoint was not called")
	}
	if crmRequest.URL.Path != "/customer-opportunity/api/v1/internal/opportunities/42/contract-link" {
		t.Fatalf("CRM path = %q", crmRequest.URL.Path)
	}
	if got := crmRequest.Header.Get("Authorization"); got != "Bearer machine-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := crmRequest.Header.Get("X-Integration-Timestamp"); got != fixedNow.Format(time.RFC3339Nano) {
		t.Fatalf("X-Integration-Timestamp = %q", got)
	}
	nonce := crmRequest.Header.Get("X-Integration-Nonce")
	raw, err := base64.RawURLEncoding.DecodeString(nonce)
	if err != nil || len(raw) != 32 {
		t.Fatalf("X-Integration-Nonce = %q (decoded %d bytes, err %v)", nonce, len(raw), err)
	}
	if got := crmRequest.Header.Get("Idempotency-Key"); got != "event-1" {
		t.Fatalf("Idempotency-Key = %q", got)
	}
}

func TestDispatcherRetriesAndDeadLettersOnCRMRejection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attempts uint
		dead     bool
	}{
		{"retry", 1, false},
		{"dead letter", 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			crmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer crmServer.Close()
			store := &linkStoreStub{claimed: true, delivery: LinkDelivery{ID: "delivery-2", Payload: linkPayload(t, 7, "event-2"), Attempts: tc.attempts}}
			dispatcher := &Dispatcher{
				Store: store, BaseURL: crmServer.URL, MaxAttempts: 3,
				TokenSource: func(context.Context) (string, error) { return "machine-token", nil },
			}
			err := dispatcher.dispatchOne(context.Background())
			if err == nil || !strings.Contains(err.Error(), "401") {
				t.Fatalf("dispatchOne() error = %v, want CRM 401", err)
			}
			if store.delivered != "" || !strings.Contains(store.failure, "401") {
				t.Fatalf("delivered = %q, failure = %q", store.delivered, store.failure)
			}
			if store.attempts != tc.attempts || store.dead != tc.dead {
				t.Fatalf("attempts = %d, dead = %v; want %d/%v", store.attempts, store.dead, tc.attempts, tc.dead)
			}
		})
	}
}

func TestDispatcherRecordsTokenFailureWithoutSendingRequest(t *testing.T) {
	requests := 0
	crmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer crmServer.Close()
	store := &linkStoreStub{claimed: true, delivery: LinkDelivery{ID: "delivery-3", Payload: linkPayload(t, 9, "event-3")}}
	dispatcher := &Dispatcher{
		Store: store, BaseURL: crmServer.URL, MaxAttempts: 20,
		TokenSource: func(context.Context) (string, error) { return "", io.ErrUnexpectedEOF },
	}
	err := dispatcher.dispatchOne(context.Background())
	if err == nil || !strings.Contains(err.Error(), "obtain CRM link machine token") {
		t.Fatalf("dispatchOne() error = %v, want token acquisition error", err)
	}
	if requests != 0 {
		t.Fatalf("CRM received %d requests, want 0 without a token", requests)
	}
	if !strings.Contains(store.failure, "obtain CRM link machine token") || store.dead {
		t.Fatalf("failure = %q, dead = %v", store.failure, store.dead)
	}
}

func TestDispatcherFailsClosedWithoutTokenSource(t *testing.T) {
	requests := 0
	crmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer crmServer.Close()
	store := &linkStoreStub{claimed: true, delivery: LinkDelivery{ID: "delivery-4", Payload: linkPayload(t, 11, "event-4")}}
	dispatcher := &Dispatcher{Store: store, BaseURL: crmServer.URL, MaxAttempts: 20}
	if err := dispatcher.dispatchOne(context.Background()); err == nil {
		t.Fatal("dispatchOne() error = nil, want missing token source error")
	}
	if requests != 0 || store.delivered != "" {
		t.Fatalf("requests = %d, delivered = %q", requests, store.delivered)
	}
}

func TestDispatcherRunLogsDisabledWhenBaseURLIsEmpty(t *testing.T) {
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	store := &linkStoreStub{}
	done := make(chan struct{})
	go func() {
		(&Dispatcher{Store: store, Logger: logger}).Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run() did not return for empty BaseURL")
	}
	if !strings.Contains(logs.String(), "CRM link integration disabled") {
		t.Fatalf("startup log = %q, want disabled notice", logs.String())
	}
}

func TestLinkNotifierSendsIntegrationHeadersAndReportsRejection(t *testing.T) {
	var request *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request = r
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	fixedNow := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	notifier := &LinkNotifier{
		BaseURL: server.URL, Client: server.Client(),
		TokenSource: func(context.Context) (string, error) { return "machine-token", nil },
		Now:         func() time.Time { return fixedNow },
		Nonce:       func() (string, error) { return crmLinkTestNonce, nil },
	}
	err := notifier.NotifyOpportunityLink(context.Background(), application.OpportunityIntake{EventID: "event-5", OpportunityID: 5, Status: application.OpportunityIntakeLinkConfirmed})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("NotifyOpportunityLink() error = %v, want CRM 401", err)
	}
	if request == nil || request.Header.Get("Authorization") != "Bearer machine-token" ||
		request.Header.Get("X-Integration-Timestamp") != fixedNow.Format(time.RFC3339Nano) ||
		request.Header.Get("X-Integration-Nonce") != crmLinkTestNonce {
		t.Fatalf("request headers = %#v", request)
	}
}
