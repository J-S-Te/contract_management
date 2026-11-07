package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	store "github.com/j-s-te/contract-management/internal/infrastructure/mysql"
	"github.com/j-s-te/contract-management/internal/testfixture"
	"github.com/j-s-te/contract-management/internal/transport/httpapi"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/client"
	temporalmocks "go.temporal.io/sdk/mocks"
	"gorm.io/gorm"
)

// historicalProvisionCustomer 是历史开立客户链路的最小替身：
// 总是返回一个已存在的 ACTIVE 客户，因此开立期不会调用创建接口。
type historicalProvisionCustomer struct{}

// LookupCustomer 返回固定候选，不调用业务 CRM。
// 参数：ctx/actor/key 为契约字段，name 为客户名称。
// 返回值：唯一的 ACTIVE 候选。
func (historicalProvisionCustomer) LookupCustomer(_ context.Context, _, _, name string) ([]application.CRMProvisionCustomerCandidate, error) {
	return []application.CRMProvisionCustomerCandidate{{ID: 501, Name: name, Status: "ACTIVE"}}, nil
}

// CreateCustomer 在测试中不会被调用。
// 参数：其余参数为客户输入。
// 返回值：固定客户。
func (historicalProvisionCustomer) CreateCustomer(_ context.Context, _, _ string, input application.CRMProvisionCustomerInput) (application.CRMProvisionCustomer, error) {
	return application.CRMProvisionCustomer{ID: 501, Name: input.Name, Status: "ACTIVE"}, nil
}

// historicalProvisionOpportunity 是商机链路的最小替身。
type historicalProvisionOpportunity struct{}

// CreateOpportunity 返回可推进的商机。
// 参数：ctx/actor/key 为契约字段，input 为商机输入。
// 返回值：固定商机。
func (historicalProvisionOpportunity) CreateOpportunity(_ context.Context, _, _ string, input application.CRMProvisionOpportunityInput) (application.CRMProvisionOpportunity, error) {
	return application.CRMProvisionOpportunity{ID: 901, Name: input.Name, CustomerID: input.CustomerID, Status: "FOLLOWING"}, nil
}

// SignOpportunity 返回已签约商机。
// 参数：ctx/actor/key/opportunityID/contractRef 为推进身份。
// 返回值：已签约商机。
func (historicalProvisionOpportunity) SignOpportunity(_ context.Context, _, _ string, opportunityID uint64, _ string) (application.CRMProvisionOpportunity, error) {
	return application.CRMProvisionOpportunity{ID: opportunityID, Status: "SIGNED", Stage: "CLOSED"}, nil
}

// historicalProvisionOwners 把销售姓名解析成固定身份。
type historicalProvisionOwners struct{}

// ResolveOwnerByName 返回固定候选。
// 参数：ctx 为上下文，displayName 为销售姓名。
// 返回值：唯一身份候选。
func (historicalProvisionOwners) ResolveOwnerByName(_ context.Context, displayName string) (application.OwnerDirectoryCandidate, error) {
	return application.OwnerDirectoryCandidate{IdentityID: "identity-sales", DisplayName: displayName}, nil
}

// historicalImportOwners 是历史责任人解析替身：按请求 id 原样返回。
type historicalImportOwners struct{}

// ResolveOwner 返回请求中的责任人，使提交侧的范围校验可以自洽通过。
// 参数：ctx/tenant/id 为解析身份。
// 返回值：责任人引用。
func (historicalImportOwners) ResolveOwner(_ context.Context, _, id string) (application.UserReference, error) {
	return application.UserReference{UserID: id, DisplayName: "历史责任人"}, nil
}

// historicalImportPersonnel 是人工会签受派人目录替身。
type historicalImportPersonnel struct{}

