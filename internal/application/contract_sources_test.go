package application

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/j-s-te/contract-management/internal/contractdocument"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
	"github.com/j-s-te/contract-management/internal/testfixture"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// TestLocalSourceRejectsFakeCRMIdentity 验证LOCAL引用不混入CRM身份或不兼容出站。
// 参数：t为测试上下文。
// 返回值：无。
func TestLocalSourceRejectsFakeCRMIdentity(t *testing.T) {
	service := &Service{ContractSources: &ContractSourceOptions{CounterpartySource: "local", CategorySource: "local"}}
	actor := Principal{TenantID: "t", UserID: "u", IdentityID: "u", Permissions: map[string]bool{"contract.create": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.create": {AllowAll: true}}}
	c := contract.Contract{CounterpartySource: contract.CounterpartyLocal, LocalCounterpartyID: "local", CounterpartyVersion: 1, CategorySource: contract.CategoryLocal, CRMCustomerID: 42}
	if _, err := service.prepareContractReferences(context.Background(), actor, c, true, "contract.create"); !errors.Is(err, ErrValidation) {
		t.Fatalf("fake CRM identity accepted: %v", err)
	}
	c.CRMCustomerID = 0
	service.ContractSources.ProjectIntegrationEnabled = true
	if _, err := service.prepareContractReferences(context.Background(), actor, c, true, "contract.create"); !errors.Is(err, ErrContractSourceConflict) {
		t.Fatalf("incompatible integration accepted: %v", err)
	}
	old, _ := json.Marshal(contract.Contract{CRMCustomerID: 42})
	for _, key := range []string{"counterparty_source", "local_counterparty_id", "counterparty_version", "category_source", "integration_policy"} {
		if strings.Contains(string(old), `"`+key+`"`) {
			t.Fatalf("legacy digest changed by new field %s", key)
		}
	}
}

type catalogReferenceFixture struct {
	LocalCatalogRepository
	party    contract.LocalCounterparty
	category contract.LocalCategory
}

// GetLocalCounterparty 返回确定的独立目录边界快照，供实际应用逻辑复核。
// 参数：ctx为请求，filter为真实范围，id为引用。
// 返回值：测试来源快照，不替代SQL范围测试。
func (f *catalogReferenceFixture) GetLocalCounterparty(ctx context.Context, filter contract.ScopeFilter, id string) (contract.LocalCounterparty, error) {
	return f.party, nil
}

// GetLocalCategory 返回确定的本地类别快照供实际规范化逻辑使用。
// 参数：ctx为请求，filter为真实范围，id为稳定类别引用。
// 返回值：类别快照。
func (f *catalogReferenceFixture) GetLocalCategory(ctx context.Context, filter contract.ScopeFilter, id string) (contract.LocalCategory, error) {
	return f.category, nil
}

// localSourceFixture 创建无CRM/项目客户端的真实应用层来源配置。
// 参数：无。
// 返回值：应用服务、可信主体和待校验合同。
func localSourceFixture() (*Service, Principal, contract.Contract) {
	catalog := &catalogReferenceFixture{party: contract.LocalCounterparty{ID: "local-party", TenantID: "tenant", Name: "本地权威交易对方", Enabled: true, Version: 2}, category: contract.LocalCategory{ID: "local-category", TenantID: "tenant", Code: "SOFTWARE", Name: "软件测试", Enabled: true, Version: 3}}
	service := &Service{Repo: &recordingRepository{}, ExternalPDFEnabled: true, ContractSources: &ContractSourceOptions{CounterpartySource: "local", CategorySource: "local", Catalog: catalog}}
	actor := Principal{TenantID: "tenant", UserID: "user", IdentityID: "identity", DisplayName: "操作者", Permissions: map[string]bool{"contract.create": true}, PermissionScopes: allowAllScope("contract.create")}
	c := contract.Contract{Title: "独立合同", Type: "直签", CounterpartySource: contract.CounterpartyLocal, LocalCounterpartyID: "local-party", CounterpartyVersion: 2, CategorySource: contract.CategoryLocal, CustomerName: "客户端名称", AmountMinor: 10000, Currency: "CNY", Content: "正式原件内容", PaymentTerms: []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2026-12-01", Amount: "100.00"}}, ServiceItems: []contract.ServiceItem{{ServiceType: "软件测试", Name: "软件测试", Site: "测试站点", Batch: "第一批", Category: "浏览器名称", CategoryID: "local-category", CategoryVersion: 3, TestMode: "STANDARD"}}}
	return service, actor, c
}

// TestLocalReferencesUseAuthorityAndRetainLegacy 验证本地权威字段、范围和版本且旧来源不写入新增JSON字段。
// 参数：t为测试上下文。
// 返回值：无。
func TestLocalReferencesUseAuthorityAndRetainLegacy(t *testing.T) {
	service, actor, c := localSourceFixture()
	resolved, err := service.prepareContractReferences(context.Background(), actor, c, true, "contract.create")
	if err != nil || resolved.CustomerName != "本地权威交易对方" || resolved.CRMCustomerID != 0 || resolved.ServiceItems[0].Category != "软件测试" || resolved.IntegrationPolicy != contract.IntegrationNotApplicable {
		t.Fatalf("local resolution %+v %v", resolved, err)
	}
	c.CounterpartyVersion = 1
	if _, err := service.prepareContractReferences(context.Background(), actor, c, true, "contract.create"); !errors.Is(err, ErrLocalReferenceInvalid) {
		t.Fatalf("old directory version accepted: %v", err)
	}
	c.CounterpartyVersion = 2
	service.ContractSources.Catalog.(*catalogReferenceFixture).party.TenantID = "other"
	if _, err := service.prepareContractReferences(context.Background(), actor, c, true, "contract.create"); !errors.Is(err, ErrLocalReferenceInvalid) {
		t.Fatalf("foreign tenant accepted: %v", err)
	}
	legacy := &Service{CRMContractReferences: validCRMReference(7), DetectionCategories: enabledDetectionCategories("软件测试")}
	old := contract.Contract{CRMCustomerID: 7, CustomerName: "旧请求", ServiceItems: []contract.ServiceItem{{Category: "软件测试"}}}
	actual, err := legacy.prepareContractReferences(context.Background(), actor, old, true, "contract.create")
	if err != nil || actual.CustomerName != "CRM 权威客户" || actual.CounterpartySource != "" || actual.CategorySource != "" {
		t.Fatalf("legacy changed: %+v %v", actual, err)
	}
}

// TestPrepareContractActuallyAcceptsLocalReferences 验证正式准备链能接受本地引用与合法付款计划。
// 参数：t为测试上下文；此用例必须在三处WIP集成后真实通过。
// 返回值：无。
func TestPrepareContractActuallyAcceptsLocalReferences(t *testing.T) {
	service, actor, c := localSourceFixture()
	prepared, err := service.prepareContract(context.Background(), actor, c, true, "contract.create")
	if err != nil {
		t.Fatalf("formal LOCAL preparation still requires CRM/project: %v", err)
	}
	if prepared.CRMCustomerID != 0 || prepared.LocalCounterpartyID != "local-party" || prepared.IntegrationPolicy != contract.IntegrationNotApplicable || len(prepared.PaymentTerms) != 1 {
		t.Fatalf("local identity lost: %+v", prepared)
	}
}

// TestLocalTemplatePDFAndNormalBatch 验证正式模板、单PDF及NORMAL预检均保留独立引用和付款计划。
// 参数：t为测试上下文，持久化和网关为已有隔离测试边界。
// 返回值：无；不冒充真实MySQL、文件网关或Temporal验收。
func TestLocalTemplatePDFAndNormalBatch(t *testing.T) {
	ctx := context.Background()
	service, actor, c := localSourceFixture()
	service.Templates = &memoryTemplateRepository{items: map[string]contracttemplate.Template{"template": {ID: "template", TenantID: actor.TenantID, Name: "本地模板", Content: applicationTestDOCX(t, "正式模板正文"), Version: 1}}}
	c.TemplateID = "template"
	created, err := service.CreateContract(ctx, actor, c)
	if err != nil || created.CustomerName != "本地权威交易对方" || created.CRMCustomerID != 0 {
		t.Fatalf("template LOCAL %+v %v", created, err)
	}
	pdf := testfixture.PDF("local source contract")
	c.TemplateID = ""
	c.Document = pdf
	c.SourceFilename = "local.pdf"
	c.SourceMediaType = "application/pdf"
	created, err = service.CreateExternalContract(ctx, actor, c)
	if err != nil || created.DocumentFormat != "PDF" || created.CounterpartyVersion != 2 || len(created.PaymentTerms) != 1 {
		t.Fatalf("single PDF LOCAL %+v %v", created, err)
	}
	source, err := contractdocument.ValidateExternalSource("local.pdf", "application/pdf", pdf)
	if err != nil {
		t.Fatal(err)
	}
	repo := &importPreviewRepository{}
	service.Repo = repo
	service.StampedFileGateway = &importPreviewGateway{body: pdf}
	service.ContractImportEnabled = true
	actor.Permissions["contract.import.preview"] = true
	actor.PermissionScopes["contract.import.preview"] = contract.ScopeFilter{AllowAll: true}
	metadata := contract.ImportMetadata{Title: c.Title, ContractType: c.Type, CounterpartySource: c.CounterpartySource, LocalCounterpartyID: c.LocalCounterpartyID, CounterpartyVersion: c.CounterpartyVersion, CategorySource: c.CategorySource, CustomerName: c.CustomerName, AmountMinor: c.AmountMinor, Currency: c.Currency, PaymentTerms: c.PaymentTerms, ServiceItems: c.ServiceItems}
	repo.batch = contract.ImportBatch{ID: "batch", TenantID: actor.TenantID, ActorID: actor.UserID, Revision: 1, Version: 1, State: "DRAFT", SourceSystem: "manual", ExpiresAt: time.Now().Add(time.Hour), Sources: []contract.ImportSource{{ID: "source", ItemKey: "one", Filename: "local.pdf", MediaType: "application/pdf", SHA256: source.SHA256, GatewayID: "object", State: "READY"}}, Items: []contract.ImportItemState{{ID: "item", Item: contract.ImportItem{Key: "one", ExternalRecordID: "local-001", Mode: contract.ImportNormal, SourceFileRef: "source", Metadata: metadata}}}}
	result, err := service.PreviewImport(ctx, actor, "batch", 1)
	if err != nil || result.State != "PREVIEWED" || result.Items[0].NormalizedMetadata == nil || result.Items[0].NormalizedMetadata.CounterpartySource != contract.CounterpartyLocal {
		t.Fatalf("normal batch LOCAL state=%s issues=%+v error=%v", result.State, result.Items[0].Issues, err)
	}
	history := repo.batch.Items[0]
	history.Item.Mode = contract.ImportArchiveOnly
	if _, _, err := service.prepareImportItem(ctx, actor, repo.batch, history); !errors.Is(err, ErrImportUnsupported) {
		t.Fatalf("LOCAL historical mode widened: %v", err)
	}
}
