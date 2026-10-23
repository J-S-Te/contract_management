package mysql

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
	httpapi "github.com/j-s-te/contract-management/internal/transport/httpapi"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/oklog/ulid/v2"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
)

type financialSQLIdentity struct{ principal *application.Principal }

func (i financialSQLIdentity) Authenticate(context.Context, *http.Request) (application.Principal, error) {
	return *i.principal, nil
}

type financialSQLCRM struct{}

func (financialSQLCRM) Resolve(context.Context, uint64, string, string) (application.CRMContractReference, error) {
	return application.CRMContractReference{Customer: application.CRMCustomerReference{ID: 42, Name: "authoritative customer", Status: "ACTIVE"}}, nil
}

type financialSQLPersonnel struct{}

func (financialSQLPersonnel) ListEligibleUsers(context.Context, application.Principal, []string) ([]application.UserReference, error) {
	return []application.UserReference{{UserID: "sales-director", Roles: []string{"sales_director"}}, {UserID: "tech-director", Roles: []string{"tech_director"}}, {UserID: "finance-director", Roles: []string{"finance_director"}}}, nil
}

type financialSQLWorkflowClient struct {
	client.Client
	execute func(client.StartWorkflowOptions, workflows.ContractApprovalInput)
}

func (c financialSQLWorkflowClient) ExecuteWorkflow(_ context.Context, options client.StartWorkflowOptions, _ interface{}, args ...interface{}) (client.WorkflowRun, error) {
	c.execute(options, args[0].(workflows.ContractApprovalInput))
	return financialSQLWorkflowRun{id: options.ID}, nil
}

type financialSQLWorkflowRun struct {
	client.WorkflowRun
	id string
}

func (r financialSQLWorkflowRun) GetID() string  { return r.id }
func (financialSQLWorkflowRun) GetRunID() string { return "native-workflow-fixture" }

