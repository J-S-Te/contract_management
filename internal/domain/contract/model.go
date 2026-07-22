package contract

import "time"

type Contract struct {
	ID                  string     `json:"id"`
	TenantID            string     `json:"tenant_id"`
	Number              string     `json:"contract_number"`
	Title               string     `json:"title"`
	Type                string     `json:"contract_type"`
	ServiceType         string     `json:"service_type"`
	CustomerCreditLevel string     `json:"customer_credit_level,omitempty"`
	OwnerUserID         string     `json:"owner_user_id"`
	AmountMinor         int64      `json:"amount_minor"`
	Currency            string     `json:"currency"`
	Content             string     `json:"content"`
	Status              Status     `json:"status"`
	Version             uint64     `json:"version"`
	EndDate             *time.Time `json:"end_date,omitempty"`
	ContentHash         string     `json:"content_hash"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
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
