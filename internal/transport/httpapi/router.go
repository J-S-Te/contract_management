package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
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

type PublicPathResolver interface {
	PublicPath(string) string
}

type Handler struct {
	service  *application.Service
	identity Identity
	audit    platform.AuditReporter
}

func NewRouter(service *application.Service, identity Identity, audits ...platform.AuditReporter) *gin.Engine {
	var audit platform.AuditReporter
	if len(audits) > 0 {
		audit = audits[0]
	}
	h := &Handler{service: service, identity: identity, audit: audit}
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
	r.GET("/healthz", func(c *gin.Context) {
		writeJSON(c, http.StatusOK, envelope{Code: "OK", Message: "ok", Data: map[string]string{"status": "up"}})
	})
	api := r.Group("/api/v1", h.authenticate(), h.auditWrites())
	api.GET("/auth/me", h.me)
	api.GET("/dashboard", h.dashboard)
	api.POST("/contracts", h.createContract)
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
	api.GET("/contracts/:contractID/preview", h.previewContract)
	api.GET("/contracts/:contractID/export", h.exportContract)
	api.GET("/contract-templates", h.listTemplates)
	api.POST("/contract-templates", h.createTemplate)
	api.PUT("/contract-templates/:templateID", h.updateTemplate)
	api.DELETE("/contract-templates/:templateID", h.deleteTemplate)
	api.POST("/contract-templates/:templateID/preview", h.previewTemplate)
	api.POST("/contracts/:contractID/submit-approval", h.submitApproval)
	api.POST("/contracts/:contractID/status-changes", h.changeStatus)
	api.GET("/approvals", h.listApprovals)
	api.GET("/approvals/tasks", h.listTasks)
	api.GET("/approvals/:approvalID", h.getApproval)
	api.GET("/approvals/:approvalID/contract-preview", h.previewApprovalContract)
	api.GET("/approval-rules", h.listRules)
	api.POST("/approval-rules", h.createRule)
	api.PUT("/approval-rules/:ruleID", h.updateRule)
	api.DELETE("/approval-rules/:ruleID", h.deleteRule)
	for _, action := range []string{"approve", "reject", "sign", "transfer", "return", "withdraw", "urge", "comments"} {
		api.POST("/approvals/:approvalID/"+action, h.command(action))
	}
	return r
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

func (h *Handler) auditWrites() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.audit == nil || c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead || c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		c.Next()
		p := principal(c)
		result := "SUCCESS"
		if c.Writer.Status() >= 400 {
			result = "FAILURE"
		}
		resourceType, resourceID := auditResource(c)
		requestID := requestIDFrom(c.Request.Context())
		_ = h.audit.Report(c.Request.Context(), platform.AuditEvent{ActorID: p.UserID, Action: auditAction(c.Request), ResourceType: resourceType, ResourceID: resourceID, RequestID: requestID, CorrelationID: requestID, Result: result, ReasonCode: strconv.Itoa(c.Writer.Status())})
	}
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
	Content             string                 `json:"content"`
	TemplateID          string                 `json:"template_id"`
	TemplateValues      map[string]string      `json:"template_values"`
	StartDate           *time.Time             `json:"start_date"`
	EndDate             *time.Time             `json:"end_date"`
}

func (h *Handler) createContract(c *gin.Context) {
	var body createContractRequest
	if !decode(c, &body) {
		return
	}
	created, err := h.service.CreateContract(c.Request.Context(), principal(c), contract.Contract{Number: body.Number, Title: body.Title, Type: body.ContractType, ServiceType: body.ServiceType, OpportunityID: body.OpportunityID, OpportunityName: body.OpportunityName, CRMCustomerID: body.CRMCustomerID, CustomerName: body.CustomerName, CustomerAddress: body.CustomerAddress, CustomerContact: body.CustomerContact, CustomerPhone: body.CustomerPhone, Systems: body.Systems, ServiceItems: body.ServiceItems, CustomerCreditLevel: body.CustomerCreditLevel, AmountMinor: body.AmountMinor, Currency: body.Currency, Content: body.Content, TemplateID: body.TemplateID, TemplateValues: body.TemplateValues, StartDate: body.StartDate, EndDate: body.EndDate})
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusCreated, created)
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

