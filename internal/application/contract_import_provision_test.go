package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/testfixture"
)

// provisionTestTenant 是符合 26 位主键列约束的测试租户。
const provisionTestTenant = "01J00000000000000000000000"

// provisionCallCounter 记录假客户端被调用的次数：开关关闭与 NORMAL 模式必须为 0。
type provisionCallCounter struct {
	lookup, createCustomer, createOpportunity, sign, resolveOwner int
}

// provisionCustomerStub 是客户链路的可计数假客户端。
// 按客户名称区分的预置值让同一批里可以有「失败行」与「正常行」。
type provisionCustomerStub struct {
	calls            *provisionCallCounter
	candidates       []CRMProvisionCustomerCandidate
	candidatesByName map[string][]CRMProvisionCustomerCandidate
	lookupErr        error
	lookupErrByName  map[string]error
	created          CRMProvisionCustomer
	createErr        error
	createErrSeq     []error
	createdWith      []CRMProvisionCustomerInput
	createKeys       []string
}

// LookupCustomer 返回预置候选并计数。
// 参数：ctx 为上下文，actor/key 为契约字段，name 为客户名称。
// 返回值：预置候选与错误。
func (stub *provisionCustomerStub) LookupCustomer(_ context.Context, _, _, name string) ([]CRMProvisionCustomerCandidate, error) {
	stub.calls.lookup++
	if err, ok := stub.lookupErrByName[name]; ok {
		return nil, err
	}
	if candidates, ok := stub.candidatesByName[name]; ok {
		return candidates, nil
	}
	return stub.candidates, stub.lookupErr
}

// CreateCustomer 返回预置客户并计数；createErrSeq 逐次消费，用于模拟一次失败后恢复。
// 参数：ctx 为上下文，actor/key 为契约字段，input 为客户输入。
// 返回值：预置客户与错误。
func (stub *provisionCustomerStub) CreateCustomer(_ context.Context, _, key string, input CRMProvisionCustomerInput) (CRMProvisionCustomer, error) {
	stub.calls.createCustomer++
	stub.createdWith = append(stub.createdWith, input)
	stub.createKeys = append(stub.createKeys, key)
	if len(stub.createErrSeq) > 0 {
		err := stub.createErrSeq[0]
		stub.createErrSeq = stub.createErrSeq[1:]
		if err != nil {
			return CRMProvisionCustomer{}, err
		}
	}
	return stub.created, stub.createErr
}

// provisionOpportunityStub 是商机链路的可计数假客户端。
type provisionOpportunityStub struct {
	calls      *provisionCallCounter
	created    CRMProvisionOpportunity
	createErr  error
	signErr    error
	createWith []CRMProvisionOpportunityInput
	createKeys []string
	signWith   []provisionSignCall
	signKeys   []string
}

// provisionSignCall 记录一次推进签约请求。
type provisionSignCall struct {
	OpportunityID uint64
	ContractRef   string
}

// CreateOpportunity 返回预置商机并计数。
// 参数：ctx 为上下文，actor/key 为契约字段，input 为商机输入。
// 返回值：预置商机与错误。
func (stub *provisionOpportunityStub) CreateOpportunity(_ context.Context, _, key string, input CRMProvisionOpportunityInput) (CRMProvisionOpportunity, error) {
	stub.calls.createOpportunity++
	stub.createWith = append(stub.createWith, input)
	stub.createKeys = append(stub.createKeys, key)
	return stub.created, stub.createErr
}

// SignOpportunity 记录推进请求并计数。
// 参数：ctx 为上下文，actor/key 为契约字段，opportunityID/contractRef 为推进目标。
// 返回值：已签约商机与错误。
func (stub *provisionOpportunityStub) SignOpportunity(_ context.Context, _, key string, opportunityID uint64, contractRef string) (CRMProvisionOpportunity, error) {
	stub.calls.sign++
	stub.signWith = append(stub.signWith, provisionSignCall{OpportunityID: opportunityID, ContractRef: contractRef})
	stub.signKeys = append(stub.signKeys, key)
	return CRMProvisionOpportunity{ID: opportunityID, Status: "SIGNED", Stage: "CLOSED"}, stub.signErr
}

// provisionOwnerStub 是负责人目录的可计数假实现。
// 按姓名区分的错误让失败行与正常行可以共存在同一批。
type provisionOwnerStub struct {
	calls     *provisionCallCounter
	candidate OwnerDirectoryCandidate
	err       error
	errByName map[string]error
}

// ResolveOwnerByName 返回预置候选并计数。
// 参数：ctx 为上下文，displayName 为销售姓名。
// 返回值：预置候选与错误。
func (stub *provisionOwnerStub) ResolveOwnerByName(_ context.Context, displayName string) (OwnerDirectoryCandidate, error) {
	stub.calls.resolveOwner++
	if err, ok := stub.errByName[displayName]; ok {
		return OwnerDirectoryCandidate{}, err
	}
	return stub.candidate, stub.err
}

// provisionOwnerResolver 是历史责任人解析的假实现。
type provisionOwnerResolver struct{}

// ResolveOwner 返回固定责任人。
// 参数：ctx/tenant/id 为调用身份。
// 返回值：责任人引用与无错误。
func (provisionOwnerResolver) ResolveOwner(_ context.Context, tenant, id string) (UserReference, error) {
	_ = tenant
	return UserReference{UserID: id, DisplayName: "历史责任人"}, nil
}

// provisionImportRepository 是同时承载批次、开立台账与商机回写的测试仓储。
type provisionImportRepository struct {
	recordingRepository
	batch   contract.ImportBatch
	records map[contract.ImportCRMProvisionKey]contract.ImportCRMProvision
	commits int
	// commitErrs 逐次消费，用于模拟建合同步骤的失败。
	commitErrs []error
	// autoApproveSetting 是测试用的租户级运行开关；零值表示默认关闭。
	autoApproveSetting contract.ImportAutoApproveSetting
	// autoApproveCalls 记录每次自动确认收到的策略快照，用于断言 pin/live 口径。
	autoApproveCalls []ImportAutoApprovePolicy
	written          struct {
		contractID    string
		opportunityID uint64
		name          string
	}
}

// newProvisionImportRepository 建立带空台账的测试仓储。
// 参数：batch 为初始批次。
// 返回值：可用仓储。
func newProvisionImportRepository(batch contract.ImportBatch) *provisionImportRepository {
	return &provisionImportRepository{batch: batch, records: map[contract.ImportCRMProvisionKey]contract.ImportCRMProvision{}}
}

// CreateImportBatch 保存测试批次。
// 参数：ctx 为上下文，b 为批次。
// 返回值：无错误。
func (r *provisionImportRepository) CreateImportBatch(_ context.Context, b contract.ImportBatch) error {
	r.batch = b
	return nil
}

// GetImportBatch 返回测试批次。
// 参数：ctx/tenant/id 为调用身份。
// 返回值：批次与无错误。
func (r *provisionImportRepository) GetImportBatch(context.Context, string, string) (contract.ImportBatch, error) {
	return r.batch, nil
}

// UpdateImportBatch 执行测试回调并递增版本。
// 参数：ctx/tenant/id 为调用身份，change 为修改回调。
// 返回值：回调错误。
func (r *provisionImportRepository) UpdateImportBatch(_ context.Context, _, _ string, change func(*contract.ImportBatch) error) error {
	if err := change(&r.batch); err != nil {
		return err
	}
	r.batch.Version++
	return nil
}

