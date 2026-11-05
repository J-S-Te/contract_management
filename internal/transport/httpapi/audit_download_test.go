package httpapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/contractdocument"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
	"github.com/j-s-te/contract-management/internal/testfixture"
)

type downloadAuditReporter func(context.Context, platform.AuditEvent) error

func (f downloadAuditReporter) Report(ctx context.Context, event platform.AuditEvent) error {
	return f(ctx, event)
}

type auditDownloadRepository struct {
	application.Repository
	documentAccess []string
	// documentAccessErr 注入持久文档访问审计失败，用于验证失败关闭。
	documentAccessErr error
	// stored 覆盖默认DOCX夹具，用于覆盖PDF导出分支。
	stored *contract.Contract
}

func (r *auditDownloadRepository) GetContract(_ context.Context, tenantID, id string) (contract.Contract, error) {
	if tenantID != "audit-test-tenant" || id != "existing" {
		return contract.Contract{}, application.ErrForbidden
	}
	if r.stored != nil {
		return *r.stored, nil
	}
	return contract.Contract{ID: id, TenantID: tenantID, Status: contract.StatusActive, Number: "CON-EXISTING", Document: []byte("existing-private-document")}, nil
}

// RecordDocumentAccess 让测试替身满足服务层"下载必须先建立审计依据"的要求；
// 记录次数在用例里断言，避免下载在无审计记录时也能成功。
func (r *auditDownloadRepository) RecordDocumentAccess(_ context.Context, tenantID, contractID, documentKind, actorID string) error {
	if r.documentAccessErr != nil {
		return r.documentAccessErr
	}
	r.documentAccess = append(r.documentAccess, documentKind)
	return nil
}

func (auditDownloadRepository) GetStampedDocument(_ context.Context, tenantID, id string) (contract.StampedDocument, error) {
	if tenantID != "audit-test-tenant" || id != "existing" {
		return contract.StampedDocument{}, application.ErrForbidden
	}
	return contract.StampedDocument{OriginalFilename: "signed.pdf", Document: []byte("existing-private-document")}, nil
}

func TestRegisteredContractDownloadsEnforceAuditAndPermission(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	gin.SetMode(gin.TestMode)
	for _, download := range []struct {
		path string
		kind string
	}{
		{"/api/v1/approved-contracts/existing/docx", "DOCX"},
		{"/api/v1/approved-contracts/existing/stamped-pdf", "STAMPED_PDF"},
		{"/api/v1/contracts/existing/export", "EXPORT_DOCX"},
	} {
		path := download.path
		for _, tc := range []struct {
			name     string
			auditErr error
			allowed  bool
			want     int
		}{
			{"audit_rejected", errors.New("ingest unavailable"), true, 503},
			{"audit_accepted", nil, true, 200},
			{"permission_denied", nil, false, 403},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				actor := application.Principal{TenantID: "audit-test-tenant", UserID: "actor", IdentityID: "actor",
					Permissions:      map[string]bool{"contract.read": tc.allowed, "contract.document.download": tc.allowed},
					PermissionScopes: map[string]contract.ScopeFilter{"contract.read": {AllowAll: true}, "contract.document.download": {AllowAll: true}}}
				repository := &auditDownloadRepository{}
				service := &application.Service{Repo: repository, LicenseGate: historyOnlyLicense{}}
				router := NewRouter(service, identityFunc(func(context.Context, *http.Request) (application.Principal, error) { return actor, nil }), nil, &stubAuditReporter{err: tc.auditErr})
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
				if response.Code != tc.want {
					t.Fatalf("status=%d want=%d body=%q", response.Code, tc.want, response.Body.String())
				}
				// 持久文档访问审计独立于HTTP审计中间件：越权读取在被拒绝前不得留下记录，
				// 记录成功后的平台审计摄取失败只丢弃响应字节，不撤销已建立的访问依据。
				want := []string{download.kind}
				if tc.want == http.StatusForbidden {
					want = nil
				}
				if strings.Join(repository.documentAccess, ",") != strings.Join(want, ",") {
					t.Errorf("document access audit=%v want=%v", repository.documentAccess, want)
				}
				if tc.want == 200 {
					if response.Body.String() != "existing-private-document" || response.Result().Header.Get("Content-Disposition") == "" {
						t.Errorf("registered history export failed: headers=%v body=%q", response.Result().Header, response.Body.String())
					}
				} else if strings.Contains(response.Body.String(), "existing-private-document") || response.Result().Header.Get("Content-Disposition") != "" {
					t.Errorf("rejected export leaked document: headers=%v body=%q", response.Result().Header, response.Body.String())
				}
			})
		}
	}
}

