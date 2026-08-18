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
	Content             string            `json:"content"`
	TemplateID          string            `json:"template_id,omitempty"`
	TemplateValues      map[string]string `json:"template_values,omitempty"`
	Document            []byte            `json:"-"`
	Status              Status            `json:"status"`
	Version             uint64            `json:"version"`
	StartDate           *time.Time        `json:"start_date,omitempty"`
	EndDate             *time.Time        `json:"end_date,omitempty"`
	ContentHash         string            `json:"content_hash"`
	CreatedAt           time.Time         `json:"created_at"`
	UpdatedAt           time.Time         `json:"updated_at"`
}

// ScopeFilter is the row-level contract boundary calculated from the platform
// authorization context. An empty filter never means unrestricted access.
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
	OriginalFilename string    `json:"original_filename"`
	Document         []byte    `json:"-"`
	UploadedAt       time.Time `json:"uploaded_at"`
	UploadedBy       string    `json:"uploaded_by"`
}

type SigningStatus string

const (
	SigningPendingShipment SigningStatus = "pending_shipment"
	SigningInReturn        SigningStatus = "in_return"
	SigningPendingReview   SigningStatus = "pending_review"
	SigningCompleted       SigningStatus = "completed"
)

type SigningRecord struct {
	Contract             Contract      `json:"contract"`
	Method               string        `json:"method"`
	Status               SigningStatus `json:"status"`
	CourierNumber        string        `json:"courier_number,omitempty"`
	RecipientName        string        `json:"recipient_name,omitempty"`
	RecipientPhone       string        `json:"recipient_phone,omitempty"`
	RecipientAddress     string        `json:"recipient_address,omitempty"`
	MailedAt             *time.Time    `json:"mailed_at,omitempty"`
	CustomerReceivedAt   *time.Time    `json:"customer_received_at,omitempty"`
	ReturnedDocumentName string        `json:"returned_document_name,omitempty"`
	ReturnedAt           *time.Time    `json:"returned_at,omitempty"`
	SealVerified         bool          `json:"seal_verified"`
	SignatureVerified    bool          `json:"signature_verified"`
	SignedAt             *time.Time    `json:"signed_at,omitempty"`
	ConfirmedAt          *time.Time    `json:"confirmed_at,omitempty"`
	ReminderCount        uint          `json:"reminder_count"`
	LastRemindedAt       *time.Time    `json:"last_reminded_at,omitempty"`
	Version              uint64        `json:"version"`
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

type Dashboard struct {
	TenantID              string              `json:"tenant_id"`
	TotalAmountMinor      int64               `json:"total_amount_minor"`
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
