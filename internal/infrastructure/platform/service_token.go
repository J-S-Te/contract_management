package platform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// ErrInvalidServiceToken is deliberately distinct from a browser session
// error: callers must never fall back to routing headers when a bearer token
// was supplied but cannot be verified.
var ErrInvalidServiceToken = errors.New("invalid service token")

// ClientCredentialsTokenVerifier verifies the Keycloak access token used by a
// trusted service-to-service caller. It verifies signature, issuer, expiry,
// token type, authorized party and audience; it does not accept an ID token.
type ClientCredentialsTokenVerifier interface {
	VerifyClientCredentials(context.Context, string) error
}

type ClientCredentialsVerifierOptions struct {
	Issuer, BackchannelBaseURL, ClientID, Audience string
	Timeout                                        time.Duration
}

type keycloakClientCredentialsVerifier struct {
	verifier *oidc.IDTokenVerifier
	clientID string
	audience string
}

type serviceTokenClaims struct {
	AuthorizedParty string `json:"azp"`
	ClientID        string `json:"client_id"`
	Type            string `json:"typ"`
	TokenUse        string `json:"token_use"`
}

func NewClientCredentialsTokenVerifier(ctx context.Context, options ClientCredentialsVerifierOptions) (ClientCredentialsTokenVerifier, error) {
	issuer := strings.TrimRight(strings.TrimSpace(options.Issuer), "/")
	backchannel := strings.TrimRight(strings.TrimSpace(options.BackchannelBaseURL), "/")
	clientID := strings.TrimSpace(options.ClientID)
	audience := strings.TrimSpace(options.Audience)
	if issuer == "" || clientID == "" || audience == "" {
		return nil, fmt.Errorf("%w: issuer, client ID and audience are required", ErrInvalidServiceToken)
	}
	if options.Timeout <= 0 {
		options.Timeout = 10 * time.Second
	}
	client := &http.Client{Timeout: options.Timeout}
	if backchannel != "" {
		publicURL, err := url.Parse(issuer)
		if err != nil {
			return nil, fmt.Errorf("%w: issuer: %v", ErrInvalidServiceToken, err)
		}
		backchannelURL, err := url.Parse(backchannel)
		if err != nil {
			return nil, fmt.Errorf("%w: backchannel: %v", ErrInvalidServiceToken, err)
		}
		client.Transport = &backchannelTransport{base: http.DefaultTransport, public: publicURL, backchannel: backchannelURL}
	}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), issuer)
	if err != nil {
		return nil, fmt.Errorf("%w: load discovery: %v", ErrInvalidServiceToken, err)
	}
	// Access tokens are issued for the machine-client audience, not the
	// browser RP Client ID. The audience is validated explicitly below.
	return &keycloakClientCredentialsVerifier{
		verifier: provider.Verifier(&oidc.Config{SkipClientIDCheck: true}),
		clientID: clientID,
		audience: audience,
	}, nil
}

func (v *keycloakClientCredentialsVerifier) VerifyClientCredentials(ctx context.Context, rawToken string) error {
	if v == nil || v.verifier == nil || strings.TrimSpace(rawToken) == "" {
		return ErrInvalidServiceToken
	}
	token, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return fmt.Errorf("%w: signature, issuer or expiry: %v", ErrInvalidServiceToken, err)
	}
	claims := serviceTokenClaims{}
	if err := token.Claims(&claims); err != nil {
		return fmt.Errorf("%w: claims: %v", ErrInvalidServiceToken, err)
	}
	return v.validateClaims(claims, token.Audience)
}

func (v *keycloakClientCredentialsVerifier) validateClaims(claims serviceTokenClaims, audiences []string) error {
	if !strings.EqualFold(strings.TrimSpace(claims.Type), "bearer") {
		return fmt.Errorf("%w: token typ is not bearer", ErrInvalidServiceToken)
	}
	// token_use is deliberately required in addition to typ.  The Keycloak
	// client scope mapper for the contract integration client must emit the
	// literal access_token value; ID tokens and tokens minted for another use
	// therefore fail closed even when they are otherwise correctly signed.
	if strings.TrimSpace(claims.TokenUse) != "access_token" {
		return fmt.Errorf("%w: token_use is not access_token", ErrInvalidServiceToken)
	}
	// azp is the authenticated Keycloak caller.  Do not fall back to client_id:
	// the latter is an optional mapper and is not the authorized-party contract.
	if strings.TrimSpace(claims.AuthorizedParty) != v.clientID || !containsAudience(audiences, v.audience) {
		return fmt.Errorf("%w: authorized party or audience", ErrInvalidServiceToken)
	}
	return nil
}

func containsAudience(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
