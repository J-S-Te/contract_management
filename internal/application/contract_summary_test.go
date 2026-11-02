package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/oklog/ulid/v2"
)

type contractSummarySourceStub struct {
	Repository
	candidate ContractSummaryCandidate
	err       error
	tenant    string
	ref       string
}

func (s *contractSummarySourceStub) FindContractSummaryCandidate(_ context.Context, tenantID, contractRef string) (ContractSummaryCandidate, error) {
	s.tenant, s.ref = tenantID, contractRef
	return s.candidate, s.err
}

func TestServiceContractSummaryRequiresMatchingCustomerAndOpportunity(t *testing.T) {
	contractID := ulid.Make().String()
	linkedAt := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	base := ContractSummaryCandidate{
		ContractID: contractID, ContractNumber: "HT-1", CRMCustomerID: 42,
		CRMOpportunityID: 7, LinkedAt: linkedAt, LinkedAtSource: ContractSummaryLinkSourceCRMReview,
		UpdatedAt: linkedAt.Add(time.Hour),
	}
	cases := []struct {
		name      string
		candidate ContractSummaryCandidate
		customer  uint64
		wantErr   error
	}{
		{name: "customer mismatch", candidate: base, customer: 99, wantErr: apperrors.ErrNotFound},
		{name: "unlinked customer", candidate: func() ContractSummaryCandidate {
			value := base
			value.CRMCustomerID = 0
			return value
		}(), customer: 42, wantErr: apperrors.ErrNotFound},
		{name: "missing contract number", candidate: func() ContractSummaryCandidate {
			value := base
			value.ContractNumber = ""
			return value
		}(), customer: 42, wantErr: apperrors.ErrNotFound},
		{name: "non canonical contract id", candidate: func() ContractSummaryCandidate {
			value := base
			value.ContractID = "not-a-ulid"
			return value
		}(), customer: 42, wantErr: ErrContractSummarySourceUnavailable},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			source := &contractSummarySourceStub{candidate: testCase.candidate}
			service := &Service{Repo: source}
			if _, err := service.ContractSummary(context.Background(), "tenant-1", "HT-1", testCase.customer); !errors.Is(err, testCase.wantErr) {
				t.Fatalf("err=%v want=%v", err, testCase.wantErr)
			}
		})
	}
}

// TestServiceContractSummaryAllowsCustomerMatchedContractWithoutOpportunity 固化
// “客户一致即可签约核验”的口径：合同可以先归属 CRM 客户、后关联商机，
// 摘要接口不能因 opportunity_id 为空把客户一致的合同误报为不存在。
func TestServiceContractSummaryAllowsCustomerMatchedContractWithoutOpportunity(t *testing.T) {
	contractID := ulid.Make().String()
	updatedAt := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	source := &contractSummarySourceStub{candidate: ContractSummaryCandidate{
		ContractID: contractID, ContractNumber: "HT-3", CRMCustomerID: 42,
		CRMOpportunityID: 0, UpdatedAt: updatedAt,
	}}
	service := &Service{Repo: source}
	summary, err := service.ContractSummary(context.Background(), "tenant-1", "HT-3", 42)
	if err != nil {
		t.Fatal(err)
	}
	if summary.CRMOpportunityID != 0 || !summary.LinkedAt.Equal(updatedAt) {
		t.Fatalf("summary=%+v want opportunity 0 and updated_at fallback", summary)
	}
	// 全无证据时必须显式标注近似来源与 approx 精度，不能冒充精确关联时间。
	if summary.LinkedAtSource != ContractSummaryLinkSourceApproxContractUpdated ||
		summary.LinkedAtPrecision != ContractSummaryPrecisionApprox ||
		!summary.FirstLinkedAt.Equal(updatedAt) {
		t.Fatalf("summary=%+v want APPROX_CONTRACT_UPDATED/approx/updated_at", summary)
	}
}

