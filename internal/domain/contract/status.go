package contract

import (
	"errors"
	"fmt"
)

// Status 是 CON-002 定义的持久化合同生命周期状态。
type Status string

const (
	StatusDraft      Status = "draft"
	StatusPending    Status = "pending"
	StatusApproved   Status = "approved"
	StatusActive     Status = "active"
	StatusInProgress Status = "in_progress"
	StatusPendingPay Status = "pending_pay"
	StatusCompleted  Status = "completed"
	StatusTerminated Status = "terminated"
	StatusArchived   Status = "archived"
)

var (
	ErrInvalidStatus     = errors.New("invalid contract status")
	ErrInvalidTransition = errors.New("invalid contract status transition")
)

var transitions = map[Status]map[Status]struct{}{
	StatusDraft:      {StatusPending: {}},
	StatusPending:    {StatusDraft: {}, StatusApproved: {}},
	StatusApproved:   {StatusActive: {}},
	StatusActive:     {StatusInProgress: {}, StatusTerminated: {}, StatusArchived: {}},
	StatusInProgress: {StatusPendingPay: {}, StatusCompleted: {}, StatusTerminated: {}, StatusArchived: {}},
	StatusPendingPay: {StatusCompleted: {}, StatusTerminated: {}, StatusArchived: {}},
	StatusCompleted:  {StatusArchived: {}},
	StatusTerminated: {StatusArchived: {}},
	StatusArchived:   {},
}

func (s Status) Valid() bool {
	_, ok := transitions[s]
	return ok
}

func (s Status) Terminal() bool { return s == StatusArchived }

func (s Status) ApprovalPassed() bool {
	switch s {
	case StatusApproved, StatusActive, StatusInProgress, StatusPendingPay, StatusCompleted, StatusTerminated, StatusArchived:
		return true
	default:
		return false
	}
}

func ApprovalPassedStatuses() []Status {
	return []Status{StatusApproved, StatusActive, StatusInProgress, StatusPendingPay, StatusCompleted, StatusTerminated, StatusArchived}
}

// RequiresApproval 遵循 APP-002。pending 由专用
// 合同审批工作流处理，因此不属于状态变更审批。
func (s Status) RequiresApproval() bool {
	switch s {
	case StatusInProgress, StatusPendingPay, StatusTerminated, StatusArchived:
		return true
	default:
		return false
	}
}

func CanTransition(from, to Status) bool {
	allowed, ok := transitions[from]
	if !ok {
		return false
	}
	_, ok = allowed[to]
	return ok
}

func ValidateTransition(from, to Status) error {
	if !from.Valid() || !to.Valid() {
		return fmt.Errorf("%w: %q -> %q", ErrInvalidStatus, from, to)
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	return nil
}
