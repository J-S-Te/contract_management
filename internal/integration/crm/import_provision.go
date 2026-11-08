package crm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/j-s-te/contract-management/internal/application"
)

// 历史导入开立 CRM 的专用最小 scope（设计定稿 4 节）。
const (
	// ImportProvisionCustomerScope 覆盖客户查重与客户创建。
	ImportProvisionCustomerScope = "customer.import_provision"
	// ImportProvisionOpportunityScope 覆盖商机创建与推进签约。
	ImportProvisionOpportunityScope = "opportunity.import_provision"
	// importProvisionTimeout 是单次开立调用（含超时）上限。
	importProvisionTimeout = 10 * time.Second
	// importProvisionMaxResponseBytes 是响应体上限，超过即按依赖故障处理，不解析部分内容。
	importProvisionMaxResponseBytes = 1 << 20
)

// CRM import-provision 端点路径（设计定稿 4 节）。
const (
	importProvisionPathCustomersLookup = "/api/v1/internal/import-provision/customers/lookup"
	importProvisionPathCustomers       = "/api/v1/internal/import-provision/customers"
	importProvisionPathOpportunities   = "/api/v1/internal/import-provision/opportunities"
)

// ErrImportProvisionUnavailable 表示开立调用遇到可重试的依赖故障
// （网络、超时、HTTP 5xx、响应超限或协议不可解析）。
// 它与 application.ErrCRMProvisionUnavailable 是同一个哨兵值：编排层因此可以
// 在不导入本集成包（避免循环依赖）的前提下用 errors.Is 判定可重试分类。
var ErrImportProvisionUnavailable = application.ErrCRMProvisionUnavailable

// ProvisionRejectedError 是 CRM 明确拒绝（HTTP 4xx 且带稳定 code）的业务失败。
// 它表示数据本身不合法或状态冲突，重试不会改变结果。
type ProvisionRejectedError struct {
	StatusCode int
	Code       string
	Message    string
	RequestID  string
}

// Error 输出不含凭据的分类信息。
// 参数：无；使用接收者字段。
// 返回值：可直接落 Issue 的短描述。
func (e *ProvisionRejectedError) Error() string {
	return fmt.Sprintf("CRM import provision rejected: status=%d code=%s message=%s", e.StatusCode, e.Code, e.Message)
}

// Retryable 表明业务拒绝不可通过重试恢复。
// 参数：无。
// 返回值：恒为 false。
func (e *ProvisionRejectedError) Retryable() bool { return false }

// Unwrap 让编排层用 errors.Is(err, application.ErrCRMProvisionRejected) 判定业务拒绝，
// 而不必依赖本包的具体错误类型。
// 参数：无。
// 返回值：应用层业务拒绝哨兵。
func (e *ProvisionRejectedError) Unwrap() error { return application.ErrCRMProvisionRejected }

// ProvisionCode 暴露 CRM 稳定错误码，供编排映射为逐项 Issue 码。
// 参数：无。
// 返回值：CRM 返回的 code（可能为空）。
func (e *ProvisionRejectedError) ProvisionCode() string { return e.Code }

// ImportProvisionUnavailableError 是可重试的依赖故障，同时保留 CRM 稳定 code。
// 它用于 5xx 带 code 的语义化失败（例如 503 CRM_IMPORT_PROVISION_CONTRACT_NOT_VERIFIED）：
// 分类仍是可重试依赖故障，但编排需要看到 code 才能给出可操作提示。
type ImportProvisionUnavailableError struct {
	StatusCode int
	Code       string
}

// Error 输出不含凭据的分类信息。
// 参数：无；使用接收者字段。
// 返回值：可直接落 Issue 的短描述。
func (e *ImportProvisionUnavailableError) Error() string {
	return fmt.Sprintf("CRM import provision unavailable: status %d code=%s", e.StatusCode, e.Code)
}

// Unwrap 保留 errors.Is(err, ErrImportProvisionUnavailable) 语义。
// 参数：无。
// 返回值：可重试依赖故障哨兵。
func (e *ImportProvisionUnavailableError) Unwrap() error { return ErrImportProvisionUnavailable }

// ProvisionCode 暴露 CRM 稳定错误码，供编排映射为逐项 Issue 码。
// 参数：无。
// 返回值：CRM 返回的 code（可能为空）。
func (e *ImportProvisionUnavailableError) ProvisionCode() string { return e.Code }

