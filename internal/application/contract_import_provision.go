package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// 历史导入自动开立 CRM 的逐项错误码（包含可恢复依赖故障分类）。
// 它们只写进既有 Issue 机制（ImportItemState.Issues），不改变 HTTP 错误码契约。
const (
	// CodeImportCRMProvisionDisabled 表示批次按开立通道预检，但执行时开关已关闭。
	CodeImportCRMProvisionDisabled = "IMPORT_CRM_PROVISION_DISABLED"
	// CodeImportProvisionOwnerUnresolved 表示销售姓名无法解析成唯一平台身份。
	CodeImportProvisionOwnerUnresolved = "IMPORT_PROVISION_OWNER_UNRESOLVED"
	// CodeImportProvisionCustomerAmbiguous 表示客户名称多命中或命中已停用客户。
	CodeImportProvisionCustomerAmbiguous = "IMPORT_PROVISION_CUSTOMER_AMBIGUOUS"
	// CodeImportProvisionCRMRejected 表示 CRM 明确拒绝本次开立（业务 4xx）。
	CodeImportProvisionCRMRejected = "IMPORT_PROVISION_CRM_REJECTED"
	// CodeImportProvisionCRMUnavailable 表示依赖故障，不能引导用户修改正确的业务数据。
	CodeImportProvisionCRMUnavailable = "IMPORT_PROVISION_CRM_UNAVAILABLE"
	// CodeImportProvisionContractNotVerified 表示 CRM 侧合同验真未通过，需要配置凭据。
	CodeImportProvisionContractNotVerified = "IMPORT_PROVISION_CONTRACT_NOT_VERIFIED"
	// codeImportValidationFailed 复用既有预检失败码，用于清单声明本身不合法但
	// 无法用五个开立错误码表达的防御分支（例如客户名称为空）。
	codeImportValidationFailed = "IMPORT_VALIDATION_FAILED"
	// codeImportFieldTooLong 复用既有清单字段长度错误码（与 contract_import_fields.go
	// 的 TOO_LONG 逐字一致），让「某一列超长」在预览期就能按字段路径定位。
	codeImportFieldTooLong = "TOO_LONG"
)

// crmProvisionContractNotVerified 是 CRM 商机推进接口返回的稳定 503 业务码
// （customer_and_opportunity opportunity/provision.go）。它代表配置缺失而非数据问题，
// 因此单独映射成可操作提示，而不是笼统的 CRM_REJECTED。
const crmProvisionContractNotVerified = "CRM_IMPORT_PROVISION_CONTRACT_NOT_VERIFIED"

// 开立通道的跨包哨兵：集成层把它们重新导出为自身包级变量，
// 编排层因此可以在不导入集成包（会形成循环依赖）的前提下用 errors.Is 分类。
var (
	// ErrCRMProvisionUnavailable 表示开立 CRM 调用遇到可重试依赖故障（网络/超时/5xx/协议异常）。
	ErrCRMProvisionUnavailable = errors.New("CRM import provision is unavailable")
	// ErrCRMProvisionRejected 表示 CRM 明确拒绝本次开立（数据不合法或状态冲突）。
	ErrCRMProvisionRejected = errors.New("CRM import provision rejected")
	// ErrProvisionOwnerNotFound 表示平台负责人目录 0 命中。
	ErrProvisionOwnerNotFound = errors.New("owner directory has no matching owner")
	// ErrProvisionOwnerAmbiguous 表示平台负责人目录多命中。
	ErrProvisionOwnerAmbiguous = errors.New("owner directory has multiple matching owners")
	// ErrProvisionOwnerDirectoryUnavailable 表示平台负责人目录不可用（可重试）。
	ErrProvisionOwnerDirectoryUnavailable = errors.New("owner directory is unavailable")
)

// ContractImportCRMProvisionStore 是开立台账（con_import_crm_provision）的最小持久化能力。
// 键固定为 (tenant_id, source_system, external_record_id)，重跑据此续做而不重复开立。
type ContractImportCRMProvisionStore interface {
	GetImportCRMProvision(context.Context, contract.ImportCRMProvisionKey) (contract.ImportCRMProvision, error)
	UpsertStartImportCRMProvision(context.Context, contract.ImportCRMProvisionKey, string) (contract.ImportCRMProvision, error)
	AdvanceImportCRMProvision(context.Context, contract.ImportCRMProvisionKey, contract.ImportCRMProvisionAdvance) (contract.ImportCRMProvision, error)
	RecordImportCRMProvisionError(context.Context, contract.ImportCRMProvisionKey, string, int, string, string) (contract.ImportCRMProvision, error)
	ResetImportCRMProvision(context.Context, contract.ImportCRMProvisionKey, int) (contract.ImportCRMProvision, error)
}

// ContractImportNumberPreparer 在CRM验真前仅预留编号，不确认合同或生成下游目标。
type ContractImportNumberPreparer interface {
	// PrepareImportContractNumber 在本地历史导入围栏内预留稳定系统号供CRM验真。
	// 参数：上下文、租户、批次、清单版本、执行代次、单项ID和合同ID定位授权基线。
	// 返回值：预留编号；围栏失效或来源不一致时返回冲突，其余返回存储错误。
	PrepareImportContractNumber(context.Context, string, string, uint64, uint64, string, string) (string, error)
}

// ContractImportOpportunityWriter 回写开立通道取得的商机引用。
// 它必须保持合同 version/status 不变：历史确认链路要求 version==1 才能落 archived。
type ContractImportOpportunityWriter interface {
	UpdateImportContractOpportunity(context.Context, string, string, uint64, string, string) error
}

