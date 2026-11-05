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

// expiryStatuses 是「已过期」派生判定参与的状态集合，口径与前端
// frontend/src/modules/contract_management/utils/contractBusinessTime.js 的
// ACTIVE_CONTRACT_STATUSES 完全一致：生效、执行中、待收款。
// 草稿、审批中、已完成、已终止、已归档都不参与过期判定（归档是终态，
// 其 end_date 可能已过，但业务上不是「已过期」，看板亦单独计数）。
// 顺序固定：它既是 SQL IN 的绑定参数顺序，也是测试锁定的契约。
var expiryStatuses = []Status{StatusActive, StatusInProgress, StatusPendingPay}

// ExpiryStatuses 返回参与「已过期」判定的状态集合（返回副本，调用方不得改写内部切片）。
func ExpiryStatuses() []Status {
	return append([]Status(nil), expiryStatuses...)
}

// ExpiryTracked 报告 status 是否参与「已过期」判定。
// 内存兼容路径与 SQL 谓词共用该集合，避免两套口径。
func (s Status) ExpiryTracked() bool {
	for _, candidate := range expiryStatuses {
		if s == candidate {
			return true
		}
	}
	return false
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
