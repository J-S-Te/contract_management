package approval

import "time"

type Kind string
type Status string
type NodeStatus string
type Decision string
type CountersignMode string

const (
	KindContract     Kind = "contract_approval"
	KindStatusChange Kind = "status_change"

	StatusRunning   Status = "running"
	StatusApproved  Status = "approved"
	StatusRejected  Status = "rejected"
	StatusWithdrawn Status = "withdrawn"
	StatusExpired   Status = "expired"
	StatusFailed    Status = "failed"

	NodePending  NodeStatus = "pending"
	NodeActive   NodeStatus = "active"
	NodeApproved NodeStatus = "approved"
	NodeRejected NodeStatus = "rejected"
	NodeSkipped  NodeStatus = "skipped"

	DecisionApprove Decision = "approve"
	DecisionReject  Decision = "reject"

	CountersignAll CountersignMode = "all"
	CountersignAny CountersignMode = "any"
)

type Node struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	RoleCode    string          `json:"role_code"`
	AssigneeIDs []string        `json:"assignee_ids"`
	Countersign CountersignMode `json:"countersign"`
	Timeout     time.Duration   `json:"timeout"`
}

type Rule struct {
	ID         string     `json:"id"`
	TenantID   string     `json:"tenant_id"`
	Name       string     `json:"name"`
	Priority   int        `json:"priority"`
	Enabled    bool       `json:"enabled"`
	Expression Expression `json:"expression"`
	Nodes      []Node     `json:"nodes"`
	Version    uint64     `json:"version"`
}

type Meta struct {
	ID                   string `json:"id"`
	TenantID             string `json:"tenant_id"`
	ContractID           string `json:"contract_id"`
	ApplicantUserID      string `json:"applicant_user_id"`
	ApplicantDisplayName string `json:"applicant_display_name"`
	WorkflowID           string `json:"workflow_id"`
	RunID                string `json:"run_id"`
	Kind                 Kind   `json:"kind"`
	Status               Status `json:"status"`
	FromStatus           string `json:"from_status"`
	TargetStatus         string `json:"target_status"`
	Reason               string `json:"reason,omitempty"`
	RuleID               string `json:"rule_id,omitempty"`
	RuleVersion          uint64 `json:"rule_version,omitempty"`
}

type Task struct {
	ApprovalID     string     `json:"approval_id"`
	ContractID     string     `json:"contract_id"`
	NodeID         string     `json:"node_id"`
	NodeName       string     `json:"node_name"`
	AssigneeUserID string     `json:"assignee_user_id"`
	Kind           Kind       `json:"kind"`
	Status         NodeStatus `json:"status"`
	NodeIndex      int        `json:"node_index"`
	CreatedAt      time.Time  `json:"created_at"`
}

type Summary struct {
	ApprovalID           string    `json:"approval_id"`
	ContractID           string    `json:"contract_id"`
	ApplicantUserID      string    `json:"applicant_user_id"`
	ApplicantDisplayName string    `json:"applicant_display_name"`
	Kind                 Kind      `json:"kind"`
	Status               Status    `json:"status"`
	CurrentNodeIndex     int       `json:"current_node_index"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type Action struct {
	ID               string    `json:"id"`
	CommandID        string    `json:"command_id"`
	NodeID           string    `json:"node_id,omitempty"`
	Action           string    `json:"action"`
	ActorUserID      string    `json:"actor_user_id"`
	ActorDisplayName string    `json:"actor_display_name"`
	Comment          string    `json:"comment,omitempty"`
	OccurredAt       time.Time `json:"occurred_at"`
}
