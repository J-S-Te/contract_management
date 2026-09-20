package crm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
)

func TestCRMTokenSourceRequestsOnlyContractReferenceScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		clientID, secret, ok := request.BasicAuth()
		if !ok || clientID != "contract-reader" || secret != "secret" || request.FormValue("grant_type") != "client_credentials" || request.FormValue("scope") != contractReferenceScope || request.FormValue("audience") != "" {
			t.Fatalf("unexpected token request client=%q form=%v", clientID, request.Form)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"access_token":"token-1","token_type":"Bearer","expires_in":300,"scope":"customer.contract_reference.read"}`)
	}))
	defer server.Close()
	source := NewClientCredentialsTokenSource(context.Background(), server.URL, "contract-reader", "secret", contractReferenceScope)
	token, err := source(context.Background())
	if err != nil || token != "token-1" {
		t.Fatalf("token=%q err=%v", token, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestContractReferenceDirectorySendsLeastPrivilegeReplayHeaders(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/internal/contract-references/customers/7" || request.URL.Query().Get("opportunity_id") != "9" {
			t.Fatalf("request=%s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer machine-token" || request.Header.Get("X-Integration-Timestamp") != "2026-09-20T01:02:03Z" || request.Header.Get("X-Integration-Nonce") != "nonce-1" || request.Header.Get("X-Actor-Identity-ID") != "identity-1" {
			t.Fatalf("headers=%v", request.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"code":"OK","message":"success","request_id":"r1","data":{"customer":{"id":7,"name":"权威客户","status":"ACTIVE"},"opportunity":{"id":9,"name":"权威商机","customer_id":7,"status":"FOLLOWING"}}}`)), Header: make(http.Header)}, nil
	})}
	directory := &ContractReferenceDirectory{BaseURL: "https://crm.example", Client: client,
		TokenSource: func(context.Context) (string, error) { return "machine-token", nil },
		Now:         func() time.Time { return time.Date(2026, 9, 20, 1, 2, 3, 0, time.UTC) }, Nonce: func() (string, error) { return "nonce-1", nil }}
	result, err := directory.Resolve(context.Background(), 7, "9", "identity-1")
	if err != nil || result.Customer.Name != "权威客户" || result.Opportunity == nil || result.Opportunity.Name != "权威商机" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestContractReferenceDirectoryClassifiesInvalidAndDependencyFailures(t *testing.T) {
	for _, test := range []struct {
		status int
		want   error
	}{
		{status: http.StatusUnprocessableEntity, want: application.ErrCRMReferenceInvalid},
		{status: http.StatusNotFound, want: application.ErrCRMReferenceInvalid},
		{status: http.StatusUnauthorized},
	} {
		directory := &ContractReferenceDirectory{BaseURL: "https://crm.example", TokenSource: func(context.Context) (string, error) { return "token", nil }, Nonce: func() (string, error) { return "nonce", nil }, Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
		})}}
		_, err := directory.Resolve(context.Background(), 7, "", "identity-1")
		if test.want != nil && !errors.Is(err, test.want) {
			t.Fatalf("status=%d error=%v want=%v", test.status, err, test.want)
		}
		if test.want == nil && (err == nil || errors.Is(err, application.ErrCRMReferenceInvalid)) {
			t.Fatalf("status=%d error=%v, want dependency failure", test.status, err)
		}
	}
}
