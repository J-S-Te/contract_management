package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/auditctx"
	"github.com/j-s-te/contract-management/internal/auditoutbox"
	"github.com/j-s-te/contract-management/internal/contractdocument"
	"github.com/j-s-te/contract-management/internal/docx"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
	contractpdf "github.com/j-s-te/contract-management/internal/pdf"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/oklog/ulid/v2"
)

type Identity interface {
	Authenticate(context.Context, *http.Request) (application.Principal, error)
}

type OIDCFlow interface {
	Login(http.ResponseWriter, *http.Request)
	Callback(http.ResponseWriter, *http.Request)
	Logout(http.ResponseWriter, *http.Request)
}

type LocalSessionLogout interface {
	LogoutLocal(http.ResponseWriter, *http.Request)
}

type BackchannelLogout interface {
	BackchannelLogout(http.ResponseWriter, *http.Request)
}

type PublicPathResolver interface {
	PublicPath(string) string
}

type Handler struct {
	service     *application.Service
	identity    Identity
	audit       platform.AuditReporter
	auditOutbox *auditoutbox.GORMStore
	// handover 仅在启用人员交接机器协议时非空；未启用时不注册该组路由。
	handover *HandoverIntegration
}

// HandoverIntegrationOptions 配置基础平台调用的人员交接机器协议端点。
// 与结算/项目集成一致：租户边界只来自令牌绑定，请求体不能覆盖。
type HandoverIntegrationOptions struct {
	Enabled        bool
	RequireBearer  bool
	BearerVerifier platform.ClientCredentialsTokenVerifier
}

// DashboardIntegrationOptions 数据看板系统（服务器到服务器）机器接入配置。
type DashboardIntegrationOptions struct {
	Enabled        bool
	RequireBearer  bool
	BearerVerifier platform.ClientCredentialsTokenVerifier
}

// SettlementIntegrationOptions 配置结算系统读取已完成合同的服务器到服务器接入。
// 该接口只返回当前令牌绑定租户的数据，不能使用请求头覆盖租户边界。
type SettlementIntegrationOptions struct {
	Enabled        bool
	RequireBearer  bool
	BearerVerifier platform.ClientCredentialsTokenVerifier
}

type ProjectIntegrationOptions struct {
	Enabled        bool
	RequireBearer  bool
	BearerVerifier platform.ClientCredentialsTokenVerifier
}

type CRMSignedCountIntegrationOptions struct {
	Enabled        bool
	RequireBearer  bool
	BearerVerifier platform.ClientCredentialsTokenVerifier
	// 商机接收是 CRM → 合同写入路径，有意
	// 使用独立校验器和范围，不复用只读签约计数的
	// 机器身份。
	OpportunityIntakeEnabled        bool
	OpportunityIntakeRequireBearer  bool
	OpportunityIntakeBearerVerifier platform.ClientCredentialsTokenVerifier
}

// NewRouter 创建兼容现有调用方的合同管理路由。
func NewRouter(service *application.Service, identity Identity, dashboardOptions *DashboardIntegrationOptions, audits ...platform.AuditReporter) *gin.Engine {
	return newRouter(service, identity, dashboardOptions, nil, nil, nil, nil, nil, nil, audits...)
}

// NewRouterWithSettlement 创建包含结算系统内部读取接口的路由。
func NewRouterWithSettlement(service *application.Service, identity Identity, dashboardOptions *DashboardIntegrationOptions, settlementOptions *SettlementIntegrationOptions, audits ...platform.AuditReporter) *gin.Engine {
	return newRouter(service, identity, dashboardOptions, settlementOptions, nil, nil, nil, nil, nil, audits...)
}

func NewRouterWithIntegrations(service *application.Service, identity Identity, dashboardOptions *DashboardIntegrationOptions, settlementOptions *SettlementIntegrationOptions, projectOptions *ProjectIntegrationOptions, audits ...platform.AuditReporter) *gin.Engine {
	return newRouter(service, identity, dashboardOptions, settlementOptions, projectOptions, nil, nil, nil, nil, audits...)
}

func NewRouterWithAllIntegrations(service *application.Service, identity Identity, dashboardOptions *DashboardIntegrationOptions, settlementOptions *SettlementIntegrationOptions, projectOptions *ProjectIntegrationOptions, crmSignedCountOptions *CRMSignedCountIntegrationOptions, handoverService *application.HandoverService, handoverOptions *HandoverIntegrationOptions, audits ...platform.AuditReporter) *gin.Engine {
	return newRouter(service, identity, dashboardOptions, settlementOptions, projectOptions, crmSignedCountOptions, handoverService, handoverOptions, nil, audits...)
}

func NewRouterWithAuditOutbox(service *application.Service, identity Identity, dashboardOptions *DashboardIntegrationOptions, settlementOptions *SettlementIntegrationOptions, projectOptions *ProjectIntegrationOptions, crmSignedCountOptions *CRMSignedCountIntegrationOptions, handoverService *application.HandoverService, handoverOptions *HandoverIntegrationOptions, auditOutbox *auditoutbox.GORMStore, audits ...platform.AuditReporter) *gin.Engine {
	return newRouter(service, identity, dashboardOptions, settlementOptions, projectOptions, crmSignedCountOptions, handoverService, handoverOptions, auditOutbox, audits...)
}

