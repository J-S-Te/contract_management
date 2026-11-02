package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
	"github.com/oklog/ulid/v2"
)

// crmSummaryEnvelope 复刻 CRM opportunity.HTTPContractVerifier 的严格解码形状：
// 未知字段拒绝、details 必须为空、data 必须能解出摘要。
//
// 注意：对端用 DisallowUnknownFields 解码，本结构必须与
// internal/transport/httpapi/contract_summary.go 的 contractSummaryData 逐字段一致。
// 本批新增 linked_at_source / linked_at_precision / first_linked_at 后，
// CRM 仓的解码结构必须同批次放开这三个字段，否则严格解码会把成功响应判为无效。
type crmSummaryEnvelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Data      *struct {
		ContractID        string    `json:"contract_id"`
		ContractNumber    string    `json:"contract_number"`
		CRMCustomerID     uint64    `json:"crm_customer_id"`
		CRMOpportunityID  uint64    `json:"crm_opportunity_id"`
		LinkedAt          time.Time `json:"linked_at"`
		LinkedAtSource    string    `json:"linked_at_source"`
		LinkedAtPrecision string    `json:"linked_at_precision"`
		FirstLinkedAt     time.Time `json:"first_linked_at"`
	} `json:"data"`
	Details json.RawMessage `json:"details"`
}

func decodeCRMContractSummary(t *testing.T, body []byte) crmSummaryEnvelope {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var envelope crmSummaryEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("CRM 契约解码失败: %v body=%s", err, body)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("响应包含尾随 JSON: %s", body)
	}
	return envelope
}

type contractSummaryHTTPRepository struct {
	application.Repository
	candidate application.ContractSummaryCandidate
	err       error
	tenant    string
	ref       string
}

func (r *contractSummaryHTTPRepository) FindContractSummaryCandidate(_ context.Context, tenantID, contractRef string) (application.ContractSummaryCandidate, error) {
	r.tenant, r.ref = tenantID, contractRef
	return r.candidate, r.err
}

func contractSummaryRouter(repository application.Repository, verifier platform.ClientCredentialsTokenVerifier, enabled bool) *gin.Engine {
	options := &CRMSignedCountIntegrationOptions{}
	if enabled {
		options.ContractSummary = &ContractSummaryIntegrationOptions{Enabled: true, RequireBearer: true, BearerVerifier: verifier}
	}
	return NewRouterWithAllIntegrations(&application.Service{Repo: repository}, nil, nil, nil, nil, options, nil, nil)
}

func TestContractSummaryReturnsCRMCompatibleEnvelope(t *testing.T) {
	contractID := ulid.Make().String()
	linkedAt := time.Date(2026, 10, 30, 8, 30, 0, 0, time.UTC)
	firstLinkedAt := linkedAt.Add(-72 * time.Hour)
	repository := &contractSummaryHTTPRepository{candidate: application.ContractSummaryCandidate{
		ContractID: contractID, ContractNumber: "HT-2026-0001", CRMCustomerID: 42,
		CRMOpportunityID: 7, LinkedAt: linkedAt, LinkedAtSource: application.ContractSummaryLinkSourceAuditLog,
		FirstLinkedAt: firstLinkedAt, UpdatedAt: linkedAt.Add(time.Hour),
	}}
	router := contractSummaryRouter(repository, stubVerifier{tenantID: "tenant-from-token"}, true)
	request := httptest.NewRequest(http.MethodGet, "/internal/contract-summary?contract_ref="+contractID+"&crm_customer_id=42", nil)
	request.Header.Set("Authorization", "Bearer machine-token")
	request.Header.Set("X-Tenant-ID", "attacker-tenant")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	envelope := decodeCRMContractSummary(t, response.Body.Bytes())
	if envelope.Code != "OK" || envelope.RequestID == "" || len(envelope.Details) != 0 || envelope.Data == nil {
		t.Fatalf("envelope=%+v raw=%s", envelope, response.Body.String())
	}
	data := envelope.Data
	if data.ContractID != contractID || data.ContractNumber != "HT-2026-0001" || data.CRMCustomerID != 42 || data.CRMOpportunityID != 7 || !data.LinkedAt.Equal(linkedAt) {
		t.Fatalf("data=%+v", data)
	}
	// 新增字段必须是枚举内取值且 RFC3339，linked_at 语义仍是“最新一次”。
	if data.LinkedAtSource != application.ContractSummaryLinkSourceAuditLog || data.LinkedAtPrecision != application.ContractSummaryPrecisionExact ||
		!data.FirstLinkedAt.Equal(firstLinkedAt) {
		t.Fatalf("evidence fields data=%+v want source=AUDIT_LOG precision=exact first=%s", data, firstLinkedAt)
	}
	// 租户只来自验签令牌，请求头不能覆盖。
	if repository.tenant != "tenant-from-token" || repository.ref != contractID {
		t.Fatalf("tenant=%q ref=%q", repository.tenant, repository.ref)
	}
}