// ListEligibleUsers 为每个配置角色返回同一个审批人。
// 参数：ctx/actor 为调用身份，roleCodes 为角色模板。
// 返回值：带全部角色的审批人。
func (historicalImportPersonnel) ListEligibleUsers(_ context.Context, _ application.Principal, roleCodes []string) ([]application.UserReference, error) {
	return []application.UserReference{{UserID: "approver-1", Roles: append([]string(nil), roleCodes...)}}, nil
}

// historicalAutoApproveService 组装历史导入自动审批端到端用例的服务。
// 参数：db 为隔离库，gateway 为文件网关替身，actor 为测试主体，temporal 为工作流替身
// （AUTO 路径传 nil，验证完全不依赖 Temporal）。
// 返回值：已装配历史通道、开立通道与自动审批能力的服务。
func historicalAutoApproveService(db *gorm.DB, gateway *networkImportGateway, actor application.Principal, temporal client.Client) *application.Service {
	service := &application.Service{
		Repo: store.NewRepository(db), LicenseGate: networkImportLicense{},
		StampedFileGateway: gateway, StampedFileApplicationID: "contract",
		ExternalPDFEnabled: true, ContractImportEnabled: true, ContractImportMaxItems: 3,
		ContractImportHistoryEnabled:        true,
		ContractImportAutoApproveHistorical: true,
		ContractImportConfirmationRoles:     []string{"admin"},
		ImportReport:                        &application.ImportTargetTransport{}, ImportReportReadConfigured: true,
		ImportOwners:                  historicalImportOwners{},
		Personnel:                     historicalImportPersonnel{},
		CRMContractReferences:         networkImportDirectory{},
		DetectionCategories:           networkImportDirectory{},
		CRMProvisionCustomerClient:    historicalProvisionCustomer{},
		CRMProvisionOpportunityClient: historicalProvisionOpportunity{},
		CRMProvisionOwnerDirectory:    historicalProvisionOwners{},
		Temporal:                      temporal,
	}
	service.ImportActorResolver = func(ctx context.Context, hash []byte, tenantID, userID string) (application.Principal, error) {
		if tenantID != actor.TenantID || userID != actor.UserID {
			return application.Principal{}, application.ErrForbidden
		}
		return actor, nil
	}
	return service
}

// historicalActor 构造具备导入全流程权限的隔离测试主体。
// 参数：tenant/actorID 为租户与用户身份。
// 返回值：可信主体。
func historicalActor(tenant, actorID string) application.Principal {
	actor := application.Principal{TenantID: tenant, UserID: actorID, IdentityID: actorID,
		Permissions: map[string]bool{}, PermissionScopes: map[string]contract.ScopeFilter{}}
	for _, permission := range []string{"contract.create", "contract.import.preview", "contract.import.commit"} {
		actor.Permissions[permission] = true
		actor.PermissionScopes[permission] = contract.ScopeFilter{AllowAll: true}
	}
	return actor
}

// historicalServer 启动一个真实 TCP 路由，身份/许可/文件网关/CRM 使用显式替身。
// 参数：t 为测试上下文，service 为服务，tenant/actorID 为测试身份。
// 返回值：TCP 客户端。
func historicalServer(t *testing.T, service *application.Service, tenant, actorID string) networkImportClient {
	t.Helper()
	identity := networkImportIdentity{actor: historicalActor(tenant, actorID), hash: sha256.Sum256([]byte(ulid.Make().String()))}
	server := httptest.NewUnstartedServer(nil)
	t.Setenv("APP_PUBLIC_URL", "http://"+server.Listener.Addr().String())
	server.Config.Handler = httpapi.NewRouter(service, identity, nil)
	server.Start()
	t.Cleanup(server.Close)
	return networkImportClient{url: server.URL, client: &http.Client{Timeout: 30 * time.Second}}
}

