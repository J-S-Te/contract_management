package workflows

import (
	"time"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

const (
	TaskQueue                    = "contract-management"
	ContractApprovalWorkflowName = "contract-approval"
	StatusChangeWorkflowName     = "contract-status-change"
	ExpiredArchiveWorkflowName   = "expired-contract-archive"
	CommandSignalName            = "approval.command"
	StateQueryName               = "approval.state"
	ActivityStartApproval        = "StartApproval"
	ActivityRecordCommand        = "RecordCommand"
	ActivityCompleteApproval     = "CompleteApproval"
	ActivityCreateNotification   = "CreateNotification"
	ActivityArchiveExpired       = "ArchiveExpired"
)

type ExpiredArchiveInput struct {
	AsOfDate             string   `json:"as_of_date"`
	AdditionalRecipients []string `json:"additional_recipients"`
}

type ExpiredArchiveResult struct {
	Archived int `json:"archived"`
}

type CommandAction string

const (
	ActionApprove  CommandAction = "approve"
	ActionReject   CommandAction = "reject"
	ActionAddSign  CommandAction = "add_sign"
	ActionTransfer CommandAction = "transfer"
	ActionReturn   CommandAction = "return"
	ActionWithdraw CommandAction = "withdraw"
	ActionUrge     CommandAction = "urge"
	ActionComment  CommandAction = "comment"
)

type ContractApprovalInput struct {
	ApprovalID         string          `json:"approval_id"`
	TenantID           string          `json:"tenant_id"`
	ContractID         string          `json:"contract_id"`
	ContractVersion    uint64          `json:"contract_version"`
	ApplicantUserID    string          `json:"applicant_user_id"`
	ContentHash        string          `json:"content_hash"`
	RuleID             string          `json:"rule_id,omitempty"`
	RuleVersion        uint64          `json:"rule_version,omitempty"`
	Nodes              []approval.Node `json:"nodes"`
	DefaultNodeTimeout time.Duration   `json:"default_node_timeout"`
	ReminderInterval   time.Duration   `json:"reminder_interval"`
}

type StatusChangeInput struct {
	ApprovalID      string          `json:"approval_id"`
	TenantID        string          `json:"tenant_id"`
	ContractID      string          `json:"contract_id"`
	ContractVersion uint64          `json:"contract_version"`
	ApplicantUserID string          `json:"applicant_user_id"`
	FromStatus      contract.Status `json:"from_status"`
	TargetStatus    contract.Status `json:"target_status"`
	Reason          string          `json:"reason"`
	AdminUserIDs    []string        `json:"admin_user_ids"`
	Timeout         time.Duration   `json:"timeout"`
}

type ApprovalCommand struct {
	CommandID     string                   `json:"command_id"`
	Action        CommandAction            `json:"action"`
	ActorUserID   string                   `json:"actor_user_id"`
	Comment       string                   `json:"comment,omitempty"`
	TargetUserIDs []string                 `json:"target_user_ids,omitempty"`
	Countersign   approval.CountersignMode `json:"countersign,omitempty"`
	TargetNodeID  string                   `json:"target_node_id,omitempty"`
	OccurredAt    time.Time                `json:"occurred_at"`
}

type RuntimeNode struct {
	Node        approval.Node       `json:"node"`
	Status      approval.NodeStatus `json:"status"`
	ApprovedBy  map[string]bool     `json:"approved_by"`
	StartedAt   time.Time           `json:"started_at,omitempty"`
	CompletedAt time.Time           `json:"completed_at,omitempty"`
}

type ApprovalState struct {
	ApprovalID       string          `json:"approval_id"`
	Kind             approval.Kind   `json:"kind"`
	Status           approval.Status `json:"status"`
	ContractID       string          `json:"contract_id"`
	ApplicantUserID  string          `json:"applicant_user_id"`
	CurrentNodeIndex int             `json:"current_node_index"`
	Nodes            []RuntimeNode   `json:"nodes"`
	StartedAt        time.Time       `json:"started_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	CompletedAt      time.Time       `json:"completed_at,omitempty"`
	FailureReason    string          `json:"failure_reason,omitempty"`
}

type StartApprovalActivityInput struct {
	ApprovalID, TenantID, ContractID, ApplicantUserID string
	ExpectedVersion                                   uint64
	Kind                                              approval.Kind
	FromStatus, TargetStatus                          contract.Status
	Reason, RuleID                                    string
	RuleVersion                                       uint64
	ContentHash, WorkflowID, RunID                    string
	Nodes                                             []approval.Node
}

type RecordCommandActivityInput struct {
	ApprovalID, TenantID, ContractID, NodeID string
	Command                                  ApprovalCommand
	State                                    ApprovalState
}

type CompleteApprovalActivityInput struct {
	ApprovalID, TenantID, ContractID, ActorUserID string
	Status                                        approval.Status
	Reason, WorkflowID                            string
	TargetStatus                                  contract.Status
}

type NotifyActivityInput struct {
	TenantID, ApprovalID, ContractID, Type string
	Recipients                             []string
	Title, Content, DedupeKey              string
}
