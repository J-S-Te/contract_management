// Package platform 承载合同系统对平台内部机器接口的客户端（当前为人员目录解析）。
package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
)

// OwnerDirectoryScope 是平台负责人目录仅供机器调用的最小 scope。
const OwnerDirectoryScope = "owner_directory.read"

const (
	// ownerDirectoryTimeout 是单次目录调用的超时上限。
	ownerDirectoryTimeout = 10 * time.Second
	// ownerDirectoryMaxResponseBytes 是响应体上限，超过即按依赖故障失败。
	ownerDirectoryMaxResponseBytes = 1 << 20
	// ownerDirectoryPageSize 固定为 20：历史导入按姓名解析销售只需要候选集，
	// 超过一页本身就说明重名，应判为多命中而不是翻页取全量。
	ownerDirectoryPageSize = "20"
	// ownerDirectoryPath 是平台真实路由（router.go 中 group("/api/v1") + "/internal/owner-directory"）。
	// BaseURL 只填平台 origin，/api/v1 前缀由客户端补全，与既有 personnel 目录客户端一致。
	ownerDirectoryPath = "/api/v1/internal/owner-directory"
)

// 目录解析的稳定分类错误：0 命中与多命中必须由调用方区分并各自阻断该行。
// 三个哨兵取自 application：编排层因此无需导入本集成包（避免循环依赖）
// 即可用 errors.Is 判定「0 命中 / 多命中 / 依赖不可用」。
var (
	// ErrOwnerNotFound 表示目录中没有任何匹配人员。
	ErrOwnerNotFound = application.ErrProvisionOwnerNotFound
	// ErrOwnerAmbiguous 表示目录中命中多个人员，需要人工指定。
	ErrOwnerAmbiguous = application.ErrProvisionOwnerAmbiguous
	// ErrOwnerDirectoryUnavailable 表示目录依赖不可用或协议不可解析（可重试）。
	ErrOwnerDirectoryUnavailable = application.ErrProvisionOwnerDirectoryUnavailable
)

// OwnerCandidate 复用 application 定义的人员候选：客户端因此直接满足
// application.CRMProvisionOwnerDirectory 注入接口，编排层无需二次转换。
type OwnerCandidate = application.OwnerDirectoryCandidate

// OwnerAmbiguousError 携带多命中候选，便于预览面板展示供人工选择。
type OwnerAmbiguousError struct {
	Keyword    string
	Candidates []OwnerCandidate
}

// Error 输出可落 Issue 的短描述，不回显凭据。
// 参数：无；使用接收者字段。
// 返回值：命中数量与关键字。
func (e *OwnerAmbiguousError) Error() string {
	return fmt.Sprintf("owner directory keyword %q matched %d owners", e.Keyword, len(e.Candidates))
}

// Is 让 errors.Is(err, ErrOwnerAmbiguous) 成立，同时保留候选明细。
// 参数：target 为期望的错误分类。
// 返回值：target 为 ErrOwnerAmbiguous 时返回 true。
func (e *OwnerAmbiguousError) Is(target error) bool { return target == ErrOwnerAmbiguous }

// OwnerDirectoryClient 用平台 client_credentials（scope owner_directory.read）按姓名解析销售。
type OwnerDirectoryClient struct {
	BaseURL     string
	Client      *http.Client
	TokenSource func(context.Context) (string, error)
}

// NewOwnerDirectoryTokenSource 构造只含 owner_directory.read 的机器令牌源。
// 参数：tokenURL 为平台令牌端点，clientID/clientSecret 为机器凭据。
// 返回值：令牌获取函数；任一必需项为空时返回 nil 由调用方失败关闭。
func NewOwnerDirectoryTokenSource(tokenURL, clientID, clientSecret string) func(context.Context) (string, error) {
	if strings.TrimSpace(tokenURL) == "" || strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return nil
	}
	client := &http.Client{Timeout: ownerDirectoryTimeout, CheckRedirect: rejectOwnerRedirect}
	tokenURL, clientID, clientSecret = strings.TrimSpace(tokenURL), strings.TrimSpace(clientID), strings.TrimSpace(clientSecret)
	return func(ctx context.Context) (string, error) {
		form := url.Values{"grant_type": {"client_credentials"}, "scope": {OwnerDirectoryScope}}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
		if err != nil {
			return "", err
		}
		request.SetBasicAuth(clientID, clientSecret)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := client.Do(request)
		if err != nil {
			return "", fmt.Errorf("request owner directory token: %w", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
			return "", fmt.Errorf("platform owner directory token returned %d", response.StatusCode)
		}
		var token struct {
			AccessToken string `json:"access_token"`
			TokenType   string `json:"token_type"`
			Scope       string `json:"scope"`
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&token); err != nil {
			return "", err
		}
		if strings.TrimSpace(token.AccessToken) == "" || !strings.EqualFold(token.TokenType, "bearer") ||
			!containsOwnerScope(token.Scope, OwnerDirectoryScope) {
			return "", fmt.Errorf("platform token missing %s scope", OwnerDirectoryScope)
		}
		return token.AccessToken, nil
	}
}

