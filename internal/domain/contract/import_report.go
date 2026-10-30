package contract

import "time"

type ImportReportRow struct {
	ItemID               string    `json:"item_id"`
	SourceRevision       uint64    `json:"source_revision"`
	ReportReleaseVersion uint64    `json:"report_release_version"`
	RequiredChannels     string    `json:"required_channels"`
	ImportMode           string    `json:"import_mode"`
	ReportMode           string    `json:"report_mode"`
	ContractID           string    `json:"contract_id"`
	ContractVersion      uint64    `json:"contract_version"`
	Status               string    `json:"status"`
	StartDate            *string   `json:"start_date"`
	EndDate              *string   `json:"end_date"`
	OwnerUserID          string    `json:"owner_user_id"`
	OwnerOrgID           string    `json:"owner_org_id"`
	CRMCustomerID        *uint64   `json:"crm_customer_id"`
	OpportunityID        *string   `json:"opportunity_id"`
	AmountMinor          int64     `json:"amount_minor"`
	Currency             string    `json:"currency"`
	CreatedAt            time.Time `json:"created_at"`
}
type ImportReportSource struct {
	SchemaVersion    int               `json:"schema_version"`
	TenantID         string            `json:"tenant_id"`
	SourceSnapshotID string            `json:"source_snapshot_id"`
	Complete         bool              `json:"complete"`
	Items            []ImportReportRow `json:"items"`
}
