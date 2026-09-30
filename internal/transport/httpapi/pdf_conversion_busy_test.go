package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	contractpdf "github.com/j-s-te/contract-management/internal/pdf"
)

func TestPDFConversionBusyIsReportedAsServiceUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/v1/approved-contracts/contract-1/pdf", nil)
	writeError(context, fmt.Errorf("acquire pdf conversion slot: %w: context deadline exceeded", contractpdf.ErrConversionUnavailable))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "CON_PDF_CONVERSION_BUSY") {
		t.Fatalf("body = %s", response.Body.String())
	}
}