// exportContract 必须先建立持久文档访问审计再输出字节，并与 /source 的
// SOURCE_<format> 保持一致地按格式记录 EXPORT_<format>。
// 记录失败必须失败关闭：不输出文件字节、下载响应头或成功状态。
func TestExportContractRecordsDocumentAccessBeforeBytes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pdf := testfixture.PDF("exported contract")
	docx := []byte("existing-private-document")
	for _, tc := range []struct {
		name      string
		stored    *contract.Contract
		accessErr error
		wantKind  string
		document  []byte
	}{
		{name: "docx_recorded", wantKind: "EXPORT_DOCX", document: docx},
		{name: "pdf_recorded", stored: &contract.Contract{ID: "existing", TenantID: "audit-test-tenant", Status: contract.StatusActive, Number: "CON-EXISTING", DocumentFormat: contractdocument.FormatPDF, Document: pdf}, wantKind: "EXPORT_PDF", document: pdf},
		{name: "docx_audit_failure_closed", accessErr: errors.New("audit outbox unavailable"), document: docx},
		{name: "pdf_audit_failure_closed", accessErr: errors.New("audit outbox unavailable"), stored: &contract.Contract{ID: "existing", TenantID: "audit-test-tenant", Status: contract.StatusActive, Number: "CON-EXISTING", DocumentFormat: contractdocument.FormatPDF, Document: pdf}, document: pdf},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actor := application.Principal{TenantID: "audit-test-tenant", UserID: "actor", IdentityID: "actor",
				Permissions:      map[string]bool{"contract.read": true},
				PermissionScopes: map[string]contract.ScopeFilter{"contract.read": {AllowAll: true}}}
			repository := &auditDownloadRepository{documentAccessErr: tc.accessErr, stored: tc.stored}
			service := &application.Service{Repo: repository, LicenseGate: historyOnlyLicense{}}
			router := NewRouter(service, identityFunc(func(context.Context, *http.Request) (application.Principal, error) { return actor, nil }), nil, &stubAuditReporter{})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/contracts/existing/export", nil))
			if tc.accessErr != nil {
				// writeError 对普通仓储错误走默认分支：500 CON_INTERNAL_ERROR；
				// 唯一的强制要求是失败关闭，不泄漏任何文档响应。
				if response.Code != http.StatusInternalServerError {
					t.Fatalf("status=%d want=%d body=%q", response.Code, http.StatusInternalServerError, response.Body.String())
				}
				if len(repository.documentAccess) != 0 {
					t.Errorf("failed document audit still recorded access: %v", repository.documentAccess)
				}
				if response.Result().Header.Get("Content-Disposition") != "" || bytes.Contains(response.Body.Bytes(), tc.document) {
					t.Errorf("failed document audit leaked export: headers=%v body=%q", response.Result().Header, response.Body.String())
				}
				if !strings.Contains(response.Result().Header.Get("Content-Type"), "application/json") {
					t.Errorf("failed document audit retained document Content-Type: %v", response.Result().Header)
				}
				return
			}
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d want=%d body=%q", response.Code, http.StatusOK, response.Body.String())
			}
			if len(repository.documentAccess) != 1 || repository.documentAccess[0] != tc.wantKind {
				t.Fatalf("document access audit=%v want one %s", repository.documentAccess, tc.wantKind)
			}
			if !bytes.Equal(response.Body.Bytes(), tc.document) || response.Result().Header.Get("Content-Disposition") == "" {
				t.Errorf("export after audit failed: headers=%v body=%q", response.Result().Header, response.Body.String())
			}
		})
	}
}

