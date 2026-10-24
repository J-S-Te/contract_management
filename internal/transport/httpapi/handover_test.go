package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
)

// handoverHTTPRepositoryStub 只服务处理器用例：记录平台传入的租户并返回预设结果。
type handoverHTTPRepositoryStub struct {
	tenantSeen       string
	responsibilities []application.HandoverResponsibility
	receipt          application.HandoverReceipt
	receiptFound     bool
	successorFrozen  bool
	transferCalls    int
}

func (stub *handoverHTTPRepositoryStub) CreateFreeze(_ context.Context, freeze application.HandoverFreeze) (application.HandoverFreeze, bool, error) {
	stub.tenantSeen = freeze.TenantID
	return freeze, true, nil
}
func (stub *handoverHTTPRepositoryStub) ReleaseFreeze(_ context.Context, tenantID, _, _ string, _ time.Time) error {
	stub.tenantSeen = tenantID
	return nil
}
func (stub *handoverHTTPRepositoryStub) ActiveFreeze(_ context.Context, _ string, userID string) (application.HandoverFreeze, bool, error) {
	if stub.successorFrozen {
		return application.HandoverFreeze{ID: "freeze-1", UserID: userID, Status: application.HandoverFreezeFrozen}, true, nil
	}
	return application.HandoverFreeze{}, false, nil
}
func (stub *handoverHTTPRepositoryStub) ListOwnedResponsibilities(_ context.Context, tenantID, _ string, _ string, _ int) ([]application.HandoverResponsibility, error) {
	stub.tenantSeen = tenantID
	return stub.responsibilities, nil
}
func (stub *handoverHTTPRepositoryStub) TransferOwnership(_ context.Context, in application.HandoverTransferInput, now time.Time) (application.HandoverReceipt, error) {
	stub.transferCalls++
	stub.tenantSeen = in.TenantID
	receipt := stub.receipt
	if receipt.ItemID == "" {
		receipt = application.HandoverReceipt{
			ItemID: in.ItemID, IdempotencyKey: in.IdempotencyKey, Status: application.HandoverReceiptApplied,
			AppliedVersion: in.ExpectedVersion + 1, EvidenceRef: "audit:" + in.ResourceID, ReceivedAt: now,
		}
	}
	return receipt, nil
}
func (stub *handoverHTTPRepositoryStub) SaveReceipt(_ context.Context, receipt application.HandoverReceipt) error {
	stub.receipt = receipt
	return nil
}
func (stub *handoverHTTPRepositoryStub) FindReceipt(_ context.Context, tenantID, _ string) (application.HandoverReceipt, bool, error) {
	stub.tenantSeen = tenantID
	return stub.receipt, stub.receiptFound, nil
}