func (h *Handler) exportContract(c *gin.Context) {
	found, err := h.service.GetContract(c.Request.Context(), principal(c), c.Param("contractID"))
	if err != nil {
		writeError(c, err)
		return
	}
	if len(found.Document) == 0 {
		writeEnvelopeError(c, http.StatusNotFound, "CON_DOCUMENT_NOT_FOUND", "该合同没有可导出的模板文档", nil)
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

func (h *Handler) downloadApprovedDOCX(c *gin.Context) {
	found, err := h.service.GetApprovedContract(c.Request.Context(), principal(c), c.Param("contractID"), "contract.document.download")
	if err != nil {
		writeError(c, err)
		return
	}
	if len(found.Document) == 0 {
		writeEnvelopeError(c, http.StatusNotFound, "CON_DOCUMENT_NOT_FOUND", "该合同没有 DOCX 文档", nil)
		return
	}
	h.download(c, found.Number, ".docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", found.Document)
}

func (h *Handler) downloadApprovedPDF(c *gin.Context) {
	found, err := h.service.GetApprovedContract(c.Request.Context(), principal(c), c.Param("contractID"), "contract.document.download")
	if err != nil {
		writeError(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()
	document, err := contractpdf.ConvertDOCX(ctx, found.Document)
	if err != nil {
		writeError(c, err)
		return
	}
	h.download(c, found.Number, ".pdf", "application/pdf", document)
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
	if err := h.service.SaveStampedDocument(c.Request.Context(), principal(c), c.Param("contractID"), filepath.Base(header.Filename), document); err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, map[string]any{"original_filename": filepath.Base(header.Filename)})
}

func (h *Handler) downloadStampedPDF(c *gin.Context) {
	found, err := h.service.GetStampedDocument(c.Request.Context(), principal(c), c.Param("contractID"))
	if err != nil {
		writeError(c, err)
		return
	}
	h.download(c, found.OriginalFilename, ".pdf", "application/pdf", found.Document)
}

func (h *Handler) listSigningRecords(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	items, err := h.service.ListSigningRecords(c.Request.Context(), principal(c), limit)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, items)
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
	}
	if !decode(c, &body) {
		return
	}
	signedAt, err := time.Parse("2006-01-02", body.SignedAt)
	if err != nil {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "请选择有效的签署日期", nil)
		return
	}
	err = h.service.ConfirmSigning(c.Request.Context(), principal(c), c.Param("contractID"), contract.SigningConfirmation{SealVerified: body.SealVerified, SignatureVerified: body.SignatureVerified, SignedAt: signedAt.UTC()})
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

func (h *Handler) previewContract(c *gin.Context) {
	found, err := h.service.GetContract(c.Request.Context(), principal(c), c.Param("contractID"))
	if err != nil {
		writeError(c, err)
		return
	}
	if len(found.Document) == 0 {
		writeEnvelopeError(c, http.StatusNotFound, "CON_DOCUMENT_NOT_FOUND", "该合同没有可预览的模板文档", nil)
		return
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
	ownerUserID := c.Query("owner_user_id")
	status := c.Query("status")
	contracts, err := h.service.ListContracts(c.Request.Context(), principal(c), ownerUserID, status, limit)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, contracts)
}

func (h *Handler) submitApproval(c *gin.Context) {
	var body struct {
		TermsIdentical bool `json:"terms_identical"`
	}
	if !decode(c, &body) {
		return
	}
	result, err := h.service.SubmitContract(c.Request.Context(), principal(c), c.Param("contractID"), body.TermsIdentical)
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

func (h *Handler) getApproval(c *gin.Context) {
	detail, err := h.service.GetApprovalDetail(c.Request.Context(), principal(c), c.Param("approvalID"))
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, detail)
}

func (h *Handler) previewApprovalContract(c *gin.Context) {
	detail, err := h.service.GetApprovalDetail(c.Request.Context(), principal(c), c.Param("approvalID"))
	if err != nil {
		writeError(c, err)
		return
	}
	if len(detail.Contract.Document) == 0 {
		writeEnvelopeError(c, http.StatusNotFound, "CON_DOCUMENT_NOT_FOUND", "该审批合同没有可预览的模板文档", nil)
		return
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
func writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, platform.ErrUnauthenticated):
		writeEnvelopeError(c, http.StatusUnauthorized, "AUTH_UNAUTHENTICATED", "登录状态无效", nil)
	case errors.Is(err, platform.ErrAuthorizationServiceUnavailable), errors.Is(err, application.ErrPersonnelDirectoryUnavailable):
		writeEnvelopeError(c, http.StatusServiceUnavailable, "AUTH_DEPENDENCY_UNAVAILABLE", "身份或授权服务暂时不可用", nil)
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
