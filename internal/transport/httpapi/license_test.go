package httpapi

import (
	"context"
	core "github.com/J-S-Te/license-core"
	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"net/http"
	"net/http/httptest"
	"testing"
)

type historyOnlyLicense struct{}

func (historyOnlyLicense) Check(_ context.Context, op core.Operation) error {
	if op == core.MUTATE_BUSINESS {
		return core.ErrDenied
	}
	return nil
}

func TestLicensePolicyExactRoutesAndNonGETHistory(t *testing.T) {
	for _, c := range []struct {
		method, path string
		op           core.Operation
	}{
		{"POST", "/api/v1/contract-templates/:templateID/preview", core.READ_HISTORY},
		{"GET", "/api/v1/contracts/:contractID/export", core.EXPORT_HISTORY},
		{"GET", "/api/v1/contracts", core.READ_HISTORY},
		{"POST", "/api/v1/contracts", core.MUTATE_BUSINESS},
		{"GET", "/api/v1/new-business-side-effect", core.MUTATE_BUSINESS},
		{"POST", "/contract_management/internal/opportunity-contract-counts/query", core.READ_HISTORY},
		{"GET", "/internal/v1/project/approved-contracts/:contractID/service-items", core.READ_HISTORY},
		{"POST", "/internal/v1/project/new-side-effect", core.MUTATE_BUSINESS},
		{"POST", "/api/v1/contracts/preview", core.MUTATE_BUSINESS},
	} {
		if got := contractLicenseOperation(c.method, c.path); got != c.op {
			t.Errorf("%s %s=%v", c.method, c.path, got)
		}
	}
}

func TestHistoryOnlyRuntimeDeniesBeforeHandlerPreservesHistory(t *testing.T) {
	h := &Handler{service: &application.Service{LicenseGate: historyOnlyLicense{}}}
	r := gin.New()
	r.Use(h.licenseOperation())
	called := false
	for _, p := range []string{"/api/v1/contracts", "/api/v1/new-business-side-effect"} {
		r.GET(p, func(c *gin.Context) { called = true; c.Status(200) })
	}
	for _, tc := range []struct {
		path   string
		status int
		calls  bool
	}{{"/api/v1/contracts", 200, true}, {"/api/v1/new-business-side-effect", 403, false}} {
		called = false
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if w.Code != tc.status || called != tc.calls {
			t.Fatalf("%s code%d called%v", tc.path, w.Code, called)
		}
	}
}
