package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
)

// HandoverIntegration 承载人员离职交接的机器协议端点；未配置时整组路由不注册。
type HandoverIntegration struct {
	service *application.HandoverService
	options HandoverIntegrationOptions
}

type handoverFreezeRequest struct {
	TenantID  string `json:"tenant_id"`
	BatchID   string `json:"batch_id"`
	RequestID string `json:"request_id"`
	UserID    string `json:"user_id"`
}

type handoverTransferRequest struct {
	TenantID        string `json:"tenant_id"`
	BatchID         string `json:"batch_id"`
	ItemID          string `json:"item_id"`
	RequestID       string `json:"request_id"`
	ResourceType    string `json:"resource_type"`
	ResourceID      string `json:"resource_id"`
	FromUserID      string `json:"from_user_id"`
	ToUserID        string `json:"to_user_id"`
	ExpectedVersion uint64 `json:"expected_version"`
	IdempotencyKey  string `json:"idempotency_key"`
}

type handoverCompleteRequest struct {
	TenantID  string `json:"tenant_id"`
	BatchID   string `json:"batch_id"`
	RequestID string `json:"request_id"`
	Outcome   string `json:"outcome"`
}

// registerHandoverRoutes 注册交接机器协议端点：与结算/项目集成同样使用服务间凭据，
// 租户边界只来自令牌绑定，不接受请求体或请求头覆盖。
func (h *Handler) registerHandoverRoutes(group *gin.RouterGroup) {
	group.POST("/freeze", h.handoverFreeze)
	group.GET("/responsibilities", h.handoverResponsibilities)
	group.POST("/transfers", h.handoverTransfer)
	group.GET("/transfers/:itemID", h.handoverReceipt)
	group.POST("/complete", h.handoverComplete)
}

func (h *Handler) handoverServiceReady(c *gin.Context) (*application.HandoverService, bool) {
	if h.handover == nil || h.handover.service == nil {
		c.JSON(http.StatusServiceUnavailable, envelope{Code: "HANDOVER_UNAVAILABLE", Message: "交接端点未启用"})
		return nil, false
	}
	return h.handover.service, true
}

// tenantOf 强制使用令牌绑定租户；请求体里的 tenant_id 只能与之一致，不能覆盖。
func handoverTenant(c *gin.Context, requested string) (string, bool) {
	tenant := strings.TrimSpace(principal(c).TenantID)
	if tenant == "" {
		c.JSON(http.StatusForbidden, envelope{Code: "TENANT_REQUIRED", Message: "交接请求缺少令牌绑定租户"})
		return "", false
	}
	if strings.TrimSpace(requested) != "" && strings.TrimSpace(requested) != tenant {
		c.JSON(http.StatusForbidden, envelope{Code: "TENANT_MISMATCH", Message: "请求租户与令牌绑定租户不一致"})
		return "", false
	}
	return tenant, true
}

func (h *Handler) handoverFreeze(c *gin.Context) {
	service, ok := h.handoverServiceReady(c)
	if !ok {
		return
	}
	var payload handoverFreezeRequest
	if err := c.ShouldBindJSON(&payload); err != nil {
		writeHandoverError(c, application.ErrHandoverValidation)
		return
	}
	tenant, ok := handoverTenant(c, payload.TenantID)
	if !ok {
		return
	}
	freeze, count, err := service.Freeze(c.Request.Context(), tenant, payload.BatchID, payload.RequestID, payload.UserID)
	if err != nil {
		writeHandoverError(c, err)
		return
	}
	c.JSON(http.StatusOK, envelope{Code: "OK", Message: "冻结已确认", Data: gin.H{
		"system": "contract", "frozen": freeze.Status == application.HandoverFreezeFrozen,
		"frozen_at": freeze.FrozenAt, "responsibility_count": count,
	}})
}

func (h *Handler) handoverResponsibilities(c *gin.Context) {
	service, ok := h.handoverServiceReady(c)
	if !ok {
		return
	}
	tenant, ok := handoverTenant(c, c.Query("tenant_id"))
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	items, next, err := service.Responsibilities(c.Request.Context(), tenant, c.Query("user_id"), c.Query("cursor"), limit)
	if err != nil {
		writeHandoverError(c, err)
		return
	}
	c.JSON(http.StatusOK, envelope{Code: "OK", Message: "责任清单", Data: gin.H{
		"system": "contract", "items": items, "next_cursor": next,
	}})
}