// submitHistoricalConfirmation 通过真实 HTTP 提交冻结历史确认。
// 参数：t 为测试上下文，n 为 TCP 客户端，b 为 PREVIEWED 批次，key 为幂等键。
// 返回值：202 的受理台账。
func submitHistoricalConfirmation(t *testing.T, n networkImportClient, b contract.ImportBatch, key string) contract.ImportBatch {
	t.Helper()
	return networkImportJSON[contract.ImportBatch](t, n, http.MethodPost,
		"/api/v1/contract-import-batches/"+b.ID+"/submit-confirmation",
		map[string]any{"revision": b.Revision, "preview_token": b.PreviewToken}, key, http.StatusAccepted)
}

// historicalCleanup 按租户精确清理本用例写入的导入/合同/审计行。
//
// 为什么必须清理：调度器每次执行都会全局扫描 state='STARTING' 的确认（不按租户过滤）
// 并尝试启动工作流。本用例的人工子用例会留下 STARTING 确认，若不清掉，后续用例
// （以及下一次运行）会在 Temporal 未配置时被这份残留拖成「approval workflow is
// unavailable」。这里只删本用例自己创建的 tenant_id，不做全表清理。
//
// 参数：t 为测试上下文，db 为隔离库，tenant 为本用例租户。
// 返回值：无；清理失败记为测试错误。
func historicalCleanup(t *testing.T, db *gorm.DB, tenant string) {
	t.Helper()
	// 删除顺序按外键依赖从子表到父表：先投递/留痕类，再合同自身。
	for _, table := range []string{
		"con_project_delivery_outbox", "con_notification_outbox", "con_financial_effective_outbox",
		"con_contract_responsibility_outbox", "con_contract_responsibility_stream",
		"con_contract_stamped_document", "con_contract_signing_confirmation", "con_contract_signing",
		"con_contract_payment_term", "con_contract_lifecycle_event",
		"con_import_crm_provision", "con_import_identity", "con_import_batch",
		"con_import_auto_approve_setting", "con_audit_outbox",
		"con_contract_number_registry", "con_contract",
	} {
		if err := db.Exec("DELETE FROM "+table+" WHERE tenant_id=?", tenant).Error; err != nil {
			t.Errorf("cleanup %s: %v", table, err)
		}
	}
}

// historicalContractRow 是合同终态断言的数据库投影。
type historicalContractRow struct {
	Version           int        `gorm:"column:version"`
	Status            string     `gorm:"column:status"`
	ContractNumber    string     `gorm:"column:contract_number"`
	ImportConfirmedAt *time.Time `gorm:"column:import_confirmed_at"`
}

// historicalManifest 构造一份声明 ARCHIVE_ONLY 的历史清单项。
// 参数：sourceRef 为上传返回的原件身份，actorID 为目标责任人。
// 返回值：单项历史清单。
func historicalManifest(sourceRef, actorID string) contract.ImportManifest {
	date, _ := time.Parse("2006-01-02", "2020-01-01")
	facts := []contract.ImportFact{}
	return contract.ImportManifest{SchemaVersion: 1, SourceSystem: "isolated-history", Items: []contract.ImportItem{{
		Key: "history-one", ExternalRecordID: "history-old-1", Mode: contract.ImportArchiveOnly, SourceFileRef: sourceRef,
		Policy: &contract.ImportPolicy{Version: 1, ProjectMode: "NONE", FinanceMode: "NONE",
			ReportMode: "ARCHIVE_SEPARATE", ResponsibilityMode: "INACTIVE", SigningMode: "DECLARED_ONLY"},
		Metadata: contract.ImportMetadata{Title: "历史导入自动审批合同", ContractType: "直签", ServiceType: "软件测试",
			CustomerName: "历史客户", SalesName: "张三", AmountMinor: 123456, Currency: "CNY", StartDate: &date,
			ServiceItems: []contract.ServiceItem{{ServiceType: "软件测试", Site: "北京", Batch: "第一批",
				Category: "软件测试", TestMode: "STANDARD"}}},
		HistoryEvidence: &contract.ImportHistoryEvidence{DeclaredSignedDate: "2020-01-01", HistoricalStatus: "archived",
			HistoricalCustomerName: "原客户", HistoricalOwnerLabel: "原销售", TargetOwnerUserID: actorID,
			ExpectedFacts: &facts, ReportRequiredChannels: "API_ONLY"},
	}}}
}