// ClaimImportBatch 模拟真实仓储的认领：只认领 QUEUED/RUNNING 并递增执行代次。
// 参数：ctx/now 为调度条件。
// 返回值：认领到的批次或没有任务。
func (r *provisionImportRepository) ClaimImportBatch(context.Context, time.Time) (contract.ImportBatch, bool, error) {
	if r.batch.State != "QUEUED" && r.batch.State != "RUNNING" {
		return contract.ImportBatch{}, false, nil
	}
	r.batch.State = "RUNNING"
	r.batch.Epoch++
	return r.batch, true, nil
}

// CommitImportDraft 模拟真实仓储的建单围栏：写入项合同 ID 并记录已建合同。
// 参数：ctx/tenant/batch/revision/epoch/item 为围栏，c 为待建合同，actor 为操作者。
// 返回值：合同 ID 或围栏错误。
func (r *provisionImportRepository) CommitImportDraft(_ context.Context, _ string, _ string, _, _ uint64, itemID string, c contract.Contract, _ string) (string, error) {
	for i := range r.batch.Items {
		if r.batch.Items[i].ID != itemID {
			continue
		}
		if r.batch.Items[i].ContractID != "" {
			return r.batch.Items[i].ContractID, nil
		}
		if len(r.commitErrs) > 0 {
			err := r.commitErrs[0]
			r.commitErrs = r.commitErrs[1:]
			if err != nil {
				return "", err
			}
		}
		r.commits++
		// 真实仓储建单后 version=1、status=draft；开立的商机编排依赖该基线。
		c.Version, c.Status = 1, contract.StatusDraft
		r.created, r.contract = c, c
		r.batch.Items[i].ContractID, r.batch.Items[i].State = c.ID, "CREATED"
		return c.ID, nil
	}
	return "", ErrImportConflict
}

// MarkContractSourceGatewayResult 记录源文件网关结果（开立编排不校验其实现细节）。
// 参数：ctx/tenant/contract/file/state/detail 为网关回执。
// 返回值：无错误。
func (r *provisionImportRepository) MarkContractSourceGatewayResult(context.Context, string, string, string, string, string) error {
	return nil
}

// MarkTemplateGatewayResult 让测试仓储满足 SourceFileGatewayStateRepository 的完整方法集。
// 参数：ctx/tenant/template/file/state/detail 为网关回执。
// 返回值：无错误。
func (r *provisionImportRepository) MarkTemplateGatewayResult(context.Context, string, string, string, string, string) error {
	return nil
}

// GetImportCRMProvision 读取开立台账。
// 参数：ctx 为上下文，key 为幂等键。
// 返回值：台账记录或不存在错误。
func (r *provisionImportRepository) GetImportCRMProvision(_ context.Context, key contract.ImportCRMProvisionKey) (contract.ImportCRMProvision, error) {
	value, ok := r.records[key]
	if !ok {
		return contract.ImportCRMProvision{}, apperrors.ErrNotFound
	}
	return value, nil
}

// UpsertStartImportCRMProvision 模拟真实仓储的幂等起点：不存在则 PENDING，已存在只增 attempt。
// 参数：ctx 为上下文，key 为幂等键，batchID 为批次。
// 返回值：写入后的记录。
func (r *provisionImportRepository) UpsertStartImportCRMProvision(_ context.Context, key contract.ImportCRMProvisionKey, batchID string) (contract.ImportCRMProvision, error) {
	value, ok := r.records[key]
	if !ok {
		value = contract.ImportCRMProvision{ImportCRMProvisionKey: key, BatchID: batchID, State: contract.ImportCRMProvisionPending}
	}
	value.Attempt++
	value.LastErrorCode, value.LastErrorMessage = "", ""
	r.records[key] = value
	return value, nil
}

// AdvanceImportCRMProvision 以领域纯函数推进状态，模拟 CAS 语义。
// 参数：ctx 为上下文，key 为幂等键，advance 为推进请求。
// 返回值：推进后的记录或领域错误。
func (r *provisionImportRepository) AdvanceImportCRMProvision(_ context.Context, key contract.ImportCRMProvisionKey, advance contract.ImportCRMProvisionAdvance) (contract.ImportCRMProvision, error) {
	next, err := r.records[key].Advance(advance)
	if err != nil {
		return contract.ImportCRMProvision{}, err
	}
	r.records[key] = next
	return next, nil
}

// RecordImportCRMProvisionError 把记录置为 BLOCKED。
// 参数：ctx 为上下文，key 为幂等键，from/expectedAttempt 为 CAS 前置，code/message 为失败原因。
// 返回值：写入后的记录。
func (r *provisionImportRepository) RecordImportCRMProvisionError(_ context.Context, key contract.ImportCRMProvisionKey, from string, expectedAttempt int, code, message string) (contract.ImportCRMProvision, error) {
	value, ok := r.records[key]
	if !ok || value.State != from || value.Attempt != expectedAttempt {
		return contract.ImportCRMProvision{}, apperrors.ErrStateConflict
	}
	value.State, value.LastErrorCode, value.LastErrorMessage = contract.ImportCRMProvisionBlocked, code, message
	r.records[key] = value
	return value, nil
}

// ResetImportCRMProvision 把 BLOCKED 复位为 PENDING。
// 参数：ctx 为上下文，key 为幂等键，expectedAttempt 为 CAS 前置。
// 返回值：复位后的记录。
func (r *provisionImportRepository) ResetImportCRMProvision(_ context.Context, key contract.ImportCRMProvisionKey, expectedAttempt int) (contract.ImportCRMProvision, error) {
	value, ok := r.records[key]
	if !ok || value.State != contract.ImportCRMProvisionBlocked || value.Attempt != expectedAttempt {
		return contract.ImportCRMProvision{}, apperrors.ErrStateConflict
	}
	value.State, value.Attempt = contract.ImportCRMProvisionPending, value.Attempt+1
	value.LastErrorCode, value.LastErrorMessage = "", ""
	r.records[key] = value
	return value, nil
}

// UpdateImportContractOpportunity 记录商机回写。
// 参数：ctx/tenant/contract 定位合同，opportunityID/name 为商机引用，actor 为操作者。
// 返回值：无错误。
func (r *provisionImportRepository) UpdateImportContractOpportunity(_ context.Context, _, contractID string, opportunityID uint64, opportunityName, _ string) error {
	r.written.contractID, r.written.opportunityID, r.written.name = contractID, opportunityID, opportunityName
	return nil
}

// provisionHistoryItem 构造一条通过历史证据校验的 ARCHIVE_ONLY 清单项。
// 参数：key 为单项键，external 为外部记录 ID，source 为暂存原件 ID，customer 为客户名称，
// sales 为销售姓名，amountMinor 为金额。
// 返回值：可预检/执行的清单项。
func provisionHistoryItem(key, external, source, customer, sales string, amountMinor int64) contract.ImportItem {
	start := time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC)
	return contract.ImportItem{
		Key: key, ExternalRecordID: external, Mode: contract.ImportArchiveOnly, SourceFileRef: source,
		Policy: &contract.ImportPolicy{Version: 1, ProjectMode: "NONE", FinanceMode: "NONE",
			ReportMode: "ARCHIVE_SEPARATE", ResponsibilityMode: "INACTIVE", SigningMode: "DECLARED_ONLY"},
		Metadata: contract.ImportMetadata{
			Title: "历史合同-" + key, ContractType: "直签", ServiceType: "软件测试",
			CustomerName: customer, SalesName: sales, AmountMinor: amountMinor, Currency: "CNY",
			StartDate: &start,
			ServiceItems: []contract.ServiceItem{{
				ServiceType: "软件测试", Site: "北京", Batch: "第一批", Category: "软件测试", TestMode: "STANDARD",
			}},
		},
		HistoryEvidence: &contract.ImportHistoryEvidence{
			DeclaredSignedDate: "2024-03-05", HistoricalStatus: "archived",
			HistoricalCustomerName: customer, HistoricalOwnerLabel: "原销售",
			TargetOwnerUserID: "owner", ReportRequiredChannels: "API_ONLY",
		},
	}
}

