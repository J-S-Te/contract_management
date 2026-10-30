package httpapi

import (
	"context"
	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/contractdocument"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
	"net/http"
	"strconv"
)

// submitImportConfirmation 提交冻结历史确认并返回真实受理台账。
// 参数：c为已鉴权HTTP请求，幂等键沿既有Idempotency-Key头。
// 返回值：无；响应不声称已生效。
func (h *Handler) submitImportConfirmation(c *gin.Context) {
	var body struct {
		Revision     uint64 `json:"revision"`
		PreviewToken string `json:"preview_token"`
	}
	if !decodeImportJSON(c, &body) {
		return
	}
	session, e := h.importSession(c)
	if e != nil {
		writeError(c, e)
		return
	}
	b, e := h.service.SubmitImportConfirmation(c.Request.Context(), principal(c), c.Param("id"), body.Revision, body.PreviewToken, c.GetHeader("Idempotency-Key"), session)
	if e != nil {
		writeError(c, e)
		return
	}
	importData(c, 202, b)
}

// actImportConfirmation 仅接收可信当前身份的审批命令。
// 参数：c为已鉴权请求，body提供expected_version、command_key和action。
// 返回值：无；批准结果由独立Temporal处理。
func (h *Handler) actImportConfirmation(c *gin.Context) {
	var body struct {
		ExpectedVersion uint64 `json:"expected_version"`
		CommandKey      string `json:"command_key"`
		Action          string `json:"action"`
	}
	if !decodeImportJSON(c, &body) {
		return
	}
	if e := h.service.ActImportConfirmation(c.Request.Context(), principal(c), c.Param("id"), body.ExpectedVersion, body.CommandKey, body.Action); e != nil {
		writeError(c, e)
		return
	}
	h.getImportBatch(c)
}

// releaseImportItem 接收精确目标版本的独立放行。
// 参数：c为已鉴权请求，item为稳定单项身份。
// 返回值：无；返回目标台账，不伪造接收方ACK。
func (h *Handler) releaseImportItem(c *gin.Context) {
	var body struct {
		Target          string `json:"target"`
		ExpectedVersion uint64 `json:"expected_version"`
		Phase           string `json:"phase"`
	}
	if !decodeImportJSON(c, &body) {
		return
	}
	b, e := h.service.ReleaseImportItem(c.Request.Context(), principal(c), c.Param("id"), body.Target, body.ExpectedVersion, body.Phase)
	if e != nil {
		writeError(c, e)
		return
	}
	importData(c, 202, b)
}

// listImportBatches 返回当前操作范围及真正受派人的有限候选页。
// 参数：c为请求，limit默认50最大100，offset最大10000。
// 返回值：无；逐行重新核验当前权限后返回。
func (h *Handler) listImportBatches(c *gin.Context) {
	limit, offset := 50, 0
	var e error
	if v := c.Query("limit"); v != "" {
		limit, e = strconv.Atoi(v)
		if e != nil {
			writeError(c, application.ErrValidation)
			return
		}
	}
	if v := c.Query("offset"); v != "" {
		offset, e = strconv.Atoi(v)
		if e != nil {
			writeError(c, application.ErrValidation)
			return
		}
	}
	if limit < 1 || limit > 100 || offset < 0 || offset > 10000 {
		writeError(c, application.ErrValidation)
		return
	}
	actor := principal(c)
	all, allowed := application.ImportBatchListScope(actor)
	if !allowed {
		writeError(c, application.ErrForbidden)
		return
	}
	store, ok := h.service.Repo.(interface {
		ListImportBatchesPage(context.Context, string, string, bool, int, int) ([]contract.ImportBatch, error)
	})
	if !ok {
		writeError(c, application.ErrImportUnsupported)
		return
	}
	batches, e := store.ListImportBatchesPage(c.Request.Context(), actor.TenantID, actor.UserID, all, limit, offset)
	if e != nil {
		writeError(c, e)
		return
	}
	visible := []contract.ImportBatch{}
	for _, b := range batches {
		if _, e = h.service.GetImportBatch(c.Request.Context(), actor, b.ID); e == nil {
			sanitizeImportBatch(&b)
			visible = append(visible, b)
		}
	}
	// 授权过滤可能把候选页变成空页，翻页位置必须按候选而非可见数量推进。
	next := offset + len(batches)
	hasMore := len(batches) == limit && next <= 10000
	var nextOffset *int
	if hasMore {
		nextOffset = &next
	}
	writeData(c, 200, gin.H{"items": visible, "limit": limit, "offset": offset,
		"has_more": hasMore, "next_offset": nextOffset,
		"query_limit_reached": len(batches) == limit && next > 10000})
}

// RegisterImportReportSource 注册独立机器只读历史源，不复用浏览器或普通看板身份。
// 参数：router为现API路由，service为合同服务，verifier为固定scope/caller/tenant/env/audience验签器。
// 返回值：无；未装配验签器时路径保持失败关闭。
func RegisterImportReportSource(router *gin.Engine, service *application.Service, verifier platform.ClientCredentialsTokenVerifier) {
	h := &Handler{service: service}
	group := router.Group("/internal/v1/contract-imports", h.authenticateServiceIntegration(struct {
		Enabled        bool
		RequireBearer  bool
		BearerVerifier platform.ClientCredentialsTokenVerifier
	}{true, true, verifier}, "data_analysis"), h.licenseOperation())
	group.GET("/report-source", func(c *gin.Context) {
		store, ok := service.Repo.(interface {
			ImportReportSource(context.Context, string) (contract.ImportReportSource, error)
		})
		if !ok {
			writeEnvelopeError(c, 503, "IMPORT_REPORT_SOURCE_UNAVAILABLE", "历史源尚未就绪", nil)
			return
		}
		source, e := store.ImportReportSource(c.Request.Context(), principal(c).TenantID)
		if e != nil {
			writeEnvelopeError(c, http.StatusServiceUnavailable, "IMPORT_REPORT_SOURCE_UNAVAILABLE", "完整历史源暂不可用", nil)
			return
		}
		writeData(c, 200, source)
	})
}

// describeImportDocument 返回受派审批人可访问的冻结原件描述。
// 参数：c为批次和item路径均已进入会话鉴权的请求。
// 返回值：无；descriptor与下载共同复核批次范围和原SHA。
func (h *Handler) describeImportDocument(c *gin.Context) {
	found, e := h.service.GetImportSourceDocument(c.Request.Context(), principal(c), c.Param("id"), c.Param("itemID"))
	if e != nil {
		writeError(c, e)
		return
	}
	h.writeDocumentDescriptor(c, found, "/contract-import-batches/"+c.Param("id")+"/items/"+c.Param("itemID")+"/source")
}

// downloadImportDocument 返回已持久审计且原SHA匹配的冻结字节。
// 参数：c为当前受信会话请求。
// 返回值：无；不以普通合同可见范围扩大批次访问权。
func (h *Handler) downloadImportDocument(c *gin.Context) {
	found, e := h.service.GetImportSourceDocument(c.Request.Context(), principal(c), c.Param("id"), c.Param("itemID"))
	if e != nil {
		writeError(c, e)
		return
	}
	extension, media := ".docx", contractdocument.DOCXMediaType
	if found.DocumentFormat == "PDF" {
		extension, media = ".pdf", contractdocument.PDFMediaType
	}
	h.download(c, found.SourceFilename, extension, media, found.Document)
}