// newRouter 组装合同现有认证、许可、审计及业务路由。
// 参数：service/identity为业务与身份依赖，各Options为独立机器集成配置，handoverService为交接服务，auditOutbox/audits为持久审计依赖。
// 返回值：完成中间件及路由注册的Gin引擎。
func newRouter(service *application.Service, identity Identity, dashboardOptions *DashboardIntegrationOptions, settlementOptions *SettlementIntegrationOptions, projectOptions *ProjectIntegrationOptions, crmSignedCountOptions *CRMSignedCountIntegrationOptions, handoverService *application.HandoverService, handoverOptions *HandoverIntegrationOptions, auditOutbox *auditoutbox.GORMStore, audits ...platform.AuditReporter) *gin.Engine {
	var audit platform.AuditReporter
	if len(audits) > 0 {
		audit = audits[0]
	}
	h := &Handler{service: service, identity: identity, audit: audit, auditOutbox: auditOutbox}
	if handoverOptions != nil && handoverOptions.Enabled && handoverService != nil {
		h.handover = &HandoverIntegration{service: handoverService, options: *handoverOptions}
	}
	r := gin.New()
	r.Use(requestID(), recoverer())
	if oidcFlow, ok := identity.(OIDCFlow); ok {
		r.GET("/", h.webHome)
		r.GET("/auth/login", func(c *gin.Context) { oidcFlow.Login(c.Writer, c.Request) })
		r.GET("/auth/callback", func(c *gin.Context) { oidcFlow.Callback(c.Writer, c.Request) })
		r.GET("/auth/logout", func(c *gin.Context) { oidcFlow.Logout(c.Writer, c.Request) })
		r.GET("/logged-out", h.loggedOut)
	}
	if localLogout, ok := identity.(LocalSessionLogout); ok {
		r.POST("/auth/local-logout", func(c *gin.Context) { localLogout.LogoutLocal(c.Writer, c.Request) })
	}
	if backchannelLogout, ok := identity.(BackchannelLogout); ok {
		r.POST("/auth/backchannel-logout", func(c *gin.Context) { backchannelLogout.BackchannelLogout(c.Writer, c.Request) })
	}
	r.GET("/healthz", func(c *gin.Context) {
		writeJSON(c, http.StatusOK, envelope{Code: "OK", Message: "ok", Data: map[string]string{"status": "up", "audit": auditAvailability(audit)}})
	})
	r.GET("/readyz", func(c *gin.Context) {
		status := http.StatusOK
		state := "ready"
		phoneKeyState := "ready"
		// 签署人手机号列加密密钥必填（SEC-D7，*_KEY_BASE64 模式）：缺失或非法时
		// 就绪检查失败关闭，避免带密钥缺口上线后所有寄送写入只能报错。
		if err := contract.SigningPhoneKeyStatus(); err != nil {
			status = http.StatusServiceUnavailable
			state = "not_ready"
			phoneKeyState = "missing"
		}
		if auditRequired() && audit == nil {
			status = http.StatusServiceUnavailable
			state = "not_ready"
		}
		writeJSON(c, status, envelope{Code: "OK", Message: state, Data: map[string]string{"status": state, "audit": auditAvailability(audit), "signing_phone_key": phoneKeyState}})
	})
	if dashboardOptions != nil && dashboardOptions.Enabled {
		internal := r.Group("/internal/v1")
		internal.Use(h.authenticateDashboardIntegration(*dashboardOptions), h.licenseOperation())
		internal.GET("/dashboard", h.dashboard)
	}
	if settlementOptions != nil && settlementOptions.Enabled {
		internal := r.Group("/internal/v1/settlement")
		internal.Use(h.authenticateServiceIntegration(*settlementOptions, "结算系统"), h.licenseOperation())
		internal.GET("/completed-contracts", h.listSettlementCompletedContracts)
	}
	if projectOptions != nil && projectOptions.Enabled {
		internal := r.Group("/internal/v1/project")
		internal.Use(h.authenticateServiceIntegration(*projectOptions, "项目管理系统"), h.licenseOperation())
		internal.GET("/approved-contracts", h.listProjectApprovedContracts)
		internal.GET("/approved-contract-references", h.listProjectApprovedContractReferences)
		internal.GET("/approved-contracts/:contractID", h.getProjectApprovedContract)
		internal.GET("/approved-contracts/:contractID/service-items", h.getProjectApprovedContractServiceItems)
	}
	if crmSignedCountOptions != nil && crmSignedCountOptions.Enabled {
		internal := r.Group("/contract_management/internal/opportunity-contract-counts")
		internal.Use(h.authenticateServiceIntegration(struct {
			Enabled        bool
			RequireBearer  bool
			BearerVerifier platform.ClientCredentialsTokenVerifier
		}{crmSignedCountOptions.Enabled, crmSignedCountOptions.RequireBearer, crmSignedCountOptions.BearerVerifier}, "客户与商机管理系统"), h.licenseOperation())
		internal.POST("/query", h.countSignedContractsByOpportunity)
	}
	if crmSignedCountOptions != nil && crmSignedCountOptions.OpportunityIntakeEnabled {
		internal := r.Group("/internal/opportunity-signed-events")
		internal.Use(
			h.authenticateServiceIntegration(struct {
				Enabled        bool
				RequireBearer  bool
				BearerVerifier platform.ClientCredentialsTokenVerifier
			}{crmSignedCountOptions.OpportunityIntakeEnabled, crmSignedCountOptions.OpportunityIntakeRequireBearer, crmSignedCountOptions.OpportunityIntakeBearerVerifier}, "客户与商机管理系统"),
			h.licenseOperation(),
			h.grantOpportunityIntakePermission(),
		)
		internal.POST("", h.acceptOpportunityIntake)
	}
	if h.handover != nil {
		// 人员离职交接机器协议：租户边界来自令牌绑定，与结算/项目集成同一模式。
		internal := r.Group("/internal/v1/handover")
		internal.Use(
			h.authenticateServiceIntegration(struct {
				Enabled        bool
				RequireBearer  bool
				BearerVerifier platform.ClientCredentialsTokenVerifier
			}{Enabled: h.handover.options.Enabled, RequireBearer: h.handover.options.RequireBearer, BearerVerifier: h.handover.options.BearerVerifier}, "基础平台交接"),
			h.licenseOperation(),
		)
		h.registerHandoverRoutes(internal)
	}
	// SEC-D11：cookie 会话写请求必须通过同源 Origin 校验（失败关闭）；
	// 放在 auditWrites 之后使跨站拒绝尝试同样进入审计。
	api := r.Group("/api/v1", h.authenticate(), h.auditWrites(), h.requireSameOriginWrite(), h.licenseOperation())
	api.GET("/auth/me", h.me)
	api.GET("/audit-outbox/failed", h.listFailedAuditEvents)
	api.POST("/audit-outbox/failed/:id/retry", h.retryFailedAuditEvent)
	api.GET("/dashboard", h.dashboard)
	api.POST("/opportunity-intakes", h.acceptOpportunityIntake)
	api.GET("/opportunity-intakes", h.listOpportunityIntakes)
	api.GET("/opportunity-intakes/:intakeID", h.getOpportunityIntake)
	api.POST("/opportunity-intakes/:intakeID/reviews", h.reviewOpportunityIntake)
	api.POST("/contracts", h.createContract)
	api.PUT("/contracts/:contractID/draft", h.updateContractDraft)
	api.POST("/contracts/external", h.createExternalContract)
	api.GET("/detection-categories", h.listDetectionCategories)
	api.GET("/contracts", h.listContracts)
	api.GET("/approved-contracts", h.listApprovedContracts)
	api.GET("/approved-contracts/:contractID/docx", h.downloadApprovedDOCX)
	api.GET("/approved-contracts/:contractID/pdf", h.downloadApprovedPDF)
	api.PUT("/approved-contracts/:contractID/stamped-pdf", h.uploadStampedPDF)
	api.GET("/approved-contracts/:contractID/stamped-pdf", h.downloadStampedPDF)
	api.GET("/signing-records", h.listSigningRecords)
	api.GET("/signing-records/:contractID", h.getSigningRecord)
	api.PUT("/signing-records/:contractID/shipment", h.saveSigningShipment)
	api.POST("/signing-records/:contractID/received", h.markSigningReceived)
	api.POST("/signing-records/:contractID/reminders", h.recordSigningReminder)
	api.POST("/signing-records/:contractID/confirm", h.confirmSigning)
	api.GET("/contracts/:contractID", h.getContract)
	api.GET("/contracts/:contractID/lifecycle", h.listContractLifecycle)
	api.GET("/contracts/:contractID/document-descriptor", h.describeContractDocument)
	api.GET("/contract-source-capabilities", h.contractSourceCapabilities)
	h.registerContractImports(api)
	api.GET("/contracts/:contractID/source", h.downloadContractSource)
	api.GET("/contracts/:contractID/preview", h.previewContract)
	api.GET("/contracts/:contractID/export", h.exportContract)
	api.GET("/contract-templates", h.listTemplates)
	api.POST("/contract-templates", h.createTemplate)
	api.PUT("/contract-templates/:templateID", h.updateTemplate)
	api.PUT("/contract-templates/:templateID/source", h.replaceTemplateSource)
	api.DELETE("/contract-templates/:templateID", h.deleteTemplate)
	api.POST("/contract-templates/:templateID/preview", h.previewTemplate)
	api.POST("/contracts/:contractID/submit-approval", h.submitApproval)
	api.POST("/contracts/:contractID/status-changes", h.changeStatus)
	api.GET("/approvals", h.listApprovals)
	api.GET("/approvals/tasks", h.listTasks)
	api.GET("/approvals/:approvalID", h.getApproval)
	api.GET("/approvals/:approvalID/contract-preview", h.previewApprovalContract)
	api.GET("/approvals/:approvalID/contract-source", h.downloadApprovalSource)
	api.POST("/approvals/:approvalID/recover", h.recoverApproval)
	api.GET("/approval-rules", h.listRules)
	api.POST("/approval-rules", h.createRule)
	api.PUT("/approval-rules/:ruleID", h.updateRule)
	api.DELETE("/approval-rules/:ruleID", h.deleteRule)
	for _, action := range []string{"approve", "reject", "sign", "transfer", "return", "withdraw", "urge", "comments"} {
		api.POST("/approvals/:approvalID/"+action, h.command(action))
	}
	return r
}

type opportunitySignedCountQuery struct {
	OpportunityIDs []string `json:"opportunity_ids"`
}

func (h *Handler) countSignedContractsByOpportunity(c *gin.Context) {
	var input opportunitySignedCountQuery
	if !decode(c, &input) {
		return
	}
	if len(input.OpportunityIDs) == 0 || len(input.OpportunityIDs) > 1000 {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "商机编号数量必须在 1 到 1000 之间", nil)
		return
	}
	seen := make(map[string]struct{}, len(input.OpportunityIDs))
	for index, value := range input.OpportunityIDs {
		value = strings.TrimSpace(value)
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil || parsed == 0 || strconv.FormatUint(parsed, 10) != value {
			writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "商机编号必须是规范的正整数", nil)
			return
		}
		if _, exists := seen[value]; exists {
			writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "商机编号不能重复", nil)
			return
		}
		seen[value] = struct{}{}
		input.OpportunityIDs[index] = value
	}
	counts, err := h.service.CountSignedContractsByOpportunityIDs(c.Request.Context(), principal(c).TenantID, input.OpportunityIDs)
	if err != nil {
		writeError(c, err)
		return
	}
	items := make([]map[string]any, 0, len(input.OpportunityIDs))
	for _, opportunityID := range input.OpportunityIDs {
		items = append(items, map[string]any{"opportunity_id": opportunityID, "signed_contract_count": counts[opportunityID]})
	}
	writeData(c, http.StatusOK, map[string]any{"items": items})
}

func auditAvailability(audit platform.AuditReporter) string {
	if audit == nil {
		return "disabled"
	}
	return "enabled"
}

func auditRequired() bool {
	raw := strings.TrimSpace(os.Getenv("PLATFORM_AUDIT_REQUIRED"))
	if raw != "" {
		required, err := strconv.ParseBool(raw)
		return err == nil && required
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("PLATFORM_ENVIRONMENT_CODE")), "prod")
}

func (h *Handler) acceptOpportunityIntake(c *gin.Context) {
	if !principal(c).Has("opportunity_intake.receive") {
		writeError(c, application.ErrForbidden)
		return
	}
	var input application.OpportunityIntake
	if !decode(c, &input) {
		return
	}
	if input.TenantID == "" {
		input.TenantID = principal(c).TenantID
	} else if principal(c).TenantID != "" && input.TenantID != principal(c).TenantID {
		writeError(c, application.ErrForbidden)
		return
	}
	if input.IntakeID == "" {
		input.IntakeID = ulid.Make().String()
	}
	result, err := h.service.AcceptOpportunityIntake(c.Request.Context(), input)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusAccepted, map[string]any{"intake_id": result.IntakeID, "event_id": result.EventID, "status": result.Status})
}

func (h *Handler) listOpportunityIntakes(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("page_size"))
	page, err := h.service.ListOpportunityIntakes(c.Request.Context(), principal(c), c.Query("status"), c.Query("cursor"), limit)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, page)
}
func (h *Handler) getOpportunityIntake(c *gin.Context) {
	item, err := h.service.GetOpportunityIntake(c.Request.Context(), principal(c), c.Param("intakeID"))
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, item)
}
func (h *Handler) reviewOpportunityIntake(c *gin.Context) {
	var input application.OpportunityIntakeReview
	if !decode(c, &input) {
		return
	}
	item, err := h.service.ReviewOpportunityIntake(c.Request.Context(), principal(c), c.Param("intakeID"), input, c.GetHeader("Idempotency-Key"))
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, item)
}

func (h *Handler) me(c *gin.Context) {
	p := principal(c)
	permissions := make([]string, 0, len(p.Permissions))
	for permission, granted := range p.Permissions {
		if granted {
			permissions = append(permissions, permission)
		}
	}
	sort.Strings(permissions)
	roles := append([]string(nil), p.Roles...)
	role := map[string]string{}
	if len(roles) > 0 {
		role["code"] = roles[0]
	}
	writeData(c, http.StatusOK, map[string]any{
		"tenant_id": p.TenantID, "user_id": p.UserID, "identity_id": p.IdentityID, "person_id": p.PersonID,
		"display_name": p.DisplayName, "user_name": p.UserName, "email": p.Email, "role": role, "roles": roles,
		"permissions": permissions, "data_scopes": p.DataScopes, "authorization_revision": p.AuthorizationRevision,
		"catalog_version": p.CatalogVersion,
	})
}