// provisionImportActor 构造具备开立预览/确认/建单权限的操作者。
// 参数：无。
// 返回值：可信主体。
func provisionImportActor() Principal {
	scopes := allowAllScope("contract.create")
	scopes["contract.import.preview"] = contract.ScopeFilter{AllowAll: true}
	scopes["contract.import.commit"] = contract.ScopeFilter{AllowAll: true}
	return Principal{TenantID: provisionTestTenant, UserID: "actor", IdentityID: "identity-actor", DisplayName: "操作者",
		Permissions:      map[string]bool{"contract.create": true, "contract.import.preview": true, "contract.import.commit": true},
		PermissionScopes: scopes}
}

// provisionTestSource 生成与原件字节一致的暂存来源。
// 参数：t 为测试上下文，itemKey 为单项键，pdf 为原件字节。
// 返回值：READY 状态来源。
func provisionTestSource(itemKey string, pdf []byte) contract.ImportSource {
	sum := sha256.Sum256(pdf)
	return contract.ImportSource{ID: "source-" + itemKey, ItemKey: itemKey, Filename: itemKey + ".pdf",
		MediaType: "application/pdf", SHA256: hex.EncodeToString(sum[:]), GatewayID: "object-" + itemKey, State: "READY"}
}

// provisionImportBatch 构造一份已暂存原件的历史批次。
// 参数：items 为清单项，source 为暂存来源。
// 返回值：可预检的批次。
func provisionImportBatch(items []contract.ImportItem, sources ...contract.ImportSource) contract.ImportBatch {
	states := make([]contract.ImportItemState, 0, len(items))
	for _, item := range items {
		states = append(states, contract.ImportItemState{ID: "item-" + item.Key, Item: item, State: "PENDING", Issues: []contract.ImportIssue{}})
	}
	return contract.ImportBatch{
		ID: "batch", TenantID: provisionTestTenant, ActorID: "actor", SourceSystem: "legacy",
		Revision: 1, Version: 1, State: "DRAFT", ExpiresAt: time.Now().Add(time.Hour),
		Items: states, Sources: sources,
	}
}

// provisionTestService 组装带开立通道注入点的服务与假依赖。
// 参数：repo 为测试仓储，pdf 为原件字节，三条假依赖为开立通道注入点。
// 返回值：服务实例。
func provisionTestService(repo *provisionImportRepository, pdf []byte,
	customer *provisionCustomerStub, opportunity *provisionOpportunityStub, owners *provisionOwnerStub) *Service {
	return &Service{
		Repo: repo, StampedFileGateway: &importPreviewGateway{body: pdf},
		ExternalPDFEnabled: true, ContractImportEnabled: true, ContractImportMaxItems: 100,
		ContractImportHistoryEnabled: true, ContractImportConfirmationRoles: []string{"ROLE_IMPORT_APPROVER"},
		ImportReportReadConfigured: true, ImportReport: &ImportTargetTransport{},
		ImportOwners:                  provisionOwnerResolver{},
		DetectionCategories:           enabledDetectionCategories("软件测试"),
		CRMProvisionCustomerClient:    customer,
		CRMProvisionOpportunityClient: opportunity,
		CRMProvisionOwnerDirectory:    owners,
	}
}

// newProvisionStubs 建立三条假依赖与调用计数。
// 参数：无。
// 返回值：计数、三条假依赖。
func newProvisionStubs() (*provisionCallCounter, *provisionCustomerStub, *provisionOpportunityStub, *provisionOwnerStub) {
	calls := &provisionCallCounter{}
	customer := &provisionCustomerStub{calls: calls, created: CRMProvisionCustomer{ID: 501, Name: "历史客户", Status: "ACTIVE"}}
	opportunity := &provisionOpportunityStub{calls: calls, created: CRMProvisionOpportunity{ID: 901, Name: "历史合同-one", CustomerID: 501, Status: "FOLLOWING"}}
	owners := &provisionOwnerStub{calls: calls, candidate: OwnerDirectoryCandidate{IdentityID: "identity-sales", DisplayName: "张三"}}
	return calls, customer, opportunity, owners
}

// TestImportProvisionPreviewLeavesLegacyBehaviourWhenDisabledOrNormal 验证零调用回归：
// 开关关闭的历史批次与开关开启的 NORMAL 批次都不得调用任何开立依赖。
// 参数：t 为测试上下文。
// 返回值：出现调用或历史行为被改变时失败。
func TestImportProvisionPreviewLeavesLegacyBehaviourWhenDisabledOrNormal(t *testing.T) {
	ctx := context.Background()
	actor := provisionImportActor()
	pdf := testfixture.PDF("historical contract")

	// 开关关闭：历史批次保持既有行为（缺 crm_customer_id 阻断），且一个外部调用都没有。
	calls, customer, opportunity, owners := newProvisionStubs()
	item := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
	repo := newProvisionImportRepository(provisionImportBatch([]contract.ImportItem{item}, provisionTestSource("one", pdf)))
	service := provisionTestService(repo, pdf, customer, opportunity, owners)
	service.CRMProvisionCustomerClient, service.CRMProvisionOpportunityClient, service.CRMProvisionOwnerDirectory = nil, nil, nil

	previewed, err := service.PreviewImport(ctx, actor, "batch", 1)
	if err != nil {
		t.Fatalf("disabled preview error: %v", err)
	}
	if calls.lookup+calls.createCustomer+calls.createOpportunity+calls.sign+calls.resolveOwner != 0 {
		t.Fatalf("disabled preview called provision clients: %+v", calls)
	}
	if previewed.Items[0].State != "BLOCKED" || len(previewed.Items[0].Issues) == 0 ||
		previewed.Items[0].Issues[0].Field != "metadata.crm_customer_id" || previewed.Items[0].Issues[0].Code != "REQUIRED" {
		t.Fatalf("disabled preview changed legacy behaviour: %+v", previewed.Items[0])
	}
	if len(previewed.ProvisionPlan) != 0 {
		t.Fatalf("disabled preview produced a provision plan: %+v", previewed.ProvisionPlan)
	}

	// 开关开启 + NORMAL：仍走既有链路，零开立调用。
	calls, customer, opportunity, owners = newProvisionStubs()
	normal := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
	normal.Mode, normal.Policy, normal.HistoryEvidence = contract.ImportNormal, nil, nil
	normal.Metadata.CRMCustomerID = 7
	repo = newProvisionImportRepository(provisionImportBatch([]contract.ImportItem{normal}, provisionTestSource("one", pdf)))
	service = provisionTestService(repo, pdf, customer, opportunity, owners)
	service.CRMContractReferences = validCRMReference(7)

	previewed, err = service.PreviewImport(ctx, actor, "batch", 1)
	if err != nil {
		t.Fatalf("normal preview error: %v", err)
	}
	if calls.lookup+calls.createCustomer+calls.createOpportunity+calls.sign+calls.resolveOwner != 0 {
		t.Fatalf("normal preview called provision clients: %+v", calls)
	}
	if previewed.Items[0].State != "READY" || len(previewed.ProvisionPlan) != 0 {
		t.Fatalf("normal preview entered provision path: %+v", previewed.Items[0])
	}
}

