package settlement

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type financialDeliveryTestStore struct {
	delivery          Delivery
	delivered, failed string
	dead              bool
	claims            int
}

func (s *financialDeliveryTestStore) ClaimFinancialEffectiveDelivery(context.Context) (Delivery, bool, error) {
	s.claims++
	return s.delivery, true, nil
}
func (s *financialDeliveryTestStore) MarkFinancialEffectiveDelivered(_ context.Context, id string) error {
	s.delivered = id
	return nil
}
func (s *financialDeliveryTestStore) MarkFinancialEffectiveFailed(_ context.Context, id, _ string, _ uint, dead bool) error {
	s.failed, s.dead = id, dead
	return nil
}

type financialRoundTrip func(*http.Request) (*http.Response, error)

func (f financialRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFinancialDispatcherUsesExactSettlementEndpointAndFrozenPayload(t *testing.T) {
	payload := `{"event_id":"event","event_type":"contract.financial_effective.v1","tenant_id":"tenant","contract":{"id":"contract","version":5,"amount":"100.00","currency":"CNY","payment_terms":[{"installment_no":1,"due_date":"2026-12-01","amount":"100.00"}]}}`
	store := &financialDeliveryTestStore{delivery: Delivery{ID: "event", TenantID: "tenant", Payload: []byte(payload), Attempts: 1}}
	calls := 0
	client := &http.Client{Transport: financialRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != payload || r.Method != http.MethodPost || r.URL.String() != "http://settlement-api:8085/internal/v1/settlement/events/contracts" {
			t.Fatalf("unexpected financial delivery request: %v", err)
		}
		if r.Header.Get("Authorization") != "Bearer test-only-machine-token" || r.Header.Get("X-Financial-Effective-Tenant-ID") != "tenant" || r.Header.Get("X-Financial-Effective-Delivery-ID") != "event" {
			t.Fatal("machine binding metadata missing")
		}
		return &http.Response{StatusCode: 202, Body: io.NopCloser(strings.NewReader(`{"accepted":true}`)), Header: make(http.Header)}, nil
	})}
	d := &Dispatcher{Store: store, BaseURL: "http://settlement-api:8085", MaxAttempts: 3, Client: client, TokenSource: func(context.Context) (string, error) { return "test-only-machine-token", nil }}
	if err := d.dispatchOne(context.Background()); err != nil || calls != 1 || store.delivered != "event" || store.failed != "" {
		t.Fatal("financial event was not delivered exactly once", err)
	}
}

func TestFinancialDispatcherPersistsTokenAndReceiverFailures(t *testing.T) {
	for _, tokenFailure := range []bool{true, false} {
		store := &financialDeliveryTestStore{delivery: Delivery{ID: "event", TenantID: "tenant", Payload: []byte(`{}`), Attempts: 3}}
		calls := 0
		client := &http.Client{Transport: financialRoundTrip(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 422, Body: io.NopCloser(strings.NewReader("invalid schedule")), Header: make(http.Header)}, nil
		})}
		d := &Dispatcher{Store: store, BaseURL: "http://settlement-api:8085", MaxAttempts: 3, Client: client, TokenSource: func(context.Context) (string, error) {
			if tokenFailure {
				return "", errors.New("test token endpoint unavailable")
			}
			return "test-only-machine-token", nil
		}}
		if err := d.dispatchOne(context.Background()); err == nil || store.failed != "event" || !store.dead || store.delivered != "" || tokenFailure && calls != 0 || !tokenFailure && calls != 1 {
			t.Fatal("failure was swallowed or falsely acknowledged")
		}
	}
}