func (h *Handler) dashboard(c *gin.Context) {
	summary, err := h.service.ContractDashboard(c.Request.Context(), principal(c))
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, summary)
}

func (h *Handler) listContractLifecycle(c *gin.Context) {
	events, err := h.service.ListContractLifecycle(c.Request.Context(), principal(c), c.Param("contractID"))
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, events)
}

// auditWrites 在业务 handler 完成后把写请求（与敏感读）的结果上报平台审计。
//
// 安全理由（SEC-D4b）：原实现对审计管道 失败时放行 —— reporter 为 nil 直接放行、
// Report 错误被空白标识符丢弃，凭据失效或 ingest 故障时合同审批/签署/状态变更等
// 所有写入都没有审计且不可见。现在：
//  1. 上报失败一律 error 日志（含路由与请求 id），不再静默；
//  2. 强制审计模式（PLATFORM_AUDIT_REQUIRED 或 PLATFORM_ENVIRONMENT_CODE=prod）下：
//     - reporter 缺失 => 在执行业务 handler 之前就拒绝请求（503），不产生无审计的写；
//     - 写请求与敏感读先隔离响应，Report 成功才提交；失败则丢弃业务响应并返回
//     503，保证“审计写不进去 ⇒ 客户端拿不到成功”。
//
// 非强制模式保持原有放行语义，仅补上可告警的错误日志。
func (h *Handler) auditWrites() gin.HandlerFunc {
	return func(c *gin.Context) {
		if skipAudit(c.Request.Method, c.Request.URL.Path) {
			c.Next()
			return
		}
		route := c.Request.Method + " " + c.Request.URL.Path
		requestID := requestIDFrom(c.Request.Context())
		if h.audit == nil {
			if !auditRequired() {
				// 非强制模式：审计按配置关闭，/healthz 与 /readyz 已暴露 disabled 状态。
				c.Next()
				return
			}
			slog.ErrorContext(c.Request.Context(), "audit reporter unavailable, rejecting request",
				"route", route,
				"request_id", requestID,
			)
			c.Abort()
			writeAuditError(c, "AUDIT_UNAVAILABLE", "审计服务不可用，操作已被拒绝")
			return
		}
		required := auditRequired()
		origWriter := c.Writer
		var buffered *auditBuffer
		if required {
			buffered = newAuditBuffer(origWriter)
			buffered.head = c.Request.Method == http.MethodHead
			c.Writer = buffered
		}
		// handler panic 展平时先恢复真实 writer，外层 recoverer 才能把 500 写出去；
		// 缓冲中的半成品响应被直接丢弃，不会被当成成功提交。
		defer func() { c.Writer = origWriter }()
		// 来源信息必须在业务 handler 执行前注入：事务内审计在 handler 内部读取它，
		// 放到 c.Next() 之后只能得到空值。
		clientIP, ipSource := requestClientIP(c.Request), requestIPSource(c.Request)
		// 同一请求内的业务审计要能定位到稳定操作者：主体由上游 authenticate 建立。
		c.Request = c.Request.WithContext(auditctx.WithMeta(c.Request.Context(), auditctx.Meta{
			ClientIP: clientIP, IPSource: ipSource, RequestID: requestID, ActorID: principal(c).UserID,
		}))
		c.Next()
		status := c.Writer.Status()
		if status == 0 {
			status = http.StatusOK
		}
		if buffered != nil && buffered.err != nil {
			status = http.StatusServiceUnavailable
		}
		p := principal(c)
		resourceType, resourceID := auditResource(c)
		event := platform.AuditEvent{ActorID: p.UserID, Action: auditAction(c.Request), ResourceType: resourceType, ResourceID: resourceID, RequestID: requestID, CorrelationID: requestID, Result: auditResult(status), RiskLevel: auditRiskLevel(c.Request.Method, c.Request.URL.Path, status), ReasonCode: strconv.Itoa(status), ClientIP: clientIP, IPSource: ipSource}
		if err := h.audit.Report(c.Request.Context(), event); err != nil {
			slog.ErrorContext(c.Request.Context(), "report platform audit failed",
				"route", route,
				"request_id", requestID,
				"status", status,
				"error", err,
			)
			if required {
				// 强制审计模式：审计事件写入失败必须拒绝该请求，不能静默成功。
				c.Writer = origWriter
				writeAuditError(c, "AUDIT_WRITE_REJECTED", "审计记录写入失败，操作未被确认")
			}
			return
		}
		if buffered != nil {
			if buffered.err != nil {
				c.Writer = origWriter
				slog.ErrorContext(c.Request.Context(), "audited response capture failed", "route", route, "request_id", requestID, "error", buffered.err)
				writeAuditError(c, "AUDIT_RESPONSE_UNAVAILABLE", "响应超出安全审计交付范围，请联系管理员")
				return
			}
			buffered.flushTo(origWriter)
		}
	}
}

// HEAD须遵守无响应体语义；失败时只使用原始writer，不能保留文档响应头。
func writeAuditError(c *gin.Context, code, message string) {
	if c.Request.Method == http.MethodHead {
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.Status(http.StatusServiceUnavailable)
		c.Writer.WriteHeaderNow()
		return
	}
	writeEnvelopeError(c, http.StatusServiceUnavailable, code, message, nil)
}

// isUnsafeMethod 判定产生业务副作用的不安全HTTP方法。
func isUnsafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

// auditResult 区分成功、拒绝与失败：401/403 记为 DENIED，便于识别越权/未授权尝试。
func auditResult(status int) string {
	switch {
	case status >= 200 && status < 400:
		return "SUCCESS"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "DENIED"
	default:
		return "FAILURE"
	}
}

// auditRiskLevel 计算粗略风险等级，使敏感/破坏性/被拒绝操作能在审计查询中被筛出。
func auditRiskLevel(method, path string, status int) string {
	if status >= http.StatusInternalServerError {
		return "HIGH"
	}
	lowered := strings.ToLower(path)
	if method == http.MethodDelete || containsAnyAudit(lowered,
		"delete", "approval", "approve", "reject", "sign", "password", "credential",
		"secret", "permission", "role", "authorization", "admin") {
		return "HIGH"
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return "MEDIUM"
	}
	return "LOW"
}

func containsAnyAudit(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

// skipAudit 跳过普通读请求；写请求和敏感读（下载/导出/令牌签发）仍会审计。
func skipAudit(method, path string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return !sensitiveRead(path)
	default:
		return false
	}
}

// sensitiveRead 识别正文、原件及导出读取，确保实际下载路径受审计约束。
// 参数：path为当前请求路径，包含普通及导入原件入口。
// 返回值：涉及敏感原件或导出返回true，否则返回false。
func sensitiveRead(path string) bool {
	lowered := strings.ToLower(path)
	if containsAnyAudit(lowered, "embed", "download", "export") {
		return true
	}
	for _, suffix := range []string{"/docx", "/pdf", "/stamped-pdf", "/source", "/contract-source"} {
		if strings.HasSuffix(lowered, suffix) {
			return true
		}
	}
	return false
}

// requestClientIP 从受管前端代理提取公开客户端
// 地址。X-Real-IP 为权威值，最右侧 XFF 值作为
// 回退，防止客户端提供的最左值伪造审计记录。
// 仅在直接对端是可信反向代理时信任转发头，
// 确保可直接访问的服务不会被客户端请求头
// 伪造审计源地址。
// 显式可信代理列表：只有列入该列表的直接对端才允许提供 X-Real-IP / X-Forwarded-For。
// 由组合根在启动时调用 ConfigureTrustedProxies 注入（默认仅回环）。
var trustedProxies atomic.Value

// ConfigureTrustedProxies 由组合根注入可信代理（CIDR 或单地址）。
func ConfigureTrustedProxies(values []string) {
	trustedProxies.Store(parseTrustedProxies(values))
}

func currentTrustedProxies() []netip.Prefix {
	if value, ok := trustedProxies.Load().([]netip.Prefix); ok {
		return value
	}
	return parseTrustedProxies(nil)
}

func parseTrustedProxies(values []string) []netip.Prefix {
	if len(values) == 0 {
		values = []string{"127.0.0.1/32", "::1/128"}
	}
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(value); err == nil {
			prefixes = append(prefixes, prefix)
			continue
		}
		if address, err := netip.ParseAddr(value); err == nil {
			prefixes = append(prefixes, netip.PrefixFrom(address, address.BitLen()))
		}
	}
	return prefixes
}

// clientIPAddress 接受任意合法单播地址（含办公网/VPN/回环）。
// AUD-IP-2026：按地址段过滤会把真实办公网地址丢掉；是否可信由获取链路决定。
func clientIPAddress(value string) *netip.Addr {
	addr, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || addr.IsUnspecified() || addr.IsMulticast() {
		return nil
	}
	return &addr
}

// publicClientIP 保留旧名称以兼容既有调用点。
func publicClientIP(value string) *netip.Addr { return clientIPAddress(value) }

