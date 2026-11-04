package mysql

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// signingShipment 构造用于并发寄送验证的合法寄送信息。
// 参数：action 为区分测试寄送请求的标记。
// 返回值：包含快递单号、收件人、地址及寄出日期的输入。
func signingShipment(action string) contract.SigningShipment {
	return contract.SigningShipment{CourierNumber: "SF" + action, RecipientName: "张三", RecipientPhone: "13800138000", RecipientAddress: "北京市朝阳区", MailedAt: time.Now().UTC()}
}

// signingShipmentTestKey 设置寄送登记所需的手机号列加密密钥（测试专用固定密钥）。该密钥缺失时
// SaveSigningShipment 按生产语义失败关闭，故 E2E 必须先注入。
// 参数：t 为测试上下文，用于设置仅本用例生效的固定列加密密钥。
// 返回值：无。
func signingShipmentTestKey(t *testing.T) {
	t.Helper()
	t.Setenv(contract.SigningPhoneKeyEnv, base64.StdEncoding.EncodeToString(make([]byte, 32)))
}

// TestSaveSigningShipmentRejectedAfterStampedUploadE2E 覆盖 M-7 顺序回归：已上传盖章件（
// pending_review）后登记寄出必须 409，台账状态保持 pending_review，confirmed_* 不被改写，随后
// ConfirmSigning 仍能成功（不会被残留 in_return 永久 409）。
// 参数：t 为测试上下文。
// 返回值：无；实际行为偏离断言时用例失败。
func TestSaveSigningShipmentRejectedAfterStampedUploadE2E(t *testing.T) {
	signingShipmentTestKey(t)
	repository, tenant := draftE2ERepository(t)
	ctx := context.Background()
	future := time.Now().UTC().AddDate(1, 0, 0)
	item := seedSigningLedgerContract(t, repository, tenant, "寄送顺序守卫", &future)
	document, err := repository.SaveStampedDocument(ctx, tenant, contract.StampedDocumentUpload{
		ContractID: item.ID, OriginalFilename: "signed.pdf", Document: []byte("signed body"),
		UploadedAt: time.Now().UTC(), UploadedBy: "actor-1",
	})
	if err != nil {
		t.Fatalf("upload stamped document: %v", err)
	}
	before, err := repository.GetSigningRecord(ctx, tenant, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != contract.SigningPendingReview || before.ConfirmedDocumentVersion != 0 || before.ConfirmedAt != nil {
		t.Fatalf("precondition signing record = %+v", before)
	}

	err = repository.SaveSigningShipment(ctx, tenant, item.ID, "actor-1", signingShipment("SEQ"))
	if !errors.Is(err, apperrors.ErrStateConflict) {
		t.Fatalf("shipment after upload err=%v, want ErrStateConflict", err)
	}
	after, err := repository.GetSigningRecord(ctx, tenant, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != contract.SigningPendingReview || after.Version != before.Version {
		t.Fatalf("rejected shipment mutated ledger: status=%s version=%d, want pending_review/%d", after.Status, after.Version, before.Version)
	}
	if after.ConfirmedDocumentVersion != 0 || after.ConfirmedDocumentSHA256 != "" || after.ConfirmedAt != nil {
		t.Fatalf("rejected shipment rewrote confirmed fields: %+v", after)
	}

	// 完成签署仍可成功：被拒绝的寄送没有污染台账。
	if err := repository.ConfirmSigning(ctx, tenant, item.ID, "actor-1", contract.SigningConfirmation{
		SealVerified: true, SignatureVerified: true, SignedAt: time.Now().UTC(),
		DocumentVersion: document.Version, ContentSHA256: document.ContentSHA256,
	}); err != nil {
		t.Fatalf("ConfirmSigning after rejected shipment: %v", err)
	}
	completed, err := repository.GetSigningRecord(ctx, tenant, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != contract.SigningCompleted || completed.ConfirmedDocumentVersion != document.Version || completed.ConfirmedDocumentSHA256 != document.ContentSHA256 {
		t.Fatalf("completed signing record = %+v", completed)
	}
}

// TestConcurrentSigningShipmentVersusUploadE2E 覆盖 M-7 并发回归：寄送登记与盖章上传并发 10 轮，
// 终态必须是 pending_review，寄送侧只允许成功或 409。
// 参数：t 为测试上下文。
// 返回值：无；实际行为偏离断言时用例失败。
func TestConcurrentSigningShipmentVersusUploadE2E(t *testing.T) {
	signingShipmentTestKey(t)
	repository, tenant := draftE2ERepository(t)
	future := time.Now().UTC().AddDate(1, 0, 0)
	for round := 0; round < 10; round++ {
		item := seedSigningLedgerContract(t, repository, tenant, fmt.Sprintf("寄送上传并发-%d", round), &future)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		start := make(chan struct{})
		shipErr := make(chan error, 1)
		uploadErr := make(chan error, 1)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			shipErr <- repository.SaveSigningShipment(ctx, tenant, item.ID, "actor-1", signingShipment(fmt.Sprintf("R%d", round)))
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := repository.SaveStampedDocument(ctx, tenant, contract.StampedDocumentUpload{
				ContractID: item.ID, OriginalFilename: fmt.Sprintf("round-%d.pdf", round),
				Document: []byte(fmt.Sprintf("signed body %d", round)), UploadedAt: time.Now().UTC(), UploadedBy: "actor-2",
			})
			uploadErr <- err
		}()
		close(start)
		wg.Wait()
		close(shipErr)
		close(uploadErr)
		cancel()

		if err := <-uploadErr; err != nil {
			t.Fatalf("round %d: upload err=%v", round, err)
		}
		if err := <-shipErr; err != nil && !errors.Is(err, apperrors.ErrStateConflict) {
			t.Fatalf("round %d: shipment err=%v, want nil or ErrStateConflict", round, err)
		}
		final, err := repository.GetSigningRecord(context.Background(), tenant, item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Status != contract.SigningPendingReview {
			t.Fatalf("round %d: terminal signing status=%s, want pending_review", round, final.Status)
		}
		var stamped int64
		if err := repository.db.Model(&stampedDocumentRecord{}).
			Where("tenant_id = ? AND contract_id = ?", tenant, item.ID).Count(&stamped).Error; err != nil {
			t.Fatal(err)
		}
		if stamped != 1 {
			t.Fatalf("round %d: stamped versions=%d, want 1", round, stamped)
		}
	}
}

// TestConcurrentSigningShipmentVersusConfirmationE2E 覆盖 M-7 并发回归：寄送登记与签署确认并发 10
// 轮，终态必须 completed 且 confirmed_* 指向已核验版本，寄送侧不得把 completed 回退成 in_return。
// 参数：t 为测试上下文。
// 返回值：无；实际行为偏离断言时用例失败。
func TestConcurrentSigningShipmentVersusConfirmationE2E(t *testing.T) {
	signingShipmentTestKey(t)
	repository, tenant := draftE2ERepository(t)
	future := time.Now().UTC().AddDate(1, 0, 0)
	for round := 0; round < 10; round++ {
		item := seedSigningLedgerContract(t, repository, tenant, fmt.Sprintf("寄送确认并发-%d", round), &future)
		document, err := repository.SaveStampedDocument(context.Background(), tenant, contract.StampedDocumentUpload{
			ContractID: item.ID, OriginalFilename: fmt.Sprintf("confirm-%d.pdf", round),
			Document: []byte(fmt.Sprintf("signed body %d", round)), UploadedAt: time.Now().UTC(), UploadedBy: "actor-2",
		})
		if err != nil {
			t.Fatalf("round %d: upload: %v", round, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		start := make(chan struct{})
		shipErr := make(chan error, 1)
		confirmErr := make(chan error, 1)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			shipErr <- repository.SaveSigningShipment(ctx, tenant, item.ID, "actor-1", signingShipment(fmt.Sprintf("C%d", round)))
		}()
		go func() {
			defer wg.Done()
			<-start
			confirmErr <- repository.ConfirmSigning(ctx, tenant, item.ID, "actor-1", contract.SigningConfirmation{
				SealVerified: true, SignatureVerified: true, SignedAt: time.Now().UTC(),
				DocumentVersion: document.Version, ContentSHA256: document.ContentSHA256,
			})
		}()
		close(start)
		wg.Wait()
		close(shipErr)
		close(confirmErr)
		cancel()

		if err := <-confirmErr; err != nil {
			t.Fatalf("round %d: confirm err=%v", round, err)
		}
		if err := <-shipErr; !errors.Is(err, apperrors.ErrStateConflict) {
			t.Fatalf("round %d: shipment err=%v, want ErrStateConflict", round, err)
		}
		final, err := repository.GetSigningRecord(context.Background(), tenant, item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Status != contract.SigningCompleted || final.ConfirmedDocumentVersion != document.Version ||
			final.ConfirmedDocumentSHA256 != document.ContentSHA256 || final.ConfirmedAt == nil {
			t.Fatalf("round %d: terminal signing record = %+v", round, final)
		}
	}
}

// TestSaveSigningShipmentFirstShipmentAndMissingContractE2E 覆盖 M-7 首寄与缺行语义：台账无行时首
// 寄建立 in_return/version=1，合同缺失返回 NotFound。
// 参数：t 为测试上下文。
// 返回值：无；实际行为偏离断言时用例失败。
func TestSaveSigningShipmentFirstShipmentAndMissingContractE2E(t *testing.T) {
	signingShipmentTestKey(t)
	repository, tenant := draftE2ERepository(t)
	ctx := context.Background()
	future := time.Now().UTC().AddDate(1, 0, 0)
	item := seedSigningLedgerContract(t, repository, tenant, "首次寄出", &future)
	shipment := signingShipment("FIRST")
	if err := repository.SaveSigningShipment(ctx, tenant, item.ID, "actor-1", shipment); err != nil {
		t.Fatalf("first shipment: %v", err)
	}
	first, err := repository.GetSigningRecord(ctx, tenant, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != contract.SigningInReturn || first.Version != 1 || first.CourierNumber != shipment.CourierNumber {
		t.Fatalf("first shipment record = %+v", first)
	}
	if err := repository.SaveSigningShipment(ctx, tenant, item.ID, "actor-1", signingShipment("SECOND")); err != nil {
		t.Fatalf("second shipment: %v", err)
	}
	second, err := repository.GetSigningRecord(ctx, tenant, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != contract.SigningInReturn || second.Version != 2 || second.CourierNumber == first.CourierNumber {
		t.Fatalf("second shipment record = %+v", second)
	}
	if err := repository.SaveSigningShipment(ctx, tenant, "missing-contract", "actor-1", shipment); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("missing contract err=%v, want ErrNotFound", err)
	}
}
