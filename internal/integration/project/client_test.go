package project

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type memoryStore struct {
	delivery  Delivery
	delivered string
	failed    string
}

func (s *memoryStore) ClaimProjectDelivery(context.Context) (Delivery, bool, error) {
	return s.delivery, true, nil
}
func (s *memoryStore) MarkProjectDeliveryDelivered(_ context.Context, id string) error {
	s.delivered = id
	return nil
}
func (s *memoryStore) MarkProjectDeliveryFailed(_ context.Context, id, _ string, _ uint, _ bool) error {
	s.failed = id
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestDispatcherPostsActivationPayloadOverInternalNetwork(t *testing.T) {
	store := &memoryStore{delivery: Delivery{ID: "01KDELIVERY0000000000000000", TenantID: "01KTENANT00000000000000000", Payload: []byte(`{"contract_id":"C-1"}`), Attempts: 1}}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil || string(body) != string(store.delivery.Payload) {
			t.Fatalf("body=%s err=%v", body, err)
		}
		if request.Header.Get("X-Contract-Delivery-ID") != store.delivery.ID || request.Header.Get("X-Contract-Tenant-ID") != store.delivery.TenantID {
			t.Fatalf("headers=%v", request.Header)
		}
		if request.URL.Path != "/internal/v1/contracts/activate" {
			t.Fatalf("path=%s", request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	})}
	dispatcher := &Dispatcher{Store: store, BaseURL: "http://project-api:8082", MaxAttempts: 3, Poll: time.Second, Client: client}
	if err := dispatcher.dispatchOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.delivered != store.delivery.ID || store.failed != "" {
		t.Fatalf("store=%+v", store)
	}
}