func trustedProxyPeer(remoteAddr string) bool {
	peer := peerAddress(remoteAddr)
	if peer == "" {
		return false
	}
	address, err := netip.ParseAddr(peer)
	if err != nil {
		return false
	}
	for _, prefix := range currentTrustedProxies() {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

// resolveClientIP 返回本次请求的操作地址与来源（DIRECT / TRUSTED_PROXY / UNKNOWN）。
func resolveClientIP(request *http.Request) (string, string) {
	if request == nil {
		return "", "UNKNOWN"
	}
	peer := peerAddress(request.RemoteAddr)
	if peer == "" {
		return "", "UNKNOWN"
	}
	if !trustedProxyPeer(request.RemoteAddr) {
		return peer, "DIRECT"
	}
	if ip := clientIPAddress(request.Header.Get("X-Real-IP")); ip != nil {
		if value := ip.String(); value != peer {
			return value, "TRUSTED_PROXY"
		}
	}
	values := strings.Split(request.Header.Get("X-Forwarded-For"), ",")
	for index := len(values) - 1; index >= 0; index-- {
		ip := clientIPAddress(values[index])
		if ip == nil {
			continue
		}
		value := ip.String()
		if trustedProxyPeer(value) {
			continue
		}
		if value != peer {
			return value, "TRUSTED_PROXY"
		}
	}
	return peer, "DIRECT"
}

func requestClientIP(request *http.Request) string {
	clientIP, _ := resolveClientIP(request)
	return clientIP
}

func requestIPSource(request *http.Request) string {
	_, source := resolveClientIP(request)
	return source
}

func peerAddress(remoteAddr string) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err != nil {
		host = strings.TrimSpace(remoteAddr)
	}
	address, err := netip.ParseAddr(host)
	if err != nil || address.IsUnspecified() || address.IsMulticast() {
		return ""
	}
	return address.String()
}

func auditAction(r *http.Request) string {
	return "CONTRACT_MANAGEMENT:" + r.Method + ":" + strings.ReplaceAll(strings.Trim(r.URL.Path, "/"), "/", ".")
}
func auditResource(c *gin.Context) (string, string) {
	if id := c.Param("contractID"); id != "" {
		return "CONTRACT", id
	}
	if id := c.Param("approvalID"); id != "" {
		return "APPROVAL", id
	}
	if id := c.Param("ruleID"); id != "" {
		return "APPROVAL_RULE", id
	}
	if id := c.Param("templateID"); id != "" {
		return "CONTRACT_TEMPLATE", id
	}
	if strings.Contains(c.Request.URL.Path, "contract-templates") {
		return "CONTRACT_TEMPLATE", ""
	}
	if strings.Contains(c.Request.URL.Path, "approval-rules") {
		return "APPROVAL_RULE", ""
	}
	return "CONTRACT", ""
}

func (h *Handler) listRules(c *gin.Context) {
	rules, err := h.service.ListRules(c.Request.Context(), principal(c))
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, rules)
}

func (h *Handler) createRule(c *gin.Context) {
	var rule approval.Rule
	if !decode(c, &rule) {
		return
	}
	created, err := h.service.CreateRule(c.Request.Context(), principal(c), rule)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusCreated, created)
}

func (h *Handler) updateRule(c *gin.Context) {
	var rule approval.Rule
	if !decode(c, &rule) {
		return
	}
	rule.ID = c.Param("ruleID")
	updated, err := h.service.UpdateRule(c.Request.Context(), principal(c), rule)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, updated)
}

func (h *Handler) deleteRule(c *gin.Context) {
	version, err := strconv.ParseUint(c.Query("version"), 10, 64)
	if err != nil {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "version 参数不合法", nil)
		return
	}
	if err := h.service.DeleteRule(c.Request.Context(), principal(c), c.Param("ruleID"), version); err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, map[string]string{"status": "deleted"})
}

type createContractRequest struct {
	Number              string                 `json:"contract_number"`
	Title               string                 `json:"title"`
	ContractType        string                 `json:"contract_type"`
	ServiceType         string                 `json:"service_type"`
	OpportunityID       string                 `json:"opportunity_id"`
	OpportunityName     string                 `json:"opportunity_name"`
	CRMCustomerID       uint64                 `json:"crm_customer_id"`
	CustomerName        string                 `json:"customer_name"`
	CustomerAddress     string                 `json:"customer_address"`
	CustomerContact     string                 `json:"customer_contact"`
	CustomerPhone       string                 `json:"customer_phone"`
	Systems             []contract.SystemInfo  `json:"systems"`
	ServiceItems        []contract.ServiceItem `json:"service_items"`
	CustomerCreditLevel string                 `json:"customer_credit_level"`
	AmountMinor         int64                  `json:"amount_minor"`
	Currency            string                 `json:"currency"`
	PaymentTerms        []contract.PaymentTerm `json:"payment_terms"`
	Content             string                 `json:"content"`
	TemplateID          string                 `json:"template_id"`
	TemplateValues      map[string]string      `json:"template_values"`
	StartDate           *time.Time             `json:"start_date"`
	EndDate             *time.Time             `json:"end_date"`
}

type externalContractMetadata struct {
	Title               string                 `json:"title"`
	ContractType        string                 `json:"contract_type"`
	ServiceType         string                 `json:"service_type"`
	OpportunityID       string                 `json:"opportunity_id"`
	OpportunityName     string                 `json:"opportunity_name"`
	CRMCustomerID       uint64                 `json:"crm_customer_id"`
	CustomerName        string                 `json:"customer_name"`
	CustomerAddress     string                 `json:"customer_address"`
	CustomerContact     string                 `json:"customer_contact"`
	CustomerPhone       string                 `json:"customer_phone"`
	Systems             []contract.SystemInfo  `json:"systems"`
	ServiceItems        []contract.ServiceItem `json:"service_items"`
	CustomerCreditLevel string                 `json:"customer_credit_level"`
	AmountMinor         int64                  `json:"amount_minor"`
	Currency            string                 `json:"currency"`
	PaymentTerms        []contract.PaymentTerm `json:"payment_terms"`
	// Content 仅用于兼容前端共享的 buildNewContractPayload()：模板路径会发送
	// content，外部合同路径复用同一构造函数。外部合同的正文来自上传的 DOCX
	// （contract.Contract.Document），因此该字段收下即丢弃，不参与建单。
	// 缺少本字段时 DisallowUnknownFields 会以 json: unknown field "content"
	// 失败（HTTP 422 CON_VALIDATION_ERROR「外部合同元数据不合法」）。
	Content   string     `json:"content"`
	StartDate *time.Time `json:"start_date"`
	EndDate   *time.Time `json:"end_date"`
}

func (h *Handler) createContract(c *gin.Context) {
	var body createContractRequest
	if !decode(c, &body) {
		return
	}
	created, err := h.service.CreateContract(c.Request.Context(), principal(c), contract.Contract{Number: body.Number, Title: body.Title, Type: body.ContractType, ServiceType: body.ServiceType, OpportunityID: body.OpportunityID, OpportunityName: body.OpportunityName, CRMCustomerID: body.CRMCustomerID, CustomerName: body.CustomerName, CustomerAddress: body.CustomerAddress, CustomerContact: body.CustomerContact, CustomerPhone: body.CustomerPhone, Systems: body.Systems, ServiceItems: body.ServiceItems, CustomerCreditLevel: body.CustomerCreditLevel, AmountMinor: body.AmountMinor, Currency: body.Currency, PaymentTerms: body.PaymentTerms, Content: body.Content, TemplateID: body.TemplateID, TemplateValues: body.TemplateValues, StartDate: body.StartDate, EndDate: body.EndDate})
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusCreated, created)
}

func (h *Handler) listDetectionCategories(c *gin.Context) {
	items, err := h.service.ListDetectionCategories(c.Request.Context(), principal(c))
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

// createExternalContract 接收单个源文件及严格结构化元数据创建草稿。
// 参数：c为multipart请求，大小受格式及入口开关共同限制。
// 返回值：无；通过统一响应输出草稿或字段/文件校验错误。
func (h *Handler) createExternalContract(c *gin.Context) {
	const metadataLimit = 1 << 20
	if _, ok := principal(c).Scope("contract.create"); !ok {
		writeError(c, application.ErrForbidden)
		return
	}
	maxFileSize := int64(docx.MaxDocumentSize)
	if h.service != nil && h.service.ExternalPDFEnabled {
		maxFileSize = contractdocument.MaxPDFSize
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxFileSize+(2<<20))
	if err := c.Request.ParseMultipartForm(metadataLimit); err != nil {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "外部合同上传参数不合法或文件过大", nil)
		return
	}
	if c.Request.MultipartForm != nil {
		defer c.Request.MultipartForm.RemoveAll()
	}
	if c.Request.MultipartForm == nil || len(c.Request.MultipartForm.Value) != 1 || len(c.Request.MultipartForm.Value["metadata"]) != 1 || len(c.Request.MultipartForm.File) != 1 || len(c.Request.MultipartForm.File["file"]) != 1 {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "外部合同请求必须且只能包含一个 file 和一个 metadata", nil)
		return
	}
	metadata := strings.TrimSpace(c.PostForm("metadata"))
	if metadata == "" || len(metadata) > metadataLimit {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "请填写外部合同元数据", nil)
		return
	}
	var body externalContractMetadata
	decoder := json.NewDecoder(strings.NewReader(metadata))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		message, details := externalMetadataDecodeError(err)
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", message, details)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "外部合同元数据只能包含一个 JSON 对象", nil)
		return
	}
	fileHeader, err := c.FormFile("file")
	if err != nil || fileHeader.Size <= 0 || fileHeader.Size > maxFileSize {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "请选择有效且未超过上限的DOCX或PDF原件", nil)
		return
	}
	stream, err := fileHeader.Open()
	if err != nil {
		writeError(c, application.ErrValidation)
		return
	}
	document, readErr := io.ReadAll(io.LimitReader(stream, maxFileSize+1))
	closeErr := stream.Close()
	if readErr != nil || closeErr != nil || int64(len(document)) > maxFileSize {
		writeError(c, application.ErrValidation)
		return
	}
	source, validationErr := contractdocument.ValidateExternalSource(fileHeader.Filename,
		fileHeader.Header.Get("Content-Type"), document)
	if validationErr != nil {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", validationErr.Error(), nil)
		return
	}
	created, err := h.service.CreateExternalContract(c.Request.Context(), principal(c), contract.Contract{
		Title: body.Title, Type: body.ContractType, ServiceType: body.ServiceType,
		OpportunityID: body.OpportunityID, OpportunityName: body.OpportunityName,
		CRMCustomerID: body.CRMCustomerID, CustomerName: body.CustomerName,
		CustomerAddress: body.CustomerAddress, CustomerContact: body.CustomerContact, CustomerPhone: body.CustomerPhone,
		Systems: body.Systems, ServiceItems: body.ServiceItems, CustomerCreditLevel: body.CustomerCreditLevel,
		AmountMinor: body.AmountMinor, Currency: body.Currency, PaymentTerms: body.PaymentTerms, Document: document,
		DocumentFormat: source.Format, SourceFilename: source.Filename, SourceMediaType: source.MediaType,
		StartDate: body.StartDate, EndDate: body.EndDate,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusCreated, created)
}

