package platform

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPlatformApplicationTokenVerifierRequiresExactDashboardScope(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	publicPath := filepath.Join(t.TempDir(), "application-jwt-public.pem")
	if err := os.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	verifier, err := NewClientCredentialsTokenVerifier(context.Background(), ClientCredentialsVerifierOptions{
		Issuer: "basic-platform", Audience: "basic-platform-application", PublicKeyPath: publicPath,
		ClientID: "data_analysis-prod-contract-dashboard", TenantID: "tenant-1",
		CallerApplicationCode: "data_analysis", CallerEnvironmentCode: "prod", RequiredScope: "dashboard.contract.read",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claims := platformApplicationTokenClaims{Issuer: "basic-platform", Audience: "basic-platform-application", TokenUse: "application", Subject: "data_analysis-prod-contract-dashboard", OAuthClientID: "oauth-client-1", TenantID: "tenant-1", ApplicationCode: "data_analysis", EnvironmentCode: "prod", Scopes: []string{"dashboard.contract.read"}, IssuedAt: now.Add(-time.Second).Unix(), NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(time.Minute).Unix()}
	if _, err := verifier.VerifyClientCredentials(context.Background(), signPlatformToken(t, privateKey, claims)); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	claims.Scopes = []string{"dashboard.project.read"}
	if _, err := verifier.VerifyClientCredentials(context.Background(), signPlatformToken(t, privateKey, claims)); err == nil {
		t.Fatal("wrong-scope token was accepted")
	}
}

func signPlatformToken(t *testing.T, privateKey ed25519.PrivateKey, claims platformApplicationTokenClaims) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(input)))
}