// ImportProvisionRetryable 判断一次开立调用错误是否值得重试。
// 参数：err 为客户端返回的错误。
// 返回值：仅可重试的依赖故障返回 true；业务拒绝与配置错误返回 false。
func ImportProvisionRetryable(err error) bool {
	return err != nil && errors.Is(err, ErrImportProvisionUnavailable)
}

// ImportProvisionClient 是历史导入开立 CRM 客户/商机的机器接入客户端。
// 令牌固定用平台 client_credentials 取得；平台侧一用途一 client 一 scope，
// 因此客户链路与商机链路必须各用一个客户端（客户链路 scope
// customer.import_provision，商机链路 scope opportunity.import_provision）。
// 每次调用带 Idempotency-Key、操作者、时间戳与一次性 nonce 头。
type ImportProvisionClient struct {
	BaseURL     string
	Client      *http.Client
	TokenSource func(context.Context) (string, error)
	// Now/Nonce 仅用于测试替换时间戳与 nonce；生产使用系统时间与 crypto/rand。
	Now   func() time.Time
	Nonce func() (string, error)
}

// ProvisionCustomerLookup 是客户查重请求体。
type ProvisionCustomerLookup struct {
	Name string `json:"name"`
}

// 开立通道的请求/响应投影由 application 定义并在此以别名复用：
// 客户端方法因此直接满足 application.CRMProvision*Client 注入接口，
// 编排层不需要在 integration 与 application 之间再做一次类型转换。
type (
	// ProvisionCustomerCandidate 是客户查重返回的候选。
	ProvisionCustomerCandidate = application.CRMProvisionCustomerCandidate
	// ProvisionCustomerInput 是创建客户的白名单输入。
	ProvisionCustomerInput = application.CRMProvisionCustomerInput
	// ProvisionCustomer 是创建客户的结果投影。
	ProvisionCustomer = application.CRMProvisionCustomer
	// ProvisionOpportunityInput 是创建商机的白名单输入。
	ProvisionOpportunityInput = application.CRMProvisionOpportunityInput
	// ProvisionOpportunity 是创建/推进商机的结果投影。
	ProvisionOpportunity = application.CRMProvisionOpportunity
)

// NewImportProvisionTokenSource 构造只含单一开立专用 scope 的机器令牌源。
// 参数：ctx 仅用于绑定受控 HTTP 客户端（不随请求取消），tokenURL/clientID/clientSecret 为平台机器凭据，
// scope 为客户或商机其中一条开立链路的专用 scope（平台侧一用途一 client 一 scope，
// 传多个 scope 会被令牌端点以 invalid_scope 拒绝）。
// 返回值：令牌获取函数；任一必需项为空时返回 nil 由调用方失败关闭。
func NewImportProvisionTokenSource(ctx context.Context, tokenURL, clientID, clientSecret, scope string) func(context.Context) (string, error) {
	scopes := strings.Fields(scope)
	if strings.TrimSpace(tokenURL) == "" || strings.TrimSpace(clientID) == "" ||
		strings.TrimSpace(clientSecret) == "" || len(scopes) != 1 {
		return nil
	}
	credentials := clientcredentials.Config{
		ClientID: clientID, ClientSecret: clientSecret, TokenURL: tokenURL,
		Scopes: scopes, AuthStyle: oauth2.AuthStyleInHeader,
	}
	tokenContext := context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: importProvisionTimeout, CheckRedirect: rejectRedirect})
	source := credentials.TokenSource(tokenContext)
	return func(context.Context) (string, error) {
		token, err := source.Token()
		if err != nil {
			return "", err
		}
		if token == nil || strings.TrimSpace(token.AccessToken) == "" {
			return "", errors.New("CRM token response has no access_token")
		}
		return token.AccessToken, nil
	}
}

// NewImportProvisionClient 组装经过启动校验的开立客户端。
// 参数：ctx 用于构造令牌源，baseURL 为 CRM 服务地址，其余为平台机器凭据与专用 scope。
// 返回值：客户端；地址或凭据不全时返回 nil（调用方应按开关关闭处理）。
func NewImportProvisionClient(ctx context.Context, baseURL, tokenURL, clientID, clientSecret, scope string) *ImportProvisionClient {
	if strings.TrimSpace(baseURL) == "" {
		return nil
	}
	source := NewImportProvisionTokenSource(ctx, tokenURL, clientID, clientSecret, scope)
	if source == nil {
		return nil
	}
	return &ImportProvisionClient{BaseURL: baseURL, TokenSource: source}
}

