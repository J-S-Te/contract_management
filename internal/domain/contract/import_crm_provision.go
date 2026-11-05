package contract

import (
	"errors"
	"time"
)

// ImportCRMProvision 的六个状态取值来自设计定稿 3.1：
// PENDING → CUSTOMER_READY → CONTRACT_READY → OPPORTUNITY_READY → COMPLETED，
// 任一未完成状态失败时进入 BLOCKED。
const (
	ImportCRMProvisionPending          = "PENDING"
	ImportCRMProvisionCustomerReady    = "CUSTOMER_READY"
	ImportCRMProvisionContractReady    = "CONTRACT_READY"
	ImportCRMProvisionOpportunityReady = "OPPORTUNITY_READY"
	ImportCRMProvisionCompleted        = "COMPLETED"
	ImportCRMProvisionBlocked          = "BLOCKED"
)

// ImportCRMProvisionMaxErrorCodeLength 与 last_error_code 列宽 (VARCHAR(64)) 一致。
const ImportCRMProvisionMaxErrorCodeLength = 64

// ImportCRMProvisionMaxErrorMessageBytes 与 last_error_message 列宽 (VARCHAR(512)) 一致。
const ImportCRMProvisionMaxErrorMessageBytes = 512

// ImportItemStateNeedsProvision 是历史导入开立通道的预览行状态：
// 该行客户尚不存在（或已按名称命中），确认后由编排自动开立客户/商机；
// 它不阻断批次预检（不算 blocked），但必须由开立编排执行完成后才转 COMPLETE。
const ImportItemStateNeedsProvision = "NEEDS_PROVISION"

// 预览计划中的客户匹配状态（设计定稿 3.3）。
const (
	// ImportProvisionCustomerExists 表示按名称精确命中唯一启用客户。
	ImportProvisionCustomerExists = "EXISTS"
	// ImportProvisionCustomerToCreate 表示没有命中客户，需要新建。
	ImportProvisionCustomerToCreate = "TO_CREATE"
	// ImportProvisionCustomerAmbiguous 表示多命中或命中已停用客户，该行阻断。
	ImportProvisionCustomerAmbiguous = "AMBIGUOUS"
)

// 预览计划中的销售解析状态（设计定稿 3.3）。
const (
	// ImportProvisionSalesResolved 表示目录命中唯一身份。
	ImportProvisionSalesResolved = "RESOLVED"
	// ImportProvisionSalesUnresolved 表示目录 0 命中或依赖不可用，该行阻断。
	ImportProvisionSalesUnresolved = "UNRESOLVED"
	// ImportProvisionSalesAmbiguous 表示目录多命中，该行阻断。
	ImportProvisionSalesAmbiguous = "AMBIGUOUS"
)

// ImportProvisionPlanItem 是预览期给前端的开立计划行。
// 字段名与 JSON 标签是前端契约的一部分，逐字固定为设计定稿 3.3 的名称。
type ImportProvisionPlanItem struct {
	// CustomerName 是清单声明并经裁剪的客户名称。
	CustomerName string `json:"customer_name"`
	// CustomerState 取 EXISTS / TO_CREATE / AMBIGUOUS。
	CustomerState string `json:"customer_state"`
	// SalesName 是清单声明的销售姓名。
	SalesName string `json:"sales_name"`
	// SalesState 取 RESOLVED / UNRESOLVED / AMBIGUOUS。
	SalesState string `json:"sales_state"`
	// OpportunityAmount 是以元为单位的十进制字符串（amount_minor/100，两位小数）。
	OpportunityAmount string `json:"opportunity_amount"`
	// OpportunitySignDate 是商机预计签约日，取合同开始日期（YYYY-MM-DD）。
	OpportunitySignDate string `json:"opportunity_sign_date"`
}

var (
	// ErrImportCRMProvisionKey 表示幂等键缺失或超出列约束。
	ErrImportCRMProvisionKey = errors.New("invalid import CRM provision key")
	// ErrImportCRMProvisionState 表示状态值未知、跳步推进或从终态回退。
	ErrImportCRMProvisionState = errors.New("invalid import CRM provision state transition")
	// ErrImportCRMProvisionAttempt 表示调用方看到的 attempt 已过期（CAS 前置条件不成立）。
	ErrImportCRMProvisionAttempt = errors.New("import CRM provision attempt mismatch")
	// ErrImportCRMProvisionError 表示错误码/错误消息不符合台账列约束。
	ErrImportCRMProvisionError = errors.New("invalid import CRM provision error record")
)

// importCRMProvisionNext 是唯一允许的顺序推进边：不得跳步。
// 参数：无。
// 返回值：固定状态机映射（只读，调用方不得修改）。
var importCRMProvisionNext = map[string]string{
	ImportCRMProvisionPending:          ImportCRMProvisionCustomerReady,
	ImportCRMProvisionCustomerReady:    ImportCRMProvisionContractReady,
	ImportCRMProvisionContractReady:    ImportCRMProvisionOpportunityReady,
	ImportCRMProvisionOpportunityReady: ImportCRMProvisionCompleted,
}

