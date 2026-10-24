package application

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/j-s-te/contract-management/internal/filegatewayclient"
)

// 此存储表示应用边界已提交的意图。
// SQL 原子性及并发退役由 MySQL E2E 验证。
type dispatcherSourceStore struct{ source TemplateFileDelivery }

func (s *dispatcherSourceStore) ProcessTemplateFile(ctx context.Context, _ string, fn func(context.Context, TemplateFileDelivery) (string, error)) (bool, error) {
	id, err := fn(ctx, s.source)
	if id != "" {
		s.source.FileID = id
	}
	return true, err
}

func TestTemplateFileDispatcherRecoversUploadResponseBeforeRetiredRelease(t *testing.T) {
	var uploads, releases, binds int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/file-upload-requests/contract-template-source-generation-old" || r.URL.Path == "/api/v1/file-upload-requests/contract-template-source-generation-old/retire" {
			if uploads == 0 {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"request_id":"contract-template-source-generation-old","file_id":"recovered-file","status":"READY","retired":true}}`))
			return
		}
		if r.URL.Path == "/api/v1/files" {
			uploads++
			if r.Header.Get("Idempotency-Key") != "contract-template-source-generation-old" {
				t.Error("upload replay lost generation identity")
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			defer file.Close()
			body, _ := io.ReadAll(file)
			if string(body) != "source bytes" || header.Filename != "source.docx" || r.FormValue("purpose") != "contract.template" {
				t.Error("upload replay changed immutable source")
			}
			if uploads == 1 {
				w.WriteHeader(503)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"file_id":"recovered-file"}}`))
			return
		}
		if r.URL.Path == "/api/v1/files/recovered-file/bindings/release" {
			releases++
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload["resource_id"] != "generation-old" || payload["resource_type"] != "contract_template_source" || payload["binding_type"] != "TEMPLATE_SOURCE" {
				t.Errorf("wrong retirement identity: %+v", payload)
			}
			if releases == 1 {
				w.WriteHeader(503)
				return
			}
			w.WriteHeader(204)
			return
		}
		binds++
		w.WriteHeader(204)
	}))
	defer server.Close()
	gateway, err := filegatewayclient.New(server.URL, server.Client(), func(context.Context) (string, error) { return "machine", nil })
	if err != nil {
		t.Fatal(err)
	}
	store := &dispatcherSourceStore{source: TemplateFileDelivery{ID: "generation-old", TenantID: "tenant", TemplateID: "template", Filename: "source.docx", Content: []byte("source bytes"), ResourceType: "contract_template_source", ResourceID: "generation-old"}}
	dispatcher := TemplateFileDispatcher{Store: store, Gateway: gateway, ApplicationID: "contract", TenantID: "tenant"}
	if _, err := dispatcher.DeliverOnce(context.Background()); err == nil {
		t.Fatal("lost upload response must be retried")
	}
	// 响应未知时模板被删除。恢复必须
	// 取回原文件 ID，并仅释放对应代次。
	store.source.Retired = true
	if _, err := dispatcher.DeliverOnce(context.Background()); err == nil {
		t.Fatal("release 503 must be retried")
	}
	if store.source.FileID != "recovered-file" {
		t.Fatal("release failure discarded recovered file identity")
	}
	if _, err := dispatcher.DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if uploads != 1 || releases != 2 || binds != 0 {
		t.Fatalf("uploads=%d releases=%d binds=%d", uploads, releases, binds)
	}
}

func TestRetiredTemplateFileCleanupSurvivesExpiredLicenseWithoutUpload(t *testing.T) {
	var uploads, releases int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" || r.URL.Path == "/api/v1/file-upload-requests/contract-template-source-generation-old/retire" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"request_id":"contract-template-source-generation-old","file_id":"file-old","status":"READY","retired":true}}`))
			return
		}
		if r.URL.Path == "/api/v1/files/file-old/bindings/release" {
			releases++
			w.WriteHeader(204)
			return
		}
		uploads++
		w.WriteHeader(500)
	}))
	defer server.Close()
	gateway, err := filegatewayclient.New(server.URL, server.Client(), func(context.Context) (string, error) { return "machine", nil })
	if err != nil {
		t.Fatal(err)
	}
	store := &dispatcherSourceStore{source: TemplateFileDelivery{ID: "generation-old", TenantID: "tenant", Retired: true, Filename: "old.docx", ResourceType: "contract_template_source", ResourceID: "generation-old"}}
	d := TemplateFileDispatcher{Store: store, Gateway: gateway, ApplicationID: "contract", TenantID: "tenant", LicenseGate: deniedLicense{}}
	if _, err := d.DeliverOnce(context.Background()); err != nil || uploads != 0 || releases != 1 {
		t.Fatalf("cleanup error=%v uploads=%d releases=%d", err, uploads, releases)
	}
}

