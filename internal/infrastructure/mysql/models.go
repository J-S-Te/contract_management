package mysql

import (
	"time"

	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
)

type contractRecord struct {
	ID                   string `gorm:"primaryKey"`
	TenantID             string
	ContractNumber       *string
	ContractNumberFormat string
	Title                string
	ContractType         string
	ServiceType          string
	OpportunityID        *string
	OpportunityName      *string
	CRMCustomerID        *uint64
	CustomerName         *string
	CustomerAddress      *string
	CustomerContact      *string
	CustomerPhone        *string
	SystemsJSON          []byte `gorm:"type:json"`
	ServiceItemsJSON     []byte `gorm:"type:json"`
	CustomerCreditLevel  *string
	OwnerUserID          string
	OwnerDisplayName     string
	AmountMinor          int64
	Currency             string
	Content              string
	TemplateID           *string
	TemplateValuesJSON   []byte `gorm:"type:json"`
	RenderedDocument     []byte `gorm:"type:longblob"`
	Status               string
	StartDate            *time.Time
	EndDate              *time.Time
	ContentHash          *string
	Version              uint64
	CreatedAt            time.Time
	CreatedBy            string
	UpdatedAt            time.Time
	UpdatedBy            string
}

func (contractRecord) TableName() string { return "con_contract" }

type contractTemplateRecord struct {
	ID               string `gorm:"primaryKey"`
	TenantID         string
	Name             string
	OriginalFilename string
	NumberFormat     string
	FieldsJSON       []byte `gorm:"type:json"`
	Document         []byte `gorm:"type:longblob"`
	CreatedAt        time.Time
	CreatedBy        string
}

func (contractTemplateRecord) TableName() string { return "con_contract_template" }

func templateFromRecord(record contractTemplateRecord) contracttemplate.Template {
	return contracttemplate.Template{
		ID: record.ID, TenantID: record.TenantID, Name: record.Name,
		OriginalFilename: record.OriginalFilename, NumberFormat: record.NumberFormat, Content: record.Document,
		CreatedAt: record.CreatedAt, CreatedBy: record.CreatedBy,
	}
}

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

type stampedDocumentRecord struct {
	ContractID, TenantID, OriginalFilename, ContentSHA256 string
	Document                                              []byte `gorm:"type:longblob"`
	UploadedAt                                            time.Time
	UploadedBy                                            string
}

func (stampedDocumentRecord) TableName() string { return "con_contract_stamped_document" }

type signingRecord struct {
	ContractID, TenantID, Method, Status                string
	CourierNumber, RecipientName, RecipientAddress      *string
	MailedAt, CustomerReceivedAt, SignedAt, ConfirmedAt *time.Time
	SealVerified, SignatureVerified                     bool
	ReminderCount                                       uint
	LastRemindedAt                                      *time.Time
	Version                                             uint64
	UpdatedAt                                           time.Time
	UpdatedBy                                           string
}

func (signingRecord) TableName() string { return "con_contract_signing" }

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
	ApplicantDisplayName  string
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
	ID               string `gorm:"primaryKey"`
	TenantID         string
	ApprovalID       string
	ContractID       string
	NodeID           *string
	CommandID        string
	Action           string
	ActorUserID      string
	ActorDisplayName string
	Comment          *string
	PayloadJSON      []byte `gorm:"type:json"`
	OccurredAt       time.Time
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

type projectDeliveryOutboxRecord struct {
	ID              string `gorm:"primaryKey"`
	TenantID        string
	ContractID      string
	ContractVersion uint64
	PayloadJSON     []byte `gorm:"type:json"`
	DeliveryStatus  string
	Attempts        uint
	NextAttemptAt   time.Time
	LockedAt        *time.Time
	DeliveredAt     *time.Time
	LastError       *string
	CreatedAt       time.Time
}

func (projectDeliveryOutboxRecord) TableName() string { return "con_project_delivery_outbox" }

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