// TestContractSummaryExposesApproximatePrecision 固定历史数据（无任何关联证据）的
// 响应形状：linked_at 回退合同 updated_at，同时显式标注 APPROX_CONTRACT_UPDATED/approx。
func TestContractSummaryExposesApproximatePrecision(t *testing.T) {
	contractID := ulid.Make().String()
	updatedAt := time.Date(2026, 10, 30, 8, 30, 0, 0, time.UTC)
	repository := &contractSummaryHTTPRepository{candidate: application.ContractSummaryCandidate{
		ContractID: contractID, ContractNumber: "HT-2026-0003", CRMCustomerID: 42,
		CRMOpportunityID: 7, UpdatedAt: updatedAt,
	}}
	router := contractSummaryRouter(repository, stubVerifier{tenantID: "tenant-1"}, true)
	request := httptest.NewRequest(http.MethodGet, "/internal/contract-summary?contract_ref="+contractID+"&crm_customer_id=42", nil)
	request.Header.Set("Authorization", "Bearer machine-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	data := decodeCRMContractSummary(t, response.Body.Bytes()).Data
	if data == nil || !data.LinkedAt.Equal(updatedAt) || !data.FirstLinkedAt.Equal(updatedAt) {
		t.Fatalf("data=%+v", data)
	}
	if data.LinkedAtSource != application.ContractSummaryLinkSourceApproxContractUpdated ||
		data.LinkedAtPrecision != application.ContractSummaryPrecisionApprox {
		t.Fatalf("source=%s precision=%s want APPROX_CONTRACT_UPDATED/approx", data.LinkedAtSource, data.LinkedAtPrecision)
	}
}

func TestContractSummaryReturnsNotFoundEnvelope(t *testing.T) {
	repository := &contractSummaryHTTPRepository{err: apperrors.ErrNotFound}
	router := contractSummaryRouter(repository, stubVerifier{tenantID: "tenant-1"}, true)
	request := httptest.NewRequest(http.MethodGet, "/internal/contract-summary?contract_ref=HT-404&crm_customer_id=42", nil)
	request.Header.Set("Authorization", "Bearer machine-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	envelope := decodeCRMContractSummary(t, response.Body.Bytes())
	if envelope.Code != "CON_NOT_FOUND" || envelope.Data != nil || len(envelope.Details) != 0 || envelope.RequestID == "" {
		t.Fatalf("envelope=%+v raw=%s", envelope, response.Body.String())
	}
}

func TestContractSummaryClientMismatchIsIndistinguishableFromNotFound(t *testing.T) {
	contractID := ulid.Make().String()
	repository := &contractSummaryHTTPRepository{candidate: application.ContractSummaryCandidate{
		ContractID: contractID, ContractNumber: "HT-2026-0002", CRMCustomerID: 99,
		CRMOpportunityID: 7, UpdatedAt: time.Now().UTC(),
	}}
	router := contractSummaryRouter(repository, stubVerifier{tenantID: "tenant-1"}, true)
	request := httptest.NewRequest(http.MethodGet, "/internal/contract-summary?contract_ref="+contractID+"&crm_customer_id=42", nil)
	request.Header.Set("Authorization", "Bearer machine-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"code":"CON_NOT_FOUND"`) {
		t.Fatalf("body=%s", response.Body.String())
	}
}

func TestContractSummaryRejectsCallerOrScopeMismatch(t *testing.T) {
	router := contractSummaryRouter(&contractSummaryHTTPRepository{}, stubVerifier{err: platform.ErrInvalidServiceToken}, true)
	request := httptest.NewRequest(http.MethodGet, "/internal/contract-summary?contract_ref=HT-1&crm_customer_id=42", nil)
	request.Header.Set("Authorization", "Bearer wrong-caller-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestContractSummaryIsNotRegisteredWhenDisabled(t *testing.T) {
	router := NewRouterWithAllIntegrations(nil, nil, nil, nil, nil, &CRMSignedCountIntegrationOptions{}, nil, nil)
	for _, route := range router.Routes() {
		if route.Path == "/internal/contract-summary" {
			t.Fatalf("disabled contract summary route must not be registered: %+v", route)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/internal/contract-summary?contract_ref=HT-1&crm_customer_id=42", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("disabled status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestContractSummaryValidatesQueryParameters(t *testing.T) {
	router := contractSummaryRouter(&contractSummaryHTTPRepository{}, stubVerifier{tenantID: "tenant-1"}, true)
	for _, target := range []string{
		"/internal/contract-summary?crm_customer_id=42",
		"/internal/contract-summary?contract_ref=HT-1",
		"/internal/contract-summary?contract_ref=HT-1&crm_customer_id=0",
		"/internal/contract-summary?contract_ref=HT-1&crm_customer_id=042",
		"/internal/contract-summary?contract_ref=HT-1&crm_customer_id=abc",
	} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set("Authorization", "Bearer machine-token")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
}
