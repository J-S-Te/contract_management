package filegatewayclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReleaseBindingUsesExactAuthorizedResourceIdentity(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/api/v1/files/file-old/bindings/release" || r.Header.Get("Authorization") != "Bearer machine" {
			t.Errorf("wrong release request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload["application_id"] != "contract" || payload["resource_type"] != "contract_template_source" || payload["resource_id"] != "source-old" || payload["binding_type"] != "TEMPLATE_SOURCE" {
			t.Errorf("wrong release identity: %+v", payload)
		}
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client(), func(context.Context) (string, error) { return "machine", nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ReleaseBinding(context.Background(), "contract", "file-old", "contract_template_source", "source-old", "TEMPLATE_SOURCE"); err == nil {
		t.Fatal("HTTP 503 swallowed")
	}
	if err := client.ReleaseBinding(context.Background(), "contract", "file-old", "contract_template_source", "source-old", "TEMPLATE_SOURCE"); err != nil {
		t.Fatal(err)
	}
}

func TestRetireCleanedUploadReturnsOriginalSessionIdentityWithoutReadingBytes(t *testing.T) {
	var retireCalls, releaseCalls, contentCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/file-upload-requests/export-key/retire":
			if r.Method != http.MethodPost {
				t.Error("retirement must be POST")
			}
			retireCalls++
			w.Header().Set("Content-Type", "application/json")
			// READY is the original session outcome. Its bytes may already have
			// been cleaned; this response only supplies an identity to release.
			_, _ = w.Write([]byte(`{"data":{"request_id":"export-key","file_id":"cleaned-file","status":"READY","retired":true}}`))
		case "/api/v1/files/cleaned-file/bindings/release":
			releaseCalls++
			w.WriteHeader(204)
		default:
			contentCalls++
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, server.Client(), func(context.Context) (string, error) { return "machine", nil })
	if err != nil {
		t.Fatal(err)
	}
	id, status, err := client.RetireUploadRequest(context.Background(), "export-key")
	if err != nil || id != "cleaned-file" || status != "READY" {
		t.Fatalf("retirement id=%s status=%s err=%v", id, status, err)
	}
	if err := client.ReleaseBinding(context.Background(), "contract", id, "contract_template_source", "old-source", "TEMPLATE_SOURCE"); err != nil {
		t.Fatal(err)
	}
	if retireCalls != 1 || releaseCalls != 1 || contentCalls != 0 {
		t.Fatalf("retire=%d release=%d content=%d", retireCalls, releaseCalls, contentCalls)
	}
}
