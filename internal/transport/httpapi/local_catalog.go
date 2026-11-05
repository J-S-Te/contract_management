package httpapi

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// mountLocalCatalog 注册合同本地目录读写，沿用现有会话、许可和写请求同源门禁。
// 参数：api为已认证API路由组。
// 返回值：无；管理权限及数据范围仍由应用层逐次核验。
func (h *Handler) mountLocalCatalog(api *gin.RouterGroup) {
	h.mountLocalCatalogGrants(api)
	api.GET("/local-catalog/counterparties", h.listLocalCounterparties)
	api.POST("/local-catalog/counterparties", h.saveLocalCounterparty)
	api.PUT("/local-catalog/counterparties/:entryID", h.saveLocalCounterparty)
	api.GET("/local-catalog/categories", h.listLocalCategories)
	api.POST("/local-catalog/categories", h.saveLocalCategory)
	api.PUT("/local-catalog/categories/:entryID", h.saveLocalCategory)
}

// listLocalCounterparties 输出真实租户和授权范围的交易对方列表。
// 参数：c为当前已认证请求。
// 返回值：无；失败保留明确错误，不回空数组伪装成功。
func (h *Handler) listLocalCounterparties(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	values, err := h.service.ListLocalCounterparties(c.Request.Context(), principal(c), c.Query("keyword"), limit)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, values)
}

// saveLocalCounterparty 严格白名单读取目录字段，归属和租户始终来自会话。
// 参数：c为创建或带expected_version的更新请求。
// 返回值：无；不接受删除、CRM身份、客户端归属或状态绕过。
func (h *Handler) saveLocalCounterparty(c *gin.Context) {
	var body struct {
		Name            string `json:"name"`
		Enabled         *bool  `json:"enabled"`
		ExpectedVersion uint64 `json:"expected_version"`
	}
	if !decode(c, &body) {
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	value, err := h.service.SaveLocalCounterparty(c.Request.Context(), principal(c), contract.LocalCounterparty{ID: c.Param("entryID"), Name: body.Name, Enabled: enabled}, body.ExpectedVersion)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, value)
}

// listLocalCategories 输出真实授权范围内的类别与版本。
// 参数：c为当前已认证请求。
// 返回值：无。
func (h *Handler) listLocalCategories(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	values, err := h.service.ListLocalCategories(c.Request.Context(), principal(c), c.Query("keyword"), limit)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, values)
}

// saveLocalCategory 仅保存类别编码、名称、启用状态和乐观版本。
// 参数：c为创建或更新请求，客户端不提供租户及归属字段。
// 返回值：无；稳定编码不能在更新时替换。
func (h *Handler) saveLocalCategory(c *gin.Context) {
	var body struct {
		Code            string `json:"code"`
		Name            string `json:"name"`
		Enabled         *bool  `json:"enabled"`
		ExpectedVersion uint64 `json:"expected_version"`
	}
	if !decode(c, &body) {
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	value, err := h.service.SaveLocalCategory(c.Request.Context(), principal(c), contract.LocalCategory{ID: c.Param("entryID"), Code: body.Code, Name: body.Name, Enabled: enabled}, body.ExpectedVersion)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, value)
}
