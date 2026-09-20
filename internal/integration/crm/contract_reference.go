package crm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

const contractReferenceScope = "customer.contract_reference.read"

type ContractReferenceDirectory struct {
	BaseURL     string
	Client      *http.Client
	TokenSource func(context.Context) (string, error)
	Now         func() time.Time
	Nonce       func() (string, error)
}

func NewClientCredentialsTokenSource(ctx context.Context, tokenURL, clientID, clientSecret, scope string) func(context.Context) (string, error) {
	if strings.TrimSpace(tokenURL) == "" || strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" || strings.TrimSpace(scope) == "" {
		return nil
	}
	credentials := clientcredentials.Config{
		ClientID: clientID, ClientSecret: clientSecret, TokenURL: tokenURL,
		Scopes: []string{strings.TrimSpace(scope)}, AuthStyle: oauth2.AuthStyleInHeader,
	}
	tokenContext := context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 10 * time.Second, CheckRedirect: rejectRedirect})
	source := credentials.TokenSource(tokenContext)
	return func(context.Context) (string, error) {
		token, err := source.Token()
		if err != nil {
			return "", err
		}
		if token == nil || strings.TrimSpace(token.AccessToken) == "" {
			return "", errors.New("CRM token response has no access_token")
		}
		return token.AccessToken, nil
	}
}

func (directory *ContractReferenceDirectory) Resolve(ctx context.Context, customerID uint64, opportunityID, actorIdentityID string) (application.CRMContractReference, error) {
	if directory == nil || customerID == 0 || strings.TrimSpace(actorIdentityID) == "" || strings.TrimSpace(directory.BaseURL) == "" || directory.TokenSource == nil {
		return application.CRMContractReference{}, errors.New("CRM contract reference integration is not configured")
	}
	token, err := directory.TokenSource(ctx)
	if err != nil {
		return application.CRMContractReference{}, fmt.Errorf("obtain CRM machine token: %w", err)
	}
	endpoint := strings.TrimRight(directory.BaseURL, "/") + "/api/v1/internal/contract-references/customers/" + strconv.FormatUint(customerID, 10)
	if opportunityID != "" {
		parsed, parseErr := strconv.ParseUint(opportunityID, 10, 64)
		if parseErr != nil || parsed == 0 {
			return application.CRMContractReference{}, application.ErrCRMReferenceInvalid
		}
		query := url.Values{"opportunity_id": {strconv.FormatUint(parsed, 10)}}
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return application.CRMContractReference{}, err
	}
	now := time.Now
	if directory.Now != nil {
		now = directory.Now
	}
	nonce := randomNonce
	if directory.Nonce != nil {
		nonce = directory.Nonce
	}
	nonceValue, err := nonce()
	if err != nil {
		return application.CRMContractReference{}, fmt.Errorf("generate CRM request nonce: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Integration-Timestamp", now().UTC().Format(time.RFC3339Nano))
	request.Header.Set("X-Integration-Nonce", nonceValue)
	request.Header.Set("X-Actor-Identity-ID", strings.TrimSpace(actorIdentityID))
	client := directory.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second, CheckRedirect: rejectRedirect}
	}
	response, err := client.Do(request)
	if err != nil {
		return application.CRMContractReference{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return application.CRMContractReference{}, err
	}
	if len(body) > 1<<20 {
		return application.CRMContractReference{}, errors.New("CRM response exceeds 1 MiB")
	}
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusUnprocessableEntity {
		return application.CRMContractReference{}, application.ErrCRMReferenceInvalid
	}
	if response.StatusCode != http.StatusOK {
		return application.CRMContractReference{}, fmt.Errorf("CRM API returned status %d", response.StatusCode)
	}
	var envelope struct {
		Code      string                           `json:"code"`
		Message   string                           `json:"message"`
		RequestID string                           `json:"request_id"`
		Data      application.CRMContractReference `json:"data"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return application.CRMContractReference{}, fmt.Errorf("decode CRM contract reference: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return application.CRMContractReference{}, errors.New("CRM response contains trailing JSON")
	}
	return envelope.Data, nil
}

func randomNonce() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func rejectRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