// importProvisionFailure 是一次开立失败的逐项投影。
// Code/Retryable/Message 进入 Issue；Message 只放静态中文提示（不回显 CRM 原文）；
// Reason 只进入台账 last_error_message，便于运维定位且按列宽截断。
type importProvisionFailure struct {
	Field     string
	Code      string
	Retryable bool
	Message   string
	Reason    string
}

// Error 让失败可以作为 error 在编排内部传递。
// 参数：无；使用接收者字段。
// 返回值：错误码与可读原因。
func (failure *importProvisionFailure) Error() string {
	return failure.Code + ": " + failure.Reason
}

// issue 转换为既有 Issue 机制使用的逐项问题。
// 参数：无。
// 返回值：字段路径、稳定错误码、可重试标记与静态提示。
func (failure *importProvisionFailure) issue() contract.ImportIssue {
	return contract.ImportIssue{Field: failure.Field, Code: failure.Code, Retryable: failure.Retryable, Message: failure.Message}
}

// newProvisionFailure 组装一个失败投影，避免各处重复字段赋值。
// 参数：field 为字段路径，code 为稳定错误码，retryable 为可重试标记，
// message 为静态中文提示，reason 为只落台账的可读原因。
// 返回值：可直接返回的失败错误。
func newProvisionFailure(field, code string, retryable bool, message, reason string) *importProvisionFailure {
	return &importProvisionFailure{Field: field, Code: code, Retryable: retryable, Message: message, Reason: reason}
}

// crmProvisionEnabled 判断开立通道是否已完整装配。
// 参数：无；读取服务注入点。
// 返回值：三条依赖都存在时为 true；开关关闭时 ConfigureCRMProvision 已把它们清空。
func (s *Service) crmProvisionEnabled() bool {
	return s.CRMProvisionCustomerClient != nil && s.CRMProvisionOpportunityClient != nil && s.CRMProvisionOwnerDirectory != nil
}

// importProvisionModeApplies 判断模式是否属于设计定稿 1 节的生效范围。
// 参数：mode 为清单模式。
// 返回值：仅 ARCHIVE_ONLY / HISTORICAL_CLOSED 返回 true；NORMAL 与 CONTINUE_LIVE 不变。
func importProvisionModeApplies(mode contract.ImportMode) bool {
	return mode == contract.ImportArchiveOnly || mode == contract.ImportHistoricalClosed
}

// importProvisionItemApplies 判断某一行是否需要开立通道。
// 参数：item 为冻结清单项。
// 返回值：开关开启、历史模式，且清单未声明客户/商机引用
// （声明了引用的行仍走既有 CRM 只读回读校验，不放宽任何门禁）。
func (s *Service) importProvisionItemApplies(item contract.ImportItem) bool {
	metadata := item.Metadata
	return s.crmProvisionEnabled() && importProvisionModeApplies(item.Mode) &&
		metadata.CRMCustomerID == 0 && metadata.OpportunityID == "" && metadata.OpportunityName == "" &&
		metadata.CounterpartySource != contract.CounterpartyLocal
}

// importProvisionAmount 把最小货币单位格式化为元的两位小数字符串。
// 参数：minor 为 amount_minor。
// 返回值：十进制字符串，例如 123456 → "1234.56"。
func importProvisionAmount(minor int64) string {
	whole, fraction := minor/100, minor%100
	if fraction < 0 {
		fraction = -fraction
	}
	return fmt.Sprintf("%d.%02d", whole, fraction)
}

// importProvisionSignDate 把合同开始日期格式化为 YYYY-MM-DD。
// 参数：start 为清单声明开始日期，可为 nil。
// 返回值：UTC 日期字符串；无开始日期时为空串（开立预检与执行期均会拒绝）。
func importProvisionSignDate(start *time.Time) string {
	if start == nil {
		return ""
	}
	return start.UTC().Format("2006-01-02")
}

// importProvisionOverlongCustomerProfileColumn 返回第一个超长的客户档案列的清单字段路径。
// 参数：metadata 为清单声明。
// 返回值：形如 metadata.customer_type 的字段路径；三列都未超过上限时为空串。
// 上限取 domain 常量（64 字符），与 CRM 侧通道 DTO 的 max=64 一致。
func importProvisionOverlongCustomerProfileColumn(metadata contract.ImportMetadata) string {
	customerType, industry, region := metadata.ProvisionCustomerProfile()
	for _, column := range []struct{ path, value string }{
		{"metadata.customer_type", customerType},
		{"metadata.industry", industry},
		{"metadata.region", region},
	} {
		if utf8.RuneCountInString(column.value) > contract.ImportProvisionCustomerProfileMaxLength {
			return column.path
		}
	}
	return ""
}

// importProvisionCustomerProfileIssue 把三列长度校验结果转成预览期可定位的逐项 Issue。
// 参数：metadata 为清单声明。
// 返回值：nil 表示三列都合法；否则为指向具体列的错误码 TOO_LONG。
func importProvisionCustomerProfileIssue(metadata contract.ImportMetadata) *contract.ImportIssue {
	if field := importProvisionOverlongCustomerProfileColumn(metadata); field != "" {
		return &contract.ImportIssue{Field: field, Code: codeImportFieldTooLong}
	}
	return nil
}