// 仅暴露已知字段名及错误类别。解码器错误可能包含
// 用户提供的 JSON 值，不适合用作面向用户的诊断。
func externalMetadataDecodeError(err error) (string, map[string]string) {
	var fieldError *json.UnmarshalTypeError
	if errors.As(err, &fieldError) {
		labels := map[string]string{
			"title": "合同名称", "contract_type": "合同类型", "crm_customer_id": "CRM 客户",
			"opportunity_id": "关联商机", "amount_minor": "合同金额", "currency": "币种",
			"service_items": "服务项", "systems": "系统信息", "payment_terms": "付款计划",
			"start_date": "开始日期", "end_date": "结束日期",
		}
		field := strings.Split(fieldError.Field, ".")[0]
		if label, ok := labels[field]; ok {
			return label + "格式不正确，请检查后重试", map[string]string{"reason": "invalid_type", "field": field}
		}
		return "外部合同字段格式不正确，请检查表单后重试", map[string]string{"reason": "invalid_type"}
	}
	var dateError *time.ParseError
	if errors.As(err, &dateError) {
		return "合同开始或结束日期格式不正确，请重新选择日期", map[string]string{"reason": "invalid_date"}
	}
	if strings.HasPrefix(err.Error(), "json: unknown field ") {
		return "外部合同包含不支持的字段，请刷新页面并确认前后端版本一致", map[string]string{"reason": "unknown_field"}
	}
	return "外部合同表单数据格式不正确，请刷新后重新填写", map[string]string{"reason": "invalid_json"}
}

func (h *Handler) getContract(c *gin.Context) {
	found, err := h.service.GetContract(c.Request.Context(), principal(c), c.Param("contractID"))
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, found)
}

func (h *Handler) listTemplates(c *gin.Context) {
	items, err := h.service.ListTemplates(c.Request.Context(), principal(c))
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, items)
}

func (h *Handler) createTemplate(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, docx.MaxTemplateSize+(1<<20))
	if err := c.Request.ParseMultipartForm(docx.MaxTemplateSize); err != nil {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "模板上传参数不合法或文件过大", nil)
		return
	}
	fileHeader, err := c.FormFile("file")
	if err != nil {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "请选择 DOCX 模板文件", nil)
		return
	}
	if fileHeader.Size <= 0 || fileHeader.Size > docx.MaxTemplateSize {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "DOCX 模板不能超过 10MB", nil)
		return
	}
	file, err := fileHeader.Open()
	if err != nil {
		writeError(c, err)
		return
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, docx.MaxTemplateSize+1))
	if err != nil {
		writeError(c, err)
		return
	}
	created, err := h.service.CreateTemplate(c.Request.Context(), principal(c), c.PostForm("name"), fileHeader.Filename, content)
	if err != nil {
		if errors.Is(err, application.ErrValidation) {
			detail := strings.TrimSpace(strings.TrimPrefix(err.Error(), application.ErrValidation.Error()+":"))
			if detail == "" {
				detail = "请求参数不合法"
			}
			writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_TEMPLATE_VALIDATION_ERROR", detail, nil)
			return
		}
		writeError(c, err)
		return
	}
	writeData(c, http.StatusCreated, created)
}

func (h *Handler) updateTemplate(c *gin.Context) {
	var body struct {
		Name         string                   `json:"name"`
		NumberFormat string                   `json:"number_format"`
		Fields       []contracttemplate.Field `json:"fields"`
	}
	if !decode(c, &body) {
		return
	}
	updated, err := h.service.UpdateTemplate(c.Request.Context(), principal(c), c.Param("templateID"), body.Name, body.NumberFormat, body.Fields)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, updated)
}

func (h *Handler) deleteTemplate(c *gin.Context) {
	if err := h.service.DeleteTemplate(c.Request.Context(), principal(c), c.Param("templateID")); err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) previewTemplate(c *gin.Context) {
	var body struct {
		Values map[string]string `json:"values"`
	}
	if !decode(c, &body) {
		return
	}
	preview, err := h.service.PreviewTemplate(c.Request.Context(), principal(c), c.Param("templateID"), body.Values)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, map[string]string{"html": preview})
}

// exportContract 导出权威原件并保持实际文件类型。
// 参数：c为已认证请求，合同读取范围由服务复核。
// 返回值：无；PDF原样输出，旧DOCX维持既有导出行为。
func (h *Handler) exportContract(c *gin.Context) {
	found, err := h.service.GetContract(c.Request.Context(), principal(c), c.Param("contractID"))
	if err != nil {
		writeError(c, err)
		return
	}
	if len(found.Document) == 0 {
		writeEnvelopeError(c, http.StatusNotFound, "CON_DOCUMENT_NOT_FOUND", "该合同没有可导出的 DOCX 文档", nil)
		return
	}
	format, err := storedDocumentFormat(found)
	if err != nil {
		writeError(c, err)
		return
	}
	if format == "PDF" {
		h.download(c, found.Number, ".pdf", contractdocument.PDFMediaType, found.Document)
		return
	}
	filename := strings.TrimSuffix(filepath.Base(found.Number), filepath.Ext(found.Number))
	if filename == "" || filename == "." {
		filename = "contract"
	}
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename + ".docx"}))
	c.Data(http.StatusOK, "application/vnd.openxmlformats-officedocument.wordprocessingml.document", found.Document)
}

func (h *Handler) listApprovedContracts(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	items, err := h.service.ListApprovedContracts(c.Request.Context(), principal(c), limit)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, items)
}

// downloadApprovedDOCX 下载已批准DOCX，拒绝将PDF标成DOCX。
// 参数：c为请求，下载权限与持久访问审计均由服务执行。
// 返回值：无；成功输出DOCX原字节，否则输出统一错误。
func (h *Handler) downloadApprovedDOCX(c *gin.Context) {
	found, err := h.service.GetApprovedContract(c.Request.Context(), principal(c), c.Param("contractID"), "contract.document.download")
	if err != nil {
		writeError(c, err)
		return
	}
	format, formatErr := storedDocumentFormat(found)
	if formatErr != nil {
		writeError(c, formatErr)
		return
	}
	if format == "PDF" {
		writeEnvelopeError(c, http.StatusNotFound, "CON_DOCUMENT_NOT_FOUND", "源文件为PDF，没有DOCX版本", nil)
		return
	}
	if len(found.Document) == 0 {
		writeEnvelopeError(c, http.StatusNotFound, "CON_DOCUMENT_NOT_FOUND", "该合同没有 DOCX 文档", nil)
		return
	}
	if err := h.service.RecordDocumentAccess(c.Request.Context(), principal(c), c.Param("contractID"), "DOCX"); err != nil {
		writeError(c, err)
		return
	}
	h.download(c, found.Number, ".docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", found.Document)
}

// downloadApprovedPDF 下载源PDF或转换已保存的DOCX。
// 参数：c为已认证请求，权限及审计均失败关闭。
// 返回值：无；PDF不经重写或LibreOffice转换。
func (h *Handler) downloadApprovedPDF(c *gin.Context) {
	found, err := h.service.GetApprovedContract(c.Request.Context(), principal(c), c.Param("contractID"), "contract.document.download")
	if err != nil {
		writeError(c, err)
		return
	}
	format, formatErr := storedDocumentFormat(found)
	if formatErr != nil {
		writeError(c, formatErr)
		return
	}
	if format == "PDF" {
		h.downloadSource(c, found)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	document, err := contractpdf.ConvertDOCX(ctx, found.Document)
	if err != nil {
		writeError(c, err)
		return
	}
	if err := h.service.RecordDocumentAccess(c.Request.Context(), principal(c), c.Param("contractID"), "PDF"); err != nil {
		writeError(c, err)
		return
	}
	h.download(c, found.Number, ".pdf", "application/pdf", document)
}

func parseStampedExpectedVersion(raw string) (uint64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, errors.New("expected_version is required")
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, errors.New("expected_version is invalid: " + err.Error())
	}
	return value, nil
}

func (h *Handler) uploadStampedPDF(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, (20<<20)+(1<<20))
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "请选择不超过 20MB 的 PDF 文件", nil)
		return
	}
	defer file.Close()
	document, err := io.ReadAll(io.LimitReader(file, (20<<20)+1))
	if err != nil || !contractpdf.Valid(document) || !strings.EqualFold(filepath.Ext(header.Filename), ".pdf") {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "盖章合同必须是有效且不超过 20MB 的 PDF", nil)
		return
	}
	expectedVersion, versionErr := parseStampedExpectedVersion(c.PostForm("expected_version"))
	if versionErr != nil {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "盖章文件上传必须携带当前修订号", map[string]string{"expected_version": "缺失或格式不正确"})
		return
	}
	replacementReason := strings.TrimSpace(c.PostForm("replacement_reason"))
	var saved contract.StampedDocument
	if replacementReason != "" {
		saved, err = h.service.ReplaceStampedDocument(c.Request.Context(), principal(c), c.Param("contractID"), filepath.Base(header.Filename), document, expectedVersion, replacementReason)
	} else {
		saved, err = h.service.SaveStampedDocument(c.Request.Context(), principal(c), c.Param("contractID"), filepath.Base(header.Filename), document, expectedVersion)
	}
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, map[string]any{
		"original_filename": saved.OriginalFilename, "version": saved.Version, "content_sha256": saved.ContentSHA256,
	})
}

