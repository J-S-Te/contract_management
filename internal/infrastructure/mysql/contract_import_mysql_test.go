package mysql

import (
	"context"
	"errors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/oklog/ulid/v2"
	"sync"
	"testing"
	"time"
)

// TestImportCommitIdempotencyMySQL 验证真实并发、事务恢复及来源冲突。
// 参数：t为测试上下文；夹具只接受专用隔离测试库。
// 返回值：无；任何重复合同、过期代次写入或冲突未阻断均失败。
func TestImportCommitIdempotencyMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	actor := ulid.Make().String()
	t.Cleanup(func() {
		for _, table := range []string{"con_import_identity", "con_import_batch", "con_audit_outbox"} {
			if e := r.db.Exec("DELETE FROM "+table+" WHERE tenant_id=?", tenant).Error; e != nil {
				t.Error(e)
			}
		}
	})
	itemID, batchID := ulid.Make().String(), ulid.Make().String()
	b := contract.ImportBatch{ID: batchID, TenantID: tenant, ActorID: actor, Version: 1, Revision: 1, State: "QUEUED", SourceSystem: "qa", Items: []contract.ImportItemState{{ID: itemID, Digest: "baseline", State: "READY", Item: contract.ImportItem{ExternalRecordID: "one", Mode: contract.ImportNormal}}}}
	if e := r.CreateImportBatch(ctx, b); e != nil {
		t.Fatal(e)
	}
	b, found, e := r.ClaimImportBatch(ctx, time.Now().UTC())
	if e != nil || !found {
		t.Fatalf("claim found=%v error=%v", found, e)
	}
	draft := contract.Contract{TenantID: tenant, Title: "批次事务测试", Type: "直签", OwnerUserID: actor, OwnerIdentityID: actor, AmountMinor: 100, Currency: "CNY", Status: contract.StatusDraft, ImportMode: contract.ImportNormal, OriginImportItemID: itemID}
	var wg sync.WaitGroup
	results := make(chan string, 20)
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := draft
			c.ID = ulid.Make().String()
			id, e := r.CommitImportDraft(ctx, tenant, b.ID, b.Revision, b.Epoch, itemID, c, actor)
			results <- id
			errs <- e
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	only := ""
	for id := range results {
		if only != "" && only != id {
			t.Fatal("同键创建多个合同")
		}
		only = id
	}
	var count int64
	if e = r.db.Model(&contractRecord{}).Where("tenant_id=?", tenant).Count(&count).Error; e != nil || count != 1 {
		t.Fatalf("contract count=%d error=%v", count, e)
	}
	// 模拟响应丢失后的独立进程实例读取及旧worker围栏。
	fresh := NewRepository(r.db)
	stored, e := fresh.GetImportBatch(ctx, tenant, b.ID)
	if e != nil || stored.Items[0].ContractID != only {
		t.Fatal("事务提交结果不能恢复")
	}
	if _, e = fresh.CommitImportDraft(ctx, tenant, b.ID, b.Revision, b.Epoch+1, itemID, draft, actor); !errors.Is(e, application.ErrImportConflict) {
		t.Fatalf("旧执行围栏未拒绝: %v", e)
	}
	// 不同批次同来源但异内容不能另建单。
	other := b
	other.ID = ulid.Make().String()
	other.State = "QUEUED"
	other.Epoch = 0
	other.LeaseUntil = time.Time{}
	other.Items = []contract.ImportItemState{{ID: ulid.Make().String(), Digest: "changed", State: "READY", Item: contract.ImportItem{ExternalRecordID: "one"}}}
	if e = fresh.CreateImportBatch(ctx, other); e != nil {
		t.Fatal(e)
	}
	// 明确暂停第一批以便只认领第二批。
	if e = fresh.UpdateImportBatch(ctx, tenant, b.ID, func(current *contract.ImportBatch) error { current.State = "PAUSED"; return nil }); e != nil {
		t.Fatal(e)
	}
	other, found, e = fresh.ClaimImportBatch(ctx, time.Now().UTC())
	if e != nil || !found {
		t.Fatal(e)
	}
	draft.ID = ulid.Make().String()
	draft.OriginImportItemID = other.Items[0].ID
	if _, e = fresh.CommitImportDraft(ctx, tenant, other.ID, other.Revision, other.Epoch, other.Items[0].ID, draft, actor); !errors.Is(e, application.ErrImportIdentityConflict) {
		t.Fatalf("同来源异内容未阻断: %v", e)
	}
}