// importProvisionCustomerProfileFailure 是执行期的同一条长度校验，避免绕过预览直接开立。
// 参数：metadata 为清单声明。
// 返回值：nil 表示三列都合法；否则为指向具体列的不可重试失败投影。
func importProvisionCustomerProfileFailure(metadata contract.ImportMetadata) *importProvisionFailure {
	field := importProvisionOverlongCustomerProfileColumn(metadata)
	if field == "" {
		return nil
	}
	return newProvisionFailure(field, codeImportFieldTooLong, false,
		"客户类型、行业、地区每列最多 64 个字符，请精简后重新导入", field+" exceeds 64 characters")
}

// importProvisionRequiredFieldIssue 在新建写入前核对CRM契约，不补造日期、姓名或电话。
// 参数：metadata为真实清单，contactsRequired表示客户查重为零、必须新建登记联系人。
// 返回值：首个字段问题；满足实际CRM必填与长度限制时返回nil。
func importProvisionRequiredFieldIssue(metadata contract.ImportMetadata, contactsRequired bool) *contract.ImportIssue {
	if metadata.StartDate == nil {
		return &contract.ImportIssue{Field: "metadata.start_date", Code: "REQUIRED", Message: "自动开立CRM商机须填写合同开始日期"}
	}
	for _, column := range []struct {
		field, value string
		max          int
	}{
		{"title", metadata.Title, 200}, {"customer_name", metadata.CustomerName, 200},
		{"customer_contact", metadata.CustomerContact, 100}, {"customer_phone", metadata.CustomerPhone, 32},
	} {
		value := strings.TrimSpace(column.value)
		contactColumn := column.field == "customer_contact" || column.field == "customer_phone"
		if contactColumn && !contactsRequired {
			continue
		}
		if contactsRequired && (column.field == "customer_contact" || column.field == "customer_phone") && value == "" {
			return &contract.ImportIssue{Field: "metadata." + column.field, Code: "REQUIRED", Message: "新建CRM客户须填写真实登记联系人及联系电话"}
		}
		if utf8.RuneCountInString(value) > column.max {
			return &contract.ImportIssue{Field: "metadata." + column.field, Code: codeImportFieldTooLong, Message: "字段超出CRM开立接口长度限制，请精简后重新预检"}
		}
	}
	return nil
}

// importProvisionIdempotencyKey 生成一次 CRM 调用的稳定幂等键。
// 参数：step 为步骤名（lookup/customer/opportunity/sign），key 为开立幂等键。
// 返回值：不超过 128 字符的稳定字符串；同一 (租户, 来源, 外部记录, 步骤) 恒等，
// 因此重跑即使丢了台账进度，也不会重复建客户/商机、重复推进签约。
func importProvisionIdempotencyKey(step string, key contract.ImportCRMProvisionKey) string {
	sum := sha256.Sum256([]byte(key.TenantID + "\x00" + key.SourceSystem + "\x00" + key.ExternalRecordID))
	return "import-provision:" + step + ":" + hex.EncodeToString(sum[:])
}

// planImportProvision 生成一行开立计划，并把阻断原因转成逐项 Issue。
// 参数：ctx 为请求，actor 为可信主体（只取其身份做机器调用头），b 为批次，
// item 为已通过形状校验的清单项。
// 返回值：计划行、阻断 Issue（nil 表示可开立）。
func (s *Service) planImportProvision(ctx context.Context, actor Principal, b contract.ImportBatch, item contract.ImportItem) (contract.ImportProvisionPlanItem, *contract.ImportIssue) {
	plan := contract.ImportProvisionPlanItem{
		CustomerName:        strings.TrimSpace(item.Metadata.CustomerName),
		SalesName:           strings.TrimSpace(item.Metadata.SalesName),
		OpportunityAmount:   importProvisionAmount(item.Metadata.AmountMinor),
		OpportunitySignDate: importProvisionSignDate(item.Metadata.StartDate),
	}
	// 两项解析都执行：即使销售解析失败也要给出客户状态，计划行不会出现空枚举。
	_, salesState, salesFailure := s.resolveImportProvisionSales(ctx, plan.SalesName)
	plan.SalesState = salesState
	customerFailure := s.planImportProvisionCustomer(ctx, actor, b, item, &plan)
	if salesFailure != nil {
		issue := salesFailure.issue()
		return plan, &issue
	}
	if customerFailure != nil {
		issue := customerFailure.issue()
		return plan, &issue
	}
	return plan, nil
}

// planImportProvisionCustomer 查重客户并填写计划行的客户状态。
// 参数：ctx/actor/b/item 定位本次查重，plan 为待填充的计划行。
// 返回值：客户侧阻断失败（成功时为 nil）。
func (s *Service) planImportProvisionCustomer(ctx context.Context, actor Principal, b contract.ImportBatch, item contract.ImportItem, plan *contract.ImportProvisionPlanItem) *importProvisionFailure {
	key := contract.ImportCRMProvisionKey{TenantID: b.TenantID, SourceSystem: b.SourceSystem, ExternalRecordID: item.ExternalRecordID}
	candidates, err := s.CRMProvisionCustomerClient.LookupCustomer(ctx, actor.IdentityID,
		importProvisionIdempotencyKey("lookup", key), plan.CustomerName)
	if err != nil {
		// 依赖故障时无法给出确定结论：按「待开立」展示，真实原因由 Issue 承载。
		plan.CustomerState = contract.ImportProvisionCustomerToCreate
		return classifyImportProvisionCRMError(err)
	}
	switch len(candidates) {
	case 0:
		plan.CustomerState = contract.ImportProvisionCustomerToCreate
		if issue := importProvisionRequiredFieldIssue(item.Metadata, true); issue != nil {
			return newProvisionFailure(issue.Field, issue.Code, false, issue.Message, "CRM creation required field invalid")
		}
		return nil
	case 1:
		if !strings.EqualFold(strings.TrimSpace(candidates[0].Status), "ACTIVE") {
			plan.CustomerState = contract.ImportProvisionCustomerAmbiguous
			return newProvisionFailure("metadata.customer_name", CodeImportProvisionCustomerAmbiguous, false,
				"命中的 CRM 客户已停用，请人工核对后再导入", "preview: CRM customer is not ACTIVE")
		}
		plan.CustomerState = contract.ImportProvisionCustomerExists
		return nil
	default:
		plan.CustomerState = contract.ImportProvisionCustomerAmbiguous
		return newProvisionFailure("metadata.customer_name", CodeImportProvisionCustomerAmbiguous, false,
			"客户名称命中多个 CRM 客户，请人工消歧后再导入", "preview: multiple CRM customers matched")
	}
}

