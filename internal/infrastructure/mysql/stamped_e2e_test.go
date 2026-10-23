package mysql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/oklog/ulid/v2"
)

func stampedE2EContract(t *testing.T, repository *Repository, tenant string, status contract.Status) contract.Contract {
	t.Helper()
	item := contract.Contract{
		ID: ulid.Make().String(), TenantID: tenant, Title: "盖章版本链测试", Type: "直签",
		ServiceType: "软件测试", OwnerUserID: ulid.Make().String(), OwnerIdentityID: ulid.Make().String(),
		AmountMinor: 100000, Currency: "CNY", Content: "contract body", ContentHash: draftE2EHash("contract body"),
		Status: status, Version: 1,
	}
	if err := repository.CreateContract(context.Background(), item, item.OwnerUserID); err != nil {
		t.Fatal(err)
	}
	return item
}

func stampedDigest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func uploadStamped(t *testing.T, repository *Repository, tenant, contractID, name string, body []byte, expected uint64, reason string) contract.StampedDocument {
	t.Helper()
	saved, err := repository.SaveStampedDocument(context.Background(), tenant, contract.StampedDocumentUpload{
		ContractID: contractID, OriginalFilename: name, Document: body,
		UploadedAt: time.Now().UTC(), UploadedBy: "actor-1", ExpectedVersion: expected, ReplacementReason: reason,
	})
	if err != nil {
		t.Fatalf("upload %s: %v", name, err)
	}
	return saved
}

