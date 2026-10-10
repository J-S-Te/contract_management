package httpapi

import (
	core "github.com/J-S-Te/license-core"
	"github.com/gin-gonic/gin"
	"net/http"
)

func contractLicenseOperation(method, path string) core.Operation {
	if method == http.MethodPost && path == "/contract_management/internal/opportunity-contract-counts/query" {
		return core.READ_HISTORY
	}
	if method == http.MethodPost && path == "/api/v1/contract-templates/:templateID/preview" {
		return core.READ_HISTORY
	}
	if method == http.MethodGet {
		switch path {
		case "/internal/v1/dashboard", "/internal/v1/settlement/completed-contracts", "/internal/v1/project/approved-contracts", "/internal/v1/project/approved-contract-references", "/internal/v1/project/approved-contracts/:contractID", "/internal/v1/project/approved-contracts/:contractID/service-items":
			return core.READ_HISTORY
		case "/api/v1/auth/me":
			return core.ESSENTIAL_SERVICE
		case "/api/v1/contracts/:contractID/export", "/api/v1/approved-contracts/:contractID/docx", "/api/v1/approved-contracts/:contractID/pdf", "/api/v1/approved-contracts/:contractID/stamped-pdf":
			return core.EXPORT_HISTORY
		case "/api/v1/dashboard", "/api/v1/opportunity-intakes", "/api/v1/opportunity-intakes/:intakeID", "/api/v1/detection-categories", "/api/v1/contracts", "/api/v1/approved-contracts", "/api/v1/signing-records", "/api/v1/signing-records/:contractID", "/api/v1/contracts/:contractID", "/api/v1/contracts/:contractID/lifecycle", "/api/v1/contracts/:contractID/preview", "/api/v1/contract-templates", "/api/v1/approvals", "/api/v1/approvals/tasks", "/api/v1/approvals/:approvalID", "/api/v1/approvals/:approvalID/contract-preview", "/api/v1/approval-rules":
			return core.READ_HISTORY
		}
	}
	// New routes never inherit GET-wide history rights. Add a reviewed exact
	// path when introducing read/export semantics, otherwise require mutation.
	return core.MUTATE_BUSINESS
}

func (h *Handler) licenseOperation() gin.HandlerFunc {
	return func(c *gin.Context) {
		op := contractLicenseOperation(c.Request.Method, c.FullPath())
		if err := h.service.CheckLicense(c.Request.Context(), op); err != nil {
			writeError(c, err)
			c.Abort()
			return
		}
		c.Next()
	}
}
