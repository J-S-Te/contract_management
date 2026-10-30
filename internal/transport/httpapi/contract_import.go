package httpapi

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"io"
	"net/http"
	"strconv"
	"time"
)

type importSessionProvider interface {
	ImportExecutionSession(*http.Request) ([]byte, error)
}

// registerContractImports 注册受现有鉴权及审计中间件保护的批次端点。
// 参数：api为业务路由组。
// 返回值：无。
func (h *Handler) registerContractImports(api *gin.RouterGroup) {
	api.POST("/contract-import-batches", h.createImportBatch)
	api.GET("/contract-import-batches/:id", h.getImportBatch)
	api.GET("/contract-import-batches", h.listImportBatches)
	api.GET("/contract-import-batches/:id/items/:itemID/document-descriptor", h.describeImportDocument)
	api.GET("/contract-import-batches/:id/items/:itemID/source", h.downloadImportDocument)
	api.POST("/contract-import-batches/:id/submit-confirmation", h.submitImportConfirmation)
	api.POST("/contract-import-batches/:id/confirmation-actions", h.actImportConfirmation)
	api.POST("/contract-import-items/:id/release", h.releaseImportItem)
	api.POST("/contract-import-batches/:id/files", h.uploadImportSource)
	api.PUT("/contract-import-batches/:id/manifest", h.setImportManifest)
	api.POST("/contract-import-batches/:id/preview", h.previewImportBatch)
	api.POST("/contract-import-batches/:id/commit", h.commitImportBatch)
	api.POST("/contract-import-batches/:id/pause", h.controlImportBatch)
	api.POST("/contract-import-batches/:id/resume", h.controlImportBatch)
}

// importData 清除内部对象和执行会话标识后返回业务台账。
// 参数：c为请求，status为HTTP状态，b为已授权台账。
// 返回值：无。
func importData(c *gin.Context, status int, b contract.ImportBatch) {
	scope, allowed := principal(c).Scope("contract.import.commit")
	b.CanCommit = principal(c).UserID == b.ActorID && allowed && (scope.AllowAll || scope.AllowSelf)
	sanitizeImportBatch(&b)
	writeData(c, status, b)
}

// decodeImportJSON 严格限制导入JSON大小、未知字段与尾随数据。
// 参数：c为请求，target为白名单DTO。
// 返回值：是否成功；失败已输出结构化错误。
func decodeImportJSON(c *gin.Context, target any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2<<20)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(c, application.ErrValidation)
		return false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		writeError(c, application.ErrValidation)
		return false
	}
	return true
}

// createImportBatch 创建技术台账。
// 参数：c为已鉴权请求。
// 返回值：无。
func (h *Handler) createImportBatch(c *gin.Context) {
	var body struct {
		SchemaVersion int    `json:"schema_version"`
		SourceSystem  string `json:"source_system"`
	}
	if !decodeImportJSON(c, &body) {
		return
	}
	if body.SchemaVersion != 1 {
		writeError(c, application.ErrValidation)
		return
	}
	b, e := h.service.CreateImportBatch(c.Request.Context(), principal(c), body.SourceSystem)
	if e != nil {
		writeError(c, e)
		return
	}
	importData(c, 201, b)
}

// getImportBatch 读取已有台账，不受新增开关关闭影响。
// 参数：c为已鉴权请求。
// 返回值：无。
func (h *Handler) getImportBatch(c *gin.Context) {
	b, e := h.service.GetImportBatch(c.Request.Context(), principal(c), c.Param("id"))
	if e != nil {
		writeError(c, e)
		return
	}
	importData(c, 200, b)
}

// uploadImportSource 接收单文件，拒绝额外字段及多文件。
// 参数：c为已鉴权multipart请求。
// 返回值：无。
func (h *Handler) uploadImportSource(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, (20<<20)+(1<<20))
	if e := c.Request.ParseMultipartForm(1 << 20); e != nil {
		writeError(c, application.ErrValidation)
		return
	}
	form := c.Request.MultipartForm
	defer form.RemoveAll()
	if len(form.File) != 1 || len(form.File["file"]) != 1 || len(form.Value) != 2 || len(form.Value["item_key"]) != 1 || len(form.Value["expected_version"]) != 1 {
		writeError(c, application.ErrValidation)
		return
	}
	version, e := strconv.ParseUint(form.Value["expected_version"][0], 10, 64)
	if e != nil {
		writeError(c, application.ErrValidation)
		return
	}
	file := form.File["file"][0]
	opened, e := file.Open()
	if e != nil {
		writeError(c, application.ErrValidation)
		return
	}
	defer opened.Close()
	body, e := io.ReadAll(io.LimitReader(opened, (20<<20)+1))
	if e != nil {
		writeError(c, application.ErrValidation)
		return
	}
	source, e := h.service.UploadImportSource(c.Request.Context(), principal(c), c.Param("id"), version, form.Value["item_key"][0], file.Filename, file.Header.Get("Content-Type"), body)
	if e != nil {
		writeError(c, e)
		return
	}
	source.GatewayID = ""
	writeData(c, 201, source)
}

