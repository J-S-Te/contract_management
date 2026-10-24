package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/oklog/ulid/v2"
)

func seedSigningLedgerContract(t *testing.T, repository *Repository, tenant, title string, endDate *time.Time) contract.Contract {
	t.Helper()
	item := contract.Contract{
		ID: ulid.Make().String(), TenantID: tenant, Title: title, Type: "直签",
		ServiceType: "软件测试", OwnerUserID: "owner-1", OwnerIdentityID: "owner-1",
		AmountMinor: 100000, Currency: "CNY", Content: "body", ContentHash: draftE2EHash("body"),
		Status: contract.StatusActive, Version: 1, EndDate: endDate,
	}
	if err := repository.CreateContract(context.Background(), item, "owner-1"); err != nil {
		t.Fatalf("create contract %s: %v", title, err)
	}
	return item
}

// 签署台账必须在服务端筛选/分页：未完成优先、状态/关键字可查、统计覆盖全量而非当前页。
func TestSigningLedgerScopedPaginationAndStats(t *testing.T) {
	repository, tenant := draftE2ERepository(t)
	ctx := context.Background()
	future := time.Now().UTC().AddDate(1, 0, 0)
	past := time.Now().UTC().AddDate(-1, 0, 0)
	filter := contract.ScopeFilter{TenantID: tenant, AllowAll: true}

	completed := seedSigningLedgerContract(t, repository, tenant, "已完成合同", &future)
	doc, err := repository.SaveStampedDocument(ctx, tenant, contract.StampedDocumentUpload{
		ContractID: completed.ID, OriginalFilename: "completed.pdf", Document: []byte("completed"),
		UploadedAt: time.Now().UTC(), UploadedBy: "owner-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ConfirmSigning(ctx, tenant, completed.ID, "owner-1", contract.SigningConfirmation{
		SealVerified: true, SignatureVerified: true, SignedAt: time.Now().UTC(),
		DocumentVersion: doc.Version, ContentSHA256: doc.ContentSHA256,
	}); err != nil {
		t.Fatal(err)
	}

	review := seedSigningLedgerContract(t, repository, tenant, "待核验合同", &future)
	if _, err := repository.SaveStampedDocument(ctx, tenant, contract.StampedDocumentUpload{
		ContractID: review.ID, OriginalFilename: "review.pdf", Document: []byte("review"),
		UploadedAt: time.Now().UTC(), UploadedBy: "owner-1",
	}); err != nil {
		t.Fatal(err)
	}
	shipment := seedSigningLedgerContract(t, repository, tenant, "待寄出合同", &future)
	expired := seedSigningLedgerContract(t, repository, tenant, "已失效合同", &past)

	page, err := repository.ListSigningRecordsScoped(ctx, filter, contract.SigningRecordQuery{Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if page.Total != 4 || len(page.Items) != 2 || !page.HasMore {
		t.Fatalf("first page = total %d items %d has_more %v", page.Total, len(page.Items), page.HasMore)
	}
	for _, item := range page.Items {
		if item.Status == contract.SigningCompleted {
			t.Fatalf("completed record must not be prioritized before unfinished: %+v", page.Items)
		}
	}
	if page.Stats.Total != 4 || page.Stats.Completed != 1 || page.Stats.Processing != 2 || page.Stats.Expired != 1 || page.Stats.Monthly != 1 {
		t.Fatalf("stats = %+v", page.Stats)
	}
	second, err := repository.ListSigningRecordsScoped(ctx, filter, contract.SigningRecordQuery{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Items) != 2 || second.HasMore {
		t.Fatalf("second page = %+v", second)
	}
	if second.Items[0].Status != contract.SigningCompleted && second.Items[1].Status != contract.SigningCompleted {
		t.Fatalf("completed record should be last page: %+v", second.Items)
	}

	byKeyword, err := repository.ListSigningRecordsScoped(ctx, filter, contract.SigningRecordQuery{Keyword: "待核验"})
	if err != nil || byKeyword.Total != 1 || len(byKeyword.Items) != 1 || byKeyword.Items[0].Contract.ID != review.ID {
		t.Fatalf("keyword filter = %+v err=%v", byKeyword, err)
	}
	byStatus, err := repository.ListSigningRecordsScoped(ctx, filter, contract.SigningRecordQuery{Status: "pending_shipment"})
	if err != nil || byStatus.Total != 1 || len(byStatus.Items) != 1 || byStatus.Items[0].Contract.ID != shipment.ID {
		t.Fatalf("status filter = %+v err=%v", byStatus, err)
	}
	byExpired, err := repository.ListSigningRecordsScoped(ctx, filter, contract.SigningRecordQuery{Status: "expired"})
	if err != nil || byExpired.Total != 1 || len(byExpired.Items) != 1 || byExpired.Items[0].Contract.ID != expired.ID {
		t.Fatalf("expired filter = %+v err=%v", byExpired, err)
	}
}