// resolveImportProvisionSales 把销售姓名解析成唯一平台身份。
// 参数：ctx 为请求，salesName 为清单声明姓名。
// 返回值：identity_id、预览计划用的解析状态、失败投影（成功时为 nil）。
func (s *Service) resolveImportProvisionSales(ctx context.Context, salesName string) (string, string, *importProvisionFailure) {
	name := strings.TrimSpace(salesName)
	if name == "" {
		return "", contract.ImportProvisionSalesUnresolved, newProvisionFailure("metadata.sales_name", CodeImportProvisionOwnerUnresolved, false,
			"清单未提供销售姓名，无法确定商机负责人", "sales name is empty")
	}
	if len([]rune(name)) > 128 {
		return "", contract.ImportProvisionSalesUnresolved, newProvisionFailure("metadata.sales_name", CodeImportProvisionOwnerUnresolved, false,
			"销售姓名超过 128 个字符，无法解析", "sales name exceeds 128 characters")
	}
	candidate, err := s.CRMProvisionOwnerDirectory.ResolveOwnerByName(ctx, name)
	switch {
	case err == nil:
		if identity := strings.TrimSpace(candidate.IdentityID); identity != "" {
			return identity, contract.ImportProvisionSalesResolved, nil
		}
		return "", contract.ImportProvisionSalesUnresolved, newProvisionFailure("metadata.sales_name", CodeImportProvisionOwnerUnresolved, true,
			"平台负责人目录返回了空身份，请稍后重试", "owner directory returned an empty identity")
	case errors.Is(err, ErrProvisionOwnerAmbiguous):
		return "", contract.ImportProvisionSalesAmbiguous, newProvisionFailure("metadata.sales_name", CodeImportProvisionOwnerUnresolved, false,
			"销售姓名命中多个平台身份，请先消歧后再导入", "owner directory matched multiple owners")
	case errors.Is(err, ErrProvisionOwnerNotFound):
		return "", contract.ImportProvisionSalesUnresolved, newProvisionFailure("metadata.sales_name", CodeImportProvisionOwnerUnresolved, false,
			"销售姓名在平台负责人目录中不存在，请核对后重新导入", "owner directory has no matching owner")
	default:
		return "", contract.ImportProvisionSalesUnresolved, newProvisionFailure("metadata.sales_name", CodeImportProvisionOwnerUnresolved, true,
			"平台负责人目录暂不可用，请稍后重试", "owner directory is unavailable")
	}
}

// classifyImportProvisionCRMError 把 CRM 客户端错误分类为逐项失败投影。
// 参数：err 为客户端返回的错误。
// 返回值：合同验真、依赖故障与业务拒绝分别映射；依赖恢复后允许从原批次续做。
func classifyImportProvisionCRMError(err error) *importProvisionFailure {
	if err == nil {
		return nil
	}
	var failure *importProvisionFailure
	if errors.As(err, &failure) {
		return failure
	}
	var coded interface{ ProvisionCode() string }
	if errors.As(err, &coded) {
		if code := strings.TrimSpace(coded.ProvisionCode()); code == crmProvisionContractNotVerified {
			return newProvisionFailure("metadata.opportunity_id", CodeImportProvisionContractNotVerified, false,
				"CRM 合同验真未通过：请先在 CRM 侧配置合同验真凭据后重试",
				"CRM sign rejected by contract verification: "+err.Error())
		}
	}
	if errors.Is(err, ErrCRMProvisionUnavailable) {
		return newProvisionFailure("metadata", CodeImportProvisionCRMUnavailable, true,
			"CRM 自动开立服务暂不可用，已保留处理进度；服务恢复后请从原批次恢复执行",
			err.Error())
	}
	return newProvisionFailure("metadata", CodeImportProvisionCRMRejected, false,
		"CRM 拒绝了本次开立，请核对客户与商机数据", err.Error())
}

// failImportProvision 把失败记成 BLOCKED 并返回可落 Issue 的失败错误。
// 参数：ctx 为请求，store 为台账，key/record 为 CAS 前置，failure 为失败投影。
// 返回值：始终返回 failure，供调度器置 BLOCKED + Issue（台账写入失败不吞掉原始原因）。
func (s *Service) failImportProvision(ctx context.Context, store ContractImportCRMProvisionStore, key contract.ImportCRMProvisionKey, record contract.ImportCRMProvision, failure *importProvisionFailure) error {
	if failure.Code == CodeImportCRMProvisionDisabled {
		return failure
	}
	// CAS 未命中说明并发者已推进/阻断：仍以本次失败阻断该行，避免静默继续。
	_, _ = store.RecordImportCRMProvisionError(ctx, key, record.State, record.Attempt, failure.Code, failure.Reason)
	return failure
}