// LookupCustomer 按客户名称精确查重（0 命中表示需要新建）。
// 参数：ctx 限定请求，actor 为真实操作者身份，idempotencyKey 为本次查重键，
// name 为客户名称（调用方应已归一化）。
// 返回值：候选列表（可能为空）；业务拒绝返回 *ProvisionRejectedError，依赖故障返回可重试错误。
func (c *ImportProvisionClient) LookupCustomer(ctx context.Context, actor, idempotencyKey, name string) ([]ProvisionCustomerCandidate, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("CRM import provision customer lookup requires a name")
	}
	var data struct {
		Candidates []ProvisionCustomerCandidate `json:"candidates"`
	}
	if err := c.do(ctx, http.MethodPost, importProvisionPathCustomersLookup, actor, idempotencyKey,
		ProvisionCustomerLookup{Name: name}, &data); err != nil {
		return nil, err
	}
	return data.Candidates, nil
}

// CreateCustomer 在 IMPORT_PROVISION 通道创建 CRM 客户。
// 参数：ctx 限定请求，actor 为真实操作者身份，idempotencyKey 为创建幂等键，
// input 为白名单客户字段（含销售 identity_id）。
// 返回值：已创建的客户；缺失 ID 的响应按依赖故障处理，业务拒绝返回 *ProvisionRejectedError。
func (c *ImportProvisionClient) CreateCustomer(ctx context.Context, actor, idempotencyKey string, input ProvisionCustomerInput) (ProvisionCustomer, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || strings.TrimSpace(input.OwnerIdentityID) == "" {
		return ProvisionCustomer{}, errors.New("CRM import provision customer requires name and owner identity")
	}
	var data ProvisionCustomer
	if err := c.do(ctx, http.MethodPost, importProvisionPathCustomers, actor, idempotencyKey, input, &data); err != nil {
		return ProvisionCustomer{}, err
	}
	if data.ID == 0 {
		return ProvisionCustomer{}, fmt.Errorf("%w: CRM returned customer without id", ErrImportProvisionUnavailable)
	}
	return data, nil
}

// CreateOpportunity 在 IMPORT_PROVISION 通道创建 CRM 商机。
// 参数：ctx 限定请求，actor 为真实操作者身份，idempotencyKey 为创建幂等键，
// input 为客户 ID、销售 identity_id、名称、金额与预计签约日。
// 返回值：已创建的商机；缺失 ID 的响应按依赖故障处理，业务拒绝返回 *ProvisionRejectedError。
func (c *ImportProvisionClient) CreateOpportunity(ctx context.Context, actor, idempotencyKey string, input ProvisionOpportunityInput) (ProvisionOpportunity, error) {
	if input.CustomerID == 0 || strings.TrimSpace(input.OwnerIdentityID) == "" ||
		strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.Amount) == "" {
		return ProvisionOpportunity{}, errors.New("CRM import provision opportunity requires customer, owner, name and amount")
	}
	var data ProvisionOpportunity
	if err := c.do(ctx, http.MethodPost, importProvisionPathOpportunities, actor, idempotencyKey, input, &data); err != nil {
		return ProvisionOpportunity{}, err
	}
	if data.ID == 0 || data.Version == 0 {
		return ProvisionOpportunity{}, fmt.Errorf("%w: CRM returned opportunity without id", ErrImportProvisionUnavailable)
	}
	return data, nil
}

// SignOpportunity 把商机推进到「已签约 + CLOSED」，由 CRM 侧校验 contract_ref 与合同验真。
// 参数：ctx 限定请求，actor 为真实操作者身份，idempotencyKey 为推进幂等键，
// opportunityID 为已创建商机，contractRef 为已有编号的合同引用，expectedVersion为CRM返回版本。
// 返回值：推进后的商机；验真失败等业务拒绝返回 *ProvisionRejectedError。
func (c *ImportProvisionClient) SignOpportunity(ctx context.Context, actor, idempotencyKey string, opportunityID uint64, contractRef string, expectedVersion uint64) (ProvisionOpportunity, error) {
	contractRef = strings.TrimSpace(contractRef)
	if opportunityID == 0 || contractRef == "" || expectedVersion == 0 {
		return ProvisionOpportunity{}, errors.New("CRM import provision sign requires opportunity id and contract ref")
	}
	var data ProvisionOpportunity
	path := importProvisionPathOpportunities + "/" + strconv.FormatUint(opportunityID, 10) + "/sign"
	payload := struct {
		ContractRef     string `json:"contract_ref"`
		ExpectedVersion uint64 `json:"expected_version"`
		Reason          string `json:"reason"`
	}{ContractRef: contractRef, ExpectedVersion: expectedVersion, Reason: "历史合同导入确认签约"}
	if err := c.do(ctx, http.MethodPost, path, actor, idempotencyKey, payload, &data); err != nil {
		return ProvisionOpportunity{}, err
	}
	if data.ID != opportunityID || data.Version == 0 || data.Status != "CLOSED" || data.Stage != "已签约" {
		return ProvisionOpportunity{}, fmt.Errorf("%w: CRM did not confirm the requested signed opportunity", ErrImportProvisionUnavailable)
	}
	return data, nil
}

