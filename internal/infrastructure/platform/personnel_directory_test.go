package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
)

func TestPersonnelDirectoryUsesMachineScopeAndRoleFilter(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/token":
			if err := r.ParseForm(); err != nil || r.Form.Get("scope") != "owner_directory.read" {
				t.Fatalf("token form=%v err=%v", r.Form, err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"machine-token","token_type":"Bearer","scope":"owner_directory.read","expires_in":300}`))
		case "/api/v1/internal/owner-directory":
			if r.Header.Get("Authorization") != "Bearer machine-token" {
				t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
			}
			role := r.URL.Query().Get("role_code")
			if role != "sales_director" && role != "finance_director" {
				t.Fatalf("role=%q", role)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"items":[{"user_id":"user-1","display_name":"负责人甲"}],"page":1,"page_size":50,"total":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	directory := NewPersonnelDirectory(server.URL, "directory-client", "secret", time.Second)
	users, err := directory.ListEligibleUsers(context.Background(), application.Principal{TenantID: "tenant-1"}, []string{"sales_director", "finance_director"})
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].UserID != "user-1" || strings.Join(users[0].Roles, ",") != "finance_director,sales_director" {
		t.Fatalf("users=%+v", users)
	}
}

func TestPersonnelDirectoryFailsClosedOnPlatformError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	directory := NewPersonnelDirectory(server.URL, "client", "secret", time.Second)
	if _, err := directory.ListEligibleUsers(context.Background(), application.Principal{TenantID: "tenant-1"}, []string{"sales_director"}); err == nil {
		t.Fatal("error=nil")
	}
}
