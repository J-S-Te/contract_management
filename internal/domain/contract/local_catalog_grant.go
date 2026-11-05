package contract

import "time"

type LocalCatalogTarget struct {
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
	Name        string `json:"name"`
}

type LocalCatalogGrant struct {
	SubjectType string    `json:"subject_type"`
	SubjectID   string    `json:"subject_id"`
	SubjectName string    `json:"subject_name"`
	Enabled     bool      `json:"enabled"`
	Version     uint64    `json:"version"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type LocalCatalogGrantInput struct {
	SubjectType     string `json:"subject_type"`
	SubjectID       string `json:"subject_id"`
	Enabled         bool   `json:"enabled"`
	ExpectedVersion uint64 `json:"expected_version"`
}

// LocalCatalogVisibility 仅用于目录引用，不扩大合同数据范围。
type LocalCatalogVisibility struct {
	IdentityID      string
	OrganizationIDs []string
}
