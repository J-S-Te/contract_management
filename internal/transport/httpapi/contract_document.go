package httpapi

import (
	"crypto/sha256"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/contractdocument"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// contractSourceCapabilities 提供会话可读取的源文件接入能力。
// 参数：c为已认证请求，需合同创建、读取或可用导入操作权限。
// 返回值：无；仅公开开关及固定体积，不公开运行配置或凭据。
func (h *Handler) contractSourceCapabilities(c *gin.Context) {
	actor := principal(c)
	_, canCreate := actor.Scope("contract.create")
	_, canImport := application.ImportBatchListScope(actor)
	if !canCreate && !actor.Has("contract.read") && !canImport {
		writeError(c, application.ErrForbidden)
		return
	}
	writeData(c, http.StatusOK, map[string]any{"pdf_enabled": h.service != nil && h.service.ExternalPDFEnabled,
		"batch_enabled": h.service != nil && h.service.ContractImportEnabled, "supported_modes": h.service.AvailableImportModes(),
		"max_docx_bytes": 10 << 20, "max_pdf_bytes": contractdocument.MaxPDFSize})
}

// storedDocumentFormat 决定文档输出类型，保留旧DOCX原件下载兼容。
// 参数：found为已按租户及权限读取的合同或不可变审批快照。
// 返回值：DOCX或PDF；未知显式类型及损坏PDF返回校验错误。
func storedDocumentFormat(found contract.Contract) (string, error) {
	if found.DocumentFormat == "" {
		return contractdocument.FormatDOCX, nil
	}
	format, err := contractdocument.EffectiveDocumentFormat(found.DocumentFormat, found.Document)
	if err != nil {
		return "", fmt.Errorf("%w: 合同文档类型或内容异常", application.ErrValidation)
	}
	if format == contractdocument.FormatPDF && found.SourceSHA256 != "" &&
		found.SourceSHA256 != fmt.Sprintf("%x", sha256.Sum256(found.Document)) {
		return "", fmt.Errorf("%w: PDF原件哈希不一致", application.ErrValidation)
	}
	return format, nil
}

// describeContractDocument 返回无原件字节的文档类型与下载能力。
// 参数：c为已认证HTTP请求，合同ID来自路径。
// 返回值：无；通过统一响应输出描述，权限或文档缺失时输出错误。
func (h *Handler) describeContractDocument(c *gin.Context) {
	found, err := h.service.GetContract(c.Request.Context(), principal(c), c.Param("contractID"))
	if err != nil {
		writeError(c, err)
		return
	}
	h.writeDocumentDescriptor(c, found, "/contracts/"+found.ID+"/source")
}

// writeDocumentDescriptor 输出当前文档或同一审批快照的原件描述。
// 参数：c为HTTP响应，found为权威文档，sourcePath为本应用内受鉴权的下载路径。
// 返回值：无；没有文档时返回404，非法类型不提供伪装下载。
func (h *Handler) writeDocumentDescriptor(c *gin.Context, found contract.Contract, sourcePath string) {
	if len(found.Document) == 0 {
		writeEnvelopeError(c, http.StatusNotFound, "CON_DOCUMENT_NOT_FOUND", "该合同没有原件文档", nil)
		return
	}
	format, err := storedDocumentFormat(found)
	if err != nil {
		writeError(c, err)
		return
	}
	mediaType := contractdocument.DOCXMediaType
	if format == contractdocument.FormatPDF {
		mediaType = contractdocument.PDFMediaType
	}
	writeData(c, http.StatusOK, map[string]any{"format": format, "media_type": mediaType,
		"filename": found.SourceFilename, "current_sha256": fmt.Sprintf("%x", sha256.Sum256(found.Document)),
		"source_sha256": found.SourceSHA256, "source_path": sourcePath, "can_download_docx": format == "DOCX"})
}

// downloadContractSource 在持久访问审计后返回合同原件。
// 参数：c为已认证请求，读取和数据范围由合同服务复核。
// 返回值：无；审计、权限或文件异常时不输出文件字节。
func (h *Handler) downloadContractSource(c *gin.Context) {
	found, err := h.service.GetContract(c.Request.Context(), principal(c), c.Param("contractID"))
	if err != nil {
		writeError(c, err)
		return
	}
	h.downloadSource(c, found)
}

// downloadSource 按类型下载已授权的原件并建立访问审计。
// 参数：c为HTTP请求，found为当前合同或审批快照中的精确原件。
// 返回值：无；成功返回正确MIME及原字节，失败输出统一错误。
func (h *Handler) downloadSource(c *gin.Context, found contract.Contract) {
	if len(found.Document) == 0 {
		writeEnvelopeError(c, http.StatusNotFound, "CON_DOCUMENT_NOT_FOUND", "该合同没有原件文档", nil)
		return
	}
	format, err := storedDocumentFormat(found)
	if err != nil {
		writeError(c, err)
		return
	}
	if err = h.service.RecordDocumentAccess(c.Request.Context(), principal(c), found.ID, "SOURCE_"+format); err != nil {
		writeError(c, err)
		return
	}
	name := found.Number
	if name == "" {
		name = found.SourceFilename
	}
	if format == "PDF" {
		h.download(c, name, ".pdf", contractdocument.PDFMediaType, found.Document)
		return
	}
	h.download(c, name, ".docx", contractdocument.DOCXMediaType, found.Document)
}

// downloadApprovalSource 下载审批权限允许查看的同一不可变快照原件。
// 参数：c为审批请求，approvalID来自路径并经审批服务复核参与范围。
// 返回值：无；不以当前合同文件替换审批快照。
func (h *Handler) downloadApprovalSource(c *gin.Context) {
	detail, err := h.service.GetApprovalDetail(c.Request.Context(), principal(c), c.Param("approvalID"))
	if err != nil {
		writeError(c, err)
		return
	}
	h.downloadSource(c, detail.Contract)
}
