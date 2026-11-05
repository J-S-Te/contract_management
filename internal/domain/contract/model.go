package contract

import (
	"strings"
	"time"
)

type Contract struct {
	ID                     string            `json:"id"`
	TenantID               string            `json:"tenant_id"`
	Number                 string            `json:"contract_number"`
	NumberFormat           string            `json:"-"`
	Title                  string            `json:"title"`
	Type                   string            `json:"contract_type"`
	ServiceType            string            `json:"service_type"`
	OpportunityID          string            `json:"opportunity_id,omitempty"`
	OpportunityName        string            `json:"opportunity_name,omitempty"`
	CRMCustomerID          uint64            `json:"crm_customer_id,omitempty"`
	CounterpartySource     string            `json:"counterparty_source,omitempty"`
	LocalCounterpartyID    string            `json:"local_counterparty_id,omitempty"`
	CounterpartyVersion    uint64            `json:"counterparty_version,omitempty"`
	CategorySource         string            `json:"category_source,omitempty"`
	IntegrationPolicy      string            `json:"-"`
	CustomerName           string            `json:"customer_name,omitempty"`
	CustomerAddress        string            `json:"customer_address,omitempty"`
	CustomerContact        string            `json:"customer_contact,omitempty"`
	CustomerPhone          string            `json:"customer_phone,omitempty"`
	OwnerIdentityID        string            `json:"owner_identity_id,omitempty"`
	OwnerOrgID             string            `json:"owner_org_id,omitempty"`
	ProjectID              string            `json:"project_id,omitempty"`
	Systems                []SystemInfo      `json:"systems,omitempty"`
	ServiceItems           []ServiceItem     `json:"service_items,omitempty"`
	CustomerCreditLevel    string            `json:"customer_credit_level,omitempty"`
	OwnerUserID            string            `json:"owner_user_id"`
	OwnerDisplayName       string            `json:"owner_display_name"`
	AmountMinor            int64             `json:"amount_minor"`
	Currency               string            `json:"currency"`
	PaymentTerms           []PaymentTerm     `json:"payment_terms"`
	Content                string            `json:"content"`
	TemplateID             string            `json:"template_id,omitempty"`
	TemplateValues         map[string]string `json:"template_values,omitempty"`
	Document               []byte            `json:"-"`
	DocumentFormat         string            `json:"document_format,omitempty"`
	SourceMediaType        string            `json:"source_media_type,omitempty"`
	SourceFilename         string            `json:"source_filename,omitempty"`
	SourceSHA256           string            `json:"source_sha256,omitempty"`
	ExternalPaperNumber    string            `json:"external_paper_number,omitempty"`
	OriginKind             string            `json:"origin_kind,omitempty"`
	ImportMode             ImportMode        `json:"import_mode,omitempty"`
	OriginImportItemID     string            `json:"origin_import_item_id,omitempty"`
	HistoricalStatus       string            `json:"historical_status,omitempty"`
	HistoricalCustomerName string            `json:"historical_customer_name,omitempty"`
	HistoricalOwnerLabel   string            `json:"historical_owner_label,omitempty"`
	ImportPolicy           *ImportPolicy     `json:"import_policy,omitempty"`
	SourceFileID           string            `json:"source_file_id,omitempty"`
	SourceFileStatus       string            `json:"source_file_status,omitempty"`
	Status                 Status            `json:"status"`
	Version                uint64            `json:"version"`
	StartDate              *time.Time        `json:"start_date,omitempty"`
	EndDate                *time.Time        `json:"end_date,omitempty"`
	ContentHash            string            `json:"content_hash"`
	CreatedAt              time.Time         `json:"created_at"`
	CreatedBy              string            `json:"created_by"`
	CanEditDraft           bool              `json:"can_edit_draft"`
	UpdatedAt              time.Time         `json:"updated_at"`
}

// ScopeFilter 是根据平台授权上下文计算的合同
// 行级边界。空筛选绝不表示无限访问。
type ScopeFilter struct {
	TenantID        string
	IdentityID      string
	OrganizationIDs []string
	ProjectIDs      []string
	AllowAll        bool
	AllowSelf       bool
}

// ContractListQuery 是合同台账的服务端筛选与分页条件。
// Type 精确匹配 con_contract.contract_type：取值来自 CONTRACT_TYPE 字典（外部合同为
// 直签/三方），这里按等值 SQL 过滤而不做枚举校验——未知值自然返回空集，与 Status 口径一致。
// Offset 从 0 开始；Limit 越界由服务层钳制。
type ContractListQuery struct {
	Keyword string
	Status  string
	Type    string
	// Expired 是台账「已过期」派生徽标的服务端筛选：只接受 "true"/"false"
	// （忽略大小写与两端空白），其它取值（含空串、非法值）表示不筛选——与
	// Status/Type 的宽松口径一致，非法值不返回 422，而是退化成不过滤。
	Expired string
	Offset  int
	Limit   int
}