// historicalPreparedBatch 走完整 HTTP 预检：建批次 → 上传原件 → 保存历史清单 → 预检。
// 参数：t 为测试上下文，n 为 TCP 客户端，actorID 为目标责任人。
// 返回值：PREVIEWED 批次。
func historicalPreparedBatch(t *testing.T, n networkImportClient, actorID string) contract.ImportBatch {
	t.Helper()
	pdf := testfixture.PDF("historical auto approve e2e")
	b := networkImportJSON[contract.ImportBatch](t, n, http.MethodPost, "/api/v1/contract-import-batches",
		map[string]any{"schema_version": 1, "source_system": "isolated-history"}, "", http.StatusCreated)
	path := "/api/v1/contract-import-batches/" + b.ID
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, err := form.CreateFormFile("file", "history-original.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write(pdf); err != nil {
		t.Fatal(err)
	}
	for field, value := range map[string]string{"item_key": "history-one", "expected_version": strconv.FormatUint(b.Version, 10)} {
		if err = form.WriteField(field, value); err != nil {
			t.Fatal(err)
		}
	}
	if err = form.Close(); err != nil {
		t.Fatal(err)
	}
	status, data, err := n.request(http.MethodPost, path+"/files", body.Bytes(), form.FormDataContentType(), "", false)
	if err != nil || status != http.StatusCreated {
		t.Fatalf("upload status=%d err=%v response=%s", status, err, data)
	}
	var upload struct {
		Data contract.ImportSource `json:"data"`
	}
	if json.Unmarshal(data, &upload) != nil || upload.Data.ID == "" {
		t.Fatalf("upload did not return file identity: %s", data)
	}
	b = networkImportJSON[contract.ImportBatch](t, n, http.MethodGet, path, nil, "", http.StatusOK)
	b = networkImportJSON[contract.ImportBatch](t, n, http.MethodPut, path+"/manifest",
		map[string]any{"expected_version": b.Version, "manifest": historicalManifest(upload.Data.ID, actorID)}, "", http.StatusOK)
	b = networkImportJSON[contract.ImportBatch](t, n, http.MethodPost, path+"/preview",
		map[string]any{"revision": b.Revision}, "", http.StatusOK)
	if b.State != "PREVIEWED" || b.PreviewToken == "" || len(b.Items) != 1 {
		t.Fatalf("preview state=%s items=%+v issues=%+v", b.State, b.Items, b.ProvisionPlan)
	}
	return b
}

// historicalDispatchUntilSettled 反复执行真实调度器，直到批次离开 QUEUED/RUNNING。
// 参数：t 为测试上下文，service 为服务，n 为 TCP 客户端，path 为批次路径。
// 返回值：收敛后的批次。
func historicalDispatchUntilSettled(t *testing.T, service *application.Service, n networkImportClient, path string) contract.ImportBatch {
	t.Helper()
	for step := 0; step < 10; step++ {
		if err := service.DispatchImportOnce(context.Background()); err != nil {
			t.Fatalf("dispatch step %d error: %v", step, err)
		}
		b := networkImportJSON[contract.ImportBatch](t, n, http.MethodGet, path, nil, "", http.StatusOK)
		switch b.State {
		case "QUEUED", "RUNNING":
			continue
		}
		return b
	}
	t.Fatal("批次在 10 次调度内未收敛")
	return contract.ImportBatch{}
}

// auditRow 是审计断言所需的最小投影。
type auditRow struct {
	ActorID     string `gorm:"column:actor_id"`
	PayloadJSON []byte `gorm:"column:payload_json"`
}