func (h *Handler) downloadStampedPDF(c *gin.Context) {
	found, err := h.service.GetStampedDocument(c.Request.Context(), principal(c), c.Param("contractID"))
	if err != nil {
		writeError(c, err)
		return
	}
	if err := h.service.RecordDocumentAccess(c.Request.Context(), principal(c), c.Param("contractID"), "STAMPED_PDF"); err != nil {
		writeError(c, err)
		return
	}
	h.download(c, found.OriginalFilename, ".pdf", "application/pdf", found.Document)
}

func (h *Handler) listSigningRecords(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	page, err := h.service.ListSigningRecords(c.Request.Context(), principal(c), contract.SigningRecordQuery{
		Keyword: c.Query("keyword"), Status: c.Query("status"), Method: c.Query("method"),
		Offset: offset, Limit: limit,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, page)
}

func (h *Handler) getSigningRecord(c *gin.Context) {
	item, err := h.service.GetSigningRecord(c.Request.Context(), principal(c), c.Param("contractID"))
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, item)
}

func (h *Handler) saveSigningShipment(c *gin.Context) {
	var body struct {
		CourierNumber    string `json:"courier_number"`
		RecipientName    string `json:"recipient_name"`
		RecipientPhone   string `json:"recipient_phone"`
		RecipientAddress string `json:"recipient_address"`
		MailedAt         string `json:"mailed_at"`
	}
	if !decode(c, &body) {
		return
	}
	mailedAt, err := time.Parse("2006-01-02", body.MailedAt)
	if err != nil || strings.TrimSpace(body.CourierNumber) == "" || strings.TrimSpace(body.RecipientName) == "" || strings.TrimSpace(body.RecipientPhone) == "" || strings.TrimSpace(body.RecipientAddress) == "" {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "请完整填写快递单号、收件人、收件人联系方式、收件地址和邮寄日期", nil)
		return
	}
	err = h.service.SaveSigningShipment(c.Request.Context(), principal(c), c.Param("contractID"), contract.SigningShipment{CourierNumber: strings.TrimSpace(body.CourierNumber), RecipientName: strings.TrimSpace(body.RecipientName), RecipientPhone: strings.TrimSpace(body.RecipientPhone), RecipientAddress: strings.TrimSpace(body.RecipientAddress), MailedAt: mailedAt.UTC()})
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, map[string]bool{"saved": true})
}

func (h *Handler) markSigningReceived(c *gin.Context) {
	if err := h.service.MarkSigningReceived(c.Request.Context(), principal(c), c.Param("contractID")); err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, map[string]bool{"recorded": true})
}

func (h *Handler) recordSigningReminder(c *gin.Context) {
	if err := h.service.RecordSigningReminder(c.Request.Context(), principal(c), c.Param("contractID")); err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, map[string]bool{"recorded": true})
}

func (h *Handler) confirmSigning(c *gin.Context) {
	var body struct {
		SealVerified      bool   `json:"seal_verified"`
		SignatureVerified bool   `json:"signature_verified"`
		SignedAt          string `json:"signed_at"`
		DocumentVersion   uint64 `json:"document_version"`
		ContentSHA256     string `json:"content_sha256"`
	}
	if !decode(c, &body) {
		return
	}
	signedAt, err := time.Parse("2006-01-02", body.SignedAt)
	if err != nil {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "请选择有效的签署日期", nil)
		return
	}
	err = h.service.ConfirmSigning(c.Request.Context(), principal(c), c.Param("contractID"), contract.SigningConfirmation{
		SealVerified: body.SealVerified, SignatureVerified: body.SignatureVerified, SignedAt: signedAt.UTC(),
		DocumentVersion: body.DocumentVersion, ContentSHA256: body.ContentSHA256,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, map[string]bool{"confirmed": true})
}

func (h *Handler) download(c *gin.Context, name, extension, contentType string, document []byte) {
	name = strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	if name == "" || name == "." {
		name = "contract"
	}
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + extension}))
	c.Data(http.StatusOK, contentType, document)
}

// previewContract 按原件类型预览当前合同或不可变审批快照。
// 参数：c为已认证请求，具体权限由应用服务复核。
// 返回值：无；DOCX输出安全HTML，PDF输出受鉴权原件描述。
func (h *Handler) previewContract(c *gin.Context) {
	found, err := h.service.GetContract(c.Request.Context(), principal(c), c.Param("contractID"))
	if err != nil {
		writeError(c, err)
		return
	}
	if len(found.Document) == 0 {
		writeEnvelopeError(c, http.StatusNotFound, "CON_DOCUMENT_NOT_FOUND", "该合同没有可预览的 DOCX 文档", nil)
		return
	}
	if found.DocumentFormat != "" {
		format, formatErr := storedDocumentFormat(found)
		if formatErr != nil {
			writeError(c, formatErr)
			return
		}
		if format == "PDF" {
			h.writeDocumentDescriptor(c, found, "/contracts/"+found.ID+"/source")
			return
		}
	}
	preview, err := docx.PreviewHTML(found.Document)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, map[string]string{"html": preview})
}

func (h *Handler) listContracts(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	keyword := strings.TrimSpace(c.Query("keyword"))
	status := c.Query("status")
	contracts, err := h.service.ListContracts(c.Request.Context(), principal(c), keyword, status, limit)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, contracts)
}

// settlementCompletedContract 是合同管理向结算系统提供的稳定读取契约。
// 不直接暴露合同正文、模板和渲染文档，避免跨系统同步时传输无关敏感内容。
type settlementCompletedContract struct {
	ID             string                 `json:"id"`
	ContractNumber string                 `json:"contract_number"`
	Title          string                 `json:"title"`
	CRMCustomerID  uint64                 `json:"crm_customer_id,omitempty"`
	CustomerName   string                 `json:"customer_name,omitempty"`
	Currency       string                 `json:"currency"`
	AmountMinor    int64                  `json:"amount_minor"`
	Status         contract.Status        `json:"status"`
	Version        uint64                 `json:"version"`
	UpdatedAt      time.Time              `json:"updated_at"`
	PaymentTerms   []contract.PaymentTerm `json:"payment_terms"`
}

// listSettlementCompletedContracts 返回当前租户状态为 completed 的合同摘要。
// limit 由服务层限制在 1~200，默认 50；keyword 支持编号、标题和客户名称检索。
func (h *Handler) listSettlementCompletedContracts(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	keyword := strings.TrimSpace(c.Query("keyword"))
	items, err := h.service.ListContracts(c.Request.Context(), principal(c), keyword, string(contract.StatusCompleted), limit)
	if err != nil {
		writeError(c, err)
		return
	}
	result := make([]settlementCompletedContract, 0, len(items))
	for _, item := range items {
		terms := item.PaymentTerms
		if terms == nil {
			terms = []contract.PaymentTerm{}
		}
		result = append(result, settlementCompletedContract{
			ID: item.ID, ContractNumber: item.Number, Title: item.Title, CRMCustomerID: item.CRMCustomerID,
			CustomerName: item.CustomerName, Currency: item.Currency, AmountMinor: item.AmountMinor,
			Status: item.Status, Version: item.Version, UpdatedAt: item.UpdatedAt, PaymentTerms: terms,
		})
	}
	writeData(c, http.StatusOK, result)
}

type projectApprovedContract struct {
	ID             string          `json:"id"`
	ContractNumber string          `json:"contract_number"`
	Title          string          `json:"title"`
	CustomerID     string          `json:"customer_id"`
	CustomerName   string          `json:"customer_name"`
	Version        uint64          `json:"version"`
	Status         contract.Status `json:"status"`
	ApprovalPassed bool            `json:"approval_passed"`
}

func projectApprovedContractView(item contract.Contract) projectApprovedContract {
	return projectApprovedContract{
		ID: item.ID, ContractNumber: item.Number, Title: item.Title,
		CustomerID: strconv.FormatUint(item.CRMCustomerID, 10), CustomerName: item.CustomerName,
		Version: item.Version, Status: item.Status, ApprovalPassed: item.Status.ApprovalPassed(),
	}
}

func (h *Handler) listProjectApprovedContracts(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	items, err := h.service.ListApprovedContracts(c.Request.Context(), principal(c), limit)
	if err != nil {
		writeError(c, err)
		return
	}
	result := make([]projectApprovedContract, 0, len(items))
	for _, item := range items {
		result = append(result, projectApprovedContractView(item))
	}
	writeData(c, http.StatusOK, result)
}

func (h *Handler) listProjectApprovedContractReferences(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	contracts, err := h.service.ListApprovedContractReferences(c.Request.Context(), principal(c), strings.TrimSpace(c.Query("after_id")), limit)
	if err != nil {
		writeError(c, err)
		return
	}
	nextAfterID := ""
	if len(contracts) == limit {
		nextAfterID = contracts[len(contracts)-1].ID
	}
	references := make([]gin.H, 0, len(contracts))
	for _, item := range contracts {
		references = append(references, gin.H{"id": item.ID, "contract_number": item.Number, "version": item.Version})
	}
	writeData(c, http.StatusOK, gin.H{"contracts": references, "next_after_id": nextAfterID})
}