// TestImportProvisionPreviewPlanFieldsMatchFrontendContract 验证预检计划字段名与值：
// 金额由 amount_minor 格式化为元的两位小数，签约日取合同开始日期。
// 参数：t 为测试上下文。
// 返回值：字段名或值不符时失败。
func TestImportProvisionPreviewPlanFieldsMatchFrontendContract(t *testing.T) {
	ctx := context.Background()
	actor := provisionImportActor()
	pdf := testfixture.PDF("historical contract")
	calls, customer, opportunity, owners := newProvisionStubs()
	customer.candidates = []CRMProvisionCustomerCandidate{{ID: 501, Name: "历史客户", Status: "ACTIVE"}}
	item := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
	repo := newProvisionImportRepository(provisionImportBatch([]contract.ImportItem{item}, provisionTestSource("one", pdf)))
	service := provisionTestService(repo, pdf, customer, opportunity, owners)

	previewed, err := service.PreviewImport(ctx, actor, "batch", 1)
	if err != nil {
		t.Fatalf("preview error: %v", err)
	}
	if previewed.State != "PREVIEWED" {
		t.Fatalf("provision preview state=%s issues=%+v", previewed.State, previewed.Items[0].Issues)
	}
	if previewed.Items[0].State != contract.ImportItemStateNeedsProvision || previewed.Items[0].Digest == "" {
		t.Fatalf("provision row state=%s digest=%q", previewed.Items[0].State, previewed.Items[0].Digest)
	}
	if calls.lookup != 1 || calls.resolveOwner != 1 || calls.createCustomer != 0 {
		t.Fatalf("preview calls=%+v", calls)
	}
	if len(previewed.ProvisionPlan) != 1 {
		t.Fatalf("provision plan=%+v", previewed.ProvisionPlan)
	}
	plan := previewed.ProvisionPlan[0]
	if plan.CustomerName != "历史客户" || plan.CustomerState != contract.ImportProvisionCustomerExists ||
		plan.SalesName != "张三" || plan.SalesState != contract.ImportProvisionSalesResolved ||
		plan.OpportunityAmount != "1234.56" || plan.OpportunitySignDate != "2024-03-05" {
		t.Fatalf("provision plan=%+v", plan)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	fields := map[string]any{}
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal plan: %v", err)
	}
	for _, name := range []string{"customer_name", "customer_state", "sales_name", "sales_state", "opportunity_amount", "opportunity_sign_date"} {
		if _, ok := fields[name]; !ok {
			t.Fatalf("plan json missing %q: %s", name, raw)
		}
	}
	if len(fields) != 6 {
		t.Fatalf("plan json has extra fields: %s", raw)
	}
	// 批次 DTO 上必须挂在 provision_plan 键下（前端读 batch.provision_plan）。
	batchRaw, err := json.Marshal(previewed)
	if err != nil {
		t.Fatalf("marshal batch: %v", err)
	}
	batchFields := map[string]json.RawMessage{}
	if err = json.Unmarshal(batchRaw, &batchFields); err != nil {
		t.Fatalf("unmarshal batch: %v", err)
	}
	if _, ok := batchFields["provision_plan"]; !ok {
		t.Fatalf("batch json missing provision_plan: %s", batchRaw)
	}
}

// TestImportProvisionPreviewClassifiesBlockingCauses 验证销售未命中、客户多命中、
// CRM 4xx/5xx 各自产出对应 Issue 与阻断，且另一行不受影响。
// 参数：t 为测试上下文。
// 返回值：Issue 码、可重试标记或邻行状态不符时失败。
func TestImportProvisionPreviewClassifiesBlockingCauses(t *testing.T) {
	ctx := context.Background()
	actor := provisionImportActor()
	pdf := testfixture.PDF("historical contract")
	healthy := provisionHistoryItem("two", "old-2", "source-two", "正常客户", "李四", 2000)
	cases := []struct {
		name      string
		setup     func(*provisionCustomerStub, *provisionOwnerStub)
		wantCode  string
		retryable bool
		wantState string
	}{
		{name: "owner-not-found", wantCode: CodeImportProvisionOwnerUnresolved,
			setup: func(_ *provisionCustomerStub, owners *provisionOwnerStub) {
				owners.errByName = map[string]error{"张三": ErrProvisionOwnerNotFound}
			},
			wantState: contract.ImportProvisionSalesUnresolved},
		{name: "owner-ambiguous", wantCode: CodeImportProvisionOwnerUnresolved,
			setup: func(_ *provisionCustomerStub, owners *provisionOwnerStub) {
				owners.errByName = map[string]error{"张三": ErrProvisionOwnerAmbiguous}
			},
			wantState: contract.ImportProvisionSalesAmbiguous},
		{name: "owner-directory-unavailable", wantCode: CodeImportProvisionOwnerUnresolved, retryable: true,
			setup: func(_ *provisionCustomerStub, owners *provisionOwnerStub) {
				owners.errByName = map[string]error{"张三": ErrProvisionOwnerDirectoryUnavailable}
			},
			wantState: contract.ImportProvisionSalesUnresolved},
		{name: "customer-ambiguous", wantCode: CodeImportProvisionCustomerAmbiguous,
			setup: func(customer *provisionCustomerStub, _ *provisionOwnerStub) {
				customer.candidatesByName = map[string][]CRMProvisionCustomerCandidate{"历史客户": {
					{ID: 1, Name: "历史客户", Status: "ACTIVE"}, {ID: 2, Name: "历史客户", Status: "ACTIVE"}}}
			},
			wantState: contract.ImportProvisionCustomerAmbiguous},
		{name: "customer-inactive", wantCode: CodeImportProvisionCustomerAmbiguous,
			setup: func(customer *provisionCustomerStub, _ *provisionOwnerStub) {
				customer.candidatesByName = map[string][]CRMProvisionCustomerCandidate{"历史客户": {
					{ID: 1, Name: "历史客户", Status: "INACTIVE"}}}
			},
			wantState: contract.ImportProvisionCustomerAmbiguous},
		{name: "crm-rejected", wantCode: CodeImportProvisionCRMRejected,
			setup: func(customer *provisionCustomerStub, _ *provisionOwnerStub) {
				customer.lookupErrByName = map[string]error{"历史客户": ErrCRMProvisionRejected}
			}},
		{name: "crm-unavailable", wantCode: CodeImportProvisionCRMRejected, retryable: true,
			setup: func(customer *provisionCustomerStub, _ *provisionOwnerStub) {
				customer.lookupErrByName = map[string]error{"历史客户": ErrCRMProvisionUnavailable}
			}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			calls, customer, opportunity, owners := newProvisionStubs()
			testCase.setup(customer, owners)
			blocked := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
			repo := newProvisionImportRepository(provisionImportBatch([]contract.ImportItem{blocked, healthy},
				provisionTestSource("one", pdf), provisionTestSource("two", pdf)))
			service := provisionTestService(repo, pdf, customer, opportunity, owners)

			previewed, err := service.PreviewImport(ctx, actor, "batch", 1)
			if err != nil {
				t.Fatalf("preview error: %v", err)
			}
			first := previewed.Items[0]
			if first.State != "BLOCKED" || len(first.Issues) != 1 {
				t.Fatalf("blocked row=%+v", first)
			}
			if first.Issues[0].Code != testCase.wantCode || first.Issues[0].Retryable != testCase.retryable {
				t.Fatalf("issue=%+v want code=%s retryable=%v", first.Issues[0], testCase.wantCode, testCase.retryable)
			}
			// 邻行不受影响：仍是待开立且没有 Issue。
			second := previewed.Items[1]
			if second.State != contract.ImportItemStateNeedsProvision || len(second.Issues) != 0 {
				t.Fatalf("healthy row affected: %+v", second)
			}
			if len(previewed.ProvisionPlan) == 0 {
				t.Fatalf("blocked row produced no plan: %+v", previewed.Items[0])
			}
			plan := previewed.ProvisionPlan[0]
			// 计划行的两个状态必须是固定枚举，任何阻断原因都不留空值。
			customerStates := map[string]bool{contract.ImportProvisionCustomerExists: true,
				contract.ImportProvisionCustomerToCreate: true, contract.ImportProvisionCustomerAmbiguous: true}
			salesStates := map[string]bool{contract.ImportProvisionSalesResolved: true,
				contract.ImportProvisionSalesUnresolved: true, contract.ImportProvisionSalesAmbiguous: true}
			if !customerStates[plan.CustomerState] || !salesStates[plan.SalesState] {
				t.Fatalf("plan enum invalid: %+v", plan)
			}
			if testCase.wantState != "" && plan.CustomerState != testCase.wantState && plan.SalesState != testCase.wantState {
				t.Fatalf("plan state=%+v want %s", plan, testCase.wantState)
			}
			// 阻断行不得产生任何 CRM 写调用。
			if calls.createCustomer != 0 || calls.createOpportunity != 0 || calls.sign != 0 {
				t.Fatalf("blocked preview wrote to CRM: %+v", calls)
			}
			if previewed.State != "BLOCKED" {
				t.Fatalf("batch state=%s want BLOCKED", previewed.State)
			}
		})
	}
}