// ExpiredFilter 解析 Expired 筛选值，是「expired=true|false」参数契约的唯一解析点：
// 仓储 SQL 与服务层内存兼容路径都必须经由它，避免两处口径分叉。
// 返回值：filter 为是否只看已过期；ok 为是否启用该筛选（false 表示不追加任何谓词）。
func (q ContractListQuery) ExpiredFilter() (filter bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(q.Expired)) {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

// ContractPage 是合同台账的分页信封。只有显式带 page/page_size 的请求才返回该形状；
// 旧调用方（内置 web 页、脚本、跨系统同步）继续收到裸数组，见
// internal/transport/httpapi/router.go 的 listContracts。
type ContractPage struct {
	Items    []Contract `json:"items"`
	Total    int64      `json:"total"`
	Page     int        `json:"page"`
	PageSize int        `json:"page_size"`
}

// ClampContractListOffset 把请求 offset 收敛到 COUNT(*) 给出的真实数据边界：min(offset, total)。
// 参数：offset 为从 0 开始的请求偏移（负值按 0 处理），total 为同一组筛选谓词命中的行数。
// 返回值：可安全执行取页语句的 offset，永远不会超过 total。
//
// 动机：offset 直接进 SQL 的 OFFSET 时，深页请求（page=90000&page_size=200 →
// OFFSET 17,999,800）会让 MySQL 先扫完并丢弃过滤集里的所有行才返回空页；按 total
// 收敛后，最深的语句最多丢弃 total 行，不再随页码放大。
//
// 语义边界：total 为 0（过滤集为空）时原样返回 offset——没有可丢弃的行，OFFSET 不带来
// 额外扫描成本，保持请求值也避免改写空台账的信封。请求页超出末尾时取页语句绑定
// offset = total（空页），页码为 total/limit+1；total 恰为 limit 整数倍时该页码比
// 最后一页大 1，反映的是「offset 已经到数据末尾」而不是最后一页。
func ClampContractListOffset(offset int, total int64) int {
	if offset < 0 {
		return 0
	}
	if total <= 0 || int64(offset) <= total {
		return offset
	}
	return int(total)
}

type SystemInfo struct {
	Name  string `json:"name"`
	Level string `json:"level"`
}

type ServiceItem struct {
	CategoryID      string       `json:"category_id,omitempty"`
	CategoryVersion uint64       `json:"category_version,omitempty"`
	SourceID        string       `json:"source_id,omitempty"`
	Name            string       `json:"name,omitempty"`
	ServiceType     string       `json:"service_type"`
	Site            string       `json:"site,omitempty"`
	Batch           string       `json:"batch,omitempty"`
	Category        string       `json:"category,omitempty"`
	Requirement     string       `json:"requirement,omitempty"`
	TestMode        string       `json:"test_mode,omitempty"`
	Systems         []SystemInfo `json:"systems,omitempty"`
}

type StampedDocument struct {
	ContractID       string    `json:"contract_id"`
	Version          uint64    `json:"version"`
	OriginalFilename string    `json:"original_filename"`
	ContentSHA256    string    `json:"content_sha256"`
	Document         []byte    `json:"-"`
	UploadedAt       time.Time `json:"uploaded_at"`
	UploadedBy       string    `json:"uploaded_by"`
	PlatformFileID   string    `json:"-"`
	FileGatewayState string    `json:"file_gateway_state,omitempty"`
}

// StampedDocumentUpload 是一次盖章文件版本写入。ExpectedVersion 指向操作者
// 打开/核验的版本；0 表示兼容旧客户端，由仓储使用当前版本。
type StampedDocumentUpload struct {
	ContractID        string
	OriginalFilename  string
	Document          []byte
	UploadedAt        time.Time
	UploadedBy        string
	ExpectedVersion   uint64
	ReplacementReason string
}

type SigningStatus string

const (
	SigningPendingShipment SigningStatus = "pending_shipment"
	SigningInReturn        SigningStatus = "in_return"
	SigningPendingReview   SigningStatus = "pending_review"
	SigningCompleted       SigningStatus = "completed"
)

type SigningRecord struct {
	Contract                 Contract      `json:"contract"`
	Method                   string        `json:"method"`
	Status                   SigningStatus `json:"status"`
	CourierNumber            string        `json:"courier_number,omitempty"`
	RecipientName            string        `json:"recipient_name,omitempty"`
	RecipientPhone           string        `json:"recipient_phone,omitempty"`
	RecipientAddress         string        `json:"recipient_address,omitempty"`
	MailedAt                 *time.Time    `json:"mailed_at,omitempty"`
	CustomerReceivedAt       *time.Time    `json:"customer_received_at,omitempty"`
	ReturnedDocumentName     string        `json:"returned_document_name,omitempty"`
	ReturnedAt               *time.Time    `json:"returned_at,omitempty"`
	DocumentVersion          uint64        `json:"document_version"`
	DocumentSHA256           string        `json:"document_sha256,omitempty"`
	ConfirmedDocumentVersion uint64        `json:"confirmed_document_version,omitempty"`
	ConfirmedDocumentSHA256  string        `json:"confirmed_document_sha256,omitempty"`
	SealVerified             bool          `json:"seal_verified"`
	SignatureVerified        bool          `json:"signature_verified"`
	SignedAt                 *time.Time    `json:"signed_at,omitempty"`
	ConfirmedAt              *time.Time    `json:"confirmed_at,omitempty"`
	ReminderCount            uint          `json:"reminder_count"`
	LastRemindedAt           *time.Time    `json:"last_reminded_at,omitempty"`
	Version                  uint64        `json:"version"`
}

// SigningRecordQuery 是签署台账服务端筛选/分页条件；Status 使用页面状态
// （pending_shipment/in_return/pending_review/completed/expired），不是数据库任意值。
type SigningRecordQuery struct {
	Keyword string `json:"keyword,omitempty"`
	Status  string `json:"status,omitempty"`
	Method  string `json:"method,omitempty"`
	Offset  int    `json:"offset"`
	Limit   int    `json:"limit"`
}

type SigningStats struct {
	Total      int64 `json:"total"`
	Completed  int64 `json:"completed"`
	Processing int64 `json:"processing"`
	Monthly    int64 `json:"monthly"`
	Expired    int64 `json:"expired"`
}

type SigningRecordPage struct {
	Items   []SigningRecord `json:"items"`
	Total   int64           `json:"total"`
	Offset  int             `json:"offset"`
	Limit   int             `json:"limit"`
	HasMore bool            `json:"has_more"`
	Stats   SigningStats    `json:"stats"`
}

type SigningShipment struct {
	CourierNumber    string
	RecipientName    string
	RecipientPhone   string
	RecipientAddress string
	MailedAt         time.Time
}

type SigningConfirmation struct {
	SealVerified      bool
	SignatureVerified bool
	SignedAt          time.Time
	// DocumentVersion/ContentSHA256 是操作者实际打开并核验的文件版本；
	// 与当前最新版本不一致时确认必须失败（防止 A 看 v1、B 换 v2 后 A 确认 v2）。
	DocumentVersion uint64
	ContentSHA256   string
}

type DashboardContract struct {
	ID                  string     `json:"id"`
	Number              string     `json:"contract_number"`
	Title               string     `json:"title"`
	Type                string     `json:"contract_type"`
	ServiceType         string     `json:"service_type"`
	CustomerCreditLevel string     `json:"customer_credit_level,omitempty"`
	OwnerDisplayName    string     `json:"owner_display_name"`
	AmountMinor         int64      `json:"amount_minor"`
	Currency            string     `json:"currency"`
	Content             string     `json:"content"`
	Status              Status     `json:"status"`
	StartDate           *time.Time `json:"start_date,omitempty"`
	EndDate             *time.Time `json:"end_date,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	InApproval          bool       `json:"in_approval"`
	ActiveUnexpired     bool       `json:"active_unexpired"`
	Expired             bool       `json:"expired"`
}

// CurrencyTotal 是按币种隔离的合同金额统计；不同币种不得相加。
type CurrencyTotal struct {
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
	Contracts   int64  `json:"contracts"`
}

type Dashboard struct {
	TenantID string `json:"tenant_id"`
	// TotalAmountMinor 仅保留给旧消费方；跨币种展示必须使用 CurrencyTotals。
	TotalAmountMinor      int64               `json:"total_amount_minor"`
	CurrencyTotals        []CurrencyTotal     `json:"currency_totals"`
	TotalContracts        int64               `json:"total_contracts"`
	ApprovalContracts     int64               `json:"approval_contracts"`
	ActiveContracts       int64               `json:"active_contracts"`
	ExpiredContracts      int64               `json:"expired_contracts"`
	Contracts             []DashboardContract `json:"contracts"`
	ContractDetailLimited bool                `json:"contract_detail_limited"`
}

type LifecycleEvent struct {
	ID          string    `json:"id"`
	ContractID  string    `json:"contract_id"`
	FromStatus  Status    `json:"from_status"`
	ToStatus    Status    `json:"to_status"`
	ActorUserID string    `json:"actor_user_id"`
	Reason      string    `json:"reason"`
	OccurredAt  time.Time `json:"occurred_at"`
}