// historicalAudits 读取某合同上指定动作的审计行。
// 参数：t 为测试上下文，db 为隔离库，tenant/action/resourceID 为定位条件。
// 返回值：审计行；条数为 0 或 >1 时由调用方断言。
func historicalAudits(t *testing.T, db *gorm.DB, tenant, action, resourceID string) []auditRow {
	t.Helper()
	var rows []auditRow
	if err := db.Raw("SELECT actor_id, payload_json FROM con_audit_outbox WHERE tenant_id=? AND action=? AND resource_id=? ORDER BY id",
		tenant, action, resourceID).Scan(&rows).Error; err != nil {
		t.Fatalf("read audits: %v", err)
	}
	return rows
}

// decodeAuditPayload 把审计载荷解析成键值。
// 参数：t 为测试上下文，row 为审计行。
// 返回值：载荷键值。
func decodeAuditPayload(t *testing.T, row auditRow) map[string]any {
	t.Helper()
	decoded := map[string]any{}
	if err := json.Unmarshal(row.PayloadJSON, &decoded); err != nil {
		t.Fatalf("decode audit payload: %v", err)
	}
	return decoded
}

// TestHistoricalImportAutoApproveEndToEnd 覆盖 P0 修复的真实生产链路：
// HTTP 建批次 → 上传原件 → 历史清单 → 预检 → 提交确认（AUTO）→ 调度开立 → 自动归档。
// 断言重点是「提交冻结的 AUTO 台账真的被执行期认可」：合同到 version=2 且按声明归档、
// 批次进入 TARGETS_PENDING（不是 CONFIRMATION_PENDING）、审计同时具备策略来源，
// 以及运行开关在提交后关闭也不影响已固化批次（pin）。
// 参数：t 为测试上下文；仅 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：受理状态、策略字段、合同终态、批次状态、审计或幂等性不符时失败。
func TestHistoricalImportAutoApproveEndToEnd(t *testing.T) {
	db := networkImportDatabase(t)
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "false")
	ctx := context.Background()

	t.Run("auto-approve-freezes-policy-and-archives", func(t *testing.T) {
		tenant, actorID := ulid.Make().String(), ulid.Make().String()
		t.Cleanup(func() { historicalCleanup(t, db, tenant) })
		gateway := &networkImportGateway{objects: map[string][]byte{}, bindings: map[string]bool{}}
		repo := store.NewRepository(db)
		if _, err := repo.SetImportAutoApproveSetting(ctx, tenant, true, 0, "admin"); err != nil {
			t.Fatal(err)
		}
		// 刻意不配置 Temporal：AUTO 提交与执行都不得依赖工作流。
		service := historicalAutoApproveService(db, gateway, historicalActor(tenant, actorID), nil)
		n := historicalServer(t, service, tenant, actorID)
		b := historicalPreparedBatch(t, n, actorID)

		submitted := submitHistoricalConfirmation(t, n, b, "history-auto-submit-key")
		confirmation := submitted.Confirmation
		if confirmation == nil || confirmation.State != "APPROVED" {
			t.Fatalf("提交后的确认台账=%+v", confirmation)
		}
		if confirmation.PolicyDecision != contract.ImportPolicyDecisionAuto || confirmation.PolicyVersion != 1 {
			t.Fatalf("策略判定=%q version=%d", confirmation.PolicyDecision, confirmation.PolicyVersion)
		}
		if len(submitted.Items) != 1 || submitted.Items[0].Confirmed {
			t.Fatalf("提交阶段不得确认单项: %+v", submitted.Items)
		}

		path := "/api/v1/contract-import-batches/" + b.ID
		final := historicalDispatchUntilSettled(t, service, n, path)
		if final.State != "TARGETS_PENDING" {
			t.Fatalf("批次状态=%s items=%+v, want TARGETS_PENDING", final.State, final.Items)
		}
		if !final.Items[0].Confirmed || final.Items[0].ContractID == "" {
			t.Fatalf("单项未确认: %+v", final.Items[0])
		}
		contractID := final.Items[0].ContractID
		var row historicalContractRow
		if err := db.Raw("SELECT version,status,contract_number,import_confirmed_at FROM con_contract WHERE tenant_id=? AND id=?",
			tenant, contractID).Scan(&row).Error; err != nil {
			t.Fatal(err)
		}
		if row.Version != 2 || row.Status != "archived" || strings.TrimSpace(row.ContractNumber) == "" || row.ImportConfirmedAt == nil {
			t.Fatalf("合同终态 version=%d status=%s number=%q confirmed_at=%v",
				row.Version, row.Status, row.ContractNumber, row.ImportConfirmedAt)
		}

		autoAudits := historicalAudits(t, db, tenant, "CONTRACT_MANAGEMENT:IMPORT_AUTO_APPROVED", contractID)
		if len(autoAudits) != 1 || autoAudits[0].ActorID != "SYSTEM" {
			t.Fatalf("IMPORT_AUTO_APPROVED 审计=%+v", autoAudits)
		}
		autoPayload := decodeAuditPayload(t, autoAudits[0])
		if autoPayload["policy"] != "auto" || autoPayload["policy_source"] != "tenant_setting" ||
			autoPayload["setting_version"] != float64(1) {
			t.Fatalf("IMPORT_AUTO_APPROVED 载荷=%+v", autoPayload)
		}
		confirmedAudits := historicalAudits(t, db, tenant, "CONTRACT_MANAGEMENT:IMPORT_CONFIRMED", contractID)
		if len(confirmedAudits) != 1 || confirmedAudits[0].ActorID != "SYSTEM" {
			t.Fatalf("IMPORT_CONFIRMED 审计=%+v", confirmedAudits)
		}
		if decodeAuditPayload(t, confirmedAudits[0])["policy"] != "auto" {
			t.Fatalf("IMPORT_CONFIRMED 载荷=%+v", decodeAuditPayload(t, confirmedAudits[0]))
		}

		// 幂等：再跑一次调度不得重复取号、重复写审计或再次推进合同版本。
		if err := service.DispatchImportOnce(ctx); err != nil {
			t.Fatalf("idle dispatch error: %v", err)
		}
		if again := historicalAudits(t, db, tenant, "CONTRACT_MANAGEMENT:IMPORT_AUTO_APPROVED", contractID); len(again) != 1 {
			t.Fatalf("重复调度新增自动审批审计: %d", len(again))
		}
		var repeated historicalContractRow
		if err := db.Raw("SELECT version,status,contract_number FROM con_contract WHERE tenant_id=? AND id=?",
			tenant, contractID).Scan(&repeated).Error; err != nil {
			t.Fatal(err)
		}
		if repeated.Version != row.Version || repeated.ContractNumber != row.ContractNumber {
			t.Fatalf("重复调度改写了合同: %+v", repeated)
		}
	})

	t.Run("pinned-auto-survives-switch-off", func(t *testing.T) {
		tenant, actorID := ulid.Make().String(), ulid.Make().String()
		t.Cleanup(func() { historicalCleanup(t, db, tenant) })
		gateway := &networkImportGateway{objects: map[string][]byte{}, bindings: map[string]bool{}}
		repo := store.NewRepository(db)
		if _, err := repo.SetImportAutoApproveSetting(ctx, tenant, true, 0, "admin"); err != nil {
			t.Fatal(err)
		}
		service := historicalAutoApproveService(db, gateway, historicalActor(tenant, actorID), nil)
		n := historicalServer(t, service, tenant, actorID)
		b := historicalPreparedBatch(t, n, actorID)
		submitted := submitHistoricalConfirmation(t, n, b, "history-pin-submit-key")
		if submitted.Confirmation == nil || !submitted.Confirmation.AutoApproved() {
			t.Fatalf("提交后未固化 AUTO: %+v", submitted.Confirmation)
		}
		// 执行前把租户开关关掉（version 1 → 2）：已固化批次必须继续按 AUTO 完成。
		if _, err := repo.SetImportAutoApproveSetting(ctx, tenant, false, 1, "admin"); err != nil {
			t.Fatal(err)
		}

		path := "/api/v1/contract-import-batches/" + b.ID
		final := historicalDispatchUntilSettled(t, service, n, path)
		if final.State != "TARGETS_PENDING" || final.Confirmation == nil || !final.Confirmation.AutoApproved() {
			t.Fatalf("pin 失效: state=%s confirmation=%+v", final.State, final.Confirmation)
		}
		// 判定版本仍是提交时读到的 1，而不是关闭后的 2。
		if final.Confirmation.PolicyVersion != 1 {
			t.Fatalf("policy_version=%d, want 1", final.Confirmation.PolicyVersion)
		}
		var row historicalContractRow
		if err := db.Raw("SELECT version,status,contract_number FROM con_contract WHERE tenant_id=? AND id=?",
			tenant, final.Items[0].ContractID).Scan(&row).Error; err != nil {
			t.Fatal(err)
		}
		if row.Version != 2 || row.Status != "archived" || strings.TrimSpace(row.ContractNumber) == "" {
			t.Fatalf("pin 批次未归档: %+v", row)
		}
	})

	t.Run("manual-submit-keeps-confirmation-pending", func(t *testing.T) {
		tenant, actorID := ulid.Make().String(), ulid.Make().String()
		t.Cleanup(func() { historicalCleanup(t, db, tenant) })
		gateway := &networkImportGateway{objects: map[string][]byte{}, bindings: map[string]bool{}}
		// 反向：提交时运行开关关闭（无记录 = Version 0/关闭），必须走人工会签。
		temporal := temporalmocks.NewClient(t)
		run := temporalmocks.NewWorkflowRun(t)
		run.On("GetID").Return("workflow-1").Maybe()
		run.On("GetRunID").Return("run-1").Maybe()
		temporal.On("ExecuteWorkflow", mock.Anything, mock.Anything, workflows.ImportConfirmationWorkflowName, mock.Anything).Return(run, nil)
		service := historicalAutoApproveService(db, gateway, historicalActor(tenant, actorID), temporal)
		n := historicalServer(t, service, tenant, actorID)
		b := historicalPreparedBatch(t, n, actorID)

		submitted := submitHistoricalConfirmation(t, n, b, "history-manual-submit-key")
		if submitted.Confirmation == nil || submitted.Confirmation.State != "STARTING" ||
			submitted.Confirmation.PolicyDecision != contract.ImportPolicyDecisionManual {
			t.Fatalf("反向提交应落人工路径: %+v", submitted.Confirmation)
		}
		if len(temporal.Calls) != 1 {
			t.Fatalf("人工提交应启动一次工作流: calls=%d", len(temporal.Calls))
		}

		path := "/api/v1/contract-import-batches/" + b.ID
		final := historicalDispatchUntilSettled(t, service, n, path)
		if final.State != "CONFIRMATION_PENDING" {
			t.Fatalf("批次状态=%s, want CONFIRMATION_PENDING", final.State)
		}
		if final.Items[0].Confirmed {
			t.Fatalf("人工会签在途时被自动确认: %+v", final.Items[0])
		}
		var row historicalContractRow
		if err := db.Raw("SELECT version,status,contract_number FROM con_contract WHERE tenant_id=? AND id=?",
			tenant, final.Items[0].ContractID).Scan(&row).Error; err != nil {
			t.Fatal(err)
		}
		if row.Version != 1 || row.Status != string(contract.StatusDraft) || strings.TrimSpace(row.ContractNumber) != "" {
			t.Fatalf("人工路径不得归档: %+v", row)
		}
	})
}