// NewOwnerDirectoryClient 组装负责人目录客户端。
// 参数：baseURL 为平台 origin（如 http://platform-api:8080，不要带 /api/v1，客户端自行补全），
// tokenURL/clientID/clientSecret 为机器凭据。
// 返回值：客户端；地址或凭据不全时返回 nil（调用方应按依赖未配置处理）。
func NewOwnerDirectoryClient(baseURL, tokenURL, clientID, clientSecret string) *OwnerDirectoryClient {
	if strings.TrimSpace(baseURL) == "" {
		return nil
	}
	source := NewOwnerDirectoryTokenSource(tokenURL, clientID, clientSecret)
	if source == nil {
		return nil
	}
	return &OwnerDirectoryClient{BaseURL: baseURL, TokenSource: source}
}

// Search 按姓名关键字查询负责人目录候选（固定 page=1、page_size=20）。
// 参数：ctx 限定请求，keyword 为销售姓名（调用方应已归一化）。
// 返回值：去重后的候选列表（可能为空）；依赖不可用或协议异常返回可重试错误。
func (c *OwnerDirectoryClient) Search(ctx context.Context, keyword string) ([]OwnerCandidate, error) {
	if c == nil || strings.TrimSpace(c.BaseURL) == "" || c.TokenSource == nil {
		return nil, errors.New("owner directory integration is not configured")
	}
	keyword = strings.TrimSpace(keyword)
	if keyword == "" || len(keyword) > 128 {
		return nil, errors.New("owner directory keyword must be 1..128 characters")
	}
	token, err := c.TokenSource(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: obtain owner directory token: %v", ErrOwnerDirectoryUnavailable, err)
	}
	query := url.Values{"keyword": {keyword}, "page": {"1"}, "page_size": {ownerDirectoryPageSize}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.BaseURL, "/")+ownerDirectoryPath+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	request.Header.Set("Accept", "application/json")
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: ownerDirectoryTimeout, CheckRedirect: rejectOwnerRedirect}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: query owner directory: %v", ErrOwnerDirectoryUnavailable, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, ownerDirectoryMaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read owner directory response: %v", ErrOwnerDirectoryUnavailable, err)
	}
	if len(raw) > ownerDirectoryMaxResponseBytes {
		return nil, fmt.Errorf("%w: owner directory response exceeds 1 MiB", ErrOwnerDirectoryUnavailable)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: owner directory returned status %d", ErrOwnerDirectoryUnavailable, response.StatusCode)
	}
	var envelope struct {
		Data *struct {
			Items []OwnerCandidate `json:"items"`
			Total int64            `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("%w: decode owner directory: %v", ErrOwnerDirectoryUnavailable, err)
	}
	if envelope.Data == nil || envelope.Data.Total < int64(len(envelope.Data.Items)) {
		return nil, fmt.Errorf("%w: owner directory response is incomplete", ErrOwnerDirectoryUnavailable)
	}
	seen := map[string]bool{}
	result := make([]OwnerCandidate, 0, len(envelope.Data.Items))
	for _, item := range envelope.Data.Items {
		item.IdentityID = strings.TrimSpace(item.IdentityID)
		item.DisplayName = strings.TrimSpace(item.DisplayName)
		if item.IdentityID == "" || len(item.IdentityID) > 128 || len(item.DisplayName) > 255 {
			return nil, fmt.Errorf("%w: owner directory returned an invalid candidate", ErrOwnerDirectoryUnavailable)
		}
		// 同一身份重复返回不算多命中；不同身份同名才算。
		if seen[item.IdentityID] {
			continue
		}
		seen[item.IdentityID] = true
		result = append(result, item)
	}
	return result, nil
}

// ResolveOwnerByName 把「销售姓名」解析成唯一 identity_id。
// 参数：ctx 限定请求，displayName 为 Excel 中的销售姓名。
// 返回值：唯一候选；0 命中返回包装 ErrOwnerNotFound 的错误，
// 多命中返回 *OwnerAmbiguousError（可用 errors.Is 判定 ErrOwnerAmbiguous）。
func (c *OwnerDirectoryClient) ResolveOwnerByName(ctx context.Context, displayName string) (OwnerCandidate, error) {
	candidates, err := c.Search(ctx, displayName)
	if err != nil {
		return OwnerCandidate{}, err
	}
	switch len(candidates) {
	case 0:
		return OwnerCandidate{}, fmt.Errorf("%w: %s", ErrOwnerNotFound, strings.TrimSpace(displayName))
	case 1:
		return candidates[0], nil
	default:
		return OwnerCandidate{}, &OwnerAmbiguousError{Keyword: strings.TrimSpace(displayName), Candidates: candidates}
	}
}

// containsOwnerScope 判断令牌 scope 是否覆盖给定 scope（空格分隔）。
// 参数：scope 为令牌返回的 scope 串，expected 为期望 scope。
// 返回值：覆盖返回 true。
func containsOwnerScope(scope, expected string) bool {
	for _, value := range strings.Fields(scope) {
		if value == expected {
			return true
		}
	}
	return false
}

// rejectOwnerRedirect 拒绝跟随重定向，避免机器令牌被转发到非契约地址。
// 参数：无。
// 返回值：固定 http.ErrUseLastResponse。
func rejectOwnerRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
