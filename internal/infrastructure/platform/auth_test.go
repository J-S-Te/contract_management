package platform

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuthenticatorMapsPlatformContractPermissions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/api/v1/auth/me", request.URL.Path)
		cookie, err := request.Cookie("bp_session")
		require.NoError(t, err)
		require.Equal(t, "session-token", cookie.Value)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"code":"OK",
			"data":{
				"tenant":{"id":"tenant-1","name":"Tenant"},
				"user":{"id":"user-1","name":"User"},
				"permission_codes":[
					"platform:contract:create",
					"platform:approval_rule:manage"
				]
			}
		}`))
	}))
	defer server.Close()

	incoming := httptest.NewRequest(http.MethodGet, "/api/v1/contracts", nil)
	incoming.AddCookie(&http.Cookie{Name: "bp_session", Value: "session-token"})

	principal, err := NewAuthenticator(server.URL, "bp_session").Authenticate(incoming.Context(), incoming)

	require.NoError(t, err)
	require.Equal(t, "tenant-1", principal.TenantID)
	require.Equal(t, "user-1", principal.UserID)
	require.True(t, principal.Has("contract.create"))
	require.True(t, principal.Has("approval_rule.manage"))
}