func financialSQLDocument(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	z := zip.NewWriter(&buffer)
	for name, body := range map[string]string{
		"[Content_Types].xml": `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`,
		"word/document.xml":   `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>合同金额 {{合同金额}}</w:t></w:r></w:p></w:body></w:document>`,
	} {
		file, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// 使用真实 MySQL 迁移、HTTP 处理器、草稿 CAS 和审批活动。
// OIDC、CRM、人员及 Temporal 传输明确作为夹具边界；
// 不访问既有业务库或已部署身份提供方。
func TestFinancialContractHTTPApprovalOutboxE2E(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	gin.SetMode(gin.TestMode)
	t.Setenv("APP_PUBLIC_URL", "https://contract.example.test")
	t.Setenv("APP_CORS_ALLOWED_ORIGINS", "")
	ctx := context.Background()
	actorID := ulid.Make().String()
	actor := application.Principal{TenantID: tenant, UserID: actorID, IdentityID: actorID, Roles: []string{"admin"}, Permissions: map[string]bool{"contract.create": true, "contract.edit": true, "contract.read": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.create": {AllowAll: true}, "contract.edit": {AllowAll: true}, "contract.read": {AllowAll: true}}}
	templateID := ulid.Make().String()
	if err := r.CreateTemplate(ctx, contracttemplate.Template{ID: templateID, TenantID: tenant, Name: "financial test", NumberFormat: "HT-{ID8}", CreatedBy: actorID, Content: financialSQLDocument(t), Fields: []contracttemplate.Field{{Name: "合同金额", Label: "合同金额"}}}); err != nil {
		t.Fatal(err)
	}
	service := &application.Service{Repo: r, Templates: r, CRMContractReferences: financialSQLCRM{}, Personnel: financialSQLPersonnel{}}
	var contractID string
	service.Temporal = financialSQLWorkflowClient{execute: func(options client.StartWorkflowOptions, input workflows.ContractApprovalInput) {
		var suite testsuite.WorkflowTestSuite
		env := suite.NewTestWorkflowEnvironment()
		env.SetStartWorkflowOptions(options)
		env.RegisterActivity(&workflows.Activities{Store: r})
		env.RegisterDelayedCallback(func() {
			pending, err := r.GetContract(ctx, tenant, contractID)
			if err != nil || pending.Status != contract.StatusPending {
				t.Fatal("approval did not freeze draft")
			}
			if err := r.UpdateContractDraft(ctx, pending, pending.Version, actorID); !errors.Is(err, apperrors.ErrStateConflict) {
				t.Fatalf("pending terms mutable: %v", err)
			}
		}, time.Second)
		for index, node := range input.Nodes {
			userID := node.AssigneeIDs[0]
			env.RegisterDelayedCallback(func() {
				env.SignalWorkflow(workflows.CommandSignalName, workflows.ApprovalCommand{CommandID: ulid.Make().String(), Action: workflows.ActionApprove, ActorUserID: userID, OccurredAt: time.Now().UTC()})
			}, time.Duration(index+2)*time.Minute)
		}
		env.ExecuteWorkflow(workflows.ContractApprovalWorkflow, input)
		if err := env.GetWorkflowError(); err != nil {
			t.Fatal(err)
		}
	}}
	router := httpapi.NewRouter(service, financialSQLIdentity{&actor}, nil)
	request := func(method, path string, payload interface{}) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://contract.example.test")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	schedule := []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2026-12-01", Amount: "30.00"}, {InstallmentNo: 2, DueDate: "2027-01-01", Amount: "70.00"}}
	payload := map[string]interface{}{"title": "financial contract", "contract_type": "直签", "template_id": templateID, "amount_minor": 10000, "currency": "CNY", "crm_customer_id": 42, "customer_name": "browser snapshot", "service_items": []contract.ServiceItem{{ServiceType: "软件测试"}}, "payment_terms": schedule}
	response := request("POST", "/api/v1/contracts", payload)
	if response.Code != http.StatusCreated {
		t.Fatalf("HTTP create: %d %s", response.Code, response.Body.String())
	}
	var decoded struct {
		Data contract.Contract `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	contractID = decoded.Data.ID
	stored, err := r.GetContract(ctx, tenant, contractID)
	if err != nil || len(stored.PaymentTerms) != 2 || stored.CustomerName != "authoritative customer" {
		t.Fatal("HTTP schedule not persisted with trusted customer", err)
	}
	payload["expected_version"] = stored.Version
	payload["payment_terms"] = []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2026-12-02", Amount: "40.00"}, {InstallmentNo: 2, DueDate: "2027-01-02", Amount: "60.00"}}
	response = request("PUT", "/api/v1/contracts/"+contractID+"/draft", payload)
	if response.Code != http.StatusOK {
		t.Fatalf("HTTP schedule update: %d %s", response.Code, response.Body.String())
	}
	if stale := request("PUT", "/api/v1/contracts/"+contractID+"/draft", payload); stale.Code != http.StatusConflict {
		t.Fatalf("stale financial schedule accepted: %d", stale.Code)
	}
	stored, err = r.GetContract(ctx, tenant, contractID)
	if err != nil || stored.Version != 2 || stored.PaymentTerms[0].Amount != "40.00" {
		t.Fatal(stored, err)
	}
	payload["expected_version"] = stored.Version
	payload["amount_minor"] = 10100
	if bad := request("PUT", "/api/v1/contracts/"+contractID+"/draft", payload); bad.Code != 422 {
		t.Fatalf("mismatched total accepted: %d", bad.Code)
	}
	response = request("POST", "/api/v1/contracts/"+contractID+"/submit-approval", map[string]interface{}{"expected_version": stored.Version})
	if response.Code != http.StatusAccepted {
		t.Fatalf("HTTP approval: %d %s", response.Code, response.Body.String())
	}
	stored, err = r.GetContract(ctx, tenant, contractID)
	if err != nil || stored.Status != contract.StatusActive || len(stored.PaymentTerms) != 2 {
		t.Fatal("approval snapshot lost schedule", err)
	}
	var rows []financialEffectiveOutboxRecord
	if err := r.db.Where("tenant_id = ? AND contract_id = ?", tenant, contractID).Find(&rows).Error; err != nil || len(rows) != 1 {
		t.Fatalf("transactional event rows=%d err=%v", len(rows), err)
	}
	var event financialEffectivePayload
	if err := json.Unmarshal(rows[0].PayloadJSON, &event); err != nil {
		t.Fatal(err)
	}
	if event.Contract.Amount != "100.00" || event.Contract.Version != int(stored.Version) || len(event.Contract.PaymentTerms) != 2 || event.Contract.PaymentTerms[0].Amount != "40.00" || event.Contract.CustomerID != "42" {
		t.Fatalf("financial event did not use frozen approved schedule: %+v", event.Contract)
	}
	if output := os.Getenv("CONTRACT_TEST_FINANCIAL_EVENT_OUTPUT"); output != "" {
		// Go 从 internal/infrastructure/mysql 运行此包。
		allowed, err := filepath.Abs("../../../../.ai/qa/p1-business-fixes-20261013/agents")
		if err != nil || filepath.Dir(filepath.Clean(output)) != allowed || !strings.HasPrefix(filepath.Base(output), "contract-") || filepath.Ext(output) != ".json" {
			t.Fatal("financial test artifact must be a new contract-* JSON in owned QA directory")
		}
		file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal("financial test artifact creation failed")
		}
		_, writeErr := file.Write(rows[0].PayloadJSON)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatal("financial test artifact persistence failed")
		}
	}
	if _, err := r.ReconcileFinancialEffectives(ctx); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := r.db.Model(&financialEffectiveOutboxRecord{}).Where("tenant_id = ? AND contract_id = ?", tenant, contractID).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("reconcile duplicated approved event", err)
	}
	// 常规历史权限仍约束新的财务字段。
	actor.Permissions = nil
	if denied := request("GET", "/api/v1/contracts/"+contractID, nil); denied.Code != http.StatusForbidden {
		t.Fatal("schedule read bypassed permissions")
	}
	if _, err := r.GetContractScoped(ctx, contract.ScopeFilter{TenantID: tenant, IdentityID: "another-owner", AllowSelf: true}, contractID); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatal("schedule read bypassed owner scope")
	}
	if _, err := r.GetContract(ctx, ulid.Make().String(), contractID); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatal("schedule read bypassed tenant")
	}
	if err := r.UpdateContractDraft(ctx, stored, stored.Version, actorID); !errors.Is(err, apperrors.ErrStateConflict) {
		t.Fatal("effective schedule mutable")
	}
	if !strings.Contains(event.Contract.PaymentTerms[0].DueDate, "2026-12-02") {
		t.Fatal("due date changed")
	}
}
