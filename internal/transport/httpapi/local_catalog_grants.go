package httpapi

import (
	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"net/http"
)

// mountLocalCatalogGrants 复用原API认证、CSRF和许可外壳注册目录授权入口。
// 参数：api为既有已认证路由组。
// 返回值：无；写操作仍由服务端管理权限及真实目标校验。
func (h *Handler) mountLocalCatalogGrants(api *gin.RouterGroup) {
	api.GET("/local-catalog/grant-targets", h.localCatalogTargets)
	for _, kind := range []string{"counterparties", "categories"} {
		api.GET("/local-catalog/"+kind+"/:entryID/grants", func(c *gin.Context) { h.listCatalogGrants(c, kind) })
		api.POST("/local-catalog/"+kind+"/:entryID/grants", func(c *gin.Context) { h.saveCatalogGrant(c, kind) })
	}
}

// localCatalogTargets 返回平台当前有效销售与组织，禁止前端自由填写目标ID。
// 参数：c为已有会话请求。
// 返回值：无；权限或目录故障保留明确失败。
func (h *Handler) localCatalogTargets(c *gin.Context) {
	values, err := h.service.ListLocalCatalogTargets(c.Request.Context(), principal(c))
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, values)
}

// listCatalogGrants 返回当前可管理条目的授权及撤销版本。
// 参数：c为会话请求，kind为服务端固定条目种类。
// 返回值：无；不存在或越范围沿原错误契约失败关闭。
func (h *Handler) listCatalogGrants(c *gin.Context, kind string) {
	values, err := h.service.ListLocalCatalogGrants(c.Request.Context(), principal(c), kind, c.Param("entryID"))
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, values)
}

// saveCatalogGrant 严格读取稳定目标、显式启停和预期版本，不允许覆盖租户或归属。
// 参数：c为原安全外壳下的写请求，kind为固定条目种类。
// 返回值：无；真实授权版本或授权、目标、版本及审计错误。
func (h *Handler) saveCatalogGrant(c *gin.Context, kind string) {
	var body struct {
		SubjectType     string `json:"subject_type"`
		SubjectID       string `json:"subject_id"`
		Enabled         *bool  `json:"enabled"`
		ExpectedVersion uint64 `json:"expected_version"`
	}
	if !decode(c, &body) {
		return
	}
	if body.Enabled == nil {
		writeError(c, application.ErrValidation)
		return
	}
	value, err := h.service.SaveLocalCatalogGrant(c.Request.Context(), principal(c), kind, c.Param("entryID"), contract.LocalCatalogGrantInput{SubjectType: body.SubjectType, SubjectID: body.SubjectID, Enabled: *body.Enabled, ExpectedVersion: body.ExpectedVersion})
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, value)
}