func (h *Handler) handoverTransfer(c *gin.Context) {
	service, ok := h.handoverServiceReady(c)
	if !ok {
		return
	}
	var payload handoverTransferRequest
	if err := c.ShouldBindJSON(&payload); err != nil {
		writeHandoverError(c, application.ErrHandoverValidation)
		return
	}
	tenant, ok := handoverTenant(c, payload.TenantID)
	if !ok {
		return
	}
	receipt, err := service.Transfer(c.Request.Context(), application.HandoverTransferInput{
		TenantID: tenant, BatchID: payload.BatchID, ItemID: payload.ItemID, RequestID: payload.RequestID,
		ResourceType: payload.ResourceType, ResourceID: payload.ResourceID,
		FromUserID: payload.FromUserID, ToUserID: payload.ToUserID,
		ExpectedVersion: payload.ExpectedVersion, IdempotencyKey: payload.IdempotencyKey,
	})
	if err != nil {
		writeHandoverError(c, err)
		return
	}
	c.JSON(http.StatusOK, envelope{Code: "OK", Message: "转移回执", Data: gin.H{
		"system": "contract", "item_id": receipt.ItemID, "idempotency_key": receipt.IdempotencyKey,
		"status": receipt.Status, "applied_version": receipt.AppliedVersion,
		"reason_code": receipt.ReasonCode, "evidence_ref": receipt.EvidenceRef, "received_at": receipt.ReceivedAt,
	}})
}

func (h *Handler) handoverReceipt(c *gin.Context) {
	service, ok := h.handoverServiceReady(c)
	if !ok {
		return
	}
	tenant, ok := handoverTenant(c, c.Query("tenant_id"))
	if !ok {
		return
	}
	receipt, err := service.Receipt(c.Request.Context(), tenant, c.Query("idempotency_key"))
	if err != nil {
		writeHandoverError(c, err)
		return
	}
	c.JSON(http.StatusOK, envelope{Code: "OK", Message: "转移回执", Data: gin.H{
		"system": "contract", "item_id": receipt.ItemID, "idempotency_key": receipt.IdempotencyKey,
		"status": receipt.Status, "applied_version": receipt.AppliedVersion,
		"reason_code": receipt.ReasonCode, "evidence_ref": receipt.EvidenceRef, "received_at": receipt.ReceivedAt,
	}})
}

func (h *Handler) handoverComplete(c *gin.Context) {
	service, ok := h.handoverServiceReady(c)
	if !ok {
		return
	}
	var payload handoverCompleteRequest
	if err := c.ShouldBindJSON(&payload); err != nil {
		writeHandoverError(c, application.ErrHandoverValidation)
		return
	}
	tenant, ok := handoverTenant(c, payload.TenantID)
	if !ok {
		return
	}
	if err := service.Complete(c.Request.Context(), tenant, payload.BatchID, payload.Outcome); err != nil {
		writeHandoverError(c, err)
		return
	}
	c.JSON(http.StatusOK, envelope{Code: "OK", Message: "交接已结束", Data: gin.H{"system": "contract", "batch_id": payload.BatchID, "outcome": strings.ToUpper(payload.Outcome)}})
}

// writeHandoverError 把交接错误映射为确定性状态码，避免平台猜测结果。
func writeHandoverError(c *gin.Context, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, application.ErrHandoverValidation):
		c.JSON(http.StatusUnprocessableEntity, envelope{Code: "HANDOVER_VALIDATION", Message: "交接请求参数不合法"})
	case errors.Is(err, application.ErrHandoverNotFound):
		c.JSON(http.StatusNotFound, envelope{Code: "HANDOVER_NOT_FOUND", Message: "交接记录不存在"})
	case errors.Is(err, application.ErrHandoverFrozen):
		c.JSON(http.StatusConflict, envelope{Code: "HANDOVER_SUCCESSOR_FROZEN", Message: "承接人处于交接冻结状态"})
	case errors.Is(err, application.ErrHandoverConflict):
		c.JSON(http.StatusConflict, envelope{Code: "HANDOVER_CONFLICT", Message: "交接状态或版本冲突"})
	default:
		c.JSON(http.StatusInternalServerError, envelope{Code: "HANDOVER_INTERNAL", Message: "交接处理失败"})
	}
}
