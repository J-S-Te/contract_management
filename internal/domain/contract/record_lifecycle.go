package contract

import "time"

// RecordState 独立于合同业务状态，逻辑删除不改变审批或履约状态。
type RecordState string

const (
	RecordActive  RecordState = "active"
	RecordDeleted RecordState = "deleted"
)

type RecordBlocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RecordSummary 只提供台账与回收站摘要，不包含正文、文件或客户联系方式。
type RecordSummary struct {
	ID               string      `json:"id"`
	Number           string      `json:"contract_number"`
	Title            string      `json:"title"`
	Type             string      `json:"contract_type"`
	CustomerName     string      `json:"customer_name"`
	OwnerDisplayName string      `json:"owner_display_name"`
	AmountMinor      int64       `json:"amount_minor"`
	Currency         string      `json:"currency"`
	Status           Status      `json:"status"`
	Version          uint64      `json:"version"`
	RecordState      RecordState `json:"record_state"`
	DeletedAt        *time.Time  `json:"deleted_at,omitempty"`
	DeletedBy        string      `json:"deleted_by,omitempty"`
	DeleteReason     string      `json:"delete_reason,omitempty"`
	UpdatedAt        time.Time   `json:"updated_at"`
}

type RecordPreview struct {
	RecordSummary
	CanDelete  bool            `json:"can_delete"`
	CanRestore bool            `json:"can_restore"`
	Blockers   []RecordBlocker `json:"blockers"`
}

type RecordMutation struct {
	ID          string      `json:"id"`
	Version     uint64      `json:"version"`
	RecordState RecordState `json:"record_state"`
	Changed     bool        `json:"changed"`
}

type DeletedRecordPage struct {
	Items    []RecordSummary `json:"items"`
	Total    int64           `json:"total"`
	Page     int             `json:"page"`
	PageSize int             `json:"page_size"`
}