// 验证真实审计中间件及下载写入器，仅替换外部
// 审计摄取。Reporter 失败必须先于任何文件投递。
func TestRequiredDownloadAuditRejectsBeforeDocumentOrHeaders(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	gin.SetMode(gin.TestMode)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, path := range []string{"/api/v1/approved-contracts/id/docx", "/api/v1/approved-contracts/id/pdf", "/api/v1/approved-contracts/id/stamped-pdf", "/api/v1/contracts/id/export"} {
			t.Run(method+path, func(t *testing.T) {
				response := httptest.NewRecorder()
				calls := 0
				h := &Handler{audit: downloadAuditReporter(func(context.Context, platform.AuditEvent) error {
					calls++
					if response.Body.Len() != 0 || response.Header().Get("Content-Disposition") != "" {
						t.Error("document or success headers escaped before audit acceptance")
					}
					return errors.New("audit ingest unavailable")
				})}
				r := gin.New()
				r.Use(requestID(), recoverer(), h.auditWrites())
				r.Handle(method, path, func(c *gin.Context) {
					c.Header("Content-Length", "23")
					c.Header("ETag", "secret-document-hash")
					c.Header("Cache-Control", "public, max-age=3600")
					h.download(c, "private-contract", ".pdf", "application/pdf", []byte("private-contract-bytes"))
					c.Writer.Flush()
				})
				r.ServeHTTP(response, httptest.NewRequest(method, path, nil))
				if response.Code != http.StatusServiceUnavailable || calls != 1 {
					t.Errorf("status=%d reports=%d, want 503 and one audit attempt", response.Code, calls)
				}
				for _, header := range []string{"Content-Disposition", "Content-Length", "ETag", "Cache-Control"} {
					if got := response.Result().Header.Get(header); got != "" {
						t.Errorf("failed download leaked %s=%q", header, got)
					}
				}
				if strings.Contains(response.Body.String(), "private-contract-bytes") {
					t.Error("failed audit leaked document bytes")
				}
				if !strings.Contains(response.Result().Header.Get("Content-Type"), "application/json") {
					t.Errorf("rejection retained document Content-Type: %v", response.Result().Header)
				}
				if method == http.MethodHead && response.Body.Len() != 0 {
					t.Errorf("HEAD rejection wrote %d body bytes", response.Body.Len())
				}
			})
		}
	}
}

func TestRequiredDownloadAuditAcceptsMaximumPDFSize(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	gin.SetMode(gin.TestMode)
	document := bytes.Repeat([]byte("P"), 20<<20)
	h := &Handler{audit: &stubAuditReporter{}}
	r := gin.New()
	r.Use(h.auditWrites())
	r.GET("/approved-contracts/id/pdf", func(c *gin.Context) { h.download(c, "contract", ".pdf", "application/pdf", document) })
	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest("GET", "/approved-contracts/id/pdf", nil))
	if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), document) {
		t.Errorf("maximum supported PDF not preserved: status=%d length=%d", response.Code, response.Body.Len())
	}
}

func TestRequiredAuditBufferCannotExposeUnauditedWriter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	buffer := newAuditBuffer(c.Writer)
	if wrapper, ok := any(buffer).(interface{ Unwrap() http.ResponseWriter }); ok {
		_, _ = wrapper.Unwrap().Write([]byte("unaudited-document"))
	}
	if response.Body.Len() != 0 {
		t.Fatal("buffer exposed a writer that bypasses the audit decision")
	}
}

func TestRequiredDownloadAuditHEADRetainsDocumentLengthWithoutBody(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	gin.SetMode(gin.TestMode)
	h := &Handler{audit: &stubAuditReporter{}}
	r := gin.New()
	r.Use(h.auditWrites())
	r.HEAD("/approved-contracts/id/pdf", func(c *gin.Context) {
		h.download(c, "private-contract", ".pdf", "application/pdf", []byte("private-contract-data"))
	})
	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest("HEAD", "/approved-contracts/id/pdf", nil))
	if response.Code != 200 || response.Body.Len() != 0 || response.Result().Header.Get("Content-Length") != "21" {
		t.Errorf("HEAD document metadata lost: code=%d headers=%v body bytes=%d", response.Code, response.Result().Header, response.Body.Len())
	}
}