// prepareProvisionContract 是历史开立通道的合同准备函数。
// 它与普通外部合同共用同一套字段/服务项/付款计划规则，唯一差异是不做 CRM 客户
// 引用回读：此刻客户还不存在，由编排先查重/创建再在建单前写入 crm_customer_id。
// 参数：ctx 为请求，actor 为执行身份，c 为清单声明的合同，external 必须为 true，
// permission 为业务动作权限。
// 返回值：规范化合同或校验/权限错误。
func (s *Service) prepareProvisionContract(ctx context.Context, actor Principal, c contract.Contract, external bool, permission string) (contract.Contract, error) {
	if !external {
		return c, ErrImportUnsupported
	}
	authorized, err := s.authorizeContractWrite(actor, c, permission)
	if err != nil {
		return authorized, err
	}
	shaped, err := normalizeContractShape(authorized, true)
	if err != nil {
		return shaped, err
	}
	return finalizeContractCommercials(shaped)
}

// prepareProvisionImportItem 用开立通道准备函数执行预检与执行期复算。
// 参数：ctx、actor、b、item 与 prepareImportItem 相同。
// 返回值：合同副本、静态摘要与错误；摘要与执行期一致，因此重跑可校验同一基线。
func (s *Service) prepareProvisionImportItem(ctx context.Context, actor Principal, b contract.ImportBatch, item contract.ImportItemState) (contract.Contract, string, error) {
	return s.prepareImportItemWith(ctx, actor, b, item, true)
}

