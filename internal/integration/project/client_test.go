package project

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type memoryStore struct {
	delivery   Delivery
	delivered  string
	failed     string
	reconciled int
}

func (s *memoryStore) ReconcileProjectDeliveries(context.Context) (int, error) {
	s.reconciled++
	return 2, nil
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

func TestDispatcherCarriesMachineBearerWhenTokenSourceConfigured(t *testing.T) {
	store := &memoryStore{delivery: Delivery{ID: "01KDELIVERY0000000000000000", TenantID: "01KTENANT00000000000000000", Payload: []byte(`{"contract_id":"C-1"}`), Attempts: 1}}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer machine-token-1" {
			t.Fatalf("authorization=%q, want Bearer machine-token-1", request.Header.Get("Authorization"))
		}
		return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	})}
	dispatcher := &Dispatcher{Store: store, BaseURL: "http://project-api:8082", MaxAttempts: 3, Poll: time.Second, Client: client,
		TokenSource: func(context.Context) (string, error) { return "machine-token-1", nil }}
	if err := dispatcher.dispatchOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.delivered != store.delivery.ID {
		t.Fatalf("store=%+v", store)
	}
}

func TestDispatcherRetriesWhenTokenFetchFails(t *testing.T) {
	store := &memoryStore{delivery: Delivery{ID: "01KDELIVERY0000000000000000", TenantID: "01KTENANT00000000000000000", Payload: []byte(`{"contract_id":"C-1"}`), Attempts: 1}}
	dispatcher := &Dispatcher{Store: store, BaseURL: "http://project-api:8082", MaxAttempts: 3, Poll: time.Second,
		TokenSource: func(context.Context) (string, error) { return "", errors.New("token endpoint down") }}
	if err := dispatcher.dispatchOne(context.Background()); err == nil {
		t.Fatal("dispatchOne() succeeded although the token fetch failed")
	}
	if store.delivered != "" || store.failed != store.delivery.ID {
		t.Fatalf("store=%+v", store)
	}
}

func TestDispatcherReconcilesHistoricalContractsBeforeDelivery(t *testing.T) {
	store := &memoryStore{}
	dispatcher := &Dispatcher{Store: store}

	count, err := dispatcher.reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile() error = %v", err)
	}
	if count != 2 || store.reconciled != 1 {
		t.Fatalf("reconcile() count=%d calls=%d, want 2 and 1", count, store.reconciled)
	}
}

func TestDetectionCategoryDirectoryUsesMachineBearerAndInternalReadEndpoint(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/internal/v1/contracts/detection-categories" {
			t.Fatalf("request=%s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer category-token" || request.Header.Get("X-Contract-Delivery-ID") != "" {
			t.Fatalf("headers=%v", request.Header)
		}
		body := `{"code":"OK","data":{"items":[{"category":"等保测评","enabled":true}]}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	directory := &DetectionCategoryDirectory{BaseURL: "http://project-api:8082", Client: client, TokenSource: func(context.Context) (string, error) { return "category-token", nil }}
	items, err := directory.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Category != "等保测评" || !items[0].Enabled {
		t.Fatalf("items=%+v", items)
	}
}

func TestDetectionCategoryDirectoryRejectsNonSuccessAndMissingConfiguration(t *testing.T) {
	if _, err := (&DetectionCategoryDirectory{}).List(context.Background()); err == nil {
		t.Fatal("unconfigured directory unexpectedly succeeded")
	}
	directory := &DetectionCategoryDirectory{
		BaseURL:     "http://project-api:8082",
		TokenSource: func(context.Context) (string, error) { return "category-token", nil },
		Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader(`{"code":"AUTH_FORBIDDEN"}`)), Header: make(http.Header)}, nil
		})},
	}
	if _, err := directory.List(context.Background()); err == nil {
		t.Fatal("forbidden project response unexpectedly succeeded")
	}
}

func TestDetectionCategoryDirectoryRetriesTransientProjectFailure(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		if attempts < 3 {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(`{"code":"PM_NOT_READY"}`)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"data":{"items":[{"category":"等保测评","enabled":true}]}}`)), Header: make(http.Header)}, nil
	})}
	directory := &DetectionCategoryDirectory{BaseURL: "http://project-api:8082", Client: client, TokenSource: func(context.Context) (string, error) { return "category-token", nil }}
	items, err := directory.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if attempts != 3 || len(items) != 1 || items[0].Category != "等保测评" {
		t.Fatalf("attempts=%d items=%+v, want three attempts and one category", attempts, items)
	}
}

func TestDetectionCategoryDirectoryDoesNotRetryForbiddenProjectFailure(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader(`{"code":"AUTH_FORBIDDEN"}`)), Header: make(http.Header)}, nil
	})}
	directory := &DetectionCategoryDirectory{BaseURL: "http://project-api:8082", Client: client, TokenSource: func(context.Context) (string, error) { return "category-token", nil }}
	if _, err := directory.List(context.Background()); err == nil {
		t.Fatal("forbidden project response unexpectedly succeeded")
	}
	if attempts != 1 {
		t.Fatalf("attempts=%d, want one attempt for a non-transient authorization failure", attempts)
	}
}
