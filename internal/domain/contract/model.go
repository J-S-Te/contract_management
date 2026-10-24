package contract

import "time"

type Contract struct {
	ID                  string            `json:"id"`
	TenantID            string            `json:"tenant_id"`
	Number              string            `json:"contract_number"`
	NumberFormat        string            `json:"-"`
	Title               string            `json:"title"`
	Type                string            `json:"contract_type"`
	ServiceType         string            `json:"service_type"`
	OpportunityID       string            `json:"opportunity_id,omitempty"`
	OpportunityName     string            `json:"opportunity_name,omitempty"`
	CRMCustomerID       uint64            `json:"crm_customer_id,omitempty"`
	CustomerName        string            `json:"customer_name,omitempty"`
	CustomerAddress     string            `json:"customer_address,omitempty"`
	CustomerContact     string            `json:"customer_contact,omitempty"`
	CustomerPhone       string            `json:"customer_phone,omitempty"`
	OwnerIdentityID     string            `json:"owner_identity_id,omitempty"`
	OwnerOrgID          string            `json:"owner_org_id,omitempty"`
	ProjectID           string            `json:"project_id,omitempty"`
	Systems             []SystemInfo      `json:"systems,omitempty"`
	ServiceItems        []ServiceItem     `json:"service_items,omitempty"`
	CustomerCreditLevel string            `json:"customer_credit_level,omitempty"`
	OwnerUserID         string            `json:"owner_user_id"`
	OwnerDisplayName    string            `json:"owner_display_name"`
	AmountMinor         int64             `json:"amount_minor"`
	Currency            string            `json:"currency"`
	PaymentTerms        []PaymentTerm     `json:"payment_terms"`
	Content             string            `json:"content"`
	TemplateID          string            `json:"template_id,omitempty"`
	TemplateValues      map[string]string `json:"template_values,omitempty"`
	Document            []byte            `json:"-"`
	SourceFileID        string            `json:"source_file_id,omitempty"`
	SourceFileStatus    string            `json:"source_file_status,omitempty"`
	Status              Status            `json:"status"`
	Version             uint64            `json:"version"`
	StartDate           *time.Time        `json:"start_date,omitempty"`
	EndDate             *time.Time        `json:"end_date,omitempty"`
	ContentHash         string            `json:"content_hash"`
	CreatedAt           time.Time         `json:"created_at"`
	CreatedBy           string            `json:"created_by"`
	CanEditDraft        bool              `json:"can_edit_draft"`
	UpdatedAt           time.Time         `json:"updated_at"`
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

type SystemInfo struct {
	Name  string `json:"name"`
	Level string `json:"level"`
}

type ServiceItem struct {
	SourceID    string       `json:"source_id,omitempty"`
	Name        string       `json:"name,omitempty"`
	ServiceType string       `json:"service_type"`
	Site        string       `json:"site,omitempty"`
	Batch       string       `json:"batch,omitempty"`
	Category    string       `json:"category,omitempty"`
	Requirement string       `json:"requirement,omitempty"`
	TestMode    string       `json:"test_mode,omitempty"`
	Systems     []SystemInfo `json:"systems,omitempty"`
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
