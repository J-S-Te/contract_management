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
	CustomerName        string            `json:"customer_name,omitempty"`
	CustomerAddress     string            `json:"customer_address,omitempty"`
	CustomerContact     string            `json:"customer_contact,omitempty"`
	CustomerPhone       string            `json:"customer_phone,omitempty"`
	Systems             []SystemInfo      `json:"systems,omitempty"`
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

type SystemInfo struct {
	Name  string `json:"name"`
	Level string `json:"level"`
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
	TotalAmountMinor      int64               `json:"total_amount_minor"`
	TotalContracts        int64               `json:"total_contracts"`
	ApprovalContracts     int64               `json:"approval_contracts"`
	ActiveContracts       int64               `json:"active_contracts"`
	ExpiredContracts      int64               `json:"expired_contracts"`
	Contracts             []DashboardContract `json:"contracts"`
	ContractDetailLimited bool                `json:"contract_detail_limited"`
}

type LifecycleEvent struct {
	ID             string
	TenantID       string
	ContractID     string
	FromStatus     Status
	ToStatus       Status
	ActorUserID    string
	Reason         string
	ApprovalID     string
	WorkflowID     string
	IdempotencyKey string
	OccurredAt     time.Time
}