// ImportCRMProvisionKnownState 判断状态是否属于六个合法取值。
// 参数：state 为待判定状态字符串。
// 返回值：合法返回 true；空串或未知值返回 false。
func ImportCRMProvisionKnownState(state string) bool {
	_, ok := importCRMProvisionNext[state]
	if ok {
		return true
	}
	return state == ImportCRMProvisionCompleted || state == ImportCRMProvisionBlocked
}

// ImportCRMProvisionTerminal 判断状态是否为不可再推进的终态。
// 参数：state 为待判定状态。
// 返回值：COMPLETED 与 BLOCKED 返回 true；其余返回 false。
func ImportCRMProvisionTerminal(state string) bool {
	return state == ImportCRMProvisionCompleted || state == ImportCRMProvisionBlocked
}

// ImportCRMProvisionNextState 返回顺序推进的下一个状态。
// 参数：state 为当前状态。
// 返回值：下一状态；终态或未知状态返回空串（调用方必须据此失败关闭）。
func ImportCRMProvisionNextState(state string) string {
	return importCRMProvisionNext[state]
}

// ImportCRMProvisionTransitionAllowed 判断一条状态推进是否被允许。
// 参数：from 为当前状态，to 为目标状态。
// 返回值：仅「严格下一步」或「任一未完成状态 → BLOCKED」返回 true。
func ImportCRMProvisionTransitionAllowed(from, to string) bool {
	if !ImportCRMProvisionKnownState(from) || !ImportCRMProvisionKnownState(to) {
		return false
	}
	// 终态不可回退、不可自转：COMPLETED/BLOCKED 没有出边。
	if ImportCRMProvisionTerminal(from) {
		return false
	}
	if to == ImportCRMProvisionBlocked {
		return true
	}
	return importCRMProvisionNext[from] == to
}

// ImportCRMProvisionRetryAllowed 判断一次失败后的重试复位是否被允许。
// 参数：from 为台账当前状态，to 为目标状态。
// 返回值：仅「BLOCKED → PENDING」返回 true；该边不与顺序推进矩阵混用，
// 因此不会让任一未完成状态跳步，也不会让 COMPLETED 回退。
func ImportCRMProvisionRetryAllowed(from, to string) bool {
	return from == ImportCRMProvisionBlocked && to == ImportCRMProvisionPending
}

// ImportCRMProvisionKey 是 design 2 节的幂等键：租户 + 来源系统 + 外部记录 ID。
type ImportCRMProvisionKey struct {
	TenantID         string `json:"-"`
	SourceSystem     string `json:"source_system"`
	ExternalRecordID string `json:"external_record_id"`
}

// Validate 校验幂等键与数据库主键列约束一致。
// 参数：无；使用接收者键值。
// 返回值：合法返回 nil；租户非 26 位、来源系统为空/超长/含非法字符、
// 外部记录 ID 为空/超长/含非法字符时返回 ErrImportCRMProvisionKey。
func (key ImportCRMProvisionKey) Validate() error {
	if len(key.TenantID) != 26 {
		return ErrImportCRMProvisionKey
	}
	if len(key.SourceSystem) == 0 || len(key.SourceSystem) > 64 || !importKey.MatchString(key.SourceSystem) {
		return ErrImportCRMProvisionKey
	}
	if len(key.ExternalRecordID) == 0 || len(key.ExternalRecordID) > 128 || !importKey.MatchString(key.ExternalRecordID) {
		return ErrImportCRMProvisionKey
	}
	return nil
}

