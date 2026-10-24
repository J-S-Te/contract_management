package httpapi

import (
	core "github.com/J-S-Te/license-core"
	"github.com/gin-gonic/gin"
	"net/http"
)

func contractLicenseOperation(method, path string) core.Operation {
	if method == http.MethodPost && path == "/api/v1/audit-outbox/failed/:id/retry" {
		// 仅恢复既有安全审计投递，不重执行审批或其它业务事件。
		return core.ESSENTIAL_SERVICE
	}
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
		case "/api/v1/auth/me", "/api/v1/audit-outbox/failed":
			return core.ESSENTIAL_SERVICE
		case "/internal/v1/handover/responsibilities", "/internal/v1/handover/transfers/:itemID":
			return core.READ_HISTORY
		case "/api/v1/contracts/:contractID/export", "/api/v1/approved-contracts/:contractID/docx", "/api/v1/approved-contracts/:contractID/pdf", "/api/v1/approved-contracts/:contractID/stamped-pdf":
			return core.EXPORT_HISTORY
		case "/api/v1/dashboard", "/api/v1/opportunity-intakes", "/api/v1/opportunity-intakes/:intakeID", "/api/v1/detection-categories", "/api/v1/contracts", "/api/v1/approved-contracts", "/api/v1/signing-records", "/api/v1/signing-records/:contractID", "/api/v1/contracts/:contractID", "/api/v1/contracts/:contractID/lifecycle", "/api/v1/contracts/:contractID/preview", "/api/v1/contract-templates", "/api/v1/approvals", "/api/v1/approvals/tasks", "/api/v1/approvals/:approvalID", "/api/v1/approvals/:approvalID/contract-preview", "/api/v1/approval-rules":
			return core.READ_HISTORY
		}
	}
	// 新路由绝不继承 GET 全范围历史权限。引入读取或导出语义时
	// 添加已审查的精确路径，否则要求变更权限。
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