// executeProvisionImportItem 按设计定稿 2 节顺序执行一条历史开立行。
// 参数：ctx 与 actor 为重新验证的执行身份，b/item 为冻结批次与单项。
// 返回值：完成或明确失败；失败原因已写入开立台账，并作为逐项 Issue 返回给调度器。
func (s *Service) executeProvisionImportItem(ctx context.Context, actor Principal, b contract.ImportBatch, item contract.ImportItemState) error {
	if !s.crmProvisionEnabled() {
		return newProvisionFailure("metadata", CodeImportCRMProvisionDisabled, false,
			"历史导入自动开立 CRM 的开关已关闭，该行无法执行", "CRM import provision is disabled")
	}
	if issue := importProvisionRequiredFieldIssue(item.Item.Metadata, false); issue != nil {
		return newProvisionFailure(issue.Field, issue.Code, false, issue.Message, "CRM creation required field invalid")
	}
	store, ok := s.Repo.(ContractImportCRMProvisionStore)
	if !ok {
		return ErrImportUnsupported
	}
	key := contract.ImportCRMProvisionKey{TenantID: b.TenantID, SourceSystem: b.SourceSystem, ExternalRecordID: item.Item.ExternalRecordID}
	if err := key.Validate(); err != nil {
		return ErrValidation
	}
	record, err := store.UpsertStartImportCRMProvision(ctx, key, b.ID)
	if err != nil {
		return err
	}
	if record.State == contract.ImportCRMProvisionCompleted {
		// 幂等终态：开立已完成，只补齐批次单项结果，不再调用任何 CRM 写接口。
		if item.ContractID == "" {
			return &ImportIdentityConflictError{ItemID: item.ID, Kind: ImportConflictKindLedgerMismatch}
		}
		if item.State == "COMPLETE" {
			return nil
		}
		// 开立台账已完成但单项没收口，说明上一次在建单之后中断（例如自动审批的
		// 事务回滚）。自动审批生效时必须在这里把确认重试到落地，否则恢复（resume）
		// 只补 COMPLETE、合同却永远停在 version=1 的导入草稿。仓储侧按已确认幂等，
		// 因此这里可以无条件重放。判定口径按批次已固化的策略（AUTO/MANUAL），
		// 旧数据才回退到 live 开关；读取失败必须上抛（逐项可重试依赖故障）。
		policy, policyErr := s.importAutoApprovePolicy(ctx, b)
		if policyErr != nil {
			return policyErr
		}
		if policy.Auto {
			if err := s.autoConfirmHistoricalImportItem(ctx, b, item.ID, policy); err != nil {
				return err
			}
		}
		return s.markImportItemComplete(ctx, b, item)
	}
	if record.State == contract.ImportCRMProvisionBlocked {
		// BLOCKED 是「上一次尝试」的终态；同键新尝试先复位为 PENDING，保留已取得的标识。
		if !contract.ImportCRMProvisionRetryAllowed(record.State, contract.ImportCRMProvisionPending) {
			return ErrImportUnsupported
		}
		if record, err = store.ResetImportCRMProvision(ctx, key, record.Attempt); err != nil {
			return err
		}
	}
	// 步骤 1 解析销售：客户创建与商机创建都需要 identity_id，只有这两步真正需要目录。
	ownerIdentityID, ownerResolved := "", false
	resolveOwner := func() (string, *importProvisionFailure) {
		if ownerResolved {
			return ownerIdentityID, nil
		}
		identity, _, failure := s.resolveImportProvisionSales(ctx, item.Item.Metadata.SalesName)
		if failure != nil {
			return "", failure
		}
		ownerIdentityID, ownerResolved = identity, true
		return ownerIdentityID, nil
	}
	// 步骤 2-3 客户查重与创建。
	// 重试复位后状态回到 PENDING，但已取得的 customer_id 必须保留并直接复用：
	// 这里按「状态 + 已取得的标识」决定是否真的调用 CRM，而不是只看状态。
	if record.State == contract.ImportCRMProvisionPending {
		customerID, customerCreated := record.CustomerID, record.CustomerCreated
		if customerID == 0 {
			identity, failure := resolveOwner()
			if failure != nil {
				return s.failImportProvision(ctx, store, key, record, failure)
			}
			customer, created, failure := s.lookupOrCreateImportProvisionCustomer(ctx, actor, key, item.Item.Metadata, identity)
			if failure != nil {
				return s.failImportProvision(ctx, store, key, record, failure)
			}
			customerID, customerCreated = customer.ID, created
		}
		record, err = store.AdvanceImportCRMProvision(ctx, key, contract.ImportCRMProvisionAdvance{
			From: contract.ImportCRMProvisionPending, To: contract.ImportCRMProvisionCustomerReady, ExpectedAttempt: record.Attempt,
			CustomerID: customerID, CustomerCreated: customerCreated})
		if err != nil {
			return err
		}
	}
	// 步骤 4 建合同（沿用既有导入建单路径，只是带上本通道取得的 crm_customer_id）。
	if record.State == contract.ImportCRMProvisionCustomerReady {
		if item.ContractID == "" {
			customerName, failure := s.importProvisionCustomerName(ctx, actor, key, item.Item.Metadata, record.CustomerID)
			if failure != nil {
				return s.failImportProvision(ctx, store, key, record, failure)
			}
			created, commitErr := s.commitImportContract(ctx, actor, b, item, s.prepareProvisionImportItem, func(c *contract.Contract) error {
				c.CRMCustomerID, c.CustomerName = record.CustomerID, customerName
				return validateContractTextLengths(*c)
			})
			if commitErr != nil {
				s.recordImportProvisionFailure(ctx, store, key, record, commitErr)
				return commitErr
			}
			// 合同 ID 只存在于本次提交的返回值里，必须回填到本条单项，
			// 否则后续商机编排会误判「台账称已建合同、批次却没有合同 ID」。
			item.ContractID = created
		}
		if record, err = store.AdvanceImportCRMProvision(ctx, key, contract.ImportCRMProvisionAdvance{
			From: contract.ImportCRMProvisionCustomerReady, To: contract.ImportCRMProvisionContractReady,
			ExpectedAttempt: record.Attempt}); err != nil {
			return err
		}
	}
	if record.State != contract.ImportCRMProvisionContractReady && record.State != contract.ImportCRMProvisionOpportunityReady {
		// 走到这里说明台账状态与批次进度自相矛盾（如台账称合同已建但批次没有合同 ID），
		// 按基线冲突阻断并等待人工核账，绝不猜测继续。
		return &ImportIdentityConflictError{ContractID: item.ContractID, ItemID: item.ID, Kind: ImportConflictKindLedgerMismatch}
	}
	// 步骤 5-8 在合同入库后执行：建商机 → 推进签约 → 回写商机 → 推进 COMPLETED。
	return s.finishImportItem(ctx, actor, b, item, item.ContractID, func(c contract.Contract) error {
		preparer, ok := s.Repo.(ContractImportNumberPreparer)
		if !ok {
			return ErrImportUnsupported
		}
		number, err := preparer.PrepareImportContractNumber(ctx, b.TenantID, b.ID, b.Revision, b.Epoch, item.ID, c.ID)
		if err != nil {
			return err
		}
		c.Number = number
		if err := s.completeProvisionOpportunity(ctx, actor, store, key, record, c, resolveOwner); err != nil {
			return err
		}
		// 自动审批是开立成功之后的追加步骤：判定口径取批次已固化的策略
		// （提交时 AUTO 才自动确认，MANUAL 保持等人工会签），旧数据才回退 live 开关。
		// 生效时由仓储在同一事务内按清单声明状态归档并生成待投递目标。
		policy, policyErr := s.importAutoApprovePolicy(ctx, b)
		if policyErr != nil {
			return policyErr
		}
		if policy.Auto {
			return s.autoConfirmHistoricalImportItem(ctx, b, item.ID, policy)
		}
		return nil
	})
}

// recordImportProvisionFailure 把非 CRM 步骤（建合同）的失败也落台账。
// 参数：ctx 为请求，store 为台账，key/record 为 CAS 前置，err 为原始失败。
// 返回值：无；错误码复用既有导入类别，批次侧 Issue 保持原样不被改写。
func (s *Service) recordImportProvisionFailure(ctx context.Context, store ContractImportCRMProvisionStore, key contract.ImportCRMProvisionKey, record contract.ImportCRMProvision, err error) {
	code := importErrorCode(err)
	if len(code) == 0 || len(code) > contract.ImportCRMProvisionMaxErrorCodeLength {
		code = CodeImportProvisionCRMRejected
	}
	_, _ = store.RecordImportCRMProvisionError(ctx, key, record.State, record.Attempt, code, err.Error())
}

