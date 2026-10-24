package application

import (
	"context"
	"errors"
	"io"
	"testing"

	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
)

type lifecycleTemplateRepository struct {
	memoryTemplateRepository
	writeErr error
}

func (r *lifecycleTemplateRepository) CreateTemplateWithFile(ctx context.Context, item contracttemplate.Template) error {
	if r.writeErr != nil {
		return r.writeErr
	}
	return r.CreateTemplate(ctx, item)
}
func (r *lifecycleTemplateRepository) UpdateTemplateSourceWithFile(ctx context.Context, item contracttemplate.Template) error {
	if r.writeErr != nil {
		return r.writeErr
	}
	return r.memoryTemplateRepository.UpdateTemplateSource(ctx, item)
}
func (r *lifecycleTemplateRepository) UpdateTemplateSource(context.Context, contracttemplate.Template) error {
	return r.writeErr
}

type lifecycleGateway struct{ calls int }

func (g *lifecycleGateway) Upload(context.Context, string, string, string, string, string, io.Reader) (string, error) {
	g.calls++
	return "new-file", nil
}
func (g *lifecycleGateway) Bind(context.Context, string, string, string, string, string, string) error {
	g.calls++
	return nil
}
func (g *lifecycleGateway) Download(context.Context, string) ([]byte, error) { return nil, nil }

// CAS 被拒绝后绝不能留下远程文件或绑定；唯一写入是
// 同时记录源投递意图的本地事务。
func TestReplaceTemplateRejectedWriteHasNoGatewaySideEffects(t *testing.T) {
	wantErr := errors.New("version conflict")
	r := &lifecycleTemplateRepository{memoryTemplateRepository: memoryTemplateRepository{items: map[string]contracttemplate.Template{"t": {ID: "t", TenantID: "tenant", Version: 1}}}, writeErr: wantErr}
	g := &lifecycleGateway{}
	s := &Service{Templates: r, StampedFileGateway: g, StampedFileMode: "required"}
	_, err := s.ReplaceTemplateSource(context.Background(), Principal{TenantID: "tenant", Roles: []string{"admin"}}, "t", "new.docx", applicationTestDOCX(t, "{{customer}}"))
	if !errors.Is(err, wantErr) {
		t.Fatalf("error=%v", err)
	}
	if g.calls != 0 {
		t.Fatalf("rejected source write made %d remote calls; want zero", g.calls)
	}
}

func TestCreateTemplatePersistsPendingIntentBeforeGatewayDelivery(t *testing.T) {
	r := &lifecycleTemplateRepository{}
	g := &lifecycleGateway{}
	s := &Service{Templates: r, StampedFileGateway: g, StampedFileMode: "required"}
	item, err := s.CreateTemplate(context.Background(), Principal{TenantID: "tenant", Roles: []string{"admin"}}, "template", "new.docx", applicationTestDOCX(t, "{{customer}}"))
	if err != nil {
		t.Fatal(err)
	}
	if item.FileGatewayState != "PENDING" || item.PlatformFileID != "" || g.calls != 0 {
		t.Fatalf("state=%s file=%s calls=%d; want pending durable delivery without remote calls", item.FileGatewayState, item.PlatformFileID, g.calls)
	}
}
