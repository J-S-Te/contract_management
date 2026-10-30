package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/j-s-te/contract-management/internal/testfixture"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

type pdfSourceRepository struct {
	externalContractRepository
	auditErr error
}

// GetContract 提供测试中真实创建结果的租户内读取边界。
// 参数：ctx为请求上下文，tenant和id为请求中的可信范围。
// 返回值：已创建的测试合同；范围不符时返回禁止访问错误。
func (r *pdfSourceRepository) GetContract(ctx context.Context, tenant, id string) (contract.Contract, error) {
	if tenant != r.created.TenantID || id != r.created.ID {
		return contract.Contract{}, application.ErrForbidden
	}
	return r.created, nil
}

// RecordDocumentAccess 提供测试所需的持久访问审计边界。
// 参数：ctx为上下文，tenant/id/actor为访问范围，kind为文档类型。
// 返回值：范围不符或注入审计失败时返回错误，否则允许服务继续输出。
func (r *pdfSourceRepository) RecordDocumentAccess(ctx context.Context, tenant, id, kind, actor string) error {
	if tenant != r.created.TenantID || id != r.created.ID || actor == "" {
		return application.ErrForbidden
	}
	return r.auditErr
}

// TestExternalPDFSourceLifecycle 验证真实HTTP及Service的PDF创建和原件下载。
// 参数：t 为HTTP链路测试上下文。
// 返回值：无；开关、元数据或原字节输出不正确时测试失败。
func TestExternalPDFSourceLifecycle(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "http://example.com/contract_management")
	pdf := testfixture.PDF("original contract")
	for _, enabled := range []bool{false, true} {
		repo := &pdfSourceRepository{}
		svc := &application.Service{Repo: repo, ExternalPDFEnabled: enabled,
			DetectionCategories: externalDetectionCategoryDirectory{}, CRMContractReferences: externalCRMReferenceDirectory{}}
		router := consistencyRouter(svc)
		capabilities := httptest.NewRecorder()
		router.ServeHTTP(capabilities, httptest.NewRequest(http.MethodGet, "/api/v1/contract-source-capabilities", nil))
		var capabilityBody struct {
			Data struct {
				PDFEnabled bool `json:"pdf_enabled"`
			}
		}
		if capabilities.Code != http.StatusOK || json.Unmarshal(capabilities.Body.Bytes(), &capabilityBody) != nil || capabilityBody.Data.PDFEnabled != enabled {
			t.Fatalf("能力开关未正确发布: status=%d", capabilities.Code)
		}
		request := externalContractUploadRequest(t, consistencyJSON(t, consistencyMetadata()), "已签原件.pdf", "application/pdf", pdf)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if !enabled {
			if response.Code != http.StatusUnprocessableEntity || repo.created.ID != "" {
				t.Fatalf("disabled PDF created: %s", response.Body)
			}
			continue
		}
		if response.Code != http.StatusCreated {
			t.Fatalf("PDF create=%d %s", response.Code, response.Body)
		}
		if repo.created.DocumentFormat != "PDF" || repo.created.SourceFilename != "已签原件.pdf" ||
			!bytes.Equal(repo.created.Document, pdf) || repo.created.Content != "" || repo.created.SourceSHA256 == "" {
			t.Fatalf("PDF source metadata incorrect: %+v", repo.created)
		}
		for _, suffix := range []string{"/export", "/source", "/document-descriptor", "/preview"} {
			result := httptest.NewRecorder()
			router.ServeHTTP(result, httptest.NewRequest(http.MethodGet, "/api/v1/contracts/"+repo.created.ID+suffix, nil))
			if result.Code != http.StatusOK {
				t.Fatalf("%s=%d %s", suffix, result.Code, result.Body)
			}
			if suffix == "/source" || suffix == "/export" {
				if result.Header().Get("Content-Type") != "application/pdf" || !bytes.Equal(result.Body.Bytes(), pdf) {
					t.Fatalf("%s did not return original PDF", suffix)
				}
			}
		}
	}
}