// completeProvisionOpportunity 为已入库合同开立并推进 CRM 商机，然后回写合同并收口台账。
// 参数：ctx 与 actor 为执行身份，store/key/record 为台账前置，c 为刚入库/已存在的合同，
// resolveOwner 为销售身份解析（新建及同键重放商机时需要）。
// 返回值：完成或明确失败；失败已写入台账并作为逐项 Issue 返回。
func (s *Service) completeProvisionOpportunity(ctx context.Context, actor Principal, store ContractImportCRMProvisionStore, key contract.ImportCRMProvisionKey, record contract.ImportCRMProvision, c contract.Contract, resolveOwner func() (string, *importProvisionFailure)) error {
	if record.State == contract.ImportCRMProvisionOpportunityReady {
		return s.writeBackImportProvisionOpportunity(ctx, store, key, record, c, record.OpportunityID)
	}
	// 台账不另存商机版本；复用同一创建幂等键读取CRM当前版本，避免猜测乐观锁。
	opportunityID := record.OpportunityID
	identity, failure := resolveOwner()
	if failure != nil {
		return s.failImportProvision(ctx, store, key, record, failure)
	}
	opportunity, failure := s.createImportProvisionOpportunity(ctx, actor, key, c, identity)
	if failure != nil {
		return s.failImportProvision(ctx, store, key, record, failure)
	}
	if opportunityID != 0 && opportunityID != opportunity.ID {
		return ErrImportConflict
	}
	created := record.OpportunityCreated || opportunityID == 0
	opportunityID = opportunity.ID
	opportunityVersion := opportunity.Version
	// 推进「已签约 + CLOSED」：CRM 侧校验 contract_ref 与合同验真，幂等键保证重跑不重复。
	if _, failure := s.signImportProvisionOpportunity(ctx, actor, key, opportunityID, c.ID, opportunityVersion); failure != nil {
		return s.failImportProvision(ctx, store, key, record, failure)
	}
	updated, err := store.AdvanceImportCRMProvision(ctx, key, contract.ImportCRMProvisionAdvance{
		From: record.State, To: contract.ImportCRMProvisionOpportunityReady, ExpectedAttempt: record.Attempt,
		OpportunityID: opportunityID, OpportunityCreated: created})
	if err != nil {
		return err
	}
	return s.writeBackImportProvisionOpportunity(ctx, store, key, updated, c, opportunityID)
}

// writeBackImportProvisionOpportunity 把商机引用写回合同并推进台账到 COMPLETED。
// 参数：ctx 为请求，store/key/record 为台账前置，c 为已入库合同，opportunityID 为 CRM 商机。
// 返回值：完成或明确失败；失败已写入台账。
func (s *Service) writeBackImportProvisionOpportunity(ctx context.Context, store ContractImportCRMProvisionStore, key contract.ImportCRMProvisionKey, record contract.ImportCRMProvision, c contract.Contract, opportunityID uint64) error {
	writer, ok := s.Repo.(ContractImportOpportunityWriter)
	if !ok {
		return ErrImportUnsupported
	}
	if err := writer.UpdateImportContractOpportunity(ctx, c.TenantID, c.ID, opportunityID, strings.TrimSpace(c.Title), c.CreatedBy); err != nil {
		return s.failImportProvision(ctx, store, key, record, newProvisionFailure("metadata.opportunity_id",
			CodeImportProvisionCRMRejected, false, "商机已签约但无法回写合同，请联系运维核对合同与商机",
			"write back opportunity to contract: "+err.Error()))
	}
	_, err := store.AdvanceImportCRMProvision(ctx, key, contract.ImportCRMProvisionAdvance{
		From: record.State, To: contract.ImportCRMProvisionCompleted, ExpectedAttempt: record.Attempt})
	return err
}

// lookupOrCreateImportProvisionCustomer 按名称查重客户，0 命中时创建。
// 参数：ctx/actor 为执行身份，key 为开立幂等键，metadata 为清单声明，owner 为销售 identity_id。
// 返回值：客户投影、是否本次新建、失败投影。
func (s *Service) lookupOrCreateImportProvisionCustomer(ctx context.Context, actor Principal, key contract.ImportCRMProvisionKey, metadata contract.ImportMetadata, owner string) (CRMProvisionCustomer, bool, *importProvisionFailure) {
	name := strings.TrimSpace(metadata.CustomerName)
	if name == "" {
		return CRMProvisionCustomer{}, false, newProvisionFailure("metadata.customer_name", codeImportValidationFailed, false,
			"客户名称为空，无法开立 CRM 客户", "customer name is empty")
	}
	if len([]rune(name)) > 255 {
		return CRMProvisionCustomer{}, false, newProvisionFailure("metadata.customer_name", codeImportValidationFailed, false,
			"客户名称超过 255 个字符，无法开立 CRM 客户", "customer name exceeds 255 characters")
	}
	// 预览期已按字段阻断超长行；执行期再校验一次，避免绕过预检把 CRM 打成 422。
	if failure := importProvisionCustomerProfileFailure(metadata); failure != nil {
		return CRMProvisionCustomer{}, false, failure
	}
	candidates, err := s.CRMProvisionCustomerClient.LookupCustomer(ctx, actor.IdentityID, importProvisionIdempotencyKey("lookup", key), name)
	if err != nil {
		return CRMProvisionCustomer{}, false, classifyImportProvisionCRMError(err)
	}
	switch len(candidates) {
	case 0:
		if issue := importProvisionRequiredFieldIssue(metadata, true); issue != nil {
			return CRMProvisionCustomer{}, false, newProvisionFailure(issue.Field, issue.Code, false, issue.Message, "CRM creation required field invalid")
		}
		customerType, industry, region := metadata.ProvisionCustomerProfile()
		created, err := s.CRMProvisionCustomerClient.CreateCustomer(ctx, actor.IdentityID,
			importProvisionIdempotencyKey("customer", key), CRMProvisionCustomerInput{
				Name: name, CustomerType: customerType, Industry: industry, Region: region, OwnerIdentityID: owner,
				Contacts: []CRMProvisionCustomerContact{{Name: strings.TrimSpace(metadata.CustomerContact), Phone: strings.TrimSpace(metadata.CustomerPhone), IsRegistration: true}},
			})
		if err != nil {
			return CRMProvisionCustomer{}, false, classifyImportProvisionCRMError(err)
		}
		if created.ID == 0 {
			return CRMProvisionCustomer{}, false, newProvisionFailure("metadata.customer_name", CodeImportProvisionCRMRejected, true,
				"CRM 未返回新建客户的 ID，请稍后重试", "CRM returned a customer without id")
		}
		if strings.TrimSpace(created.Name) == "" {
			created.Name = name
		}
		return created, true, nil
	case 1:
		candidate := candidates[0]
		if !strings.EqualFold(strings.TrimSpace(candidate.Status), "ACTIVE") {
			return CRMProvisionCustomer{}, false, newProvisionFailure("metadata.customer_name", CodeImportProvisionCustomerAmbiguous, false,
				"命中的 CRM 客户已停用，请人工核对后再导入", "CRM customer is not ACTIVE")
		}
		if candidate.ID == 0 {
			return CRMProvisionCustomer{}, false, newProvisionFailure("metadata.customer_name", CodeImportProvisionCRMRejected, true,
				"CRM 返回的客户候选缺少 ID，请稍后重试", "CRM returned a customer candidate without id")
		}
		return CRMProvisionCustomer{ID: candidate.ID, Name: candidate.Name, Status: candidate.Status}, false, nil
	default:
		return CRMProvisionCustomer{}, false, newProvisionFailure("metadata.customer_name", CodeImportProvisionCustomerAmbiguous, false,
			"客户名称命中多个 CRM 客户，请人工消歧后再导入", "multiple CRM customers matched")
	}
}