// do 发送一次带机器令牌与重放防护头的 JSON 请求，并统一分类失败。
// 参数：ctx 限定请求，method/path 为固定契约端点，actor 为操作者身份，
// idempotencyKey 为必填幂等键，payload 为请求体，out 为 data 段的解码目标。
// 返回值：业务拒绝返回 *ProvisionRejectedError；网络/超时/5xx/协议异常返回包装
// ErrImportProvisionUnavailable 的可重试错误；配置缺失返回不可重试错误。
func (c *ImportProvisionClient) do(ctx context.Context, method, path, actor, idempotencyKey string, payload any, out any) error {
	if c == nil || strings.TrimSpace(c.BaseURL) == "" || c.TokenSource == nil {
		return errors.New("CRM import provision integration is not configured")
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return errors.New("CRM import provision requires an actor identity")
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return errors.New("CRM import provision requires an Idempotency-Key")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode CRM import provision request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	token, err := c.TokenSource(ctx)
	if err != nil {
		return fmt.Errorf("%w: obtain CRM import provision token: %v", ErrImportProvisionUnavailable, err)
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	nonce := randomNonce
	if c.Nonce != nil {
		nonce = c.Nonce
	}
	nonceValue, err := nonce()
	if err != nil {
		return fmt.Errorf("%w: generate CRM request nonce: %v", ErrImportProvisionUnavailable, err)
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	request.Header.Set("X-Actor-Identity-ID", actor)
	request.Header.Set("X-Integration-Timestamp", now().UTC().Format(time.RFC3339Nano))
	request.Header.Set("X-Integration-Nonce", nonceValue)
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: importProvisionTimeout, CheckRedirect: rejectRedirect}
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: call CRM import provision: %v", ErrImportProvisionUnavailable, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, importProvisionMaxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("%w: read CRM import provision response: %v", ErrImportProvisionUnavailable, err)
	}
	if len(raw) > importProvisionMaxResponseBytes {
		return fmt.Errorf("%w: CRM import provision response exceeds 1 MiB", ErrImportProvisionUnavailable)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return classifyImportProvisionFailure(response.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	var envelope struct {
		Code      string          `json:"code"`
		Message   string          `json:"message"`
		RequestID string          `json:"request_id"`
		Data      json.RawMessage `json:"data"`
	}
	// 信封只依赖 data 段：允许 CRM 增加诊断字段，避免契约扩展把导入打成依赖故障。
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("%w: decode CRM import provision envelope: %v", ErrImportProvisionUnavailable, err)
	}
	if len(envelope.Data) == 0 {
		return fmt.Errorf("%w: CRM import provision response has no data", ErrImportProvisionUnavailable)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("%w: decode CRM import provision data: %v", ErrImportProvisionUnavailable, err)
	}
	return nil
}

// classifyImportProvisionFailure 按 HTTP 状态与错误码分类开立失败。
// 参数：status 为 HTTP 状态，body 为受限响应体。
// 返回值：4xx 且带稳定 code 返回 *ProvisionRejectedError（不可重试）；
// 5xx、4xx 无 code 与不可解析响应返回包装 ErrImportProvisionUnavailable 的可重试错误。
func classifyImportProvisionFailure(status int, body []byte) error {
	var failure struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}
	code, message, requestID := "", "", ""
	if json.Unmarshal(body, &failure) == nil {
		code, message, requestID = strings.TrimSpace(failure.Code), strings.TrimSpace(failure.Message), strings.TrimSpace(failure.RequestID)
	}
	if status >= 400 && status < 500 && code != "" {
		return &ProvisionRejectedError{StatusCode: status, Code: code, Message: message, RequestID: requestID}
	}
	// 5xx（含带 code 的语义化 503）仍是可重试依赖故障，但保留 code 供编排给出
	// 可操作提示（例如合同验真未配置）。
	return &ImportProvisionUnavailableError{StatusCode: status, Code: code}
}