// 旧版本核验必须在文件被替换后冲突；已完成签署只能走带原因的正式更换。
func TestStampedDocumentVersionChainRejectsStaleConfirmation(t *testing.T) {
	repository, tenant := draftE2ERepository(t)
	item := stampedE2EContract(t, repository, tenant, contract.StatusActive)
	ctx := context.Background()
	v1Body := []byte("stamped version one")
	v1 := uploadStamped(t, repository, tenant, item.ID, "v1.pdf", v1Body, 0, "")
	if v1.Version != 1 || v1.ContentSHA256 != stampedDigest(v1Body) {
		t.Fatalf("v1 = %+v", v1)
	}
	// 一旦 v1 已存在，缺失或零版本必须冲突，不能
	// 静默转为下一版本。
	if _, err := repository.SaveStampedDocument(ctx, tenant, contract.StampedDocumentUpload{
		ContractID: item.ID, OriginalFilename: "missing-version.pdf", Document: []byte("missing version"), UploadedAt: time.Now().UTC(), UploadedBy: "actor-1",
	}); !errors.Is(err, apperrors.ErrVersionConflict) {
		t.Fatalf("missing expected version error = %v, want ErrVersionConflict", err)
	}
	v2Body := []byte("stamped version two")
	v2 := uploadStamped(t, repository, tenant, item.ID, "v2.pdf", v2Body, 1, "")
	if v2.Version != 2 {
		t.Fatalf("v2 version = %d", v2.Version)
	}
	stale := contract.SigningConfirmation{
		SealVerified: true, SignatureVerified: true, SignedAt: time.Now().UTC(),
		DocumentVersion: v1.Version, ContentSHA256: v1.ContentSHA256,
	}
	if err := repository.ConfirmSigning(ctx, tenant, item.ID, "actor-1", stale); !errors.Is(err, apperrors.ErrVersionConflict) {
		t.Fatalf("stale confirmation error = %v, want ErrVersionConflict", err)
	}
	current := contract.SigningConfirmation{
		SealVerified: true, SignatureVerified: true, SignedAt: time.Now().UTC(),
		DocumentVersion: v2.Version, ContentSHA256: v2.ContentSHA256,
	}
	if err := repository.ConfirmSigning(ctx, tenant, item.ID, "actor-1", current); err != nil {
		t.Fatalf("current confirmation: %v", err)
	}
	// 重复确认同一版本按幂等成功处理。
	if err := repository.ConfirmSigning(ctx, tenant, item.ID, "actor-1", current); err != nil {
		t.Fatalf("idempotent confirmation: %v", err)
	}
	if _, err := repository.SaveStampedDocument(ctx, tenant, contract.StampedDocumentUpload{
		ContractID: item.ID, OriginalFilename: "v3.pdf", Document: []byte("v3"), UploadedAt: time.Now().UTC(), UploadedBy: "actor-1",
	}); !errors.Is(err, apperrors.ErrStateConflict) {
		t.Fatalf("direct overwrite after completion error = %v, want ErrStateConflict", err)
	}
	replacement := uploadStamped(t, repository, tenant, item.ID, "v3.pdf", []byte("v3 replacement"), 2, "客户要求更换盖章件")
	if replacement.Version != 3 {
		t.Fatalf("replacement version = %d", replacement.Version)
	}
	if err := repository.db.WithContext(ctx).Model(&contractRecord{}).Where("tenant_id = ? AND id = ?", tenant, item.ID).Update("status", string(contract.StatusArchived)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := repository.SaveStampedDocument(ctx, tenant, contract.StampedDocumentUpload{
		ContractID: item.ID, OriginalFilename: "v4.pdf", Document: []byte("v4"), UploadedAt: time.Now().UTC(), UploadedBy: "actor-1", ReplacementReason: "归档后更换",
	}); !errors.Is(err, apperrors.ErrStateConflict) {
		t.Fatalf("archived overwrite error = %v, want ErrStateConflict", err)
	}
}

// 并发上传必须得到不同版本；网关回写按版本/哈希条件落行，最新台账与 BLOB 保持一致。
func TestConcurrentStampedUploadsKeepLedgerAndBlobConsistent(t *testing.T) {
	repository, tenant := draftE2ERepository(t)
	item := stampedE2EContract(t, repository, tenant, contract.StatusActive)
	uploadStamped(t, repository, tenant, item.ID, "base.pdf", []byte("base"), 0, "")

	type result struct {
		doc contract.StampedDocument
		err error
	}
	results := make(chan result, 2)
	var start sync.WaitGroup
	start.Add(1)
	for _, body := range [][]byte{[]byte("concurrent-a"), []byte("concurrent-b")} {
		go func(body []byte) {
			start.Wait()
			doc, err := repository.SaveStampedDocument(context.Background(), tenant, contract.StampedDocumentUpload{
				ContractID: item.ID, OriginalFilename: string(body) + ".pdf", Document: body,
				UploadedAt: time.Now().UTC(), UploadedBy: "actor-1", ExpectedVersion: 1,
			})
			results <- result{doc: doc, err: err}
		}(body)
	}
	start.Done()
	var accepted int
	var winner contract.StampedDocument
	for i := 0; i < 2; i++ {
		got := <-results
		if got.err == nil {
			accepted++
			winner = got.doc
		} else if !errors.Is(got.err, apperrors.ErrVersionConflict) {
			t.Fatalf("concurrent upload error = %v, want one conflict", got.err)
		}
	}
	if accepted != 1 || winner.Version != 2 {
		t.Fatalf("concurrent accepted=%d winner=%+v, want exactly v2", accepted, winner)
	}
	byVersion := map[uint64]contract.StampedDocument{1: {Version: 1}}
	byVersion[2] = winner
	byVersion[3] = uploadStamped(t, repository, tenant, item.ID, "next.pdf", []byte("next"), 2, "")
	// 逆序回写：先写 v3 的 READY，再写 v2；台账仍必须指向 v3。
	for _, version := range []uint64{3, 2} {
		doc := byVersion[version]
		if err := repository.MarkStampedDocumentGatewayResult(context.Background(), tenant, item.ID, doc.Version, doc.ContentSHA256, "READY", "file-"+doc.OriginalFilename); err != nil {
			t.Fatalf("mark v%d ready: %v", version, err)
		}
	}
	latest, err := repository.GetStampedDocument(context.Background(), tenant, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != 3 || latest.OriginalFilename != byVersion[3].OriginalFilename || latest.ContentSHA256 != byVersion[3].ContentSHA256 || string(latest.Document) != string(byVersion[3].Document) {
		t.Fatalf("latest ledger mixed versions: %+v", latest)
	}
	if latest.PlatformFileID != "file-"+byVersion[3].OriginalFilename {
		t.Fatalf("latest gateway pointer = %q", latest.PlatformFileID)
	}
	// 同一版本/哈希的重复 READY 回写必须幂等，不能因 MySQL 报告 0 行而误判冲突。
	if err := repository.MarkStampedDocumentGatewayResult(context.Background(), tenant, item.ID, byVersion[3].Version, byVersion[3].ContentSHA256, "READY", "file-"+byVersion[3].OriginalFilename); err != nil {
		t.Fatalf("idempotent gateway writeback: %v", err)
	}
	// 旧版本/错误哈希的回写必须被拒绝，不能悄悄污染其他版本。
	if err := repository.MarkStampedDocumentGatewayResult(context.Background(), tenant, item.ID, byVersion[2].Version, "wrong-hash", "READY", "file-wrong"); !errors.Is(err, apperrors.ErrVersionConflict) {
		t.Fatalf("wrong-hash writeback error = %v, want ErrVersionConflict", err)
	}
}