// provisionNotVerifiedError 模拟 CRM 503 CRM_IMPORT_PROVISION_CONTRACT_NOT_VERIFIED：
// 既是可重试依赖故障，又带稳定业务码（集成层不导出具体类型，测试用等价桩代替）。
type provisionNotVerifiedError struct{}

// Error 返回可落台账的短描述。
// 参数：无。
// 返回值：固定描述。
func (provisionNotVerifiedError) Error() string { return "CRM sign rejected by contract verification" }

// Unwrap 保留可重试依赖故障语义。
// 参数：无。
// 返回值：可重试哨兵。
func (provisionNotVerifiedError) Unwrap() error { return ErrCRMProvisionUnavailable }

// ProvisionCode 返回 CRM 稳定业务码。
// 参数：无。
// 返回值：合同验真未通过码。
func (provisionNotVerifiedError) ProvisionCode() string { return crmProvisionContractNotVerified }

// previewProvisionBatch 先预检再回到可调度状态，模拟确认后进入执行阶段。
// 参数：t 为测试上下文，service 为服务，repo 为仓储，actor 为操作者。
// 返回值：预检后的批次。
func previewProvisionBatch(t *testing.T, service *Service, repo *provisionImportRepository, actor Principal) contract.ImportBatch {
	t.Helper()
	previewed, err := service.PreviewImport(context.Background(), actor, "batch", 1)
	if err != nil {
		t.Fatalf("preview error: %v", err)
	}
	if previewed.State != "PREVIEWED" {
		t.Fatalf("preview state=%s issues=%+v", previewed.State, previewed.Items[0].Issues)
	}
	repo.batch.State = "QUEUED"
	return repo.batch
}

// resetProvisionCalls 清空计数：预检也会调用目录/查重，执行期断言只看调度之后。
// 参数：calls 为调用计数。
// 返回值：无。
func resetProvisionCalls(calls *provisionCallCounter) {
	*calls = provisionCallCounter{}
}

// dispatchUntilSettled 反复执行单项，直到批次离开 QUEUED/RUNNING。
// 参数：t 为测试上下文，service 为服务，step 为本次调度序号。
// 返回值：无；调度错误时失败。
func dispatchUntilSettled(t *testing.T, service *Service) {
	t.Helper()
	service.ImportActorResolver = func(context.Context, []byte, string, string) (Principal, error) {
		return provisionImportActor(), nil
	}
	for step := 0; step < 5; step++ {
		if err := service.DispatchImportOnce(context.Background()); err != nil {
			t.Fatalf("dispatch step %d error: %v", step, err)
		}
	}
}

// TestImportProvisionExecutionWalksDesignOrder 验证执行期按设计定稿 2 节顺序开立：
// 解析销售 → 建客户 → 建合同（带 crm_customer_id）→ 建商机 → 推进签约 → 回写 → COMPLETED。
// 参数：t 为测试上下文。
// 返回值：顺序、字段或台账终态不符时失败。
func TestImportProvisionExecutionWalksDesignOrder(t *testing.T) {
	pdf := testfixture.PDF("historical contract")
	actor := provisionImportActor()
	calls, customer, opportunity, owners := newProvisionStubs()
	item := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
	repo := newProvisionImportRepository(provisionImportBatch([]contract.ImportItem{item}, provisionTestSource("one", pdf)))
	service := provisionTestService(repo, pdf, customer, opportunity, owners)
	previewProvisionBatch(t, service, repo, actor)
	resetProvisionCalls(calls)

	dispatchUntilSettled(t, service)

	if repo.batch.State != "COMPLETED" || repo.batch.Items[0].State != "COMPLETE" {
		t.Fatalf("batch state=%s item=%+v", repo.batch.State, repo.batch.Items[0])
	}
	if calls.createCustomer != 1 || calls.createOpportunity != 1 || calls.sign != 1 {
		t.Fatalf("provision calls=%+v", calls)
	}
	if repo.commits != 1 || repo.created.CRMCustomerID != 501 || repo.created.CustomerName != "历史客户" ||
		repo.created.OriginKind != "IMPORTED" || repo.created.ImportMode != contract.ImportArchiveOnly {
		t.Fatalf("contract=%+v", repo.created)
	}
	if repo.created.Status != contract.StatusDraft {
		// 归档终态由既有历史确认链路负责，编排不得自己写 status。
		t.Fatalf("execution wrote contract status=%s", repo.created.Status)
	}
	if len(opportunity.createWith) != 1 {
		t.Fatalf("opportunity inputs=%+v", opportunity.createWith)
	}
	created := opportunity.createWith[0]
	if created.CustomerID != 501 || created.OwnerIdentityID != "identity-sales" ||
		created.Amount != "1234.56" || created.ExpectedSignDate != "2024-03-05" ||
		created.Name != repo.created.Title {
		t.Fatalf("opportunity input=%+v", created)
	}
	if len(opportunity.signWith) != 1 || opportunity.signWith[0].OpportunityID != 901 ||
		opportunity.signWith[0].ContractRef != repo.created.ID {
		t.Fatalf("sign calls=%+v contract=%s", opportunity.signWith, repo.created.ID)
	}
	if repo.written.contractID != repo.created.ID || repo.written.opportunityID != 901 ||
		repo.written.name != repo.created.Title {
		t.Fatalf("write back=%+v", repo.written)
	}
	key := contract.ImportCRMProvisionKey{TenantID: provisionTestTenant, SourceSystem: "legacy", ExternalRecordID: "old-1"}
	if record := repo.records[key]; record.State != contract.ImportCRMProvisionCompleted ||
		record.CustomerID != 501 || record.OpportunityID != 901 || !record.CustomerCreated || !record.OpportunityCreated {
		t.Fatalf("ledger=%+v", record)
	}
	// 幂等键必须稳定：同一步骤的重跑不会造出第二个客户/商机。
	if customer.createdWith[0].OwnerIdentityID != "identity-sales" || customer.createdWith[0].Name != "历史客户" {
		t.Fatalf("customer input=%+v", customer.createdWith[0])
	}
}