// setImportManifest 严格保存清单及其读取版本。
// 参数：c为已鉴权请求。
// 返回值：无。
func (h *Handler) setImportManifest(c *gin.Context) {
	var body struct {
		ExpectedVersion uint64                  `json:"expected_version"`
		Manifest        contract.ImportManifest `json:"manifest"`
	}
	if !decodeImportJSON(c, &body) {
		return
	}
	if e := h.service.SetImportManifest(c.Request.Context(), principal(c), c.Param("id"), body.ExpectedVersion, body.Manifest); e != nil {
		writeError(c, e)
		return
	}
	h.getImportBatch(c)
}

// previewImportBatch 生成冻结的预检结果。
// 参数：c为已鉴权请求。
// 返回值：无。
func (h *Handler) previewImportBatch(c *gin.Context) {
	var body struct {
		Revision uint64 `json:"revision"`
	}
	if !decodeImportJSON(c, &body) {
		return
	}
	b, e := h.service.PreviewImport(c.Request.Context(), principal(c), c.Param("id"), body.Revision)
	if e != nil {
		writeError(c, e)
		return
	}
	importData(c, 200, b)
}

// importSession 获取后台执行需要的现有会话引用。
// 参数：c为已鉴权请求。
// 返回值：会话哈希；不支持持久身份的适配器失败关闭。
func (h *Handler) importSession(c *gin.Context) ([]byte, error) {
	provider, ok := h.identity.(importSessionProvider)
	if !ok {
		return nil, application.ErrImportUnsupported
	}
	return provider.ImportExecutionSession(c.Request)
}

// commitImportBatch 创建稳定操作，不把受理误报业务完成。
// 参数：c为已鉴权请求。
// 返回值：无。
func (h *Handler) commitImportBatch(c *gin.Context) {
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
	b, e := h.service.CommitImport(c.Request.Context(), principal(c), c.Param("id"), body.Revision, body.PreviewToken, c.GetHeader("Idempotency-Key"), session)
	if e != nil {
		writeError(c, e)
		return
	}
	importData(c, 202, b)
}

// controlImportBatch 保留结果并暂停或恢复新认领。
// 参数：c为已鉴权请求。
// 返回值：无。
func (h *Handler) controlImportBatch(c *gin.Context) {
	var body struct {
		ExpectedVersion uint64 `json:"expected_version"`
	}
	if !decodeImportJSON(c, &body) {
		return
	}
	resume := c.FullPath() == "/api/v1/contract-import-batches/:id/resume"
	var session []byte
	var e error
	if resume {
		session, e = h.importSession(c)
		if e != nil {
			writeError(c, e)
			return
		}
	}
	e = h.service.ControlImport(c.Request.Context(), principal(c), c.Param("id"), body.ExpectedVersion, resume, session)
	if e != nil {
		writeError(c, e)
		return
	}
	h.getImportBatch(c)
}

// sanitizeImportBatch 清除不对浏览器暴露的任务载荷及文件内部身份。
// 参数：b为HTTP响应副本。
// 返回值：无。
func sanitizeImportBatch(b *contract.ImportBatch) {
	for i := range b.Sources {
		b.Sources[i].GatewayID = ""
		b.Sources[i].FileID = ""
	}
	for i := range b.Items {
		for j := range b.Items[i].Targets {
			b.Items[i].Targets[j].RequestPayload = ""
			for k := range b.Items[i].Targets[j].CommandHistory {
				b.Items[i].Targets[j].CommandHistory[k].RequestPayload = ""
			}
			b.Items[i].Targets[j].LeaseUntil = time.Time{}
		}
	}
}
