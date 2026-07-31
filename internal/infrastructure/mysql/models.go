package mysql

import "time"

type contractRecord struct {
	ID                  string `gorm:"primaryKey"`
	TenantID            string
	ContractNumber      string
	Title               string
	ContractType        string
	ServiceType         string
	CustomerCreditLevel *string
	OwnerUserID         string
	OwnerUsername       string
	AmountMinor         int64
	Currency            string
	Content             string
	Status              string
	EndDate             *time.Time
	ContentHash         *string
	Version             uint64
	CreatedAt           time.Time
	CreatedBy           string
	UpdatedAt           time.Time
	UpdatedBy           string
}

func (contractRecord) TableName() string { return "con_contract" }

type lifecycleEventRecord struct {
	ID             string `gorm:"primaryKey"`
	TenantID       string
	ContractID     string
	FromStatus     string
	ToStatus       string
	ActorUserID    *string
	Reason         *string
	ApprovalID     *string
	WorkflowID     *string
	IdempotencyKey string
	OccurredAt     time.Time
}

func (lifecycleEventRecord) TableName() string { return "con_contract_lifecycle_event" }

type approvalRuleRecord struct {
	ID             string `gorm:"primaryKey"`
	TenantID       string
	Name           string
	Priority       int
	Enabled        bool
	ExpressionJSON []byte `gorm:"type:json"`
	NodesJSON      []byte `gorm:"type:json"`
	Version        uint64
	CreatedAt      time.Time
	CreatedBy      string
	UpdatedAt      time.Time
	UpdatedBy      string
}

func (approvalRuleRecord) TableName() string { return "con_approval_rule" }

type approvalInstanceRecord struct {
	ID                    string `gorm:"primaryKey"`
	TenantID              string
	ContractID            string
	Kind                  string
	Status                string
	ApplicantUserID       string
	ApplicantUsername     string
	FromStatus            string
	TargetStatus          string
	Reason                *string
	RuleID                *string
	RuleVersion           *uint64
	ContentHash           *string
	NodesJSON             []byte `gorm:"type:json"`
	RuntimeStateJSON      []byte `gorm:"type:json"`
	CurrentNodeIndex      int
	TemporalWorkflowID    string
	TemporalRunID         string
	ActiveStatusChangeKey *string
	CompletionApplied     bool
	CompletedAt           *time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

func (approvalInstanceRecord) TableName() string { return "con_approval_instance" }

type approvalTaskRecord struct {
	ApprovalID     string `gorm:"primaryKey"`
	NodeID         string `gorm:"primaryKey"`
	AssigneeUserID string `gorm:"primaryKey"`
	NodeName       string
	NodeIndex      int
	Status         string
	Approved       bool
	StartedAt      *time.Time
	CompletedAt    *time.Time
}

func (approvalTaskRecord) TableName() string { return "con_approval_task" }

type approvalActionRecord struct {
	ID            string `gorm:"primaryKey"`
	TenantID      string
	ApprovalID    string
	ContractID    string
	NodeID        *string
	CommandID     string
	Action        string
	ActorUserID   string
	ActorUsername string
	Comment       *string
	PayloadJSON   []byte `gorm:"type:json"`
	OccurredAt    time.Time
}

func (approvalActionRecord) TableName() string { return "con_approval_action" }

type notificationOutboxRecord struct {
	ID                string `gorm:"primaryKey"`
	TenantID          string
	RecipientKey      string
	RecipientUserID   *string
	RecipientRoleCode *string
	NotificationType  string
	Title             string
	Content           string
	ContractID        *string
	ApprovalID        *string
	DedupeKey         string
	DeliveryStatus    string
	Attempts          uint
	NextAttemptAt     time.Time
	DeliveredAt       *time.Time
	LastError         *string
	CreatedAt         time.Time
}

func (notificationOutboxRecord) TableName() string { return "con_notification_outbox" }

func stringPtr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func uintPtr(value uint64) *uint64 {
	if value == 0 {
		return nil
	}
	return &value
}

func timePtr(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}