func handoverTestRouter(t *testing.T, stub *handoverHTTPRepositoryStub, options *HandoverIntegrationOptions) *gin.Engine {
	t.Helper()
	service, err := application.NewHandoverService(stub,
		func(time.Time) (string, error) { return "01HANDOVERID00000000000000", nil },
		func() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	if options == nil {
		options = &HandoverIntegrationOptions{Enabled: true, RequireBearer: true, BearerVerifier: stubVerifier{tenantID: "tenant-from-token"}}
	}
	return NewRouterWithAllIntegrations(&application.Service{}, nil, nil, nil, nil, nil, service, options)
}

type handoverEnvelope struct {
	Code string          `json:"code"`
	Data json.RawMessage `json:"data"`
}

func handoverRequest(t *testing.T, router *gin.Engine, method, path, body string, bearer bool) (int, handoverEnvelope) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if bearer {
		request.Header.Set("Authorization", "Bearer machine-token")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var envelope handoverEnvelope
	_ = json.Unmarshal(response.Body.Bytes(), &envelope)
	return response.Code, envelope
}

func TestHandoverFreezeRequiresBearerAndBindsTokenTenant(t *testing.T) {
	stub := &handoverHTTPRepositoryStub{}
	router := handoverTestRouter(t, stub, nil)

	status, envelope := handoverRequest(t, router, http.MethodPost, "/internal/v1/handover/freeze", `{"batch_id":"b1","request_id":"r1","user_id":"u1"}`, false)
	if status != http.StatusUnauthorized || envelope.Code != "CON_SERVICE_BEARER_REQUIRED" {
		t.Fatalf("missing bearer → %d %s", status, envelope.Code)
	}

	status, envelope = handoverRequest(t, router, http.MethodPost, "/internal/v1/handover/freeze", `{"tenant_id":"attacker","batch_id":"b1","request_id":"r1","user_id":"u1"}`, true)
	if status != http.StatusForbidden || envelope.Code != "TENANT_MISMATCH" {
		t.Fatalf("tenant override → %d %s", status, envelope.Code)
	}

	status, envelope = handoverRequest(t, router, http.MethodPost, "/internal/v1/handover/freeze", `{"batch_id":"b1","request_id":"r1","user_id":"u1"}`, true)
	if status != http.StatusOK {
		t.Fatalf("freeze → %d %s", status, envelope.Code)
	}
	if stub.tenantSeen != "tenant-from-token" {
		t.Fatalf("service saw tenant %q, want the token-bound tenant", stub.tenantSeen)
	}
}

func TestHandoverRoutesAreAbsentWhenDisabled(t *testing.T) {
	stub := &handoverHTTPRepositoryStub{}
	router := handoverTestRouter(t, stub, &HandoverIntegrationOptions{Enabled: false})
	status, _ := handoverRequest(t, router, http.MethodPost, "/internal/v1/handover/freeze", `{}`, true)
	if status != http.StatusNotFound {
		t.Fatalf("disabled handover must not expose routes, got %d", status)
	}
}

func TestHandoverTransferDelegatesAndMapsErrors(t *testing.T) {
	stub := &handoverHTTPRepositoryStub{}
	router := handoverTestRouter(t, stub, nil)

	body := `{"batch_id":"b1","item_id":"i1","resource_type":"contract","resource_id":"c1","from_user_id":"u1","to_user_id":"u2","expected_version":3,"idempotency_key":"handover:b1:i1"}`
	status, envelope := handoverRequest(t, router, http.MethodPost, "/internal/v1/handover/transfers", body, true)
	if status != http.StatusOK || stub.transferCalls != 1 {
		t.Fatalf("transfer → %d %s calls=%d", status, envelope.Code, stub.transferCalls)
	}
	var receipt struct {
		Status         string `json:"status"`
		AppliedVersion uint64 `json:"applied_version"`
		EvidenceRef    string `json:"evidence_ref"`
	}
	if err := json.Unmarshal(envelope.Data, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Status != application.HandoverReceiptApplied || receipt.AppliedVersion != 4 || receipt.EvidenceRef == "" {
		t.Fatalf("receipt = %+v", receipt)
	}

	status, envelope = handoverRequest(t, router, http.MethodPost, "/internal/v1/handover/transfers", `{"item_id":"i1"}`, true)
	if status != http.StatusUnprocessableEntity || envelope.Code != "HANDOVER_VALIDATION" {
		t.Fatalf("incomplete transfer → %d %s", status, envelope.Code)
	}

	status, envelope = handoverRequest(t, router, http.MethodGet, "/internal/v1/handover/transfers/i1?idempotency_key=missing", "", true)
	if status != http.StatusNotFound || envelope.Code != "HANDOVER_NOT_FOUND" {
		t.Fatalf("unknown receipt → %d %s", status, envelope.Code)
	}

	frozen := &handoverHTTPRepositoryStub{successorFrozen: true}
	router = handoverTestRouter(t, frozen, nil)
	status, envelope = handoverRequest(t, router, http.MethodPost, "/internal/v1/handover/transfers", body, true)
	if status != http.StatusConflict || envelope.Code != "HANDOVER_SUCCESSOR_FROZEN" {
		t.Fatalf("frozen successor → %d %s", status, envelope.Code)
	}
	if frozen.transferCalls != 0 {
		t.Fatal("transfer must not run when the successor is frozen")
	}
}
