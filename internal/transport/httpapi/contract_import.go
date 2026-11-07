package httpapi

import (
	"encoding/json"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"io"
	"log/slog"
	"mime/multipart"
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
	// 历史导入自动审批的租户级运行开关：仅合同系统超级管理员（角色 admin）可读写。
	api.GET("/contract-import/auto-approve", h.getImportAutoApproveSetting)
	api.PUT("/contract-import/auto-approve", h.putImportAutoApproveSetting)
}

// importAutoApproveSettingData 是运行开关接口的响应数据。
// updated_at 用指针表达「无记录时为 null」，与前端契约一致。
type importAutoApproveSettingData struct {
	CapabilityEnabled bool       `json:"capability_enabled"`
	Enabled           bool       `json:"enabled"`
	Version           uint64     `json:"version"`
	UpdatedBy         string     `json:"updated_by"`
	UpdatedAt         *time.Time `json:"updated_at"`
}

// importAutoApproveSettingResponse 把设置与 env 能力开关投影成响应数据。
// 参数：setting 为租户设置（无记录为零值），capability 为 env 能力开关状态。
// 返回值：可直接序列化的响应数据。
func importAutoApproveSettingResponse(setting contract.ImportAutoApproveSetting, capability bool) importAutoApproveSettingData {
	data := importAutoApproveSettingData{
		CapabilityEnabled: capability, Enabled: setting.Enabled,
		Version: setting.Version, UpdatedBy: setting.UpdatedBy,
	}
	if !setting.UpdatedAt.IsZero() {
		updatedAt := setting.UpdatedAt
		data.UpdatedAt = &updatedAt
	}
	return data
}

// getImportAutoApproveSetting 读取当前租户的历史导入自动审批运行开关。
// 参数：c 为已鉴权请求；非 admin 由服务层拒绝（403）。
// 返回值：无；能力关闭时仍返回 capability_enabled=false 与租户现有设置。
func (h *Handler) getImportAutoApproveSetting(c *gin.Context) {
	setting, capability, e := h.service.GetImportAutoApproveSetting(c.Request.Context(), principal(c))
	if e != nil {
		writeError(c, e)
		return
	}
	writeData(c, http.StatusOK, importAutoApproveSettingResponse(setting, capability))
}

// putImportAutoApproveSetting 以乐观版本写入当前租户的运行开关。
// 参数：c 为已鉴权请求，body 必须且只能包含 enabled 与 expected_version。
// 返回值：无；成功返回与 GET 相同的结构（version 已 +1）。
func (h *Handler) putImportAutoApproveSetting(c *gin.Context) {
	var body struct {
		Enabled         *bool   `json:"enabled"`
		ExpectedVersion *uint64 `json:"expected_version"`
	}
	if !decodeImportJSON(c, &body) {
		return
	}
	// enabled 必须是显式布尔值；expected_version 必须显式提供（首次写入为 0），
	// 缺失或负数都在这里按参数不合法拒绝，不猜默认值。
	if body.Enabled == nil || body.ExpectedVersion == nil {
		writeError(c, application.ErrValidation)
		return
	}
	setting, e := h.service.SetImportAutoApproveSetting(c.Request.Context(), principal(c), *body.Enabled, *body.ExpectedVersion)
	if e != nil {
		writeError(c, e)
		return
	}
	// 写入成功意味着 env 能力必定开启（否则服务层已拒绝），返回值与 GET 同形。
	writeData(c, http.StatusOK, importAutoApproveSettingResponse(setting, h.service.ContractImportAutoApproveHistorical))
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
// 返回值：无；每个失败分支都输出具体原因码并记录不含原件内容的诊断日志。
func (h *Handler) uploadImportSource(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, (20<<20)+(1<<20))
	if e := c.Request.ParseMultipartForm(1 << 20); e != nil {
		importUploadReject(c, "IMPORT_MULTIPART_INVALID", "上传请求无法解析：表单损坏或超过 21MiB", "file", "", "", 0)
		return
	}
	form := c.Request.MultipartForm
	defer form.RemoveAll()
	if len(form.File) != 1 || len(form.File["file"]) != 1 || len(form.Value) != 2 || len(form.Value["item_key"]) != 1 || len(form.Value["expected_version"]) != 1 {
		importUploadReject(c, "IMPORT_FORM_INVALID",
			"上传表单必须恰好包含 1 个原件字段 file 与 item_key、expected_version 两个文本字段",
			"file", importFormValue(form, "item_key"), importFormFilename(form), importFormSize(form))
		return
	}
	itemKey, fileName := form.Value["item_key"][0], form.File["file"][0].Filename
	size := form.File["file"][0].Size
	version, e := strconv.ParseUint(form.Value["expected_version"][0], 10, 64)
	if e != nil {
		importUploadReject(c, "IMPORT_VERSION_INVALID", "expected_version 必须是非负整数", "expected_version", itemKey, fileName, size)
		return
	}
	file := form.File["file"][0]
	opened, e := file.Open()
	if e != nil {
		importUploadReject(c, "IMPORT_READ_FAILED", "读取上传原件失败，请重试", "file", itemKey, fileName, size)
		return
	}
	defer opened.Close()
	body, e := io.ReadAll(io.LimitReader(opened, (20<<20)+1))
	if e != nil {
		importUploadReject(c, "IMPORT_READ_FAILED", "读取上传原件失败，请重试", "file", itemKey, fileName, size)
		return
	}
	source, e := h.service.UploadImportSource(c.Request.Context(), principal(c), c.Param("id"), version, itemKey, fileName, file.Header.Get("Content-Type"), body)
	if e != nil {
		importUploadLogRejection(c, e, itemKey, fileName, int64(len(body)))
		writeError(c, e)
		return
	}
	source.GatewayID = ""
	writeData(c, 201, source)
}

