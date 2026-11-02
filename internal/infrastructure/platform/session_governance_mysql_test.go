package platform

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/j-s-te/contract-management/internal/application"
	central "github.com/j-s-te/contract-management/internal/sessiongovernance"
	"golang.org/x/oauth2"

	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/contract-management/internal/migration"
	"github.com/j-s-te/contract-management/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// centralMySQLStore 只在显式独立测试库应用真实迁移，不寻找业务配置。
// 参数：t为测试上下文；DSN仅来自SESSION_GOVERNANCE_TEST_DSN。
// 返回值：真实OIDC存储；无专用DSN则跳过，不符合回环隔离边界则失败。
func centralMySQLStore(t *testing.T) *GORMOIDCStore {
	t.Helper()
	dsn := os.Getenv("SESSION_GOVERNANCE_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated SESSION_GOVERNANCE_TEST_DSN is not configured")
	}
	config, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid isolated DSN")
	}
	host, portText, err := net.SplitHostPort(config.Addr)
	port, portErr := strconv.Atoi(portText)
	if err != nil || portErr != nil || config.Net != "tcp" || host != "127.0.0.1" ||
		port < 32768 || config.DBName != "qa_session_contract_20261101" {
		t.Fatal("refusing non-isolated session DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	if err := migration.Run(ctx, dsn, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := migration.Run(ctx, dsn, migrations.Files); err != nil {
		t.Fatal("migration rerun:", err)
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	store, err := NewGORMOIDCStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

type unavailableCentralAuthorization struct{}

// Resolve 模拟令牌已签发后的权限服务故障，不伪造授权成功。
// 参数：请求上下文及访问令牌。
// 返回值：空权限及明确的暂时依赖错误。
func (unavailableCentralAuthorization) Resolve(context.Context, string) (AuthorizationContext, error) {
	return AuthorizationContext{}, ErrAuthorizationUnavailable
}

// refreshFixtureID 签发独立测试IdP的真实RSA身份令牌供刷新路径完整验签。
// 参数：t为测试上下文，now为本轮固定认证时间。
// 返回值：签名身份令牌及只信任该临时公钥的验证器。
func refreshFixtureID(t *testing.T, now time.Time) (string, *oidc.IDTokenVerifier) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"iss": "https://isolated-issuer.test", "aud": "qa-client",
		"sub": "qa-identity", "identity_id": "qa-identity", "tenant_id": "qa-session", "sid": "qa-sid",
		"token_use": "id_token", "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	message, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := message.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	verifier := oidc.NewVerifier("https://isolated-issuer.test", &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}},
		&oidc.Config{ClientID: "qa-client", SupportedSigningAlgs: []string{"RS256"}, Now: func() time.Time { return now }})
	return raw, verifier
}

// TestCentralSessionRotationCheckpointMySQL 验证真实认证链在单次刷新后权限故障时不丢令牌。
// 参数：t为隔离MySQL上下文；IdP为本轮临时HTTP/RSA替身，不代表真实Keycloak验收。
// 返回值：无；重复刷新、旧令牌回写、伪授权或原始截止变化时失败。
func TestCentralSessionRotationCheckpointMySQL(t *testing.T) {
	store := centralMySQLStore(t)
	ctx, now := context.Background(), time.Now().UTC().Truncate(time.Millisecond)
	rawID, verifier := refreshFixtureID(t, now)
	var refreshes atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if refreshes.Add(1) != 1 || r.FormValue("refresh_token") != "isolated-old-refresh" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "isolated-access", "refresh_token": "isolated-new-refresh",
			"token_type": "Bearer", "expires_in": 300, "id_token": rawID})
	}))
	t.Cleanup(issuer.Close)
	codec, err := newSecretCodec(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	encrypt := func(value string) []byte {
		ciphertext, err := codec.encrypt(value)
		if err != nil {
			t.Fatal(err)
		}
		return ciphertext
	}
	oldRefresh := encrypt("isolated-old-refresh")
	oldAccess := encrypt("isolated-old-access")
	oldID := encrypt("isolated-old-id")
	binding := encrypt("isolated-server-binding")
	principal := application.Principal{Subject: "qa-identity", IdentityID: "qa-identity", UserID: "qa-identity",
		TenantID: "qa-session", AuthorizationRevision: 1}
	principalJSON, err := json.Marshal(principal)
	if err != nil {
		t.Fatal(err)
	}
	record := SessionRecord{SessionIDHash: tokenHash("isolated-rotation-" + now.Format("150405.000")),
		TenantID: "qa-session", IdentityID: "qa-identity", OIDCSessionID: "qa-sid", PrincipalJSON: principalJSON,
		AccessTokenCiphertext: oldAccess, RefreshTokenCiphertext: oldRefresh, IDTokenCiphertext: oldID,
		CentralBindingCiphertext: binding, CentralSessionID: "qa-central", TokenExpiresAt: now.Add(30 * time.Second),
		SessionExpiresAt: now.Add(8 * time.Hour), AuthorizationRevision: 1, AuthorizationCheckedAt: now, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateSession(ctx, record); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.db.Where("session_id_hash=?", record.SessionIDHash).Delete(&gormSession{}).Error })
	auth := &OIDCAuthenticator{store: store, codec: codec, verifier: verifier, authorization: unavailableCentralAuthorization{},
		httpClient: issuer.Client(), now: func() time.Time { return now },
		oauth2Config: oauth2.Config{ClientID: "qa-client", Endpoint: oauth2.Endpoint{TokenURL: issuer.URL, AuthStyle: oauth2.AuthStyleInHeader}},
		options: OIDCOptions{TenantID: "qa-session", ClientID: "qa-client", ApplicationCode: "contract_management", EnvironmentCode: "test",
			AuthorizationRefreshInterval: time.Minute, CentralClient: &centralContractClient{state: central.Context{
				SessionID: "qa-central", TenantID: "qa-session", SubjectID: "qa-identity", ClientID: "qa-client",
				ApplicationCode: "contract_management", EnvironmentCode: "test", PolicyVersion: 1, IdleTimeoutSeconds: 1800,
				IdleRemainingMS: 1800000, AbsoluteRemainingMS: 28800000, AbsoluteExpiresAt: record.SessionExpiresAt}}}}
	var group sync.WaitGroup
	for range 12 {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := auth.authenticateCentral(ctx, record, now); !errors.Is(err, ErrAuthorizationServiceUnavailable) {
				t.Errorf("temporary authorization outage must remain unavailable: %v", err)
			}
		}()
	}
	group.Wait()
	stored, err := store.FindSession(ctx, record.SessionIDHash, now)
	if err != nil {
		t.Fatal("successful rotation was lost and valid local session revoked:", err)
	}
	rotated, err := codec.decrypt(stored.RefreshTokenCiphertext)
	if err != nil || rotated != "isolated-new-refresh" || refreshes.Load() != 1 {
		t.Fatal("single-use refresh was lost or replayed")
	}
	if stored.AuthorizationRevision != 1 || !stored.AuthorizationCheckedAt.Before(now.Add(-time.Minute)) ||
		!stored.SessionExpiresAt.Equal(record.SessionExpiresAt) {
		t.Fatal("outage granted authority or extended original expiry")
	}
}
