package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	sqldriver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/contract-management/internal/application"
	mysqlrepo "github.com/j-s-te/contract-management/internal/infrastructure/mysql"
	"github.com/j-s-te/contract-management/internal/migration"
	"github.com/j-s-te/contract-management/migrations"
	"github.com/oklog/ulid/v2"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func handoverProtocolRouter(t *testing.T) (*gin.Engine, *gorm.DB, string) {
	t.Helper()
	dsn := os.Getenv("CONTRACT_TEST_DSN")
	if dsn == "" {
		t.Skip("CONTRACT_TEST_DSN is not configured; no database connection attempted")
	}
	cfg, err := sqldriver.ParseDSN(dsn)
	if err != nil || !strings.HasPrefix(cfg.DBName, "contract_test_") || cfg.Net != "tcp" ||
		!(strings.HasPrefix(cfg.Addr, "127.0.0.1:") || strings.HasPrefix(cfg.Addr, "localhost:")) {
		t.Fatal("CONTRACT_TEST_DSN must target an explicitly named contract_test_* database on loopback TCP")
	}
	cfg.ParseTime = true
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := migration.Run(ctx, cfg.FormatDSN(), migrations.Files); err != nil {
		t.Fatalf("dedicated test database migrations failed: %v", err)
	}
	db, err := gorm.Open(gormmysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	tenant := ulid.Make().String()
	for _, table := range []string{"con_handover_receipt", "con_handover_freeze"} {
		if err := db.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
	handoverService, err := application.NewHandoverService(mysqlrepo.NewRepository(db),
		func(time.Time) (string, error) { return ulid.Make().String(), nil }, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return NewRouterWithAllIntegrations(&application.Service{}, nil, nil, nil, nil, nil, handoverService,
		&HandoverIntegrationOptions{Enabled: true, RequireBearer: true, BearerVerifier: stubVerifier{tenantID: tenant}}), db, tenant
}

func seedProtocolContract(t *testing.T, db *gorm.DB, tenantID, ownerID, status string, version uint64) string {
	t.Helper()
	now := time.Now().UTC()
	contractID := ulid.Make().String()
	if err := db.Exec("INSERT INTO con_contract (id, tenant_id, contract_number, title, contract_type, service_type, owner_user_id, amount_minor, currency, content, status, version, created_at, created_by, updated_at, updated_by) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		contractID, tenantID, "PROTO-"+contractID, "协议一致性合同", "sales", "integration", ownerID, 100000, "CNY", "{}", status, version, now, ownerID, now, ownerID).Error; err != nil {
		t.Fatalf("seed contract: %v", err)
	}
	return contractID
}

type protocolEnvelope struct {
	Code string          `json:"code"`
	Data json.RawMessage `json:"data"`
}

func protocolCall(t *testing.T, router *gin.Engine, method, path, body string) (int, protocolEnvelope) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer machine-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var envelope protocolEnvelope
	_ = json.Unmarshal(response.Body.Bytes(), &envelope)
	return response.Code, envelope
}

// TestHandoverProtocolMatchesPlatformOrchestration 按平台编排器的真实请求序列走完整流程。
func TestHandoverProtocolMatchesPlatformOrchestration(t *testing.T) {
	router, db, tenant := handoverProtocolRouter(t)
	leaver, successor := ulid.Make().String(), ulid.Make().String()
	contractID := seedProtocolContract(t, db, tenant, leaver, "active", 3)
	batchID, itemID := ulid.Make().String(), ulid.Make().String()

	freezeBody := fmt.Sprintf(`{"batch_id":"%s","request_id":"%s","user_id":"%s"}`, batchID, itemID, leaver)
	status, envelope := protocolCall(t, router, http.MethodPost, "/internal/v1/handover/freeze", freezeBody)
	if status != http.StatusOK {
		t.Fatalf("freeze → %d %s", status, envelope.Code)
	}
	var freeze struct {
		System              string `json:"system"`
		Frozen              bool   `json:"frozen"`
		FrozenAt            string `json:"frozen_at"`
		ResponsibilityCount int    `json:"responsibility_count"`
	}
	if err := json.Unmarshal(envelope.Data, &freeze); err != nil {
		t.Fatal(err)
	}
	if !freeze.Frozen || freeze.ResponsibilityCount != 1 || freeze.System != "contract" || freeze.FrozenAt == "" {
		t.Fatalf("freeze payload = %+v", freeze)
	}

	status, envelope = protocolCall(t, router, http.MethodGet, "/internal/v1/handover/responsibilities?user_id="+leaver+"&limit=100", "")
	if status != http.StatusOK {
		t.Fatalf("responsibilities → %d %s", status, envelope.Code)
	}
	var page struct {
		System     string `json:"system"`
		NextCursor string `json:"next_cursor"`
		Items      []struct {
			ResourceType string `json:"resource_type"`
			ResourceID   string `json:"resource_id"`
			Category     string `json:"category"`
			Version      uint64 `json:"version"`
		} `json:"items"`
	}
	if err := json.Unmarshal(envelope.Data, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ResourceID != contractID || page.Items[0].Category != "CONTRACT_OWNER" || page.Items[0].Version != 3 {
		t.Fatalf("responsibility page = %+v", page)
	}

	transferBody := fmt.Sprintf(`{"batch_id":"%s","item_id":"%s","resource_type":"contract","resource_id":"%s","from_user_id":"%s","to_user_id":"%s","expected_version":3,"idempotency_key":"handover:%s:%s"}`,
		batchID, itemID, contractID, leaver, successor, batchID, itemID)
	status, envelope = protocolCall(t, router, http.MethodPost, "/internal/v1/handover/transfers", transferBody)
	if status != http.StatusOK {
		t.Fatalf("transfer → %d %s", status, envelope.Code)
	}
	var receipt struct {
		System         string `json:"system"`
		ItemID         string `json:"item_id"`
		IdempotencyKey string `json:"idempotency_key"`
		Status         string `json:"status"`
		AppliedVersion uint64 `json:"applied_version"`
		EvidenceRef    string `json:"evidence_ref"`
	}
	if err := json.Unmarshal(envelope.Data, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "APPLIED" || receipt.AppliedVersion != 4 || receipt.EvidenceRef == "" || receipt.ItemID != itemID {
		t.Fatalf("receipt = %+v", receipt)
	}
	var owner string
	var version uint64
	if err := db.Table("con_contract").Select("owner_user_id, version").Where("id = ?", contractID).Row().Scan(&owner, &version); err != nil {
		t.Fatal(err)
	}
	if owner != successor || version != 4 {
		t.Fatalf("contract owner=%s version=%d", owner, version)
	}

	status, envelope = protocolCall(t, router, http.MethodPost, "/internal/v1/handover/transfers", transferBody)
	if status != http.StatusOK {
		t.Fatalf("replayed transfer → %d %s", status, envelope.Code)
	}
	var replayed struct {
		Status         string `json:"status"`
		AppliedVersion uint64 `json:"applied_version"`
	}
	if err := json.Unmarshal(envelope.Data, &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.Status != "APPLIED" || replayed.AppliedVersion != 4 {
		t.Fatalf("replayed receipt = %+v", replayed)
	}
	if err := db.Table("con_contract").Select("version").Where("id = ?", contractID).Row().Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 4 {
		t.Fatalf("replay changed the contract version to %d", version)
	}

	staleContract := seedProtocolContract(t, db, tenant, leaver, "active", 5)
	staleBody := fmt.Sprintf(`{"batch_id":"%s","item_id":"%s","resource_type":"contract","resource_id":"%s","from_user_id":"%s","to_user_id":"%s","expected_version":2,"idempotency_key":"handover-stale:%s:%s"}`,
		batchID, itemID, staleContract, leaver, successor, batchID, itemID)
	status, envelope = protocolCall(t, router, http.MethodPost, "/internal/v1/handover/transfers", staleBody)
	if status != http.StatusOK {
		t.Fatalf("stale transfer → %d %s", status, envelope.Code)
	}
	var rejected struct {
		Status     string `json:"status"`
		ReasonCode string `json:"reason_code"`
	}
	if err := json.Unmarshal(envelope.Data, &rejected); err != nil {
		t.Fatal(err)
	}
	if rejected.Status != "REJECTED" || rejected.ReasonCode != "CONTRACT_VERSION_CONFLICT" {
		t.Fatalf("stale receipt = %+v", rejected)
	}
	if err := db.Table("con_contract").Select("owner_user_id").Where("id = ?", staleContract).Row().Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != leaver {
		t.Fatalf("rejected transfer must not change ownership, owner = %s", owner)
	}

	completeBody := fmt.Sprintf(`{"batch_id":"%s","outcome":"COMPLETED"}`, batchID)
	status, envelope = protocolCall(t, router, http.MethodPost, "/internal/v1/handover/complete", completeBody)
	if status != http.StatusOK {
		t.Fatalf("complete → %d %s", status, envelope.Code)
	}
	var freezeStatus, finalOwner string
	if err := db.Table("con_handover_freeze").Select("status").Where("tenant_id = ? AND batch_id = ?", tenant, batchID).Row().Scan(&freezeStatus); err != nil {
		t.Fatal(err)
	}
	if freezeStatus != "RELEASED" {
		t.Fatalf("freeze status = %s, want RELEASED", freezeStatus)
	}
	if err := db.Table("con_contract").Select("owner_user_id").Where("id = ?", contractID).Row().Scan(&finalOwner); err != nil {
		t.Fatal(err)
	}
	if finalOwner != successor {
		t.Fatalf("completed transfer must be preserved, owner = %s", finalOwner)
	}
}
