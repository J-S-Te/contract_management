package httpapi

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/docx"
)

// Replacing a source only affects future drafts. Existing contracts retain
// their frozen document, values and hash and must follow the change workflow.
func (h *Handler) replaceTemplateSource(c *gin.Context) {
	actor := principal(c)
	if !actor.Has("contract.template.manage") {
		admin := false
		for _, role := range actor.Roles {
			if role == "admin" {
				admin = true
			}
		}
		if !admin {
			writeError(c, application.ErrForbidden)
			return
		}
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, docx.MaxTemplateSize+(1<<20))
	if err := c.Request.ParseMultipartForm(docx.MaxTemplateSize); err != nil {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "模板上传参数不合法或文件过大", nil)
		return
	}
	if c.Request.MultipartForm != nil {
		defer func() {
			if err := c.Request.MultipartForm.RemoveAll(); err != nil {
				_ = c.Error(err)
			}
		}()
	}
	header, err := c.FormFile("file")
	if err != nil || header.Size <= 0 || header.Size > docx.MaxTemplateSize {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "请选择不超过10MB的DOCX模板文件", nil)
		return
	}
	file, err := header.Open()
	if err != nil {
		writeError(c, err)
		return
	}
	content, readErr := io.ReadAll(io.LimitReader(file, docx.MaxTemplateSize+1))
	closeErr := file.Close()
	if readErr != nil {
		writeError(c, readErr)
		return
	}
	if closeErr != nil {
		writeError(c, closeErr)
		return
	}
	updated, err := h.service.ReplaceTemplateSource(c.Request.Context(), actor, c.Param("templateID"), header.Filename, content)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, updated)
}