// importUploadReject 输出表单级422并记录可定位的拒绝原因。
// 参数：c为请求，reasonCode为稳定原因码，message为具体文案，field为出错字段，
// itemKey/fileName/sizeBytes为已解析出的非敏感元数据。
// 返回值：无；保持CON_VALIDATION_ERROR错误码契约不变。
func importUploadReject(c *gin.Context, reasonCode, message, field, itemKey, fileName string, sizeBytes int64) {
	importUploadLog(c, reasonCode, http.StatusUnprocessableEntity, itemKey, fileName, sizeBytes)
	writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", message,
		gin.H{"reason_code": reasonCode, "field": field})
}

// importUploadLogRejection 记录服务层原件准入拒绝的原因码与目标状态。
// 参数：c为请求，err为服务层错误，itemKey/fileName/sizeBytes为上传元数据。
// 返回值：无；仅当错误可定位到具体拒绝原因时记录，不记录原件字节或路径。
func importUploadLogRejection(c *gin.Context, err error, itemKey, fileName string, sizeBytes int64) {
	var rejected *application.ImportSourceRejectedError
	if !errors.As(err, &rejected) {
		return
	}
	status := http.StatusUnprocessableEntity
	if rejected.Retryable {
		status = http.StatusServiceUnavailable
	}
	importUploadLog(c, rejected.ReasonCode, status, itemKey, fileName, sizeBytes)
}

// importUploadLog 输出不含原件内容、路径与凭据的单条诊断日志。
// 参数：c为请求，reasonCode为原因码，status为响应状态，其余为上传元数据。
// 返回值：无。
func importUploadLog(c *gin.Context, reasonCode string, status int, itemKey, fileName string, sizeBytes int64) {
	slog.WarnContext(c.Request.Context(), "contract import source rejected",
		"request_id", requestIDFrom(c.Request.Context()),
		"batch", c.Param("id"),
		"item_key", itemKey,
		"file_name", fileName,
		"size_bytes", sizeBytes,
		"reason_code", reasonCode,
		"status", status,
	)
}

// importFormValue 在形状未通过时安全读取单个文本字段。
// 参数：form为已解析表单，key为字段名。
// 返回值：唯一值或空串。
func importFormValue(form *multipart.Form, key string) string {
	if form == nil || len(form.Value[key]) != 1 {
		return ""
	}
	return form.Value[key][0]
}

// importFormFilename 在形状未通过时安全读取原件名。
// 参数：form为已解析表单。
// 返回值：唯一原件名或空串。
func importFormFilename(form *multipart.Form) string {
	if form == nil || len(form.File["file"]) != 1 {
		return ""
	}
	return form.File["file"][0].Filename
}

// importFormSize 在形状未通过时安全读取声明体积。
// 参数：form为已解析表单。
// 返回值：唯一原件的字节数或0。
func importFormSize(form *multipart.Form) int64 {
	if form == nil || len(form.File["file"]) != 1 {
		return 0
	}
	return form.File["file"][0].Size
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