// TestImportProvisionExecutionBlocksRowAndKeepsOtherRows 验证 CRM 4xx/5xx 逐项失败：
// 失败行 BLOCKED + 对应 Issue，邻行保持未执行（由后续调度继续），可重试标记按语义。
// 参数：t 为测试上下文。
// 返回值：Issue 码、可重试标记或邻行状态不符时失败。
func TestImportProvisionExecutionBlocksRowAndKeepsOtherRows(t *testing.T) {
	cases := []struct {
		name       string
		createErr  error
		wantCode   string
		retryable  bool
		wantLedger string
	}{
		{name: "crm-4xx-rejected", createErr: ErrCRMProvisionRejected,
			wantCode: CodeImportProvisionCRMRejected, retryable: false, wantLedger: contract.ImportCRMProvisionBlocked},
		{name: "crm-5xx-unavailable", createErr: ErrCRMProvisionUnavailable,
			wantCode: CodeImportProvisionCRMRejected, retryable: true, wantLedger: contract.ImportCRMProvisionBlocked},
		{name: "contract-not-verified", createErr: provisionNotVerifiedError{},
			wantCode: CodeImportProvisionContractNotVerified, retryable: false, wantLedger: contract.ImportCRMProvisionBlocked},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			pdf := testfixture.PDF("historical contract")
			actor := provisionImportActor()
			calls, customer, opportunity, owners := newProvisionStubs()
			customer.createErr = testCase.createErr
			blocked := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
			healthy := provisionHistoryItem("two", "old-2", "source-two", "正常客户", "李四", 2000)
			repo := newProvisionImportRepository(provisionImportBatch([]contract.ImportItem{blocked, healthy},
				provisionTestSource("one", pdf), provisionTestSource("two", pdf)))
			service := provisionTestService(repo, pdf, customer, opportunity, owners)
			previewProvisionBatch(t, service, repo, actor)
			resetProvisionCalls(calls)

			dispatchUntilSettled(t, service)

			if repo.batch.State != "PARTIAL" {
				t.Fatalf("batch state=%s", repo.batch.State)
			}
			first := repo.batch.Items[0]
			if first.State != "BLOCKED" || len(first.Issues) != 1 || first.Issues[0].Code != testCase.wantCode ||
				first.Issues[0].Retryable != testCase.retryable {
				t.Fatalf("blocked issue=%+v", first)
			}
			if testCase.wantCode == CodeImportProvisionContractNotVerified && first.Issues[0].Message == "" {
				t.Fatal("contract verification failure must carry an actionable hint")
			}
			// 邻行不受影响：仍停在待开立，没有 Issue、没有合同。
			second := repo.batch.Items[1]
			if second.State != contract.ImportItemStateNeedsProvision || len(second.Issues) != 0 || second.ContractID != "" {
				t.Fatalf("healthy row affected: %+v", second)
			}
			key := contract.ImportCRMProvisionKey{TenantID: provisionTestTenant, SourceSystem: "legacy", ExternalRecordID: "old-1"}
			if record := repo.records[key]; record.State != testCase.wantLedger || record.LastErrorCode != testCase.wantCode {
				t.Fatalf("ledger=%+v", record)
			}
			if calls.createOpportunity != 0 || calls.sign != 0 {
				t.Fatalf("failed row reached opportunity step: %+v", calls)
			}
		})
	}
}

// TestImportProvisionExecutionIsIdempotent 验证重跑不重复开立：
// 已 COMPLETED 直接跳过；已建客户/商机直接复用。
// 参数：t 为测试上下文。
// 返回值：重复创建或多余调用时失败。
func TestImportProvisionExecutionIsIdempotent(t *testing.T) {
	key := contract.ImportCRMProvisionKey{TenantID: provisionTestTenant, SourceSystem: "legacy", ExternalRecordID: "old-1"}
	t.Run("completed-skips-everything", func(t *testing.T) {
		pdf := testfixture.PDF("historical contract")
		actor := provisionImportActor()
		calls, customer, opportunity, owners := newProvisionStubs()
		item := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
		repo := newProvisionImportRepository(provisionImportBatch([]contract.ImportItem{item}, provisionTestSource("one", pdf)))
		service := provisionTestService(repo, pdf, customer, opportunity, owners)
		previewProvisionBatch(t, service, repo, actor)
		resetProvisionCalls(calls)
		repo.records[key] = contract.ImportCRMProvision{ImportCRMProvisionKey: key, State: contract.ImportCRMProvisionCompleted,
			Attempt: 4, CustomerID: 501, OpportunityID: 901}
		repo.batch.Items[0].ContractID = "contract-existing"
		repo.contract = contract.Contract{ID: "contract-existing", TenantID: provisionTestTenant, Version: 1, CreatedBy: actor.UserID}

		dispatchUntilSettled(t, service)

		if calls.lookup+calls.createCustomer+calls.createOpportunity+calls.sign+calls.resolveOwner != 0 {
			t.Fatalf("completed row called CRM: %+v", calls)
		}
		if repo.batch.Items[0].State != "COMPLETE" || repo.commits != 0 {
			t.Fatalf("completed row not closed: %+v commits=%d", repo.batch.Items[0], repo.commits)
		}
	})

	t.Run("existing-customer-and-opportunity-are-reused", func(t *testing.T) {
		pdf := testfixture.PDF("historical contract")
		actor := provisionImportActor()
		calls, customer, opportunity, owners := newProvisionStubs()
		item := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
		repo := newProvisionImportRepository(provisionImportBatch([]contract.ImportItem{item}, provisionTestSource("one", pdf)))
		service := provisionTestService(repo, pdf, customer, opportunity, owners)
		previewProvisionBatch(t, service, repo, actor)
		resetProvisionCalls(calls)
		// 上一次尝试已建客户与商机（合同也已入库），重跑只续做未完成的签核与回写。
		repo.records[key] = contract.ImportCRMProvision{ImportCRMProvisionKey: key, State: contract.ImportCRMProvisionContractReady,
			Attempt: 3, CustomerID: 501, CustomerCreated: true, OpportunityID: 901}
		repo.batch.Items[0].ContractID = "contract-existing"
		repo.contract = contract.Contract{ID: "contract-existing", TenantID: provisionTestTenant, Title: "历史合同-one",
			CRMCustomerID: 501, Version: 1, CreatedBy: actor.UserID, SourceFileID: "file-1", Status: contract.StatusDraft}

		dispatchUntilSettled(t, service)

		if calls.createCustomer != 0 || calls.createOpportunity != 0 {
			t.Fatalf("rerun duplicated CRM writes: %+v", calls)
		}
		if calls.sign != 1 || opportunity.signWith[0].OpportunityID != 901 {
			t.Fatalf("rerun sign=%+v item=%+v ledger=%+v", opportunity.signWith, repo.batch.Items[0], repo.records[key])
		}
		if repo.written.opportunityID != 901 || repo.batch.Items[0].State != "COMPLETE" {
			t.Fatalf("write back=%+v item=%+v", repo.written, repo.batch.Items[0])
		}
		if record := repo.records[key]; record.State != contract.ImportCRMProvisionCompleted {
			t.Fatalf("ledger=%+v", record)
		}
	})

	t.Run("customer-ready-resumes-from-contract-step", func(t *testing.T) {
		pdf := testfixture.PDF("historical contract")
		actor := provisionImportActor()
		calls, customer, opportunity, owners := newProvisionStubs()
		customer.candidates = []CRMProvisionCustomerCandidate{{ID: 501, Name: "历史客户", Status: "ACTIVE"}}
		item := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
		repo := newProvisionImportRepository(provisionImportBatch([]contract.ImportItem{item}, provisionTestSource("one", pdf)))
		service := provisionTestService(repo, pdf, customer, opportunity, owners)
		previewProvisionBatch(t, service, repo, actor)
		resetProvisionCalls(calls)
		repo.records[key] = contract.ImportCRMProvision{ImportCRMProvisionKey: key, State: contract.ImportCRMProvisionCustomerReady,
			Attempt: 2, CustomerID: 501, CustomerCreated: true}

		dispatchUntilSettled(t, service)

		if calls.createCustomer != 0 {
			t.Fatalf("rerun created a customer again: %+v", calls)
		}
		if repo.commits != 1 || repo.created.CRMCustomerID != 501 {
			t.Fatalf("contract=%+v commits=%d", repo.created, repo.commits)
		}
		if calls.createOpportunity != 1 || calls.sign != 1 {
			t.Fatalf("opportunity step not resumed: %+v", calls)
		}
	})
}