// ImportCRMProvision 是 con_import_crm_provision 的领域投影：
// 只承载开立进度与已取得的 CRM 标识，不复制 CRM 客户/商机的业务字段。
type ImportCRMProvision struct {
	ImportCRMProvisionKey
	BatchID            string    `json:"batch_id,omitempty"`
	CustomerID         uint64    `json:"customer_id,omitempty"`
	CustomerCreated    bool      `json:"customer_created"`
	OpportunityID      uint64    `json:"opportunity_id,omitempty"`
	OpportunityCreated bool      `json:"opportunity_created"`
	State              string    `json:"state"`
	Attempt            int       `json:"attempt"`
	LastErrorCode      string    `json:"last_error_code,omitempty"`
	LastErrorMessage   string    `json:"last_error_message,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// Key 提取幂等键，便于仓储按主键读写。
// 参数：无；使用接收者身份。
// 返回值：不含进度字段的键。
func (value ImportCRMProvision) Key() ImportCRMProvisionKey {
	return value.ImportCRMProvisionKey
}

// Validate 校验持久化前的不变量：合法键、已知状态、非负 attempt。
// 参数：无；使用接收者记录。
// 返回值：不满足约束时返回对应领域错误。
func (value ImportCRMProvision) Validate() error {
	if err := value.Key().Validate(); err != nil {
		return err
	}
	if !ImportCRMProvisionKnownState(value.State) || value.Attempt < 0 {
		return ErrImportCRMProvisionState
	}
	if len(value.LastErrorCode) > ImportCRMProvisionMaxErrorCodeLength ||
		len(value.LastErrorMessage) > ImportCRMProvisionMaxErrorMessageBytes {
		return ErrImportCRMProvisionError
	}
	return nil
}

// ImportCRMProvisionAdvance 是一次带 CAS 前置条件的顺序推进请求。
// From/ExpectedAttempt 仅用于拼接 WHERE 条件，永远不会被写入台账。
type ImportCRMProvisionAdvance struct {
	// From 是调用方读取到的当前状态，必须与台账中的 state 一致。
	From string
	// To 是目标状态，必须是 From 的严格下一步或 BLOCKED。
	To string
	// ExpectedAttempt 是调用方读取到的 attempt，用于阻止并发覆盖。
	ExpectedAttempt int
	// CustomerID 非 0 时随推进写入 customer_id（CUSTOMER_READY 必填）。
	CustomerID uint64
	// CustomerCreated 表示客户是本通道新建（true）还是命中既有（false）；
	// 仅在 CustomerID 非 0 时随 customer_id 一起写入。
	CustomerCreated bool
	// OpportunityID 非 0 时随推进写入 opportunity_id（OPPORTUNITY_READY 必填）。
	OpportunityID uint64
	// OpportunityCreated 含义同 CustomerCreated，仅随 OpportunityID 写入。
	OpportunityCreated bool
}

// Validate 校验推进请求自洽且不跳步。
// 参数：无；使用接收者请求。
// 返回值：非法状态边、负 attempt、缺少必要 CRM 标识时返回领域错误。
func (advance ImportCRMProvisionAdvance) Validate() error {
	if advance.ExpectedAttempt < 0 {
		return ErrImportCRMProvisionState
	}
	if !ImportCRMProvisionTransitionAllowed(advance.From, advance.To) {
		return ErrImportCRMProvisionState
	}
	// 只有真正拿到 CRM 客户 ID 才允许宣告 CUSTOMER_READY，
	// 否则重跑会失去幂等锚点而重复建客户。
	if advance.To == ImportCRMProvisionCustomerReady && advance.CustomerID == 0 {
		return ErrImportCRMProvisionState
	}
	// 同理，OPPORTUNITY_READY 必须携带已创建的商机 ID。
	if advance.To == ImportCRMProvisionOpportunityReady && advance.OpportunityID == 0 {
		return ErrImportCRMProvisionState
	}
	return nil
}

// Advance 在内存中应用一次合法推进（纯函数，供编排与单测使用）。
// 参数：advance 为已带 CAS 条件的推进请求。
// 返回值：推进后的记录；状态或 attempt 与前置条件不符时返回领域错误。
func (value ImportCRMProvision) Advance(advance ImportCRMProvisionAdvance) (ImportCRMProvision, error) {
	if err := advance.Validate(); err != nil {
		return value, err
	}
	if value.State != advance.From {
		return value, ErrImportCRMProvisionState
	}
	if value.Attempt != advance.ExpectedAttempt {
		return value, ErrImportCRMProvisionAttempt
	}
	value.State = advance.To
	value.Attempt++
	if advance.CustomerID != 0 {
		value.CustomerID = advance.CustomerID
		value.CustomerCreated = advance.CustomerCreated
	}
	if advance.OpportunityID != 0 {
		value.OpportunityID = advance.OpportunityID
		value.OpportunityCreated = advance.OpportunityCreated
	}
	// 推进成功即视为本次尝试恢复，清空上一次失败痕迹。
	value.LastErrorCode, value.LastErrorMessage = "", ""
	return value, nil
}

// ValidateImportCRMProvisionErrorCode 校验失败落账的错误码是否可持久化。
// 参数：code 为 CRM/开立失败的稳定错误码。
// 返回值：错误码为空或超列宽返回 ErrImportCRMProvisionError；
// 可读原因不参与校验，过长时由 TruncateImportCRMProvisionMessage 截断。
func ValidateImportCRMProvisionErrorCode(code string) error {
	if code == "" || len(code) > ImportCRMProvisionMaxErrorCodeLength {
		return ErrImportCRMProvisionError
	}
	return nil
}

// TruncateImportCRMProvisionMessage 按 UTF-8 字符边界把消息截断到列宽以内。
// 参数：message 为原始错误消息。
// 返回值：长度不超过 512 字节的合法 UTF-8 子串。
func TruncateImportCRMProvisionMessage(message string) string {
	if len(message) <= ImportCRMProvisionMaxErrorMessageBytes {
		return message
	}
	limit := 0
	for index := range message {
		if index > ImportCRMProvisionMaxErrorMessageBytes {
			break
		}
		limit = index
	}
	return message[:limit]
}
