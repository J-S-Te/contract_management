package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
	"github.com/oklog/ulid/v2"
)

func TestTemplateFileLifecycleAtomicRetirementAndRetry(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	item := contracttemplate.Template{ID: ulid.Make().String(), TenantID: tenant, Name: "template", OriginalFilename: "old.docx", NumberFormat: contracttemplate.DefaultNumberFormat, Fields: []contracttemplate.Field{}, Content: []byte("original"), Version: 1, CreatedBy: ulid.Make().String()}
	if err := r.CreateTemplateWithFile(ctx, item); err != nil {
		t.Fatal(err)
	}
	// 事务内先插入源意图，再插入模板；
	// 模板重复失败必须同时回滚该意图。
	if err := r.CreateTemplateWithFile(ctx, item); err == nil {
		t.Fatal("duplicate template write unexpectedly accepted")
	}
	var original templateFileOutboxRecord
	if err := r.db.Where("tenant_id = ?", tenant).Take(&original).Error; err != nil {
		t.Fatal(err)
	}
	if original.Retired || original.DeliveryStatus != "pending" || original.ResourceID != original.ID || string(original.Document) != "original" {
		t.Fatalf("invalid durable intent: %+v", original)
	}
	stored, err := r.GetTemplate(ctx, tenant, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PlatformFileID != "" || stored.FileGatewayState != "PENDING" {
		t.Fatalf("template not pending: %+v", stored)
	}

	stale := item
	stale.Version = 99
	stale.Content = []byte("rejected")
	if err := r.UpdateTemplateSourceWithFile(ctx, stale); !errors.Is(err, apperrors.ErrVersionConflict) {
		t.Fatalf("CAS error=%v", err)
	}
	var count int64
	r.db.Model(&templateFileOutboxRecord{}).Where("tenant_id = ?", tenant).Count(&count)
	if count != 1 {
		t.Fatalf("rejected CAS persisted %d source intents", count)
	}

	wantErr := errors.New("lost bind response")
	if _, err := r.ProcessTemplateFile(ctx, tenant, func(_ context.Context, d application.TemplateFileDelivery) (string, error) {
		if d.ID != original.ID || d.Retired {
			t.Fatalf("delivery=%+v", d)
		}
		return "file-old", wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("delivery error=%v", err)
	}
	var retry templateFileOutboxRecord
	if err := r.db.Where("id = ?", original.ID).Take(&retry).Error; err != nil {
		t.Fatal(err)
	}
	if retry.FileID != "file-old" || retry.Attempts != 1 || retry.DeliveryStatus != "pending" || !retry.NextAttemptAt.After(time.Now().UTC()) {
		t.Fatalf("lost recoverable state: %+v", retry)
	}
	if err := r.db.Model(&templateFileOutboxRecord{}).Where("id = ?", original.ID).Update("next_attempt_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProcessTemplateFile(ctx, tenant, func(_ context.Context, d application.TemplateFileDelivery) (string, error) {
		if d.FileID != "file-old" {
			t.Fatalf("retry lost file id: %+v", d)
		}
		return d.FileID, nil
	}); err != nil {
		t.Fatal(err)
	}

	stored, err = r.GetTemplate(ctx, tenant, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Content = []byte("replacement")
	stored.OriginalFilename = "new.docx"
	if err := r.UpdateTemplateSourceWithFile(ctx, stored); err != nil {
		t.Fatal(err)
	}
	var replacement templateFileOutboxRecord
	if err := r.db.Where("tenant_id = ? AND id <> ?", tenant, original.ID).Take(&replacement).Error; err != nil {
		t.Fatal(err)
	}
	if replacement.ResourceID == original.ResourceID {
		t.Fatal("source generations share release identity")
	}
	seen := map[string]bool{}
	for index := 0; index < 2; index++ {
		if _, err := r.ProcessTemplateFile(ctx, tenant, func(_ context.Context, d application.TemplateFileDelivery) (string, error) {
			seen[d.ID] = d.Retired
			if d.ID == original.ID {
				if !d.Retired || d.FileID != "file-old" {
					t.Fatalf("old source not retired: %+v", d)
				}
				return d.FileID, nil
			}
			if d.Retired {
				t.Fatal("replacement unexpectedly retired")
			}
			return "file-new", nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 || !seen[original.ID] || seen[replacement.ID] {
		t.Fatalf("wrong source operations: %+v", seen)
	}
	if err := r.DeleteTemplate(ctx, tenant, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetTemplate(ctx, tenant, item.ID); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("deleted row lookup=%v", err)
	}
	if found, err := r.ProcessTemplateFile(ctx, tenant, func(_ context.Context, d application.TemplateFileDelivery) (string, error) {
		if d.ID != replacement.ID || !d.Retired || d.FileID != "file-new" {
			t.Fatalf("deleted source=%+v", d)
		}
		return d.FileID, nil
	}); err != nil || !found {
		t.Fatalf("delete did not retain release intent: found=%t err=%v", found, err)
	}
}

func TestTemplateFileLifecycleFailedAttemptRecoveryAndTenantSelection(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	newTemplate := func(tenantID string) contracttemplate.Template {
		return contracttemplate.Template{ID: ulid.Make().String(), TenantID: tenantID, Name: "source", OriginalFilename: "source.docx", NumberFormat: contracttemplate.DefaultNumberFormat, Fields: []contracttemplate.Field{}, Content: []byte("source"), Version: 1, CreatedBy: ulid.Make().String()}
	}
	foreignTenant := ulid.Make().String()
	foreign := newTemplate(foreignTenant)
	if err := r.CreateTemplateWithFile(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r.db.Where("tenant_id = ?", foreignTenant).Delete(&templateFileOutboxRecord{})
		r.db.Where("tenant_id = ?", foreignTenant).Delete(&contractTemplateRecord{})
	})
	if err := r.db.Model(&templateFileOutboxRecord{}).Where("tenant_id = ?", foreignTenant).Update("next_attempt_at", time.Now().UTC().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	item := newTemplate(tenant)
	if err := r.CreateTemplateWithFile(ctx, item); err != nil {
		t.Fatal(err)
	}
	if found, err := r.ProcessTemplateFile(ctx, tenant, func(_ context.Context, d application.TemplateFileDelivery) (string, error) {
		if d.TenantID != tenant || d.UploadAttempt != 0 {
			t.Fatalf("wrong selected intent: %+v", d)
		}
		return "failed-file", application.ErrTemplateUploadFailed
	}); !found || !errors.Is(err, application.ErrTemplateUploadFailed) {
		t.Fatalf("failed attempt found=%t err=%v", found, err)
	}
	var source templateFileOutboxRecord
	if err := r.db.Where("tenant_id = ?", tenant).Take(&source).Error; err != nil {
		t.Fatal(err)
	}
	var failures []struct {
		Attempt uint64 `json:"attempt"`
		FileID  string `json:"file_id"`
	}
	if err := json.Unmarshal(source.UploadFailuresJSON, &failures); err != nil {
		t.Fatal(err)
	}
	if source.UploadAttempt != 1 || source.FileID != "" || len(failures) != 1 || failures[0].Attempt != 0 || failures[0].FileID != "failed-file" {
		t.Fatalf("failed identity was lost: %+v history=%+v", source, failures)
	}
	if err := r.db.Model(&templateFileOutboxRecord{}).Where("id = ?", source.ID).Update("next_attempt_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProcessTemplateFile(ctx, tenant, func(_ context.Context, d application.TemplateFileDelivery) (string, error) {
		if d.UploadAttempt != 1 || d.FileID != "" {
			t.Fatalf("retry allocation not durable: %+v", d)
		}
		return "rejected-file", application.ErrTemplateUploadRejected
	}); !errors.Is(err, application.ErrTemplateUploadRejected) {
		t.Fatal(err)
	}
	if found, err := r.ProcessTemplateFile(ctx, tenant, func(context.Context, application.TemplateFileDelivery) (string, error) {
		t.Fatal("rejected content was automatically retried")
		return "", nil
	}); found || err != nil {
		t.Fatalf("rejected found=%t err=%v", found, err)
	}
	if err := r.DeleteTemplate(ctx, tenant, item.ID); err != nil {
		t.Fatal(err)
	}
	if found, err := r.ProcessTemplateFile(ctx, tenant, func(_ context.Context, d application.TemplateFileDelivery) (string, error) {
		if !d.Retired || d.FileID != "rejected-file" {
			t.Fatalf("rejected source retirement lost identity: %+v", d)
		}
		return d.FileID, nil
	}); !found || err != nil {
		t.Fatalf("retire rejected found=%t err=%v", found, err)
	}
}

func TestTemplateFileDeliverySerializesWithDelete(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	item := contracttemplate.Template{ID: ulid.Make().String(), TenantID: tenant, Name: "template", OriginalFilename: "source.docx", NumberFormat: contracttemplate.DefaultNumberFormat, Fields: []contracttemplate.Field{}, Content: []byte("source"), Version: 1, CreatedBy: ulid.Make().String()}
	if err := r.CreateTemplateWithFile(ctx, item); err != nil {
		t.Fatal(err)
	}
	entered, finish := make(chan struct{}), make(chan struct{})
	deliveryDone, deleteDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := r.ProcessTemplateFile(ctx, tenant, func(context.Context, application.TemplateFileDelivery) (string, error) {
			close(entered)
			select {
			case <-finish:
				return "file", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		})
		deliveryDone <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() { deleteDone <- r.DeleteTemplate(ctx, tenant, item.ID) }()
	close(finish)
	if err := <-deliveryDone; err != nil {
		t.Fatal(err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatal(err)
	}
	if found, err := r.ProcessTemplateFile(ctx, tenant, func(_ context.Context, d application.TemplateFileDelivery) (string, error) {
		if !d.Retired || d.FileID != "file" {
			t.Fatalf("wrong retirement after concurrent delete: %+v", d)
		}
		return d.FileID, nil
	}); err != nil || !found {
		t.Fatalf("release found=%t error=%v", found, err)
	}
}
