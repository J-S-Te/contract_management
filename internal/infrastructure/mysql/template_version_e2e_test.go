package mysql

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
	"github.com/oklog/ulid/v2"
)

// 元数据保存不得回写旧正文；并发源文件替换后旧版本请求必须冲突。
func TestTemplateMetadataUpdateDoesNotOverwriteReplacedSource(t *testing.T) {
	repository, tenant := draftE2ERepository(t)
	ctx := context.Background()
	item := contracttemplate.Template{
		ID: ulid.Make().String(), TenantID: tenant, Name: "模板", OriginalFilename: "old.docx",
		NumberFormat: contracttemplate.DefaultNumberFormat, Content: []byte("old body"),
		Fields:  []contracttemplate.Field{{Name: "customer_name", Label: "客户名称"}},
		Version: 1, CreatedAt: time.Now().UTC(), CreatedBy: "actor-1",
	}
	if err := repository.CreateTemplate(ctx, item); err != nil {
		t.Fatal(err)
	}
	stale, err := repository.GetTemplate(ctx, tenant, item.ID)
	if err != nil || stale.Version != 1 {
		t.Fatalf("template = %+v err=%v", stale, err)
	}
	replacement := stale
	replacement.Content = []byte("new body")
	replacement.OriginalFilename = "new.docx"
	replacement.PlatformFileID = "file-new"
	replacement.FileGatewayState = "READY"
	if err := repository.UpdateTemplateSource(ctx, replacement); err != nil {
		t.Fatalf("source update: %v", err)
	}
	// 旧元数据请求（版本 1）必须失败，而不是把 old body 写回。
	stale.Name = "旧名称"
	if err := repository.UpdateTemplate(ctx, stale); !errors.Is(err, apperrors.ErrVersionConflict) {
		t.Fatalf("stale metadata update error = %v, want ErrVersionConflict", err)
	}
	stored, err := repository.GetTemplate(ctx, tenant, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored.Content) != "new body" || stored.OriginalFilename != "new.docx" || stored.PlatformFileID != "file-new" || stored.Version != 2 {
		t.Fatalf("replaced source was overwritten: %+v", stored)
	}
	stored.Name = "新名称"
	if err := repository.UpdateTemplate(ctx, stored); err != nil {
		t.Fatalf("current metadata update: %v", err)
	}
	after, err := repository.GetTemplate(ctx, tenant, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "新名称" || string(after.Content) != "new body" || after.PlatformFileID != "file-new" || after.Version != 3 {
		t.Fatalf("metadata update changed source: %+v", after)
	}
}