func TestServiceContractSummaryUsesReviewTimeAndFallsBackToUpdatedAt(t *testing.T) {
	contractID := ulid.Make().String()
	updatedAt := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	reviewedAt := updatedAt.Add(-time.Hour)
	source := &contractSummarySourceStub{candidate: ContractSummaryCandidate{
		ContractID: contractID, ContractNumber: "HT-2", CRMCustomerID: 42,
		CRMOpportunityID: 7, LinkedAt: reviewedAt, LinkedAtSource: ContractSummaryLinkSourceCRMReview,
		FirstLinkedAt: reviewedAt, UpdatedAt: updatedAt,
	}}
	service := &Service{Repo: source}
	summary, err := service.ContractSummary(context.Background(), "tenant-1", "HT-2", 42)
	if err != nil {
		t.Fatal(err)
	}
	if !summary.LinkedAt.Equal(reviewedAt) || !summary.FirstLinkedAt.Equal(reviewedAt) {
		t.Fatalf("linked_at=%s first=%s want review time %s", summary.LinkedAt, summary.FirstLinkedAt, reviewedAt)
	}
	if summary.LinkedAtSource != ContractSummaryLinkSourceCRMReview || summary.LinkedAtPrecision != ContractSummaryPrecisionExact {
		t.Fatalf("source=%s precision=%s want CRM_REVIEW/exact", summary.LinkedAtSource, summary.LinkedAtPrecision)
	}
	// 没有可证明的商机审核时间时退回合同更新时间，仍满足非零要求。
	source.candidate.LinkedAt = time.Time{}
	summary, err = service.ContractSummary(context.Background(), "tenant-1", "HT-2", 42)
	if err != nil {
		t.Fatal(err)
	}
	if !summary.LinkedAt.Equal(updatedAt) {
		t.Fatalf("linked_at=%s want updated_at %s", summary.LinkedAt, updatedAt)
	}
	if summary.LinkedAtSource != ContractSummaryLinkSourceApproxContractUpdated || summary.LinkedAtPrecision != ContractSummaryPrecisionApprox {
		t.Fatalf("source=%s precision=%s want APPROX_CONTRACT_UPDATED/approx", summary.LinkedAtSource, summary.LinkedAtPrecision)
	}
}