func TestRetiredTemplateFileMissingRequestDoesNotCreateUpload(t *testing.T) {
	var uploads, retires int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/file-upload-requests/contract-template-source-generation/retire" {
			retires++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"request_id":"contract-template-source-generation","file_id":"","status":"CANCELLED","retired":true}}`))
			return
		}
		if r.Method != "GET" {
			uploads++
		}
		w.WriteHeader(404)
	}))
	defer server.Close()
	gateway, err := filegatewayclient.New(server.URL, server.Client(), func(context.Context) (string, error) { return "machine", nil })
	if err != nil {
		t.Fatal(err)
	}
	store := &dispatcherSourceStore{source: TemplateFileDelivery{ID: "generation", TenantID: "tenant", Retired: true, Filename: "old.docx", ResourceType: "contract_template_source", ResourceID: "generation"}}
	d := TemplateFileDispatcher{Store: store, Gateway: gateway, ApplicationID: "contract", TenantID: "tenant"}
	if _, err := d.DeliverOnce(context.Background()); err != nil || uploads != 0 || retires != 1 {
		t.Fatalf("absent retired request error=%v uploads=%d retires=%d", err, uploads, retires)
	}
}

func TestRetiredTemplateFileWritingRequestIsFencedBeforeRelease(t *testing.T) {
	var retired, released bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/file-upload-requests/contract-template-source-generation/retire" {
			retired = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"request_id":"contract-template-source-generation","file_id":"writing-file","status":"WRITING","retired":true}}`))
			return
		}
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"request_id":"contract-template-source-generation","file_id":"writing-file","status":"WRITING"}}`))
			return
		}
		if r.URL.Path == "/api/v1/files/writing-file/bindings/release" {
			if !retired {
				t.Error("release preceded durable request fence")
			}
			released = true
			w.WriteHeader(204)
			return
		}
		w.WriteHeader(500)
	}))
	defer server.Close()
	gateway, err := filegatewayclient.New(server.URL, server.Client(), func(context.Context) (string, error) { return "machine", nil })
	if err != nil {
		t.Fatal(err)
	}
	store := &dispatcherSourceStore{source: TemplateFileDelivery{ID: "generation", TenantID: "tenant", Retired: true, ResourceType: "contract_template_source", ResourceID: "generation"}}
	d := TemplateFileDispatcher{Store: store, Gateway: gateway, ApplicationID: "contract", TenantID: "tenant", LicenseGate: deniedLicense{}}
	if _, err := d.DeliverOnce(context.Background()); err != nil || !retired || !released {
		t.Fatalf("retired=%t released=%t err=%v", retired, released, err)
	}
}

func TestTemplateFileDispatcherRejectsOtherTenantBeforeGatewayUpload(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"file_id":"wrong-tenant-file"}}`))
	}))
	defer server.Close()
	gateway, err := filegatewayclient.New(server.URL, server.Client(), func(context.Context) (string, error) { return "machine", nil })
	if err != nil {
		t.Fatal(err)
	}
	store := &dispatcherSourceStore{source: TemplateFileDelivery{ID: "generation", TenantID: "other-tenant", Filename: "source.docx", Content: []byte("private source"), ResourceType: "contract_template_source", ResourceID: "generation"}}
	dispatcher := TemplateFileDispatcher{Store: store, Gateway: gateway, ApplicationID: "contract", TenantID: "authorized-tenant"}
	if _, err := dispatcher.DeliverOnce(context.Background()); err == nil || requests != 0 {
		t.Fatalf("different tenant must fail closed, error=%v requests=%d", err, requests)
	}
}

func TestTemplateFileDispatcherResolvesTerminalAndWritingRequestsWithoutReupload(t *testing.T) {
	for _, tc := range []struct {
		status      string
		wantErr     error
		wantRelease int
	}{{"FAILED", ErrTemplateUploadFailed, 1}, {"REJECTED", ErrTemplateUploadRejected, 1}, {"WRITING", ErrTemplateUploadPending, 0}, {"READY", ErrCommercialLicense, 0}, {"NOT_FOUND", ErrCommercialLicense, 0}} {
		t.Run(tc.status, func(t *testing.T) {
			var uploads, binds, releases int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					if tc.status == "NOT_FOUND" {
						w.WriteHeader(404)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"request_id": "contract-template-source-generation", "file_id": "resolved-file", "status": tc.status}})
					return
				}
				if r.URL.Path == "/api/v1/files/resolved-file/bindings/release" {
					releases++
					w.WriteHeader(204)
					return
				}
				if r.URL.Path == "/api/v1/files" {
					uploads++
				} else {
					binds++
				}
				w.WriteHeader(500)
			}))
			defer server.Close()
			gateway, err := filegatewayclient.New(server.URL, server.Client(), func(context.Context) (string, error) { return "machine", nil })
			if err != nil {
				t.Fatal(err)
			}
			store := &dispatcherSourceStore{source: TemplateFileDelivery{ID: "generation", TenantID: "tenant", Filename: "source.docx", ResourceType: "contract_template_source", ResourceID: "generation"}}
			d := TemplateFileDispatcher{Store: store, Gateway: gateway, ApplicationID: "contract", TenantID: "tenant", LicenseGate: deniedLicense{}}
			if _, err := d.DeliverOnce(context.Background()); !errors.Is(err, tc.wantErr) || uploads != 0 || binds != 0 || releases != tc.wantRelease {
				t.Fatalf("err=%v upload=%d bind=%d release=%d", err, uploads, binds, releases)
			}
		})
	}
}
