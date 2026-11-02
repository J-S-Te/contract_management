package platform

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// keycloakTestScope 只是单元测试里的占位范围，不代表任何已下发的业务 scope；
// 结算读取接口的权威 scope 名必须先由平台/结算联调下发，测试不据此推断线上名称。
const keycloakTestScope = "settlement.contract.completed.read"

// keycloakTestVerifier 直接构造校验器状态：validateClaims 是范围断言的唯一入口，
// 不需要启动 OIDC discovery 或连网络。
func keycloakTestVerifier(requiredScope string) *keycloakClientCredentialsVerifier {
	return &keycloakClientCredentialsVerifier{
		clientID: "settlement-reader", audience: "settlement-audience",
		tenantID: "tenant-1", requiredScope: requiredScope,
	}
}

// validKeycloakClaims 是能通过 client_id/audience/tenant 绑定的基准声明。
func validKeycloakClaims(scopes ...string) serviceTokenClaims {
	return serviceTokenClaims{
		AuthorizedParty: "settlement-reader",
		ClientID:        "settlement-reader",
		Type:            "bearer",
		TokenUse:        "access_token",
		TenantID:        "tenant-1",
		Scopes:          serviceTokenScopes(scopes),
	}
}

// 结算读取机器身份必须携带唯一且精确的 scope：缺失、额外多一个、指向其它用途
// 都必须拒绝，不能只靠 client_id/audience 放行。
func TestKeycloakServiceTokenRequiresExactScope(t *testing.T) {
	verifier := keycloakTestVerifier(keycloakTestScope)
	audiences := []string{"settlement-audience"}
	tests := []struct {
		name    string
		scopes  []string
		wantErr bool
	}{
		{name: "missing scope", scopes: nil, wantErr: true},
		{name: "empty scope list", scopes: []string{}, wantErr: true},
		{name: "extra scope alongside required", scopes: []string{keycloakTestScope, "platform:file:upload"}, wantErr: true},
		{name: "wrong scope only", scopes: []string{"dashboard.contract.read"}, wantErr: true},
		{name: "superstring of required scope", scopes: []string{keycloakTestScope + ".write"}, wantErr: true},
		{name: "duplicate required scope", scopes: []string{keycloakTestScope, keycloakTestScope}, wantErr: true},
		{name: "exact required scope", scopes: []string{keycloakTestScope}, wantErr: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := verifier.validateClaims(validKeycloakClaims(test.scopes...), audiences)
			if test.wantErr {
				if !errors.Is(err, ErrInvalidServiceToken) {
					t.Fatalf("validateClaims() error = %v, want ErrInvalidServiceToken", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateClaims() error = %v, want nil", err)
			}
		})
	}
}

// 平台的其它绑定条件仍先于范围断言生效：合法 scope 不能掩盖错误的 client/tenant。
func TestKeycloakServiceTokenKeepsBindingChecksWithScope(t *testing.T) {
	verifier := keycloakTestVerifier(keycloakTestScope)
	audiences := []string{"settlement-audience"}
	claims := validKeycloakClaims(keycloakTestScope)
	claims.TenantID = "other-tenant"
	if err := verifier.validateClaims(claims, audiences); !errors.Is(err, ErrInvalidServiceToken) {
		t.Fatalf("validateClaims() error = %v, want tenant binding rejection", err)
	}
	claims = validKeycloakClaims(keycloakTestScope)
	if err := verifier.validateClaims(claims, []string{"wrong-audience"}); !errors.Is(err, ErrInvalidServiceToken) {
		t.Fatalf("validateClaims() error = %v, want audience binding rejection", err)
	}
}

// Keycloak 的 scope 是空格分隔字符串，兼容实现也可能签发数组；
// 两种形态都必须收敛为同一 Scope 集合，否则形态差异会绕过唯一范围断言。
func TestServiceTokenClaimsDecodesKeycloakScopeShapes(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{name: "space separated string", body: `{"scope":"` + keycloakTestScope + `"}`, want: []string{keycloakTestScope}},
		{name: "multiple scopes in string", body: `{"scope":"openid ` + keycloakTestScope + `"}`, want: []string{"openid", keycloakTestScope}},
		{name: "array", body: `{"scope":["` + keycloakTestScope + `"]}`, want: []string{keycloakTestScope}},
		{name: "null", body: `{"scope":null}`, want: nil},
		{name: "absent", body: `{}`, want: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var claims serviceTokenClaims
			if err := json.Unmarshal([]byte(test.body), &claims); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if len(claims.Scopes) != len(test.want) {
				t.Fatalf("scopes = %#v, want %#v", claims.Scopes, test.want)
			}
			for index, want := range test.want {
				if claims.Scopes[index] != want {
					t.Fatalf("scopes = %#v, want %#v", claims.Scopes, test.want)
				}
			}
		})
	}
}

// scope 不是字符串或字符串数组时必须解码失败，不能被当成“无范围”静默放行。
func TestServiceTokenClaimsRejectsUnexpectedScopeShape(t *testing.T) {
	for _, body := range []string{`{"scope":42}`, `{"scope":{"value":"x"}}`, `{"scope":[1]}`} {
		var claims serviceTokenClaims
		if err := json.Unmarshal([]byte(body), &claims); err == nil {
			t.Fatalf("json.Unmarshal(%s) error = nil, want scope shape rejection", body)
		}
	}
}

// 构造器在联系 OIDC provider 之前就要求 RequiredScope，配置漏填时失败关闭。
func TestNewKeycloakClientCredentialsTokenVerifierRequiresScope(t *testing.T) {
	_, err := NewKeycloakClientCredentialsTokenVerifier(context.Background(), KeycloakClientCredentialsVerifierOptions{
		Issuer: "http://localhost:8080", ClientID: "settlement-reader",
		Audience: "settlement-audience", TenantID: "tenant-1",
	})
	if !errors.Is(err, ErrInvalidServiceToken) {
		t.Fatalf("NewKeycloakClientCredentialsTokenVerifier() error = %v, want ErrInvalidServiceToken", err)
	}
}