func (h *Handler) getProjectApprovedContract(c *gin.Context) {
	item, err := h.service.GetApprovedContract(c.Request.Context(), principal(c), c.Param("contractID"), "contract.approved.read")
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, projectApprovedContractView(item))
}

// getProjectApprovedContractServiceItems 仅暴露建项所需的结构化
// 服务范围。合同正文、价格、联系人及文件
// 均不属于项目管理集成边界。
func (h *Handler) getProjectApprovedContractServiceItems(c *gin.Context) {
	item, err := h.service.GetApprovedContract(c.Request.Context(), principal(c), c.Param("contractID"), "contract.approved.read")
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, gin.H{
		"contract_id":      item.ID,
		"contract_version": item.Version,
		"service_items":    contract.ProjectServiceItems(item.ID, item.ServiceItems),
	})
}

func (h *Handler) submitApproval(c *gin.Context) {
	var body struct {
		TermsIdentical  bool   `json:"terms_identical"`
		ExpectedVersion uint64 `json:"expected_version"`
	}
	if !decode(c, &body) {
		return
	}
	result, err := h.service.SubmitContractVersion(c.Request.Context(), principal(c), c.Param("contractID"), body.TermsIdentical, body.ExpectedVersion)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusAccepted, result)
}

func (h *Handler) changeStatus(c *gin.Context) {
	var body struct {
		Version uint64          `json:"version"`
		Target  contract.Status `json:"target_status"`
		Reason  string          `json:"reason"`
	}
	if !decode(c, &body) {
		return
	}
	result, err := h.service.ChangeStatus(c.Request.Context(), principal(c), c.Param("contractID"), body.Version, body.Target, strings.TrimSpace(body.Reason))
	if err != nil {
		writeError(c, err)
		return
	}
	if result.ApprovalID == "" {
		writeData(c, http.StatusOK, map[string]string{"status": "changed"})
		return
	}
	writeData(c, http.StatusAccepted, result)
}

func (h *Handler) listTasks(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	tasks, err := h.service.ListMyTasks(c.Request.Context(), principal(c), limit)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, tasks)
}

func (h *Handler) listApprovals(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	approvals, err := h.service.ListMyApprovals(c.Request.Context(), principal(c), limit)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, approvals)
}

func (h *Handler) listFailedAuditEvents(c *gin.Context) {
	if !principal(c).Has("approval.manage") {
		writeEnvelopeError(c, http.StatusForbidden, "CON_FORBIDDEN", "当前用户没有执行此操作的权限", nil)
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if h.auditOutbox == nil {
		writeEnvelopeError(c, http.StatusServiceUnavailable, "CON_UNAVAILABLE", "审计队列运维未配置", nil)
		return
	}
	rows, err := h.auditOutbox.ListFailed(c.Request.Context(), principal(c).TenantID, limit)
	if err != nil {
		writeError(c, err)
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, map[string]any{"id": row.ID, "event_id": row.EventID, "action": row.Action, "resource_type": row.ResourceType, "resource_id": row.ResourceID, "request_id": row.RequestID, "last_error": row.LastError, "attempts": row.Attempts, "created_at": row.CreatedAt.Format(time.RFC3339)})
	}
	writeData(c, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) retryFailedAuditEvent(c *gin.Context) {
	if !principal(c).Has("approval.manage") {
		writeEnvelopeError(c, http.StatusForbidden, "CON_FORBIDDEN", "当前用户没有执行此操作的权限", nil)
		return
	}
	if h.auditOutbox == nil {
		writeEnvelopeError(c, http.StatusServiceUnavailable, "CON_UNAVAILABLE", "审计队列运维未配置", nil)
		return
	}
	ok, err := h.auditOutbox.RetryFailed(c.Request.Context(), principal(c).TenantID, c.Param("id"), time.Now().UTC())
	if err != nil {
		writeError(c, err)
		return
	}
	if !ok {
		writeEnvelopeError(c, http.StatusNotFound, "CON_NOT_FOUND", "未找到可重试的审计事件", nil)
		return
	}
	writeData(c, http.StatusOK, map[string]string{"id": c.Param("id"), "status": "retry"})
}

func (h *Handler) getApproval(c *gin.Context) {
	detail, err := h.service.GetApprovalDetail(c.Request.Context(), principal(c), c.Param("approvalID"))
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, detail)
}

func (h *Handler) recoverApproval(c *gin.Context) {
	if err := h.service.RecoverApproval(c.Request.Context(), principal(c), c.Param("approvalID")); err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, map[string]bool{"recovered": true})
}

// previewApprovalContract 按原件类型预览当前合同或不可变审批快照。
// 参数：c为已认证请求，具体权限由应用服务复核。
// 返回值：无；DOCX输出安全HTML，PDF输出受鉴权原件描述。
func (h *Handler) previewApprovalContract(c *gin.Context) {
	detail, err := h.service.GetApprovalDetail(c.Request.Context(), principal(c), c.Param("approvalID"))
	if err != nil {
		writeError(c, err)
		return
	}
	if len(detail.Contract.Document) == 0 {
		writeEnvelopeError(c, http.StatusNotFound, "CON_DOCUMENT_NOT_FOUND", "该审批合同没有可预览的 DOCX 文档", nil)
		return
	}
	if detail.Contract.DocumentFormat != "" {
		format, formatErr := storedDocumentFormat(detail.Contract)
		if formatErr != nil {
			writeError(c, formatErr)
			return
		}
		if format == "PDF" {
			h.writeDocumentDescriptor(c, detail.Contract, "/approvals/"+c.Param("approvalID")+"/contract-source")
			return
		}
	}
	preview, err := docx.PreviewHTML(detail.Contract.Document)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, map[string]string{"html": preview})
}

type commandRequest struct {
	Comment       string                   `json:"comment"`
	TargetUserIDs []string                 `json:"target_user_ids"`
	Countersign   approval.CountersignMode `json:"countersign"`
	TargetNodeID  string                   `json:"target_node_id"`
}

func (h *Handler) command(pathAction string) gin.HandlerFunc {
	actions := map[string]workflows.CommandAction{"approve": workflows.ActionApprove, "reject": workflows.ActionReject, "sign": workflows.ActionAddSign, "transfer": workflows.ActionTransfer, "return": workflows.ActionReturn, "withdraw": workflows.ActionWithdraw, "urge": workflows.ActionUrge, "comments": workflows.ActionComment}
	return func(c *gin.Context) {
		var body commandRequest
		if !decode(c, &body) {
			return
		}
		command := workflows.ApprovalCommand{Action: actions[pathAction], Comment: strings.TrimSpace(body.Comment), TargetUserIDs: body.TargetUserIDs, Countersign: body.Countersign, TargetNodeID: body.TargetNodeID}
		commandID, err := h.service.Command(c.Request.Context(), principal(c), c.Param("approvalID"), command)
		if err != nil {
			writeError(c, err)
			return
		}
		writeData(c, http.StatusAccepted, map[string]string{"status": "accepted", "command_id": commandID})
	}
}

const principalKey = "principal"

func (h *Handler) authenticate() gin.HandlerFunc {
	return func(c *gin.Context) {
		p, err := h.identity.Authenticate(c.Request.Context(), c.Request)
		if err != nil {
			writeError(c, err)
			c.Abort()
			return
		}
		c.Set(principalKey, p)
		c.Next()
	}
}

// authenticateDashboardIntegration 数据看板系统机器接入：Bearer Keycloak 机器令牌
// + 全量合同读范围（看板为只读消费方，不对业务写操作授权）。
func (h *Handler) authenticateDashboardIntegration(options DashboardIntegrationOptions) gin.HandlerFunc {
	return h.authenticateServiceIntegration(options, "看板系统")
}

// authenticateServiceIntegration 校验内部服务的 Bearer 令牌并建立租户级只读主体。
// 租户只取自验签后的服务身份；任何请求头都不能扩大数据范围。
func (h *Handler) authenticateServiceIntegration(options struct {
	Enabled        bool
	RequireBearer  bool
	BearerVerifier platform.ClientCredentialsTokenVerifier
}, serviceName string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !options.Enabled {
			writeEnvelopeError(c, http.StatusNotFound, "CON_SERVICE_INTEGRATION_DISABLED", serviceName+"接口未启用", nil)
			c.Abort()
			return
		}
		if !options.RequireBearer {
			// 配置层 validate() 已强制 Enabled → RequireBearer，这里作为纵深防御兜底：
			// 无机器令牌校验时绝不构造租户级主体，避免退化为请求头租户。
			writeEnvelopeError(c, http.StatusServiceUnavailable, "CON_SERVICE_AUTH_UNAVAILABLE", serviceName+"机器身份校验未启用", nil)
			c.Abort()
			return
		}
		if options.BearerVerifier == nil {
			writeEnvelopeError(c, http.StatusServiceUnavailable, "CON_SERVICE_AUTH_UNAVAILABLE", serviceName+"机器身份校验未配置", nil)
			c.Abort()
			return
		}
		const bearerPrefix = "Bearer "
		authorization := strings.TrimSpace(c.GetHeader("Authorization"))
		if !strings.HasPrefix(authorization, bearerPrefix) || strings.TrimSpace(strings.TrimPrefix(authorization, bearerPrefix)) == "" {
			writeEnvelopeError(c, http.StatusUnauthorized, "CON_SERVICE_BEARER_REQUIRED", serviceName+"调用必须携带机器访问令牌", nil)
			c.Abort()
			return
		}
		identity, err := options.BearerVerifier.VerifyClientCredentials(c.Request.Context(), strings.TrimSpace(strings.TrimPrefix(authorization, bearerPrefix)))
		if err != nil {
			writeEnvelopeError(c, http.StatusUnauthorized, "CON_SERVICE_BEARER_INVALID", serviceName+"机器访问令牌无效", nil)
			c.Abort()
			return
		}
		// 租户边界取自已验签令牌，绝不信任请求头。
		tenantID := identity.TenantID
		if tenantID == "" {
			writeEnvelopeError(c, http.StatusUnauthorized, "CON_SERVICE_TENANT_INVALID", serviceName+"机器令牌缺少租户", nil)
			c.Abort()
			return
		}
		c.Set(principalKey, application.Principal{
			Subject: serviceName, TenantID: tenantID, UserID: serviceName, IdentityID: serviceName,
			DisplayName: serviceName,
			Permissions: map[string]bool{"contract.read": true, "contract.approved.read": true},
			PermissionScopes: map[string]contract.ScopeFilter{
				"contract.read":          {TenantID: tenantID, IdentityID: "data_analysis", AllowAll: true},
				"contract.approved.read": {TenantID: tenantID, IdentityID: serviceName, AllowAll: true},
			},
		})
		c.Next()
	}
}
func (h *Handler) grantOpportunityIntakePermission() gin.HandlerFunc {
	return func(c *gin.Context) {
		p := principal(c)
		if p.Permissions == nil {
			p.Permissions = map[string]bool{}
		}
		p.Permissions["opportunity_intake.receive"] = true
		c.Set(principalKey, p)
		c.Next()
	}
}

