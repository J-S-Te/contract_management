package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
)

// ContractSummaryIntegrationOptions 配置 CRM 商机签约前的合同摘要核验接入。
// 该端点只返回验签令牌绑定租户内、属于调用方声明客户的合同最小投影。
type ContractSummaryIntegrationOptions struct {
	Enabled        bool
	RequireBearer  bool
	BearerVerifier platform.ClientCredentialsTokenVerifier
}

// contractSummaryData 是 CRM 核验合同摘要的响应数据形状：
// contract_id 为稳定 ULID，contract_number 非空，时间为 RFC3339。
// 该端点的对端（customer_and_opportunity 的 HTTPContractVerifier）使用
// DisallowUnknownFields 解码，因此本结构任何字段增删都必须与对端解码结构
// 同批次放开，否则对端会把整个成功响应判为无效（签约核验直接失败）。
// 注意：linked_at 在历史数据（无审核/审计证据）时回退合同 updated_at，属“近似关联时间”，
// 只能用于展示/存在性校验，不得作为业务先后顺序的判据；
// linked_at_precision 已显式暴露该风险，linked_at_source 说明证据来源。
type contractSummaryData struct {
	ContractID       string `json:"contract_id"`
	ContractNumber   string `json:"contract_number"`
	CRMCustomerID    uint64 `json:"crm_customer_id"`
	CRMOpportunityID uint64 `json:"crm_opportunity_id"`
	// LinkedAt 保持“最新一次”可证明关联时间（换绑后会跳变）。
	LinkedAt string `json:"linked_at"`
	// LinkedAtSource 取 CRM_REVIEW | REVIEW_LOG | AUDIT_LOG | INTAKE_CREATED | APPROX_CONTRACT_UPDATED。
	LinkedAtSource string `json:"linked_at_source"`
	// LinkedAtPrecision 取 exact | approx；approx 仅表示“仅供参考”，不代表客户归属核验失败。
	LinkedAtPrecision string `json:"linked_at_precision"`
	// FirstLinkedAt 是同一证据层内最早一次可证明关联时间。
	FirstLinkedAt string `json:"first_linked_at"`
}

// contractSummaryEnvelope 是 CRM 契约要求的精确信封。
// 成功响应不得携带非空 details（对端要求 len(details)==0）；
// 未找到时 data 显式为 null。
type contractSummaryEnvelope struct {
	RequestID string               `json:"request_id"`
	Code      string               `json:"code"`
	Message   string               `json:"message,omitempty"`
	Data      *contractSummaryData `json:"data"`
	Details   []any                `json:"details,omitempty"`
}

// grantContractSummaryPermission 给已验签的 CRM 机器主体授予唯一的摘要读权限。
// 该路由只在独立机器身份组内注册，权限不能被 cookie 会话复用。
func (h *Handler) grantContractSummaryPermission() gin.HandlerFunc {
	return func(c *gin.Context) {
		p := principal(c)
		if p.Permissions == nil {
			p.Permissions = map[string]bool{}
		}
		p.Permissions["contract.summary.read"] = true
		c.Set(principalKey, p)
		c.Next()
	}
}

// contractSummary 返回 CRM 核验所需的当前租户合同摘要。
// 参数：c 为已通过机器 Bearer 验签的请求，contract_ref 为合同 ID 或编号，
// crm_customer_id 为调用方声明的 CRM 客户。
// 返回值：成功时 200 精确信封；不存在/不属于该客户/未关联商机统一 404 CON_NOT_FOUND，
// 不泄漏其它客户或其它租户的合同是否存在。
func (h *Handler) contractSummary(c *gin.Context) {
	if !principal(c).Has("contract.summary.read") {
		writeEnvelopeError(c, http.StatusForbidden, "CON_FORBIDDEN", "当前机器身份没有合同摘要读取范围", nil)
		return
	}
	contractRef := strings.TrimSpace(c.Query("contract_ref"))
	customerRaw := strings.TrimSpace(c.Query("crm_customer_id"))
	if contractRef == "" || len([]rune(contractRef)) > 64 || customerRaw == "" {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "contract_ref 与 crm_customer_id 为必填参数", nil)
		return
	}
	customerID, err := strconv.ParseUint(customerRaw, 10, 64)
	if err != nil || customerID == 0 || strconv.FormatUint(customerID, 10) != customerRaw {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "crm_customer_id 必须是规范的正整数", nil)
		return
	}
	summary, err := h.service.ContractSummary(c.Request.Context(), principal(c).TenantID, contractRef, customerID)
	if err != nil {
		if errors.Is(err, apperrors.ErrNotFound) {
			writeContractSummaryNotFound(c)
			return
		}
		writeError(c, err)
		return
	}
	writeJSON(c, http.StatusOK, contractSummaryEnvelope{
		RequestID: requestIDFrom(c.Request.Context()),
		Code:      "OK",
		Data: &contractSummaryData{
			ContractID:        summary.ContractID,
			ContractNumber:    summary.ContractNumber,
			CRMCustomerID:     summary.CRMCustomerID,
			CRMOpportunityID:  summary.CRMOpportunityID,
			LinkedAt:          summary.LinkedAt.UTC().Format(time.RFC3339),
			LinkedAtSource:    summary.LinkedAtSource,
			LinkedAtPrecision: summary.LinkedAtPrecision,
			FirstLinkedAt:     summary.FirstLinkedAt.UTC().Format(time.RFC3339),
		},
	})
}

// writeContractSummaryNotFound 输出 CRM 契约要求的 404 形状：data 为 null。
func writeContractSummaryNotFound(c *gin.Context) {
	writeJSON(c, http.StatusNotFound, contractSummaryEnvelope{
		RequestID: requestIDFrom(c.Request.Context()),
		Code:      "CON_NOT_FOUND",
		Message:   "合同不存在或不属于该客户",
	})
}