// TestImportProvisionExecutionDisabledAfterPreviewBlocks 验证已按开立通道预检的批次在
// 开关随后关闭时给出 IMPORT_CRM_PROVISION_DISABLED，而不是含混的校验失败。
// 参数：t 为测试上下文。
// 返回值：错误码或可重试标记不符时失败。
func TestImportProvisionExecutionDisabledAfterPreviewBlocks(t *testing.T) {
	pdf := testfixture.PDF("historical contract")
	actor := provisionImportActor()
	calls, customer, opportunity, owners := newProvisionStubs()
	item := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
	repo := newProvisionImportRepository(provisionImportBatch([]contract.ImportItem{item}, provisionTestSource("one", pdf)))
	service := provisionTestService(repo, pdf, customer, opportunity, owners)
	previewProvisionBatch(t, service, repo, actor)
	resetProvisionCalls(calls)

	// 模拟运维在确认前关掉开关（ConfigureCRMProvision(false) 会清空三个注入点）。
	service.CRMProvisionCustomerClient, service.CRMProvisionOpportunityClient, service.CRMProvisionOwnerDirectory = nil, nil, nil
	dispatchUntilSettled(t, service)

	first := repo.batch.Items[0]
	if first.State != "BLOCKED" || len(first.Issues) != 1 || first.Issues[0].Code != CodeImportCRMProvisionDisabled ||
		first.Issues[0].Retryable {
		t.Fatalf("disabled issue=%+v", first)
	}
	if calls.createCustomer != 0 || calls.createOpportunity != 0 || calls.sign != 0 {
		t.Fatalf("disabled execution called CRM: %+v", calls)
	}
}

// TestImportProvisionExecutionRetriesAfterBlockedRow 验证 BLOCKED 行可按新一次尝试续做：
// 复位后从断点继续，最终 COMPLETED，不重复建客户。
// 参数：t 为测试上下文。
// 返回值：重试路径未复位或重复开立时失败。
func TestImportProvisionExecutionRetriesAfterBlockedRow(t *testing.T) {
	pdf := testfixture.PDF("historical contract")
	actor := provisionImportActor()
	calls, customer, opportunity, owners := newProvisionStubs()
	// 预检（查重）成功、执行期第一次建客户遇到 5xx，第二次恢复。
	customer.createErrSeq = []error{ErrCRMProvisionUnavailable, nil}
	item := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
	repo := newProvisionImportRepository(provisionImportBatch([]contract.ImportItem{item}, provisionTestSource("one", pdf)))
	service := provisionTestService(repo, pdf, customer, opportunity, owners)
	previewProvisionBatch(t, service, repo, actor)
	resetProvisionCalls(calls)

	dispatchUntilSettled(t, service)
	key := contract.ImportCRMProvisionKey{TenantID: provisionTestTenant, SourceSystem: "legacy", ExternalRecordID: "old-1"}
	if record := repo.records[key]; record.State != contract.ImportCRMProvisionBlocked || record.LastErrorCode != CodeImportProvisionCRMRejected {
		t.Fatalf("first attempt ledger=%+v", record)
	}
	first := repo.batch.Items[0]
	if first.State != "BLOCKED" || !first.Issues[0].Retryable {
		t.Fatalf("first attempt issue=%+v", first)
	}

	// 依赖恢复后重新调度同一行：BLOCKED → PENDING 复位，续做并收口。
	repo.batch.State = "QUEUED"
	dispatchUntilSettled(t, service)

	if repo.batch.Items[0].State != "COMPLETE" || repo.batch.State != "COMPLETED" {
		t.Fatalf("retry did not settle: item=%+v batch=%s", repo.batch.Items[0], repo.batch.State)
	}
	if record := repo.records[key]; record.State != contract.ImportCRMProvisionCompleted {
		t.Fatalf("retry ledger=%+v", record)
	}
	// 两次建客户都使用同一幂等键：即使台账进度丢失，CRM 也只会有一个客户。
	if len(customer.createKeys) != 2 || customer.createKeys[0] != customer.createKeys[1] {
		t.Fatalf("customer idempotency keys=%v", customer.createKeys)
	}
	if calls.createOpportunity != 1 || calls.sign != 1 {
		t.Fatalf("retry duplicated opportunity writes: %+v", calls)
	}
}

// TestImportProvisionRetryAfterContractFailureReusesCustomer 验证建合同步骤失败后的重试：
// 复位回 PENDING 时已取得的 customer_id 必须被复用，不重复建客户。
// 参数：t 为测试上下文。
// 返回值：重试重新建客户或无法续做时失败。
func TestImportProvisionRetryAfterContractFailureReusesCustomer(t *testing.T) {
	pdf := testfixture.PDF("historical contract")
	actor := provisionImportActor()
	calls, customer, opportunity, owners := newProvisionStubs()
	customer.candidates = []CRMProvisionCustomerCandidate{{ID: 501, Name: "历史客户", Status: "ACTIVE"}}
	item := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
	repo := newProvisionImportRepository(provisionImportBatch([]contract.ImportItem{item}, provisionTestSource("one", pdf)))
	repo.commitErrs = []error{ErrImportConflict, nil}
	service := provisionTestService(repo, pdf, customer, opportunity, owners)
	previewProvisionBatch(t, service, repo, actor)
	resetProvisionCalls(calls)

	dispatchUntilSettled(t, service)
	key := contract.ImportCRMProvisionKey{TenantID: provisionTestTenant, SourceSystem: "legacy", ExternalRecordID: "old-1"}
	first := repo.batch.Items[0]
	if first.State != "BLOCKED" || first.Issues[0].Code != "IMPORT_REVISION_CONFLICT" {
		t.Fatalf("first attempt item=%+v", first)
	}
	if record := repo.records[key]; record.State != contract.ImportCRMProvisionBlocked ||
		record.CustomerID != 501 || record.LastErrorCode != "IMPORT_REVISION_CONFLICT" {
		t.Fatalf("first attempt ledger=%+v", record)
	}

	repo.batch.State = "QUEUED"
	dispatchUntilSettled(t, service)

	if repo.batch.Items[0].State != "COMPLETE" || repo.batch.State != "COMPLETED" {
		t.Fatalf("retry did not settle: item=%+v batch=%s", repo.batch.Items[0], repo.batch.State)
	}
	if calls.createCustomer != 0 {
		t.Fatalf("retry created a customer again: %+v", calls)
	}
	if repo.created.CRMCustomerID != 501 || calls.createOpportunity != 1 || calls.sign != 1 {
		t.Fatalf("retry did not finish provision: contract=%+v calls=%+v", repo.created, calls)
	}
	if record := repo.records[key]; record.State != contract.ImportCRMProvisionCompleted {
		t.Fatalf("retry ledger=%+v", record)
	}
}

