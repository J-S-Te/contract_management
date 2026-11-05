package contract

import "time"

const CounterpartyLocal = "LOCAL"
const CounterpartyCRM = "CRM"
const CategoryLocal = "LOCAL"
const CategoryProject = "PROJECT"
const IntegrationNotApplicable = "NOT_APPLICABLE"

// LocalCounterparty 是合同自有交易对方，不具有CRM客户身份。
type LocalCounterparty struct {
	ID              string    `json:"id"`
	TenantID        string    `json:"-"`
	Name            string    `json:"name"`
	Enabled         bool      `json:"enabled"`
	Version         uint64    `json:"version"`
	OwnerIdentityID string    `json:"-"`
	OwnerOrgID      string    `json:"-"`
	CreatedBy       string    `json:"-"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// LocalCategory 是合同自有的版本化服务类别，不借用服务项SourceID。
type LocalCategory struct {
	ID              string    `json:"id"`
	TenantID        string    `json:"-"`
	Code            string    `json:"code"`
	Name            string    `json:"name"`
	Enabled         bool      `json:"enabled"`
	Version         uint64    `json:"version"`
	OwnerIdentityID string    `json:"-"`
	OwnerOrgID      string    `json:"-"`
	CreatedBy       string    `json:"-"`
	UpdatedAt       time.Time `json:"updated_at"`
}
