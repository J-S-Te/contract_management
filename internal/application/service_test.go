package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/stretchr/testify/mock"
	temporalmocks "go.temporal.io/sdk/mocks"
)

func allowAllScope(permission string) map[string]contract.ScopeFilter {
	return map[string]contract.ScopeFilter{permission: {AllowAll: true}}
}

func allowSelfScope(permission string) map[string]contract.ScopeFilter {
	return map[string]contract.ScopeFilter{permission: {AllowSelf: true}}
}

func TestListApprovedContractReferencesUsesStableCursorAndAuthorizedTenant(t *testing.T) {
	repository := &recordingRepository{approvedContractReferences: []contract.Contract{{ID: "C-2", Number: "HT-2", Version: 3}}}
	service := &Service{Repo: repository}
	actor := Principal{
		TenantID:         "tenant-1",
		Permissions:      map[string]bool{"contract.approved.read": true},
		PermissionScopes: allowAllScope("contract.approved.read"),
	}

	references, err := service.ListApprovedContractReferences(context.Background(), actor, "C-1", 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 1 || references[0].ID != "C-2" {
		t.Fatalf("references=%+v", references)
	}
	if repository.approvedReferenceTenant != "tenant-1" || repository.approvedReferenceAfter != "C-1" || repository.approvedReferenceLimit != 500 {
		t.Fatalf("tenant=%q after=%q limit=%d", repository.approvedReferenceTenant, repository.approvedReferenceAfter, repository.approvedReferenceLimit)
	}

	actor.Permissions = map[string]bool{}
	if _, err := service.ListApprovedContractReferences(context.Background(), actor, "", 500); !errors.Is(err, ErrForbidden) {
		t.Fatalf("missing permission error=%v, want ErrForbidden", err)
	}
}

type recordingRepository struct {
	ownerUserID                string
	contract                   contract.Contract
	created                    contract.Contract
	approvalMeta               approval.Meta
	actions                    []approval.Action
	lifecycle                  []contract.LifecycleEvent
	dashboard                  contract.Dashboard
	dashboardTenantID          string
	dashboardOwnerUserID       string
	approvedContractReferences []contract.Contract
	approvedReferenceTenant    string
	approvedReferenceAfter     string
	approvedReferenceLimit     int
}

type personnelStub struct {
	users []UserReference
	err   error
}

type detectionCategoryDirectoryStub struct {
	items []DetectionCategory
	err   error
}

type crmReferenceDirectoryStub struct {
	reference CRMContractReference
	err       error
}

func (stub crmReferenceDirectoryStub) Resolve(context.Context, uint64, string, string) (CRMContractReference, error) {
	return stub.reference, stub.err
}

func validCRMReference(customerID uint64) crmReferenceDirectoryStub {
	return crmReferenceDirectoryStub{reference: CRMContractReference{Customer: CRMCustomerReference{ID: customerID, Name: "CRM 权威客户", Status: "ACTIVE"}}}
}

func (s detectionCategoryDirectoryStub) List(context.Context) ([]DetectionCategory, error) {
	return s.items, s.err
}

func enabledDetectionCategories(values ...string) detectionCategoryDirectoryStub {
	items := make([]DetectionCategory, 0, len(values))
	for _, value := range values {
		items = append(items, DetectionCategory{Category: value, Enabled: true})
	}
	return detectionCategoryDirectoryStub{items: items}
}

func (s personnelStub) ListEligibleUsers(context.Context, Principal, []string) ([]UserReference, error) {
	return s.users, s.err
}

func (r *recordingRepository) GetContract(context.Context, string, string) (contract.Contract, error) {
	return r.contract, nil
}

func (r *recordingRepository) ListContracts(_ context.Context, _, _, ownerUserID, _ string, _ int) ([]contract.Contract, error) {
	r.ownerUserID = ownerUserID
	return nil, nil
}
func (r *recordingRepository) ListApprovedContracts(context.Context, string, int) ([]contract.Contract, error) {
	return nil, nil
}
func (r *recordingRepository) ListApprovedContractReferences(_ context.Context, tenantID, afterID string, limit int) ([]contract.Contract, error) {
	r.approvedReferenceTenant = tenantID
	r.approvedReferenceAfter = afterID
	r.approvedReferenceLimit = limit
	return r.approvedContractReferences, nil
}
func (r *recordingRepository) SaveStampedDocument(context.Context, string, contract.StampedDocument) error {
	return nil
}
func (r *recordingRepository) GetStampedDocument(context.Context, string, string) (contract.StampedDocument, error) {
	return contract.StampedDocument{}, nil
}
func (r *recordingRepository) ListSigningRecords(context.Context, string, int) ([]contract.SigningRecord, error) {
	return nil, nil
}
func (r *recordingRepository) GetSigningRecord(context.Context, string, string) (contract.SigningRecord, error) {
	return contract.SigningRecord{}, nil
}
func (r *recordingRepository) SaveSigningShipment(context.Context, string, string, string, contract.SigningShipment) error {
	return nil
}
func (r *recordingRepository) MarkSigningReceived(context.Context, string, string, string) error {
	return nil
}
func (r *recordingRepository) RecordSigningReminder(context.Context, string, string, string) error {
	return nil
}
func (r *recordingRepository) ConfirmSigning(context.Context, string, string, string, contract.SigningConfirmation) error {
	return nil
}

func (r *recordingRepository) ListContractLifecycle(context.Context, string, string) ([]contract.LifecycleEvent, error) {
	return r.lifecycle, nil
}

func (r *recordingRepository) ContractDashboard(_ context.Context, tenantID, ownerUserID string, _ time.Time, _ int) (contract.Dashboard, error) {
	r.dashboardTenantID = tenantID
	r.dashboardOwnerUserID = ownerUserID
	return r.dashboard, nil
}

func (r *recordingRepository) CreateContract(_ context.Context, created contract.Contract, _ string) error {
	r.created = created
	return nil
}

func (r *recordingRepository) TransitionDirect(context.Context, string, string, uint64, contract.Status, string, string, string) error {
	return nil
}

func (r *recordingRepository) ListEnabledRules(context.Context, string) ([]approval.Rule, error) {
	return nil, nil
}

func (r *recordingRepository) ListRules(context.Context, string) ([]approval.Rule, error) {
	return nil, nil
}

func (r *recordingRepository) CreateRule(context.Context, approval.Rule, string) error {
	return nil
}

func (r *recordingRepository) UpdateRule(context.Context, approval.Rule, string) error {
	return nil
}

func (r *recordingRepository) DeleteRule(context.Context, string, string, uint64) error {
	return nil
}

func (r *recordingRepository) GetApprovalMeta(context.Context, string, string) (approval.Meta, error) {
	return r.approvalMeta, nil
}

func (r *recordingRepository) ListApprovalActions(context.Context, string, string) ([]approval.Action, error) {
	return r.actions, nil
}

func (r *recordingRepository) ListApprovals(context.Context, string, string, int) ([]approval.Summary, error) {
	return nil, nil
}

func (r *recordingRepository) ListTasks(context.Context, string, string, int) ([]approval.Task, error) {
	return nil, nil
}

func TestListContractsScopesNonManagerToAuthenticatedUser(t *testing.T) {
	repository := &recordingRepository{}
	service := &Service{Repo: repository, DetectionCategories: enabledDetectionCategories("等保测评")}
	actor := Principal{
		TenantID:         "tenant-1",
		UserID:           "user-1",
		Permissions:      map[string]bool{"contract.read": true},
		PermissionScopes: allowSelfScope("contract.read"),
	}

	if _, err := service.ListContracts(context.Background(), actor, "another-user", "", 50); err != nil {
		t.Fatalf("ListContracts() error = %v", err)
	}
	if repository.ownerUserID != actor.UserID {
		t.Fatalf("owner filter = %q, want authenticated user %q", repository.ownerUserID, actor.UserID)
	}
}

func TestSalesScopeCannotBeWidenedByBroadAuthorizationClaim(t *testing.T) {
	service := &Service{}
	actor := Principal{
		TenantID: "tenant-1", UserID: "sales-1", IdentityID: "sales-1", Roles: []string{"sales"},
		Permissions: map[string]bool{"contract.read": true},
		PermissionScopes: map[string]contract.ScopeFilter{
			"contract.read": {AllowAll: true, OrganizationIDs: []string{"org-1"}, ProjectIDs: []string{"project-1"}},
		},
	}
	filter, ok := service.contractScope(actor, "contract.read")
	if !ok || filter.AllowAll || !filter.AllowSelf || len(filter.OrganizationIDs) != 0 || len(filter.ProjectIDs) != 0 || filter.IdentityID != "sales-1" {
		t.Fatalf("sales contract scope = %#v, want self-only", filter)
	}
}

func TestAdminListContractsUsesTenantScopeAndCanReadTenantContract(t *testing.T) {
	repository := &recordingRepository{contract: contract.Contract{ID: "contract-2", TenantID: "tenant-1", OwnerUserID: "user-2"}}
	service := &Service{Repo: repository}
	actor := Principal{
		TenantID:         "tenant-1",
		UserID:           "admin-1",
		Roles:            []string{"admin"},
		Permissions:      map[string]bool{"contract.read": true},
		PermissionScopes: allowAllScope("contract.read"),
	}

	if _, err := service.ListContracts(context.Background(), actor, "", "", 50); err != nil {
		t.Fatalf("ListContracts() error = %v", err)
	}
	if repository.ownerUserID != "" {
		t.Fatalf("owner filter = %q, want tenant-wide scope", repository.ownerUserID)
	}
	if _, err := service.GetContract(context.Background(), actor, "contract-2"); err != nil {
		t.Fatalf("GetContract() error = %v", err)
	}
}

func TestAdminCanListTenantContractLifecycle(t *testing.T) {
	want := []contract.LifecycleEvent{{ID: "event-1", ContractID: "contract-2", FromStatus: contract.StatusDraft, ToStatus: contract.StatusPending}}
	repository := &recordingRepository{
		contract:  contract.Contract{ID: "contract-2", TenantID: "tenant-1", OwnerUserID: "user-2"},
		lifecycle: want,
	}
	service := &Service{Repo: repository}
	actor := Principal{
		TenantID:         "tenant-1",
		UserID:           "admin-1",
		Roles:            []string{"admin"},
		Permissions:      map[string]bool{"contract.read": true},
		PermissionScopes: allowAllScope("contract.read"),
	}

	got, err := service.ListContractLifecycle(context.Background(), actor, "contract-2")
	if err != nil {
		t.Fatalf("ListContractLifecycle() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != want[0].ID {
		t.Fatalf("ListContractLifecycle() = %#v, want %#v", got, want)
	}
}

func TestContractDashboardScopesAdminToTenantAndOtherUsersToSelf(t *testing.T) {
	want := contract.Dashboard{TotalContracts: 8, TotalAmountMinor: 9000, ApprovalContracts: 2, ActiveContracts: 3, ExpiredContracts: 1}
	repository := &recordingRepository{dashboard: want}
	service := &Service{Repo: repository}
	admin := Principal{
		TenantID:         "tenant-1",
		Roles:            []string{"admin"},
		Permissions:      map[string]bool{"contract.read": true},
		PermissionScopes: allowAllScope("contract.read"),
	}

	got, err := service.ContractDashboard(context.Background(), admin)
	if err != nil || got.TenantID != "tenant-1" || got.TotalContracts != want.TotalContracts || got.ApprovalContracts != want.ApprovalContracts {
		t.Fatalf("ContractDashboard() = %#v, %v", got, err)
	}
	if repository.dashboardTenantID != "tenant-1" || repository.dashboardOwnerUserID != "" {
		t.Fatalf("admin dashboard scope = tenant %q, owner %q", repository.dashboardTenantID, repository.dashboardOwnerUserID)
	}
	user := Principal{
		TenantID:         "tenant-2",
		UserID:           "sales-1",
		Roles:            []string{"sales"},
		Permissions:      map[string]bool{"contract.read": true},
		PermissionScopes: allowSelfScope("contract.read"),
	}
	userDashboard, err := service.ContractDashboard(context.Background(), user)
	if err != nil {
		t.Fatalf("ContractDashboard() user error = %v", err)
	}
	if userDashboard.TenantID != "tenant-2" {
		t.Fatalf("ContractDashboard() user tenant = %q, want tenant-2", userDashboard.TenantID)
	}
	if repository.dashboardTenantID != "tenant-2" || repository.dashboardOwnerUserID != "sales-1" {
		t.Fatalf("user dashboard scope = tenant %q, owner %q", repository.dashboardTenantID, repository.dashboardOwnerUserID)
	}
	if _, err := service.ContractDashboard(context.Background(), Principal{TenantID: "tenant-1"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ContractDashboard() missing permission error = %v, want ErrForbidden", err)
	}
}

func TestCreateContractStoresChineseDisplayNameSnapshot(t *testing.T) {
	repository := &recordingRepository{}
	service := serviceWithContractTemplate(t, repository)
	actor := Principal{
		TenantID:         "tenant-1",
		UserID:           "user-1",
		DisplayName:      "章六",
		Permissions:      map[string]bool{"contract.create": true},
		PermissionScopes: allowAllScope("contract.create"),
	}
	created, err := service.CreateContract(context.Background(), actor, contract.Contract{
		Number: "CON-001", Title: "合同", Type: "service", TemplateID: "template-1",
		ServiceItems: []contract.ServiceItem{{ServiceType: "consulting"}},
	})
	if err != nil {
		t.Fatalf("CreateContract() error = %v", err)
	}
	if created.OwnerUserID != "user-1" || created.OwnerDisplayName != "章六" ||
		repository.created.OwnerDisplayName != "章六" {
		t.Fatalf("created contract = %#v, persisted = %#v", created, repository.created)
	}
}

func TestCreateContractAllowsNumberToBeAssignedAfterApproval(t *testing.T) {
	repository := &recordingRepository{}
	service := serviceWithContractTemplate(t, repository)
	actor := Principal{
		TenantID:         "tenant-1",
		UserID:           "user-1",
		Permissions:      map[string]bool{"contract.create": true},
		PermissionScopes: allowAllScope("contract.create"),
	}
	created, err := service.CreateContract(context.Background(), actor, contract.Contract{
		Title: "测评合同", Type: "直签", TemplateID: "template-1", CustomerName: "示例客户",
		ServiceItems: []contract.ServiceItem{{ServiceType: "等保测评", Systems: []contract.SystemInfo{{Name: "业务系统", Level: "三级"}}}},
	})
	if err != nil {
		t.Fatalf("CreateContract() error = %v", err)
	}
	if created.Number != "" || repository.created.Number != "" || repository.created.CustomerName != "示例客户" || len(repository.created.ServiceItems) != 1 || len(repository.created.Systems) != 1 {
		t.Fatalf("created contract = %#v, persisted = %#v", created, repository.created)
	}
}

func TestCreateContractRequiresTemplateAndServiceItems(t *testing.T) {
	actor := Principal{
		TenantID:         "tenant-1",
		UserID:           "user-1",
		Permissions:      map[string]bool{"contract.create": true},
		PermissionScopes: allowAllScope("contract.create"),
	}
	withoutTemplate := &Service{Repo: &recordingRepository{}}
	_, err := withoutTemplate.CreateContract(context.Background(), actor, contract.Contract{
		Title: "测评合同", Type: "直签", Content: "手工正文",
		ServiceItems: []contract.ServiceItem{{ServiceType: "等保测评"}},
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("CreateContract() without template error = %v, want ErrValidation", err)
	}

	withoutServiceItems := serviceWithContractTemplate(t, &recordingRepository{})
	_, err = withoutServiceItems.CreateContract(context.Background(), actor, contract.Contract{
		Title: "测评合同", Type: "直签", TemplateID: "template-1",
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("CreateContract() without service items error = %v, want ErrValidation", err)
	}
}

func TestCreateContractRejectsStartDateAfterEndDate(t *testing.T) {
	start := time.Date(2026, time.February, 2, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, -1)
	service := serviceWithContractTemplate(t, &recordingRepository{})
	actor := Principal{
		TenantID:         "tenant-1",
		UserID:           "user-1",
		Permissions:      map[string]bool{"contract.create": true},
		PermissionScopes: allowAllScope("contract.create"),
	}
	_, err := service.CreateContract(context.Background(), actor, contract.Contract{
		Number: "CON-001", Title: "合同", Type: "service", TemplateID: "template-1",
		ServiceItems: []contract.ServiceItem{{ServiceType: "consulting"}},
		StartDate:    &start, EndDate: &end,
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("CreateContract() error = %v, want ErrValidation", err)
	}
}

func TestCreateExternalContractFreezesDocumentAndNormalizesServiceScope(t *testing.T) {
	repository := &recordingRepository{}
	service := &Service{Repo: repository, DetectionCategories: enabledDetectionCategories("等保测评"), CRMContractReferences: validCRMReference(7)}
	actor := Principal{
		TenantID: "tenant-1", UserID: "user-1", DisplayName: "章六", IdentityID: "identity-1",
		Permissions: map[string]bool{"contract.create": true}, PermissionScopes: allowAllScope("contract.create"),
	}
	document := applicationTestDOCX(t, "外部合同正文")
	created, err := service.CreateExternalContract(context.Background(), actor, contract.Contract{
		Title: " 外部合同 ", Type: " 直签 ", CRMCustomerID: 7, CustomerName: " 示例客户 ",
		AmountMinor: 10000, Currency: "CNY", Document: document,
		ServiceItems: []contract.ServiceItem{{
			ServiceType: " 等保测评 ", Site: " 杭州机房 ", Batch: " 第一批 ", Category: " 等保测评 ", TestMode: "standard",
			Systems: []contract.SystemInfo{{Name: " 核心系统 ", Level: "三级"}},
		}},
	})
	if err != nil {
		t.Fatalf("CreateExternalContract() error = %v", err)
	}
	if created.TemplateID != "" || created.Content != "外部合同正文" || created.ContentHash == "" || created.NumberFormat != contracttemplate.DefaultNumberFormat {
		t.Fatalf("created=%+v", created)
	}
	if len(created.ServiceItems) != 1 || created.ServiceItems[0].Name != "等保测评" || created.ServiceItems[0].TestMode != "STANDARD" || created.ServiceItems[0].Site != "杭州机房" {
		t.Fatalf("normalized service items=%+v", created.ServiceItems)
	}
	if len(repository.created.Document) == 0 || repository.created.TemplateID != "" {
		t.Fatalf("persisted=%+v", repository.created)
	}
	if repository.created.CustomerName != "CRM 权威客户" {
		t.Fatalf("customer name=%q, want authoritative CRM snapshot", repository.created.CustomerName)
	}
}

func TestExternalDetectionCategoryMustBeEnabledAndDirectoryFailsClosed(t *testing.T) {
	actor := Principal{TenantID: "tenant-1", UserID: "user-1", IdentityID: "identity-1", Permissions: map[string]bool{"contract.create": true}, PermissionScopes: allowAllScope("contract.create")}
	input := contract.Contract{
		Title: "外部合同", Type: "直签", CRMCustomerID: 7, CustomerName: "示例客户", AmountMinor: 10000, Currency: "CNY",
		Document:     applicationTestDOCX(t, "外部合同正文"),
		ServiceItems: []contract.ServiceItem{{ServiceType: "等保测评", Site: "杭州", Batch: "第一批", Category: "已停用类别", TestMode: "STANDARD"}},
	}
	service := &Service{Repo: &recordingRepository{}, DetectionCategories: enabledDetectionCategories("等保测评"), CRMContractReferences: validCRMReference(7)}
	if _, err := service.CreateExternalContract(context.Background(), actor, input); !errors.Is(err, ErrValidation) {
		t.Fatalf("disabled category error=%v, want ErrValidation", err)
	}
	service.DetectionCategories = nil
	if _, err := service.CreateExternalContract(context.Background(), actor, input); !errors.Is(err, ErrDetectionCategoryDirectoryUnavailable) {
		t.Fatalf("missing directory error=%v, want ErrDetectionCategoryDirectoryUnavailable", err)
	}
}

func TestExternalContractCRMReferenceFailsClosedAndRejectsCrossCustomerOpportunity(t *testing.T) {
	actor := Principal{TenantID: "tenant-1", UserID: "user-1", IdentityID: "identity-1", Permissions: map[string]bool{"contract.create": true}, PermissionScopes: allowAllScope("contract.create")}
	input := contract.Contract{
		Title: "外部合同", Type: "直签", CRMCustomerID: 7, CustomerName: "浏览器快照", OpportunityID: "9", OpportunityName: "浏览器商机",
		AmountMinor: 10000, Currency: "CNY", Document: applicationTestDOCX(t, "外部合同正文"),
		ServiceItems: []contract.ServiceItem{{ServiceType: "等保测评", Site: "杭州", Batch: "第一批", Category: "等保测评", TestMode: "STANDARD"}},
	}
	service := &Service{Repo: &recordingRepository{}, DetectionCategories: enabledDetectionCategories("等保测评")}
	if _, err := service.CreateExternalContract(context.Background(), actor, input); !errors.Is(err, ErrCRMReferenceDirectoryUnavailable) {
		t.Fatalf("missing CRM directory error=%v", err)
	}
	service.CRMContractReferences = crmReferenceDirectoryStub{reference: CRMContractReference{
		Customer:    CRMCustomerReference{ID: 7, Name: "权威客户", Status: "ACTIVE"},
		Opportunity: &CRMOpportunityReference{ID: 9, Name: "跨客户商机", CustomerID: 8, Status: "FOLLOWING"},
	}}
	if _, err := service.CreateExternalContract(context.Background(), actor, input); !errors.Is(err, ErrValidation) {
		t.Fatalf("cross-customer opportunity error=%v, want ErrValidation", err)
	}
	service.CRMContractReferences = crmReferenceDirectoryStub{err: ErrCRMReferenceInvalid}
	if _, err := service.CreateExternalContract(context.Background(), actor, input); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid CRM reference error=%v, want ErrValidation", err)
	}
}

func TestListDetectionCategoriesFiltersDisabledBlankAndDuplicateValues(t *testing.T) {
	service := &Service{DetectionCategories: detectionCategoryDirectoryStub{items: []DetectionCategory{
		{Category: " 等保测评 ", Enabled: true}, {Category: "等保测评", Enabled: true},
		{Category: "停用类别", Enabled: false}, {Category: " ", Enabled: true},
	}}}
	actor := Principal{TenantID: "tenant-1", Permissions: map[string]bool{"contract.create": true}, PermissionScopes: allowAllScope("contract.create")}
	items, err := service.ListDetectionCategories(context.Background(), actor)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Category != "等保测评" || !items[0].Enabled {
		t.Fatalf("items=%+v", items)
	}
}

func TestCreateExternalContractRejectsIncompleteMetadata(t *testing.T) {
	service := &Service{Repo: &recordingRepository{}, DetectionCategories: enabledDetectionCategories("等保测评"), CRMContractReferences: validCRMReference(7)}
	actor := Principal{
		TenantID: "tenant-1", UserID: "user-1", IdentityID: "identity-1", Permissions: map[string]bool{"contract.create": true},
		PermissionScopes: allowAllScope("contract.create"),
	}
	valid := contract.Contract{
		Title: "外部合同", Type: "直签", CRMCustomerID: 7, CustomerName: "示例客户", AmountMinor: 10000, Currency: "CNY",
		Document:     applicationTestDOCX(t, "外部合同正文"),
		ServiceItems: []contract.ServiceItem{{ServiceType: "等保测评", Site: "杭州", Batch: "第一批", Category: "等保测评", TestMode: "STANDARD"}},
	}
	tests := map[string]func(*contract.Contract){
		"invalid contract type": func(c *contract.Contract) { c.Type = "其他" },
		"missing CRM customer":  func(c *contract.Contract) { c.CRMCustomerID = 0 },
		"missing customer name": func(c *contract.Contract) { c.CustomerName = "" },
		"zero amount":           func(c *contract.Contract) { c.AmountMinor = 0 },
		"missing site":          func(c *contract.Contract) { c.ServiceItems[0].Site = "" },
		"missing batch":         func(c *contract.Contract) { c.ServiceItems[0].Batch = "" },
		"missing category":      func(c *contract.Contract) { c.ServiceItems[0].Category = "" },
		"missing test mode":     func(c *contract.Contract) { c.ServiceItems[0].TestMode = "" },
		"invalid test mode":     func(c *contract.Contract) { c.ServiceItems[0].TestMode = "OTHER" },
		"partial system":        func(c *contract.Contract) { c.ServiceItems[0].Systems = []contract.SystemInfo{{Name: "系统"}} },
		"opportunity no name":   func(c *contract.Contract) { c.OpportunityID = "9" },
		"invalid opportunity ID": func(c *contract.Contract) {
			c.OpportunityID, c.OpportunityName = "not-a-number", "无效商机"
		},
		"template mixed in": func(c *contract.Contract) { c.TemplateID = "template-1" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.ServiceItems = append([]contract.ServiceItem(nil), valid.ServiceItems...)
			mutate(&candidate)
			if _, err := service.CreateExternalContract(context.Background(), actor, candidate); !errors.Is(err, ErrValidation) {
				t.Fatalf("CreateExternalContract() error=%v, want ErrValidation", err)
			}
		})
	}
}

func serviceWithContractTemplate(t *testing.T, repository Repository) *Service {
	t.Helper()
	return &Service{
		Repo: repository,
		Templates: &memoryTemplateRepository{items: map[string]contracttemplate.Template{
			"template-1": {
				ID: "template-1", TenantID: "tenant-1", Name: "测试模板",
				Content: applicationTestDOCX(t, "合同正文"),
			},
		}},
	}
}

func TestListContractsIgnoresRequestedOwnerEvenWithLegacyManagerPermission(t *testing.T) {
	repository := &recordingRepository{}
	service := &Service{Repo: repository}
	actor := Principal{
		TenantID: "tenant-1",
		UserID:   "manager-1",
		// Keep legacy manager permission while enforcing scope-based checks.
		Permissions: map[string]bool{
			"contract.read":   true,
			"contract.manage": true,
		},
		PermissionScopes: allowSelfScope("contract.read"),
	}

	if _, err := service.ListContracts(context.Background(), actor, "user-2", "", 50); err != nil {
		t.Fatalf("ListContracts() error = %v", err)
	}
	if repository.ownerUserID != actor.UserID {
		t.Fatalf("owner filter = %q, want authenticated user %q", repository.ownerUserID, actor.UserID)
	}
}

func TestDefaultApprovalNodesUseManifestRoleCodes(t *testing.T) {
	nodes := defaultNodes()
	if len(nodes) != 3 {
		t.Fatalf("defaultNodes() length = %d", len(nodes))
	}
	if nodes[0].RoleCode != "sales_director" || nodes[1].RoleCode != "tech_director" || nodes[2].RoleCode != "finance_director" {
		t.Fatalf("defaultNodes() role codes = %q, %q, %q", nodes[0].RoleCode, nodes[1].RoleCode, nodes[2].RoleCode)
	}
	for _, node := range nodes {
		if node.Countersign != approval.CountersignAny {
			t.Fatalf("default node %q countersign = %q, want %q", node.ID, node.Countersign, approval.CountersignAny)
		}
	}
}

func TestResolveNodesUsesAllEffectivePlatformRoleHoldersAsAnySign(t *testing.T) {
	directory := []UserReference{
		{UserID: "director-2", Roles: []string{"sales_director"}},
		{UserID: "director-1", Roles: []string{"sales_director", "tech_director"}},
		{UserID: "ordinary-user", Roles: []string{"sales"}},
	}
	service := &Service{Personnel: personnelStub{users: directory}}
	actor := Principal{}
	nodes := []approval.Node{{
		ID: "sales-director", Name: "销售总监审批", RoleCode: "sales_director",
		Countersign: approval.CountersignAll, AssigneeIDs: []string{"stale-configured-user"},
	}}

	if err := service.resolveNodes(context.Background(), actor, nodes); err != nil {
		t.Fatalf("resolveNodes() error = %v", err)
	}
	if nodes[0].Countersign != approval.CountersignAny {
		t.Fatalf("countersign = %q, want %q", nodes[0].Countersign, approval.CountersignAny)
	}
	if len(nodes[0].AssigneeIDs) != 2 || nodes[0].AssigneeIDs[0] != "director-2" || nodes[0].AssigneeIDs[1] != "director-1" {
		t.Fatalf("assignees = %#v", nodes[0].AssigneeIDs)
	}
}

func TestResolveNodesRejectsRoleWithoutActivePlatformHolder(t *testing.T) {
	service := &Service{Personnel: personnelStub{}}
	nodes := []approval.Node{{ID: "finance", RoleCode: "finance_director"}}
	if err := service.resolveNodes(context.Background(), Principal{}, nodes); err == nil {
		t.Fatal("resolveNodes() error = nil")
	}
}

func TestGetContractRejectsNonOwnerEvenWithLegacyManagerPermission(t *testing.T) {
	repository := &recordingRepository{contract: contract.Contract{OwnerUserID: "user-2"}}
	service := &Service{Repo: repository}
	actor := Principal{
		TenantID: "tenant-1",
		UserID:   "manager-1",
		Permissions: map[string]bool{
			"contract.read":   true,
			"contract.manage": true,
		},
	}

	if _, err := service.GetContract(context.Background(), actor, "contract-1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("GetContract() error = %v, want ErrForbidden", err)
	}
}

func TestSubmitContractRejectsNonOwnerEvenWithLegacyManagerPermission(t *testing.T) {
	repository := &recordingRepository{contract: contract.Contract{OwnerUserID: "user-2", Status: contract.StatusDraft}}
	service := &Service{Repo: repository}
	actor := Principal{
		TenantID: "tenant-1",
		UserID:   "manager-1",
		Permissions: map[string]bool{
			"contract.create": true,
			"contract.manage": true,
		},
	}

	if _, err := service.SubmitContract(context.Background(), actor, "contract-1", true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("SubmitContract() error = %v, want ErrForbidden", err)
	}
}

func TestGetApprovalDetailReturnsContractToAssignedApprover(t *testing.T) {
	state := workflows.ApprovalState{ApprovalID: "approval-1", ContractID: "contract-1", Status: approval.StatusRunning}
	encoded := temporalmocks.NewEncodedValue(t)
	encoded.On("Get", mock.Anything).Run(func(arguments mock.Arguments) {
		target := arguments.Get(0).(*workflows.ApprovalState)
		*target = state
	}).Return(nil)
	temporal := temporalmocks.NewClient(t)
	temporal.On("QueryWorkflow", mock.Anything, "workflow-1", "run-1", workflows.StateQueryName).
		Return(encoded, nil)
	repository := &recordingRepository{
		contract: contract.Contract{ID: "contract-1", TenantID: "tenant-1", OwnerUserID: "applicant-1", Content: "contract body"},
		approvalMeta: approval.Meta{
			ID: "approval-1", TenantID: "tenant-1", ContractID: "contract-1",
			ApplicantUserID: "applicant-1", WorkflowID: "workflow-1", RunID: "run-1",
		},
		actions: []approval.Action{{ID: "action-1", Action: "comment", Comment: "reviewed"}},
	}
	service := &Service{Repo: repository, Temporal: temporal}
	actor := Principal{
		TenantID:         "tenant-1",
		UserID:           "approver-1",
		Permissions:      map[string]bool{"approval.process": true, "contract.read": true},
		PermissionScopes: allowAllScope("contract.read"),
	}

	detail, err := service.GetApprovalDetail(context.Background(), actor, "approval-1")
	if err != nil {
		t.Fatalf("GetApprovalDetail() error = %v", err)
	}
	if detail.Contract.Content != "contract body" || detail.State.ApprovalID != "approval-1" || len(detail.Actions) != 1 {
		t.Fatalf("detail = %#v", detail)
	}
}

func TestCommandReturnsTheSignalCommandIDForDurableConfirmation(t *testing.T) {
	temporal := temporalmocks.NewClient(t)
	state := workflows.ApprovalState{ApprovalID: "approval-1", ContractID: "contract-1", Status: approval.StatusRunning}
	encoded := temporalmocks.NewEncodedValue(t)
	encoded.On("Get", mock.Anything).Run(func(arguments mock.Arguments) {
		target := arguments.Get(0).(*workflows.ApprovalState)
		*target = state
	}).Return(nil)
	temporal.On("QueryWorkflow", mock.Anything, "workflow-1", "run-1", workflows.StateQueryName).
		Return(encoded, nil)
	temporal.On("SignalWorkflow", mock.Anything, "workflow-1", "run-1", workflows.CommandSignalName, mock.MatchedBy(func(command workflows.ApprovalCommand) bool {
		return command.CommandID != "" && command.ActorUserID == "approver-1" && command.Action == workflows.ActionApprove && command.RoleNodeOrSign
	})).Return(nil)
	repository := &recordingRepository{
		approvalMeta: approval.Meta{
			ID: "approval-1", TenantID: "tenant-1", Status: approval.StatusRunning,
			WorkflowID: "workflow-1", RunID: "run-1",
		},
		contract: contract.Contract{ID: "contract-1", TenantID: "tenant-1", OwnerUserID: "applicant-1"},
	}
	service := &Service{Repo: repository, Temporal: temporal}
	actor := Principal{
		TenantID:         "tenant-1",
		UserID:           "approver-1",
		Permissions:      map[string]bool{"approval.process": true, "contract.read": true},
		PermissionScopes: allowAllScope("contract.read"),
	}

	commandID, err := service.Command(context.Background(), actor, "approval-1", workflows.ApprovalCommand{Action: workflows.ActionApprove})
	if err != nil {
		t.Fatalf("Command() error = %v", err)
	}
	if commandID == "" {
		t.Fatal("Command() returned an empty command id")
	}
}

func TestGetApprovalDetailClassifiesTemporalQueryFailure(t *testing.T) {
	temporal := temporalmocks.NewClient(t)
	temporal.On("QueryWorkflow", mock.Anything, "workflow-1", "run-1", workflows.StateQueryName).
		Return(nil, errors.New("workflow execution not found"))
	repository := &recordingRepository{
		approvalMeta: approval.Meta{ID: "approval-1", TenantID: "tenant-1", ContractID: "contract-1", WorkflowID: "workflow-1", RunID: "run-1", ApplicantUserID: "applicant-1"},
		contract:     contract.Contract{ID: "contract-1", TenantID: "tenant-1", OwnerUserID: "applicant-1"},
	}
	service := &Service{Repo: repository, Temporal: temporal}
	actor := Principal{TenantID: "tenant-1", UserID: "approver-1", Permissions: map[string]bool{"approval.process": true, "contract.read": true}, PermissionScopes: allowAllScope("contract.read")}

	_, err := service.GetApprovalDetail(context.Background(), actor, "approval-1")
	if !errors.Is(err, ErrApprovalWorkflowUnavailable) {
		t.Fatalf("GetApprovalDetail() error = %v, want ErrApprovalWorkflowUnavailable", err)
	}
}

func TestCommandClassifiesTemporalSignalFailure(t *testing.T) {
	temporal := temporalmocks.NewClient(t)
	state := workflows.ApprovalState{ApprovalID: "approval-1", ContractID: "contract-1", Status: approval.StatusRunning}
	encoded := temporalmocks.NewEncodedValue(t)
	encoded.On("Get", mock.Anything).Run(func(arguments mock.Arguments) {
		target := arguments.Get(0).(*workflows.ApprovalState)
		*target = state
	}).Return(nil)
	temporal.On("QueryWorkflow", mock.Anything, "workflow-1", "run-1", workflows.StateQueryName).Return(encoded, nil)
	temporal.On("SignalWorkflow", mock.Anything, "workflow-1", "run-1", workflows.CommandSignalName, mock.Anything).Return(errors.New("workflow execution already completed"))
	repository := &recordingRepository{
		approvalMeta: approval.Meta{ID: "approval-1", TenantID: "tenant-1", ContractID: "contract-1", WorkflowID: "workflow-1", RunID: "run-1", Status: approval.StatusRunning},
		contract:     contract.Contract{ID: "contract-1", TenantID: "tenant-1", OwnerUserID: "applicant-1"},
	}
	service := &Service{Repo: repository, Temporal: temporal}
	actor := Principal{TenantID: "tenant-1", UserID: "approver-1", Permissions: map[string]bool{"approval.process": true, "contract.read": true}, PermissionScopes: allowAllScope("contract.read")}

	_, err := service.Command(context.Background(), actor, "approval-1", workflows.ApprovalCommand{Action: workflows.ActionApprove})
	if !errors.Is(err, ErrApprovalWorkflowUnavailable) {
		t.Fatalf("Command() error = %v, want ErrApprovalWorkflowUnavailable", err)
	}
}

func TestCommandRejectsAddSignTargetWithoutApprovalProcessRole(t *testing.T) {
	temporal := temporalmocks.NewClient(t)
	state := workflows.ApprovalState{ApprovalID: "approval-1", ContractID: "contract-1", Status: approval.StatusRunning}
	encoded := temporalmocks.NewEncodedValue(t)
	encoded.On("Get", mock.Anything).Run(func(arguments mock.Arguments) {
		target := arguments.Get(0).(*workflows.ApprovalState)
		*target = state
	}).Return(nil)
	temporal.On("QueryWorkflow", mock.Anything, "workflow-1", "run-1", workflows.StateQueryName).
		Return(encoded, nil)
	repository := &recordingRepository{approvalMeta: approval.Meta{
		ID: "approval-1", TenantID: "tenant-1", Status: approval.StatusRunning,
		WorkflowID: "workflow-1", RunID: "run-1",
	}, contract: contract.Contract{ID: "contract-1", TenantID: "tenant-1", OwnerUserID: "applicant-1"}}
	service := &Service{Repo: repository, Temporal: temporal, Personnel: personnelStub{users: []UserReference{
		{UserID: "sales-1", Roles: []string{"sales"}},
		{UserID: "specialist-1", Roles: []string{"contract_specialist"}},
	}}}
	actor := Principal{
		TenantID:         "tenant-1",
		UserID:           "approver-1",
		Permissions:      map[string]bool{"approval.process": true, "contract.read": true},
		PermissionScopes: allowAllScope("contract.read"),
	}

	for _, target := range []string{"sales-1", "specialist-1", "unknown-user"} {
		if _, err := service.Command(context.Background(), actor, "approval-1", workflows.ApprovalCommand{Action: workflows.ActionAddSign, TargetUserIDs: []string{target}}); !errors.Is(err, ErrApprovalTargetForbidden) {
			t.Fatalf("Command(add-sign %q) error = %v, want ErrApprovalTargetForbidden", target, err)
		}
	}
}

func TestCommandAllowsAddSignTargetWithApprovalProcessRole(t *testing.T) {
	temporal := temporalmocks.NewClient(t)
	state := workflows.ApprovalState{ApprovalID: "approval-1", ContractID: "contract-1", Status: approval.StatusRunning}
	encoded := temporalmocks.NewEncodedValue(t)
	encoded.On("Get", mock.Anything).Run(func(arguments mock.Arguments) {
		target := arguments.Get(0).(*workflows.ApprovalState)
		*target = state
	}).Return(nil)
	temporal.On("QueryWorkflow", mock.Anything, "workflow-1", "run-1", workflows.StateQueryName).
		Return(encoded, nil)
	temporal.On("SignalWorkflow", mock.Anything, "workflow-1", "run-1", workflows.CommandSignalName, mock.MatchedBy(func(command workflows.ApprovalCommand) bool {
		return command.Action == workflows.ActionAddSign && len(command.TargetUserIDs) == 1 && command.TargetUserIDs[0] == "finance-1"
	})).Return(nil)
	repository := &recordingRepository{approvalMeta: approval.Meta{
		ID: "approval-1", TenantID: "tenant-1", Status: approval.StatusRunning,
		WorkflowID: "workflow-1", RunID: "run-1",
	}, contract: contract.Contract{ID: "contract-1", TenantID: "tenant-1", OwnerUserID: "applicant-1"}}
	service := &Service{Repo: repository, Temporal: temporal, Personnel: personnelStub{users: []UserReference{{UserID: "finance-1", Roles: []string{"finance_director"}}}}}
	actor := Principal{
		TenantID:         "tenant-1",
		UserID:           "approver-1",
		Permissions:      map[string]bool{"approval.process": true, "contract.read": true},
		PermissionScopes: allowAllScope("contract.read"),
	}

	if commandID, err := service.Command(context.Background(), actor, "approval-1", workflows.ApprovalCommand{Action: workflows.ActionAddSign, TargetUserIDs: []string{"finance-1"}}); err != nil || commandID == "" {
		t.Fatalf("Command(add-sign) commandID=%q error=%v", commandID, err)
	}
}

func TestGetApprovalDetailRejectsUnrelatedUser(t *testing.T) {
	repository := &recordingRepository{approvalMeta: approval.Meta{ApplicantUserID: "applicant-1"}}
	service := &Service{Repo: repository}
	actor := Principal{TenantID: "tenant-1", UserID: "unrelated-user", Permissions: map[string]bool{}}

	if _, err := service.GetApprovalDetail(context.Background(), actor, "approval-1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("GetApprovalDetail() error = %v, want ErrForbidden", err)
	}
}

func TestGetApprovalDetailRejectsDifferentContractScope(t *testing.T) {
	repository := &recordingRepository{
		contract:     contract.Contract{ID: "contract-1", TenantID: "tenant-1", OwnerUserID: "applicant-1"},
		approvalMeta: approval.Meta{ID: "approval-1", TenantID: "tenant-1", ContractID: "contract-1", WorkflowID: "workflow-1", RunID: "run-1", ApplicantUserID: "applicant-1", Status: approval.StatusRunning},
	}
	service := &Service{Repo: repository}
	actor := Principal{
		TenantID:         "tenant-2",
		UserID:           "approver-1",
		Permissions:      map[string]bool{"contract.read": true},
		PermissionScopes: allowAllScope("contract.read"),
	}

	if _, err := service.GetApprovalDetail(context.Background(), actor, "approval-1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("GetApprovalDetail() error = %v, want ErrForbidden", err)
	}
}