// TestImportProvisionPassesCustomerProfileColumnsToCRM 验证客户类型/行业/地区原样进建客户请求：
// 有值时携带相同值；空值时显式传空串（既不是省略字段，也不在合同侧补默认值，
// 默认值「企业/其他/未填写」由 CRM 侧按 IMPORT_PROVISION 口径补齐）。
// 参数：t 为测试上下文。
// 返回值：透传值、空串语义或 provision_plan 六键契约不符时失败。
func TestImportProvisionPassesCustomerProfileColumnsToCRM(t *testing.T) {
	cases := []struct{ name, customerType, industry, region string }{
		{name: "provided", customerType: "企业", industry: "制造业", region: "华东"},
		{name: "empty-is-passed-through", customerType: "", industry: "", region: ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			pdf := testfixture.PDF("historical contract")
			actor := provisionImportActor()
			calls, customer, opportunity, owners := newProvisionStubs()
			item := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
			item.Metadata.CustomerType = testCase.customerType
			item.Metadata.Industry = testCase.industry
			item.Metadata.Region = testCase.region
			repo := newProvisionImportRepository(provisionImportBatch([]contract.ImportItem{item}, provisionTestSource("one", pdf)))
			service := provisionTestService(repo, pdf, customer, opportunity, owners)

			previewed, err := service.PreviewImport(ctx, actor, "batch", 1)
			if err != nil {
				t.Fatalf("preview error: %v", err)
			}
			if previewed.State != "PREVIEWED" {
				t.Fatalf("preview state=%s issues=%+v", previewed.State, previewed.Items[0].Issues)
			}
			// 透传三列不得改变 provision_plan 的六键契约。
			if len(previewed.ProvisionPlan) != 1 {
				t.Fatalf("provision plan=%+v", previewed.ProvisionPlan)
			}
			planRaw, err := json.Marshal(previewed.ProvisionPlan[0])
			if err != nil {
				t.Fatalf("marshal plan: %v", err)
			}
			planFields := map[string]json.RawMessage{}
			if err = json.Unmarshal(planRaw, &planFields); err != nil {
				t.Fatalf("unmarshal plan: %v", err)
			}
			if len(planFields) != 6 {
				t.Fatalf("provision plan keys changed: %s", planRaw)
			}
			repo.batch.State = "QUEUED"
			resetProvisionCalls(calls)

			dispatchUntilSettled(t, service)

			if repo.batch.State != "COMPLETED" || repo.batch.Items[0].State != "COMPLETE" {
				t.Fatalf("batch=%s item=%+v", repo.batch.State, repo.batch.Items[0])
			}
			if len(customer.createdWith) != 1 {
				t.Fatalf("customer inputs=%+v", customer.createdWith)
			}
			input := customer.createdWith[0]
			if input.CustomerType != testCase.customerType || input.Industry != testCase.industry || input.Region != testCase.region {
				t.Fatalf("customer input=%+v want type=%q industry=%q region=%q",
					input, testCase.customerType, testCase.industry, testCase.region)
			}
			// 空串必须是显式字段而不是被省略，否则 CRM 无法按通道口径补默认值。
			body, err := json.Marshal(input)
			if err != nil {
				t.Fatalf("marshal customer input: %v", err)
			}
			wire := map[string]any{}
			if err = json.Unmarshal(body, &wire); err != nil {
				t.Fatalf("unmarshal customer input: %v", err)
			}
			for name, want := range map[string]string{
				"customer_type": testCase.customerType, "industry": testCase.industry, "region": testCase.region,
			} {
				got, present := wire[name]
				if !present || got != want {
					t.Fatalf("customer input json %q=%v present=%v want %q: %s", name, got, present, want, body)
				}
			}
			if repo.created.CRMCustomerID != 501 {
				t.Fatalf("contract=%+v", repo.created)
			}
		})
	}
}

// TestImportProvisionPreviewRejectsOverlongCustomerProfileColumns 验证三列超长在预览期就
// 按字段路径阻断：不查重、不建客户、不产出计划行，错误码复用既有 TOO_LONG。
// 参数：t 为测试上下文。
// 返回值：字段路径、错误码或「预览期即阻断」语义不符时失败。
func TestImportProvisionPreviewRejectsOverlongCustomerProfileColumns(t *testing.T) {
	overlong := strings.Repeat("测", contract.ImportProvisionCustomerProfileMaxLength+1)
	cases := []struct {
		name  string
		field string
		apply func(*contract.ImportMetadata)
	}{
		{name: "customer-type", field: "metadata.customer_type",
			apply: func(m *contract.ImportMetadata) { m.CustomerType = overlong }},
		{name: "industry", field: "metadata.industry",
			apply: func(m *contract.ImportMetadata) { m.Industry = overlong }},
		{name: "region", field: "metadata.region",
			apply: func(m *contract.ImportMetadata) { m.Region = overlong }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			pdf := testfixture.PDF("historical contract")
			actor := provisionImportActor()
			calls, customer, opportunity, owners := newProvisionStubs()
			item := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 123456)
			testCase.apply(&item.Metadata)
			repo := newProvisionImportRepository(provisionImportBatch([]contract.ImportItem{item}, provisionTestSource("one", pdf)))
			service := provisionTestService(repo, pdf, customer, opportunity, owners)

			previewed, err := service.PreviewImport(ctx, actor, "batch", 1)
			if err != nil {
				t.Fatalf("preview error: %v", err)
			}
			first := previewed.Items[0]
			if first.State != "BLOCKED" || len(first.Issues) != 1 {
				t.Fatalf("overlong row=%+v", first)
			}
			issue := first.Issues[0]
			if issue.Field != testCase.field || issue.Code != codeImportFieldTooLong || issue.Retryable {
				t.Fatalf("issue=%+v want field=%s code=%s", issue, testCase.field, codeImportFieldTooLong)
			}
			if previewed.State != "BLOCKED" {
				t.Fatalf("batch state=%s want BLOCKED", previewed.State)
			}
			// 超长是确定性的输入错误：预览期就阻断，不得先调用 CRM 再失败。
			if calls.lookup != 0 || calls.resolveOwner != 0 || calls.createCustomer != 0 {
				t.Fatalf("overlong row reached CRM: %+v", calls)
			}
			if len(previewed.ProvisionPlan) != 0 {
				t.Fatalf("overlong row produced a plan: %+v", previewed.ProvisionPlan)
			}
		})
	}
}

// TestImportProvisionCustomerProfileLengthBoundary 验证 64 字符边界与执行期兜底校验：
// 恰好 64 个字符合法，65 个字符按字段路径给出不可重试的 TOO_LONG。
// 参数：t 为测试上下文。
// 返回值：边界判定或失败投影不符时失败。
func TestImportProvisionCustomerProfileLengthBoundary(t *testing.T) {
	exact := strings.Repeat("测", contract.ImportProvisionCustomerProfileMaxLength)
	metadata := contract.ImportMetadata{CustomerType: exact, Industry: exact, Region: "华东"}
	if failure := importProvisionCustomerProfileFailure(metadata); failure != nil {
		t.Fatalf("64 characters must be accepted: %+v", failure)
	}
	metadata.Industry = strings.Repeat("测", contract.ImportProvisionCustomerProfileMaxLength+1)
	failure := importProvisionCustomerProfileFailure(metadata)
	if failure == nil || failure.Field != "metadata.industry" || failure.Code != codeImportFieldTooLong || failure.Retryable {
		t.Fatalf("failure=%+v", failure)
	}
	if failure.Message == "" || failure.issue().Field != "metadata.industry" || failure.issue().Code != codeImportFieldTooLong {
		t.Fatalf("failure lacks a locatable issue: %+v", failure)
	}
}