// importProvisionCustomerName 在断点续做时重新取得已建客户的权威名称。
// 参数：ctx/actor 为执行身份，key 为开立幂等键，metadata 为清单声明，customerID 为已建客户。
// 返回值：客户名称；命中异常返回失败投影（按客户多命中处理，不猜名字）。
func (s *Service) importProvisionCustomerName(ctx context.Context, actor Principal, key contract.ImportCRMProvisionKey, metadata contract.ImportMetadata, customerID uint64) (string, *importProvisionFailure) {
	name := strings.TrimSpace(metadata.CustomerName)
	candidates, err := s.CRMProvisionCustomerClient.LookupCustomer(ctx, actor.IdentityID, importProvisionIdempotencyKey("lookup", key), name)
	if err != nil {
		return "", classifyImportProvisionCRMError(err)
	}
	for _, candidate := range candidates {
		if candidate.ID == customerID {
			if strings.TrimSpace(candidate.Name) == "" {
				return name, nil
			}
			return strings.TrimSpace(candidate.Name), nil
		}
	}
	if customerID != 0 && name != "" {
		// 查重响应可能尚未包含刚建的客户：沿用清单名称，避免把已建客户误判为缺失。
		return name, nil
	}
	return "", newProvisionFailure("metadata.customer_name", CodeImportProvisionCustomerAmbiguous, false,
		"无法确认已建开立客户，请人工核对后再重试", "ledger customer is not visible in CRM lookup")
}

// createImportProvisionOpportunity 为合同开立 CRM 商机。
// 参数：ctx/actor 为执行身份，key 为开立幂等键，c 为已入库合同，owner 为销售 identity_id。
// 返回值：商机投影或失败投影；金额取元十进制，签约日取合同开始日期（历史合同允许过去）。
func (s *Service) createImportProvisionOpportunity(ctx context.Context, actor Principal, key contract.ImportCRMProvisionKey, c contract.Contract, owner string) (CRMProvisionOpportunity, *importProvisionFailure) {
	opportunity, err := s.CRMProvisionOpportunityClient.CreateOpportunity(ctx, actor.IdentityID,
		importProvisionIdempotencyKey("opportunity", key), CRMProvisionOpportunityInput{
			CustomerID:         c.CRMCustomerID,
			OwnerIdentityID:    owner,
			Name:               strings.TrimSpace(c.Title),
			Amount:             importProvisionAmount(c.AmountMinor),
			ExpectedSignDate:   importProvisionSignDate(c.StartDate),
			RequirementSummary: strings.TrimSpace(c.Title),
		})
	if err != nil {
		return CRMProvisionOpportunity{}, classifyImportProvisionCRMError(err)
	}
	if opportunity.ID == 0 {
		return CRMProvisionOpportunity{}, newProvisionFailure("metadata.opportunity_id", CodeImportProvisionCRMRejected, true,
			"CRM 未返回新建商机的 ID，请稍后重试", "CRM returned an opportunity without id")
	}
	return opportunity, nil
}

// signImportProvisionOpportunity 把商机推进到「已签约 + CLOSED」。
// 参数：ctx/actor为执行身份，key为幂等键，opportunityID/contractRef为目标与合同ID，
//
//	expectedVersion为CRM返回的当前商机版本，用于拒绝并发变更。
//
// 返回值：推进后的商机或失败投影；合同验真未通过映射为可操作提示。
func (s *Service) signImportProvisionOpportunity(ctx context.Context, actor Principal, key contract.ImportCRMProvisionKey, opportunityID uint64, contractRef string, expectedVersion uint64) (CRMProvisionOpportunity, *importProvisionFailure) {
	opportunity, err := s.CRMProvisionOpportunityClient.SignOpportunity(ctx, actor.IdentityID,
		importProvisionIdempotencyKey("sign", key), opportunityID, contractRef, expectedVersion)
	if err != nil {
		return CRMProvisionOpportunity{}, classifyImportProvisionCRMError(err)
	}
	return opportunity, nil
}