func principal(c *gin.Context) application.Principal {
	value, _ := c.Get(principalKey)
	p, _ := value.(application.Principal)
	return p
}

type envelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
	Data      any    `json:"data,omitempty"`
	Details   any    `json:"details,omitempty"`
}

func decode(c *gin.Context, target any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "请求参数不合法", err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "请求只能包含一个 JSON 对象", nil)
		return false
	}
	return true
}

func writeData(c *gin.Context, status int, data any) {
	writeJSON(c, status, envelope{Code: "OK", Message: "操作成功", RequestID: requestIDFrom(c.Request.Context()), Data: data})
}

// writeError 将真实业务错误转换为统一HTTP信封及可定位错误码。
// 参数：c为HTTP上下文，err为业务或依赖错误。
// 返回值：无；不泄漏内部凭据，保留权限、版本、来源及恢复冲突语义。
func writeError(c *gin.Context, err error) {
	var assigneeUnavailable *application.ApprovalAssigneeUnavailableError
	switch {
	case errors.Is(err, application.ErrImportMixedLifecycle):
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "IMPORT_MIXED_LIFECYCLE", "NORMAL与历史合同须分别建立批次", map[string]any{"field_path": "items[].mode"})
	case errors.Is(err, application.ErrImportCorrectionRequired):
		writeEnvelopeError(c, http.StatusConflict, "IMPORT_CORRECTION_REQUIRED", "已创建合同的基线不能在批次内覆盖，请先核对原合同并走更正流程", nil)
	case errors.Is(err, application.ErrImportNeedsReview):
		writeEnvelopeError(c, http.StatusConflict, "IMPORT_NEEDS_REVIEW", "已创建合同版本发生变化，请先核对原合同", nil)
	case errors.Is(err, application.ErrImportConflict):
		writeEnvelopeError(c, http.StatusConflict, "IMPORT_REVISION_CONFLICT", "批次版本或幂等请求已变化，请先查询结果", nil)
	case errors.Is(err, application.ErrImportIdentityConflict):
		writeEnvelopeError(c, http.StatusConflict, "IMPORT_SOURCE_ID_CONFLICT", "来源业务身份与冻结基线冲突，需要核对", nil)
	case errors.Is(err, application.ErrImportUnsupported):
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "IMPORT_POLICY_UNSUPPORTED", "导入能力或此模式尚未开放", nil)
	case errors.Is(err, application.ErrCommercialLicense):
		writeEnvelopeError(c, http.StatusForbidden, "COMMERCIAL_LICENSE_RESTRICTED", "商业授权不允许此操作；历史查询与导出仍可使用", nil)
	case errors.Is(err, platform.ErrUnauthenticated):
		writeEnvelopeError(c, http.StatusUnauthorized, "AUTH_UNAUTHENTICATED", "登录状态无效", nil)
	case errors.Is(err, platform.ErrAuthorizationServiceUnavailable), errors.Is(err, application.ErrPersonnelDirectoryUnavailable):
		writeEnvelopeError(c, http.StatusServiceUnavailable, "AUTH_DEPENDENCY_UNAVAILABLE", "身份或授权服务暂时不可用", nil)
	case errors.Is(err, application.ErrApprovalWorkflowUnavailable):
		writeEnvelopeError(c, http.StatusServiceUnavailable, "CON_APPROVAL_WORKFLOW_UNAVAILABLE", "审批流程服务暂时不可用，请稍后重试", nil)
	case errors.As(err, &assigneeUnavailable):
		// 审批人解析发生在合同状态和审批实例写入之前，必须明确告知“本次未提交”。
		// 若按 5xx 返回，客户端只会提示写入结果未知，诱发重复提交。
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_APPROVAL_ASSIGNEE_UNAVAILABLE", approvalAssigneeMessage(assigneeUnavailable.RoleCodes), gin.H{"role_codes": assigneeUnavailable.RoleCodes})
	case errors.Is(err, application.ErrDetectionCategoryDirectoryUnavailable):
		writeEnvelopeError(c, http.StatusServiceUnavailable, "CON_DETECTION_CATEGORY_DIRECTORY_UNAVAILABLE", "检测类别目录暂时不可用，请稍后重试", nil)
	case errors.Is(err, application.ErrCRMReferenceDirectoryUnavailable):
		writeEnvelopeError(c, http.StatusServiceUnavailable, "CON_CRM_REFERENCE_DIRECTORY_UNAVAILABLE", "CRM 客户与商机目录暂时不可用，请稍后重试", nil)
	case errors.Is(err, contractpdf.ErrConversionUnavailable):
		writeEnvelopeError(c, http.StatusServiceUnavailable, "CON_PDF_CONVERSION_BUSY", "PDF 转换并发已达上限，请稍后重试", nil)
	case errors.Is(err, application.ErrForbidden):
		writeEnvelopeError(c, http.StatusForbidden, "AUTH_FORBIDDEN", "无权执行该操作", nil)
	case errors.Is(err, application.ErrApprovalTargetForbidden):
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_APPROVAL_TARGET_FORBIDDEN", "只能加签具有合同审批权限的用户", nil)
	case errors.Is(err, apperrors.ErrNotFound):
		writeEnvelopeError(c, http.StatusNotFound, "CON_NOT_FOUND", "资源不存在", nil)
	case errors.Is(err, apperrors.ErrVersionConflict):
		writeEnvelopeError(c, http.StatusConflict, "CON_VERSION_CONFLICT", "数据版本已变化，请刷新后重试", nil)
	case errors.Is(err, apperrors.ErrStateConflict), errors.Is(err, contract.ErrInvalidTransition):
		writeEnvelopeError(c, http.StatusConflict, "CON_STATE_CONFLICT", "当前状态不允许该操作", nil)
	case errors.Is(err, application.ErrValidation), errors.Is(err, contract.ErrInvalidStatus):
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "请求参数不合法", nil)
	default:
		slog.ErrorContext(c.Request.Context(), "contract request failed",
			"request_id", requestIDFrom(c.Request.Context()),
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"error", err,
		)
		writeEnvelopeError(c, http.StatusInternalServerError, "CON_INTERNAL_ERROR", "服务暂时不可用", nil)
	}
}

// contractApprovalRoleLabels 与合同权限清单的角色名称保持一致，仅用于把
// 审批人缺失这一配置问题翻译成可执行的用户提示；未知角色保留原始编码。
var contractApprovalRoleLabels = map[string]string{
	"admin":               "超级管理员",
	"sales_director":      "销售总监",
	"tech_director":       "技术总监",
	"finance_director":    "财务总监",
	"sales":               "销售人员",
	"contract_specialist": "合同专员",
	"audit_admin":         "审计管理员",
}

func approvalAssigneeMessage(roleCodes []string) string {
	labels := make([]string, 0, len(roleCodes))
	for _, code := range roleCodes {
		code = strings.TrimSpace(code)
		if code == "" {
			continue
		}
		label, known := contractApprovalRoleLabels[code]
		if !known {
			label = code
		}
		labels = append(labels, label)
	}
	const guidance = "本次未提交。请先在基础平台为合同应用分配相应角色的人员，或调整审批规则后重试。"
	if len(labels) == 0 {
		return "审批流程缺少可用审批人。" + guidance
	}
	if len(labels) > 5 {
		labels = append(labels[:5], "等")
	}
	return "审批流程缺少可用审批人：" + strings.Join(labels, "、") + "。" + guidance
}

func writeEnvelopeError(c *gin.Context, status int, code, message string, details any) {
	writeJSON(c, status, envelope{Code: code, Message: message, RequestID: requestIDFrom(c.Request.Context()), Details: details})
}
func writeJSON(c *gin.Context, status int, value any) {
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.JSON(status, value)
}

type requestIDKey struct{}

func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := strings.ToUpper(strings.TrimSpace(c.GetHeader("X-Request-ID")))
		if _, err := ulid.ParseStrict(id); err != nil {
			id = ulid.Make().String()
		}
		c.Header("X-Request-ID", id)
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), requestIDKey{}, id))
		c.Next()
	}
}
func requestIDFrom(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}
func recoverer() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.ErrorContext(c.Request.Context(), "contract request panicked",
					"request_id", requestIDFrom(c.Request.Context()),
					"method", c.Request.Method,
					"path", c.Request.URL.Path,
					"panic", recovered,
				)
				c.Abort()
				if !c.Writer.Written() {
					writeEnvelopeError(c, http.StatusInternalServerError, "CON_INTERNAL_ERROR", "服务暂时不可用", nil)
				}
			}
		}()
		c.Next()
	}
}
