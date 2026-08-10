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

type recordingRepository struct {
	ownerUserID          string
	contract             contract.Contract
	created              contract.Contract
	approvalMeta         approval.Meta
	actions              []approval.Action
	lifecycle            []contract.LifecycleEvent
	dashboard            contract.Dashboard
	dashboardTenantID    string
	dashboardOwnerUserID string
}

func (r *recordingRepository) GetContract(context.Context, string, string) (contract.Contract, error) {
	return r.contract, nil
}

func (r *recordingRepository) ListContracts(_ context.Context, _, ownerUserID, _ string, _ int) ([]contract.Contract, error) {
	r.ownerUserID = ownerUserID
	return nil, nil
}
func (r *recordingRepository) ListApprovedContracts(context.Context, string, int) ([]contract.Contract, error) {
	return nil, nil
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
	service := &Service{Repo: repository}
	actor := Principal{
		TenantID:    "tenant-1",
		UserID:      "user-1",
		Permissions: map[string]bool{"contract.read": true},
	}

	if _, err := service.ListContracts(context.Background(), actor, "another-user", "", 50); err != nil {
		t.Fatalf("ListContracts() error = %v", err)
	}
	if repository.ownerUserID != actor.UserID {
		t.Fatalf("owner filter = %q, want authenticated user %q", repository.ownerUserID, actor.UserID)
	}
}

func TestAdminListContractsUsesTenantScopeAndCanReadTenantContract(t *testing.T) {
	repository := &recordingRepository{contract: contract.Contract{ID: "contract-2", TenantID: "tenant-1", OwnerUserID: "user-2"}}
	service := &Service{Repo: repository}
	actor := Principal{TenantID: "tenant-1", UserID: "admin-1", Roles: []string{"admin"}, Permissions: map[string]bool{"contract.read": true}}

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
	actor := Principal{TenantID: "tenant-1", UserID: "admin-1", Roles: []string{"admin"}, Permissions: map[string]bool{"contract.read": true}}

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
	admin := Principal{TenantID: "tenant-1", Roles: []string{"admin"}, Permissions: map[string]bool{"contract.read": true}}

	got, err := service.ContractDashboard(context.Background(), admin)
	if err != nil || got.TotalContracts != want.TotalContracts || got.ApprovalContracts != want.ApprovalContracts {
		t.Fatalf("ContractDashboard() = %#v, %v", got, err)
	}
	if repository.dashboardTenantID != "tenant-1" || repository.dashboardOwnerUserID != "" {
		t.Fatalf("admin dashboard scope = tenant %q, owner %q", repository.dashboardTenantID, repository.dashboardOwnerUserID)
	}
	user := Principal{TenantID: "tenant-2", UserID: "sales-1", Roles: []string{"sales"}, Permissions: map[string]bool{"contract.read": true}}
	if _, err := service.ContractDashboard(context.Background(), user); err != nil {
		t.Fatalf("ContractDashboard() user error = %v", err)
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
		TenantID: "tenant-1", UserID: "user-1", DisplayName: "章六",
		Permissions: map[string]bool{"contract.create": true},
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
	actor := Principal{TenantID: "tenant-1", UserID: "user-1", Permissions: map[string]bool{"contract.create": true}}
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
	actor := Principal{TenantID: "tenant-1", UserID: "user-1", Permissions: map[string]bool{"contract.create": true}}
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
	actor := Principal{TenantID: "tenant-1", UserID: "user-1", Permissions: map[string]bool{"contract.create": true}}
	_, err := service.CreateContract(context.Background(), actor, contract.Contract{
		Number: "CON-001", Title: "合同", Type: "service", TemplateID: "template-1",
		ServiceItems: []contract.ServiceItem{{ServiceType: "consulting"}},
		StartDate:    &start, EndDate: &end,
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("CreateContract() error = %v, want ErrValidation", err)
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
		Permissions: map[string]bool{
			"contract.read":   true,
			"contract.manage": true,
		},
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
	service := &Service{}
	actor := Principal{UserDirectory: []UserReference{
		{UserID: "director-2", Roles: []string{"sales_director"}},
		{UserID: "director-1", Roles: []string{"sales_director", "tech_director"}},
		{UserID: "ordinary-user", Roles: []string{"sales"}},
	}}
	nodes := []approval.Node{{
		ID: "sales-director", Name: "销售总监审批", RoleCode: "sales_director",
		Countersign: approval.CountersignAll, AssigneeIDs: []string{"stale-configured-user"},
	}}

	if err := service.resolveNodes(actor, nodes); err != nil {
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
	service := &Service{}
	nodes := []approval.Node{{ID: "finance", RoleCode: "finance_director"}}
	if err := service.resolveNodes(Principal{}, nodes); err == nil {
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
		contract: contract.Contract{ID: "contract-1", OwnerUserID: "applicant-1", Content: "contract body"},
		approvalMeta: approval.Meta{
			ID: "approval-1", TenantID: "tenant-1", ContractID: "contract-1",
			ApplicantUserID: "applicant-1", WorkflowID: "workflow-1", RunID: "run-1",
		},
		actions: []approval.Action{{ID: "action-1", Action: "comment", Comment: "reviewed"}},
	}
	service := &Service{Repo: repository, Temporal: temporal}
	actor := Principal{
		TenantID: "tenant-1", UserID: "approver-1",
		Permissions: map[string]bool{"approval.process": true},
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
	temporal.On("SignalWorkflow", mock.Anything, "workflow-1", "run-1", workflows.CommandSignalName, mock.MatchedBy(func(command workflows.ApprovalCommand) bool {
		return command.CommandID != "" && command.ActorUserID == "approver-1" && command.Action == workflows.ActionApprove
	})).Return(nil)
	repository := &recordingRepository{approvalMeta: approval.Meta{
		ID: "approval-1", TenantID: "tenant-1", Status: approval.StatusRunning,
		WorkflowID: "workflow-1", RunID: "run-1",
	}}
	service := &Service{Repo: repository, Temporal: temporal}
	actor := Principal{TenantID: "tenant-1", UserID: "approver-1", Permissions: map[string]bool{"approval.process": true}}

	commandID, err := service.Command(context.Background(), actor, "approval-1", workflows.ApprovalCommand{Action: workflows.ActionApprove})
	if err != nil {
		t.Fatalf("Command() error = %v", err)
	}
	if commandID == "" {
		t.Fatal("Command() returned an empty command id")
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
