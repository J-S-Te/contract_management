package contract

import (
	"fmt"
	"time"
)

type ImportFact struct {
	SourceSystem   string `json:"source_system"`
	FactType       string `json:"fact_type"`
	ExternalFactID string `json:"external_fact_id"`
}
type ImportHistoryEvidence struct {
	DeclaredSignedDate     string        `json:"declared_signed_date"`
	HistoricalStatus       string        `json:"historical_status"`
	HistoricalCustomerName string        `json:"historical_customer_name"`
	HistoricalOwnerLabel   string        `json:"historical_owner_label"`
	TargetOwnerUserID      string        `json:"target_owner_user_id"`
	ExistingProjectID      string        `json:"existing_project_id,omitempty"`
	ExpectedProjectVersion uint64        `json:"expected_project_version,omitempty"`
	Relationship           string        `json:"relationship,omitempty"`
	OriginalContractRef    string        `json:"original_contract_ref,omitempty"`
	ExpectedFacts          *[]ImportFact `json:"expected_facts"`
	ReportRequiredChannels string        `json:"report_required_channels"`
}
type ImportConfirmation struct {
	WorkflowID    string                      `json:"workflow_id"`
	Revision      uint64                      `json:"revision"`
	Digest        string                      `json:"manifest_digest"`
	State         string                      `json:"state"`
	Roles         []string                    `json:"roles"`
	Assignees     map[string][]string         `json:"assignees"`
	ApprovedRoles []string                    `json:"approved_roles"`
	Commands      []ImportConfirmationCommand `json:"commands"`
	SubmittedAt   time.Time                   `json:"submitted_at"`
	ExpiresAt     time.Time                   `json:"expires_at"`
}
type ImportConfirmationCommand struct {
	Key     string    `json:"command_key"`
	ActorID string    `json:"actor_id"`
	Action  string    `json:"action"`
	Roles   []string  `json:"roles"`
	At      time.Time `json:"at"`
}
type ImportTargetCommandHistory struct {
	CommandID           string     `json:"command_id"`
	Generation          uint64     `json:"command_generation"`
	RequestDigest       string     `json:"request_digest"`
	RequestPayload      string     `json:"request_payload,omitempty"`
	HTTPStatus          int        `json:"http_status"`
	RecipientCode       string     `json:"recipient_code"`
	RejectedAt          time.Time  `json:"rejected_at"`
	RecoveryRequestedBy string     `json:"recovery_requested_by,omitempty"`
	RecoveryRequestedAt *time.Time `json:"recovery_requested_at,omitempty"`
}
type ImportTarget struct {
	CommandGeneration     uint64                       `json:"command_generation"`
	CommandHistory        []ImportTargetCommandHistory `json:"command_history,omitempty"`
	RecipientHTTPStatus   int                          `json:"recipient_http_status,omitempty"`
	RecipientCode         string                       `json:"recipient_code,omitempty"`
	RejectedAt            *time.Time                   `json:"rejected_at,omitempty"`
	RecoveryRequestedBy   string                       `json:"recovery_requested_by,omitempty"`
	Target                string                       `json:"target"`
	State                 string                       `json:"state"`
	Version               uint64                       `json:"version"`
	ReleaseVersion        uint64                       `json:"release_version"`
	Phase                 string                       `json:"phase"`
	CommandID             string                       `json:"command_id"`
	ReceiptID             string                       `json:"receipt_id,omitempty"`
	ErrorCode             string                       `json:"error_code,omitempty"`
	Attempts              uint                         `json:"attempts"`
	Epoch                 uint64                       `json:"execution_epoch"`
	LeaseUntil            time.Time                    `json:"lease_until"`
	NextAttemptAt         time.Time                    `json:"next_attempt_at"`
	OccurredAt            time.Time                    `json:"occurred_at"`
	RequestJSON           []byte                       `json:"-"`
	RequestPayload        string                       `json:"request_payload,omitempty"` // 持久任务冻结载荷；HTTP必须清除。
	ResponsibilityVersion uint64                       `json:"responsibility_version,omitempty"`
}

// ValidateHistoryEvidence 校验显式历史事实且拒绝尚未支持的财务恢复类型。
// 参数：item为已解析的白名单单项。
// 返回值：不完整、矛盾或不支持的证据返回校验错误。
func ValidateHistoryEvidence(item ImportItem) error {
	e, p := item.HistoryEvidence, item.Policy
	if item.Mode == ImportNormal {
		if e != nil {
			return ErrImportManifest
		}
		return nil
	}
	if e == nil || ValidateImportPolicy(item.Mode, p) != nil || e.TargetOwnerUserID == "" || len(e.TargetOwnerUserID) > 64 || len([]rune(e.HistoricalCustomerName)) > 255 || len([]rune(e.HistoricalOwnerLabel)) > 128 {
		return ErrImportManifest
	}
	if e.DeclaredSignedDate != "" {
		if _, err := time.Parse("2006-01-02", e.DeclaredSignedDate); err != nil {
			return ErrImportManifest
		}
	}
	if e.ReportRequiredChannels != "API_ONLY" && e.ReportRequiredChannels != "API_AND_SOURCE_FACTS" {
		return ErrImportManifest
	}
	switch item.Mode {
	case ImportArchiveOnly:
		if e.HistoricalStatus != "archived" {
			return ErrImportManifest
		}
	case ImportContinueLive:
		if e.HistoricalStatus != "active" && e.HistoricalStatus != "in_progress" && e.HistoricalStatus != "pending_pay" {
			return ErrImportManifest
		}
	case ImportHistoricalClosed:
		if e.HistoricalStatus != "completed" && e.HistoricalStatus != "terminated" && e.HistoricalStatus != "archived" {
			return ErrImportManifest
		}
	default:
		return ErrImportManifest
	}
	if len(e.ExistingProjectID) > 64 || len(e.OriginalContractRef) > 128 {
		return ErrImportManifest
	}
	if p.ProjectMode != "NONE" && item.Metadata.StartDate == nil {
		return ErrImportManifest
	}
	if p.ProjectMode == "LINK_EXISTING" {
		if e.ExistingProjectID == "" || e.ExpectedProjectVersion == 0 || e.OriginalContractRef == "" || (e.Relationship != "PRIMARY" && e.Relationship != "SUPPLEMENT") {
			return ErrImportManifest
		}
	} else if e.ExistingProjectID != "" || e.ExpectedProjectVersion != 0 || e.OriginalContractRef != "" {
		return ErrImportManifest
	}
	if p.ProjectMode == "CREATE_NEW" && e.Relationship != "PRIMARY" {
		return ErrImportManifest
	}
	if p.FinanceMode == "FULL_REPLAY" && (e.ExpectedFacts == nil || len(item.Metadata.PaymentTerms) == 0) {
		return ErrImportManifest
	}
	if e.ExpectedFacts != nil {
		if len(*e.ExpectedFacts) > 1000 {
			return ErrImportManifest
		}
		seen := map[string]bool{}
		for _, f := range *e.ExpectedFacts {
			key := fmt.Sprintf("%s/%s/%s", f.SourceSystem, f.FactType, f.ExternalFactID)
			if len(f.SourceSystem) > 64 || !importKey.MatchString(f.SourceSystem) || !importKey.MatchString(f.ExternalFactID) || seen[key] || (f.FactType != "RECEIPT" && f.FactType != "ALLOCATION") {
				return ErrImportManifest
			}
			seen[key] = true
		}
	}
	return nil
}