// TestServiceContractSummaryEvidenceSources 固定“证据来源 → 精度”的映射与
// linked_at（最新）/ first_linked_at（最早）的语义差异。
// 参数：t 为测试上下文。
// 返回值：无。
func TestServiceContractSummaryEvidenceSources(t *testing.T) {
	contractID := ulid.Make().String()
	updatedAt := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	latestAt := updatedAt.Add(-time.Hour)
	cases := []struct {
		name          string
		candidate     ContractSummaryCandidate
		wantLinkedAt  time.Time
		wantFirst     time.Time
		wantSource    string
		wantPrecision string
	}{
		{
			name: "audit log only",
			candidate: ContractSummaryCandidate{
				LinkedAt: latestAt, LinkedAtSource: ContractSummaryLinkSourceAuditLog, FirstLinkedAt: latestAt,
			},
			wantLinkedAt: latestAt, wantFirst: latestAt,
			wantSource: ContractSummaryLinkSourceAuditLog, wantPrecision: ContractSummaryPrecisionExact,
		},
		{
			name: "intake created only",
			candidate: ContractSummaryCandidate{
				LinkedAt: latestAt, LinkedAtSource: ContractSummaryLinkSourceIntakeCreated, FirstLinkedAt: latestAt,
			},
			wantLinkedAt: latestAt, wantFirst: latestAt,
			wantSource: ContractSummaryLinkSourceIntakeCreated, wantPrecision: ContractSummaryPrecisionApprox,
		},
		{
			name: "no evidence falls back to updated_at",
			candidate: ContractSummaryCandidate{
				UpdatedAt: updatedAt,
			},
			wantLinkedAt: updatedAt, wantFirst: updatedAt,
			wantSource: ContractSummaryLinkSourceApproxContractUpdated, wantPrecision: ContractSummaryPrecisionApprox,
		},
		{
			name: "multiple intakes keep latest linked_at and earliest first_linked_at",
			candidate: ContractSummaryCandidate{
				LinkedAt: latestAt, LinkedAtSource: ContractSummaryLinkSourceCRMReview,
				FirstLinkedAt: latestAt.Add(-48 * time.Hour),
			},
			wantLinkedAt: latestAt, wantFirst: latestAt.Add(-48 * time.Hour),
			wantSource: ContractSummaryLinkSourceCRMReview, wantPrecision: ContractSummaryPrecisionExact,
		},
		{
			name: "missing first_linked_at degrades to linked_at",
			candidate: ContractSummaryCandidate{
				LinkedAt: latestAt, LinkedAtSource: ContractSummaryLinkSourceReviewLog,
			},
			wantLinkedAt: latestAt, wantFirst: latestAt,
			wantSource: ContractSummaryLinkSourceReviewLog, wantPrecision: ContractSummaryPrecisionExact,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := testCase.candidate
			candidate.ContractID = contractID
			candidate.ContractNumber = "HT-EVIDENCE"
			candidate.CRMCustomerID = 42
			candidate.CRMOpportunityID = 7
			service := &Service{Repo: &contractSummarySourceStub{candidate: candidate}}
			summary, err := service.ContractSummary(context.Background(), "tenant-1", "HT-EVIDENCE", 42)
			if err != nil {
				t.Fatal(err)
			}
			if !summary.LinkedAt.Equal(testCase.wantLinkedAt) || !summary.FirstLinkedAt.Equal(testCase.wantFirst) ||
				summary.LinkedAtSource != testCase.wantSource || summary.LinkedAtPrecision != testCase.wantPrecision {
				t.Fatalf("summary=%+v want linked=%s first=%s source=%s precision=%s",
					summary, testCase.wantLinkedAt, testCase.wantFirst, testCase.wantSource, testCase.wantPrecision)
			}
		})
	}
}

// TestServiceContractSummaryRejectsUnknownEvidenceSource 固定“来源必须成对可解释”：
// 仓储给了时间却漏标/标错来源时失败关闭，不能对外发出枚举外的 linked_at_source。
// 参数：t 为测试上下文。
// 返回值：无。
func TestServiceContractSummaryRejectsUnknownEvidenceSource(t *testing.T) {
	source := &contractSummarySourceStub{candidate: ContractSummaryCandidate{
		ContractID: ulid.Make().String(), ContractNumber: "HT-4", CRMCustomerID: 42,
		CRMOpportunityID: 7, LinkedAt: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC),
	}}
	service := &Service{Repo: source}
	if _, err := service.ContractSummary(context.Background(), "tenant-1", "HT-4", 42); !errors.Is(err, ErrContractSummarySourceUnavailable) {
		t.Fatalf("err=%v want ErrContractSummarySourceUnavailable", err)
	}
}

func TestServiceContractSummaryRejectsInvalidInputAndMissingSource(t *testing.T) {
	service := &Service{Repo: &contractSummarySourceStub{}}
	if _, err := service.ContractSummary(context.Background(), "", "HT-1", 42); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty tenant err=%v", err)
	}
	if _, err := service.ContractSummary(context.Background(), "tenant-1", " ", 42); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty ref err=%v", err)
	}
	if _, err := service.ContractSummary(context.Background(), "tenant-1", "HT-1", 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("zero customer err=%v", err)
	}
	// Repo 未实现摘要查询能力时必须失败关闭，不能误报“合同不存在”。
	service = &Service{Repo: nil}
	if _, err := service.ContractSummary(context.Background(), "tenant-1", "HT-1", 42); !errors.Is(err, ErrContractSummarySourceUnavailable) {
		t.Fatalf("missing source err=%v", err)
	}
}