func TestRequiredDownloadAuditSuccessReleasesResponseAfterReport(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	gin.SetMode(gin.TestMode)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			response := httptest.NewRecorder()
			calls := 0
			h := &Handler{audit: downloadAuditReporter(func(context.Context, platform.AuditEvent) error {
				calls++
				if response.Body.Len() != 0 || response.Header().Get("Content-Disposition") != "" {
					t.Error("response was released before audit acceptance")
				}
				return nil
			})}
			r := gin.New()
			r.Use(requestID(), recoverer(), func(c *gin.Context) { c.Header("X-Upstream", "preserved"); c.Next() }, h.auditWrites())
			r.Handle(method, "/approved-contracts/id/pdf", func(c *gin.Context) {
				if got := c.Writer.Header().Get("X-Upstream"); got != "preserved" {
					t.Errorf("buffer discarded upstream header: %q", got)
				}
				c.Header("X-Upstream", "")
				c.Header("Content-Length", "21")
				h.download(c, "private-contract", ".pdf", "application/pdf", []byte("private-contract-data"))
			})
			r.ServeHTTP(response, httptest.NewRequest(method, "/approved-contracts/id/pdf", nil))
			if response.Code != 200 || calls != 1 || response.Header().Get("Content-Disposition") == "" || response.Header().Get("Content-Length") != "21" {
				t.Errorf("successful download changed status/headers: code=%d calls=%d headers=%v", response.Code, calls, response.Header())
			}
			if response.Header().Get("X-Upstream") != "" || response.Header().Get("X-Request-ID") == "" {
				t.Errorf("header deletion or upstream request identity lost: %v", response.Header())
			}
			want := "private-contract-data"
			if method == http.MethodHead {
				want = ""
			}
			if response.Body.String() != want {
				t.Errorf("body=%q, want %q", response.Body.String(), want)
			}
		})
	}
}

func TestRequiredDownloadAuditPanicDiscardsPartialDocument(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	gin.SetMode(gin.TestMode)
	h := &Handler{audit: &stubAuditReporter{}}
	r := gin.New()
	r.Use(requestID(), recoverer(), h.auditWrites())
	r.GET("/approved-contracts/id/pdf", func(c *gin.Context) {
		h.download(c, "private-contract", ".pdf", "application/pdf", []byte("private-partial-document"))
		panic("conversion response interrupted")
	})
	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest("GET", "/approved-contracts/id/pdf", nil))
	if response.Code != 500 || strings.Contains(response.Body.String(), "private-partial-document") || response.Header().Get("Content-Disposition") != "" {
		t.Errorf("panic leaked partial response: code=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
}

// 合同 DOCX 上限为 10 MiB，PDF 上限为 20 MiB。分块响应
// 超过 32 MiB 时，即使处理器忽略 Write 错误也必须安全拒绝。
func TestRequiredDownloadAuditOversizedStreamNeverReachesClient(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	gin.SetMode(gin.TestMode)
	h := &Handler{audit: &stubAuditReporter{}}
	r := gin.New()
	r.Use(requestID(), recoverer(), h.auditWrites())
	r.GET("/approved-contracts/id/pdf", func(c *gin.Context) {
		c.Header("Content-Disposition", "attachment; filename=oversized.pdf")
		chunk := bytes.Repeat([]byte("X"), 1<<20)
		for i := 0; i < 33; i++ {
			_, _ = c.Writer.Write(chunk)
			c.Writer.Flush()
		}
	})
	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest("GET", "/approved-contracts/id/pdf", nil))
	if response.Code != 503 || response.Header().Get("Content-Disposition") != "" || bytes.Contains(response.Body.Bytes(), bytes.Repeat([]byte("X"), 64)) {
		t.Errorf("oversized capture escaped: code=%d headers=%v body length=%d", response.Code, response.Header(), response.Body.Len())
	}
}

func TestDownloadAuditLeavesOptionalAndOrdinaryReadsUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		required, path string
		reports        int
	}{
		{"false", "/approved-contracts/id/pdf", 1},
		{"true", "/contracts/id", 0},
	} {
		t.Run(tc.required+tc.path, func(t *testing.T) {
			t.Setenv("PLATFORM_AUDIT_REQUIRED", tc.required)
			reporter := &stubAuditReporter{err: errors.New("audit unavailable")}
			h := &Handler{audit: reporter}
			r := gin.New()
			r.Use(h.auditWrites())
			r.GET(tc.path, func(c *gin.Context) { c.String(200, "ordinary response") })
			response := httptest.NewRecorder()
			r.ServeHTTP(response, httptest.NewRequest("GET", tc.path, nil))
			if response.Code != 200 || response.Body.String() != "ordinary response" || reporter.calls != tc.reports {
				t.Errorf("unaffected read changed: code=%d body=%q reports=%d", response.Code, response.Body.String(), reporter.calls)
			}
		})
	}
}
