package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

const (
	OpportunityIntakeAccepted      = "ACCEPTED"
	OpportunityIntakeLinkConfirmed = "LINK_CONFIRMED"
	OpportunityIntakeLinkException = "LINK_EXCEPTION"

	// maxOpportunityIntakeRefLength 与 con_opportunity_intake 的 VARCHAR(128) 列宽一致。
	maxOpportunityIntakeRefLength = 128
)

// ErrOpportunityLinkRejected 表示商机核对命中了来源/状态/客户归属门禁。
// HTTP 层据此返回 409 并要求走受控更正，而不是把改写当成成功。
var ErrOpportunityLinkRejected = errors.New("opportunity link rejected")

type OpportunityIntake struct {
	IntakeID            string     `json:"intake_id"`
	TenantID            string     `json:"tenant_id,omitempty"`
	EventID             string     `json:"event_id"`
	OpportunityID       uint64     `json:"opportunity_id"`
	EventVersion        uint64     `json:"event_version"`
	OpportunityNo       string     `json:"opportunity_no"`
	CustomerID          uint64     `json:"customer_id"`
	ContractRef         string     `json:"contract_ref"`
	ContractID          string     `json:"contract_id,omitempty"`
	ContractNumber      string     `json:"contract_number,omitempty"`
	ExpectedAmount      string     `json:"expected_amount"`
	OccurredAt          time.Time  `json:"occurred_at"`
	SourceRequestID     string     `json:"source_request_id,omitempty"`
	Status              string     `json:"status"`
	Version             uint64     `json:"version"`
	ReviewedBy          string     `json:"reviewed_by,omitempty"`
	ReviewerDisplayName string     `json:"reviewer_display_name,omitempty"`
	ReviewedAt          *time.Time `json:"reviewed_at,omitempty"`
	ReviewReason        string     `json:"review_reason,omitempty"`
	CreatedAt           time.Time  `json:"created_at,omitempty"`
}

type OpportunityIntakeReview struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	Version  uint64 `json:"version"`
}

type OpportunityIntakePage struct {
	Items      []OpportunityIntake `json:"items"`
	PageSize   int                 `json:"page_size"`
	NextCursor string              `json:"next_cursor,omitempty"`
	HasMore    bool                `json:"has_more"`
}

type OpportunityIntakeRepository interface {
	AcceptOpportunityIntake(context.Context, OpportunityIntake) (OpportunityIntake, error)
	ListOpportunityIntakes(context.Context, string, string, string, int) (OpportunityIntakePage, error)
	GetOpportunityIntake(context.Context, string, string) (OpportunityIntake, error)
	ReviewOpportunityIntake(context.Context, string, string, string, OpportunityIntakeReview, string, string, string) (OpportunityIntake, error)
}

func (s *Service) AcceptOpportunityIntake(ctx context.Context, intake OpportunityIntake) (OpportunityIntake, error) {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return OpportunityIntake{}, err
	}
	intake.ContractRef = strings.TrimSpace(intake.ContractRef)
	intake.OpportunityNo = strings.TrimSpace(intake.OpportunityNo)
	intake.SourceRequestID = strings.TrimSpace(intake.SourceRequestID)
	intake.ExpectedAmount = strings.TrimSpace(intake.ExpectedAmount)
	if strings.TrimSpace(intake.TenantID) == "" || strings.TrimSpace(intake.EventID) == "" || intake.OpportunityID == 0 || intake.EventVersion == 0 || intake.ContractRef == "" || intake.OccurredAt.IsZero() {
		return OpportunityIntake{}, ErrValidation
	}
	// 文本字段与 con_opportunity_intake 的 VARCHAR(128) 列宽对齐，超长在入库前拒绝。
	for _, field := range []struct{ name, value string }{
		{"contract_ref", intake.ContractRef},
		{"opportunity_no", intake.OpportunityNo},
		{"source_request_id", intake.SourceRequestID},
	} {
		if len([]rune(field.value)) > maxOpportunityIntakeRefLength {
			return OpportunityIntake{}, fmt.Errorf("%w: %s 不能超过 %d 个字符", ErrValidation, field.name, maxOpportunityIntakeRefLength)
		}
	}
	// 金额按合同域的十进制解析校验：拒绝符号、科学计数与三位以上小数。
	// 解析以 int64 分为单位，其上限 92233720368547758.07 严格小于 DECIMAL(20,2) 的
	// 999999999999999999.99，因此解析成功即保证写入不溢出、不被数据库静默舍入。
	if _, err := contract.PaymentAmountMinor(intake.ExpectedAmount); err != nil {
		return OpportunityIntake{}, fmt.Errorf("%w: expected_amount 必须是范围合法的两位小数金额", ErrValidation)
	}
	repo, ok := s.Repo.(OpportunityIntakeRepository)
	if !ok {
		return OpportunityIntake{}, ErrValidation
	}
	return repo.AcceptOpportunityIntake(ctx, intake)
}

func (s *Service) ListOpportunityIntakes(ctx context.Context, actor Principal, status, cursor string, limit int) (OpportunityIntakePage, error) {
	if !actor.Has("opportunity_intake.read") {
		return OpportunityIntakePage{}, ErrForbidden
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	repo, ok := s.Repo.(OpportunityIntakeRepository)
	if !ok {
		return OpportunityIntakePage{}, ErrValidation
	}
	return repo.ListOpportunityIntakes(ctx, actor.TenantID, status, cursor, limit)
}

func (s *Service) GetOpportunityIntake(ctx context.Context, actor Principal, id string) (OpportunityIntake, error) {
	if !actor.Has("opportunity_intake.read") {
		return OpportunityIntake{}, ErrForbidden
	}
	repo, ok := s.Repo.(OpportunityIntakeRepository)
	if !ok {
		return OpportunityIntake{}, ErrValidation
	}
	return repo.GetOpportunityIntake(ctx, actor.TenantID, id)
}

func (s *Service) ReviewOpportunityIntake(ctx context.Context, actor Principal, id string, review OpportunityIntakeReview, idempotencyKey string) (OpportunityIntake, error) {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return OpportunityIntake{}, err
	}
	if !actor.Has("opportunity_intake.process") {
		return OpportunityIntake{}, ErrForbidden
	}
	review.Decision = strings.TrimSpace(review.Decision)
	review.Reason = strings.TrimSpace(review.Reason)
	if (review.Decision != OpportunityIntakeLinkConfirmed && review.Decision != OpportunityIntakeLinkException) || review.Reason == "" || len(review.Reason) > 500 || review.Version == 0 || strings.TrimSpace(idempotencyKey) == "" || len(idempotencyKey) > 128 {
		return OpportunityIntake{}, ErrValidation
	}
	hash := sha256.Sum256([]byte(review.Decision + "\x00" + review.Reason + "\x00" + strconv.FormatUint(review.Version, 10)))
	repo, ok := s.Repo.(OpportunityIntakeRepository)
	if !ok {
		return OpportunityIntake{}, ErrValidation
	}
	result, err := repo.ReviewOpportunityIntake(ctx, actor.TenantID, id, actor.UserID, review, idempotencyKey, hex.EncodeToString(hash[:]), actor.DisplayName)
	if err != nil {
		return OpportunityIntake{}, err
	}
	return result, nil
}
