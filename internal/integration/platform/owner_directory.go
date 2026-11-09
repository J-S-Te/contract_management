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
	"strconv"
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
	// ownerDirectoryPageSize 保留平台默认页长，完整读取后再按精确姓名消歧。
	ownerDirectoryPageSize = 20
	// ownerDirectoryMaxCandidates 限制模糊查询总量；超限失败关闭，不能以首分页推断唯一。
	ownerDirectoryMaxCandidates = 1000
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

// ownerDirectoryWireUser 是平台的用户契约；user_id才是可传给CRM复核的身份。
type ownerDirectoryWireUser struct {
	UserID        string `json:"user_id"`
	DisplayName   string `json:"display_name"`
	Organizations []struct {
		ID        string `json:"organization_id"`
		Name      string `json:"organization_name"`
		IsPrimary bool   `json:"is_primary"`
	} `json:"organizations"`
}

// ownerDirectoryWirePage 保留必需字段是否存在，缺失不能伪装成空目录。
type ownerDirectoryWirePage struct {
	Items    *[]ownerDirectoryWireUser `json:"items"`
	Total    *int64                    `json:"total"`
	Page     int                       `json:"page"`
	PageSize int                       `json:"page_size"`
}

// Search 有界读取平台模糊姓名目录的完整分页候选。
// 参数：ctx限定整次查询，keyword为不超过100个Unicode字符的姓名关键字。
// 返回值：按权威user_id去重的候选；目录故障、超限或分页不完整时失败关闭。
func (c *OwnerDirectoryClient) Search(ctx context.Context, keyword string) ([]OwnerCandidate, error) {
	if c == nil || strings.TrimSpace(c.BaseURL) == "" || c.TokenSource == nil {
		return nil, errors.New("owner directory integration is not configured")
	}
	keyword = strings.TrimSpace(keyword)
	if keyword == "" || len([]rune(keyword)) > 100 {
		return nil, errors.New("owner directory keyword must be 1..100 characters")
	}
	ctx, cancel := context.WithTimeout(ctx, ownerDirectoryTimeout)
	defer cancel()
	token, err := c.TokenSource(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: obtain owner directory token: %v", ErrOwnerDirectoryUnavailable, err)
	}
	client := http.Client{Timeout: ownerDirectoryTimeout}
	if c.Client != nil {
		client = *c.Client
	}
	client.CheckRedirect = rejectOwnerRedirect
	seen, total := map[string]string{}, int64(-1)
	result := []OwnerCandidate{}
	for page := 1; page <= ownerDirectoryMaxCandidates/ownerDirectoryPageSize; page++ {
		data, err := c.fetchOwnerPage(ctx, &client, token, keyword, page)
		if err != nil {
			return nil, fmt.Errorf("%w: query owner directory: %v", ErrOwnerDirectoryUnavailable, err)
		}
		if total >= 0 && total != *data.Total {
			return nil, fmt.Errorf("%w: owner directory total changed", ErrOwnerDirectoryUnavailable)
		}
		total = *data.Total
		owners, err := normalizeOwnerDirectoryUsers(*data.Items, seen)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrOwnerDirectoryUnavailable, err)
		}
		result = append(result, owners...)
		if int64(page*ownerDirectoryPageSize) >= total {
			return result, nil
		}
	}
	return nil, fmt.Errorf("%w: owner directory exceeds page limit", ErrOwnerDirectoryUnavailable)
}

// fetchOwnerPage 请求单页并校验平台分页元数据及实际行数。
// 参数：ctx为整次查询上下文，client为禁重定向客户端，token为专有令牌，keyword和page为查询。
// 返回值：完整的单页；HTTP、体积、协议或分页边界异常返回错误。
func (c *OwnerDirectoryClient) fetchOwnerPage(ctx context.Context, client *http.Client,
	token, keyword string, page int) (ownerDirectoryWirePage, error) {
	query := url.Values{"keyword": {keyword}, "page": {strconv.Itoa(page)},
		"page_size": {strconv.Itoa(ownerDirectoryPageSize)}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.BaseURL, "/")+ownerDirectoryPath+"?"+query.Encode(), nil)
	if err != nil {
		return ownerDirectoryWirePage{}, err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return ownerDirectoryWirePage{}, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, ownerDirectoryMaxResponseBytes+1))
	if err != nil {
		return ownerDirectoryWirePage{}, err
	}
	if len(raw) > ownerDirectoryMaxResponseBytes || response.StatusCode != http.StatusOK {
		return ownerDirectoryWirePage{}, fmt.Errorf("owner directory response rejected (status %d)", response.StatusCode)
	}
	return decodeOwnerDirectoryPage(raw, page)
}

// decodeOwnerDirectoryPage 校验缺字段、总数、页码及页长，拒绝截断或空中间页。
// 参数：raw为有界响应字节，page为本次实际请求页码。
// 返回值：可信单页；不完整或错误形状返回协议错误。
func decodeOwnerDirectoryPage(raw []byte, page int) (ownerDirectoryWirePage, error) {
	var envelope struct {
		Data *ownerDirectoryWirePage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return ownerDirectoryWirePage{}, err
	}
	data := envelope.Data
	if data == nil || data.Items == nil || data.Total == nil || *data.Total < 0 ||
		*data.Total > ownerDirectoryMaxCandidates || data.Page != page || data.PageSize != ownerDirectoryPageSize {
		return ownerDirectoryWirePage{}, errors.New("owner directory pagination is incomplete or exceeds limit")
	}
	remaining := *data.Total - int64((page-1)*ownerDirectoryPageSize)
	if remaining < 0 || int64(len(*data.Items)) != min(int64(ownerDirectoryPageSize), remaining) {
		return ownerDirectoryWirePage{}, errors.New("owner directory page is incomplete")
	}
	return *data, nil
}

// normalizeOwnerDirectoryUsers 将平台user_id显式映射为合同候选并跨页去重。
// 参数：users为当前原始页，seen为已读取身份及姓名，用于拒绝身份姓名冲突。
// 返回值：新身份候选；缺身份、缺姓名或冲突返回错误，未提供的员工号和主组织保持空。
func normalizeOwnerDirectoryUsers(users []ownerDirectoryWireUser, seen map[string]string) ([]OwnerCandidate, error) {
	result := make([]OwnerCandidate, 0, len(users))
	for _, user := range users {
		id, name := strings.TrimSpace(user.UserID), strings.TrimSpace(user.DisplayName)
		if id == "" || len(id) > 128 || name == "" || len(name) > 255 {
			return nil, errors.New("owner directory returned an invalid candidate")
		}
		if previous, ok := seen[id]; ok {
			if previous != name {
				return nil, errors.New("owner directory returned conflicting names for one identity")
			}
			continue
		}
		seen[id] = name
		result = append(result, OwnerCandidate{IdentityID: id, DisplayName: name})
	}
	return result, nil
}

// ResolveOwnerByName 按完整精确姓名从完整候选集中解析唯一平台身份。
// 参数：ctx限定请求，displayName为Excel中的销售姓名，仅归一化首尾空格。
// 返回值：唯一候选；零精确命中返回ErrOwnerNotFound，不同身份同名返回ErrOwnerAmbiguous。
func (c *OwnerDirectoryClient) ResolveOwnerByName(ctx context.Context, displayName string) (OwnerCandidate, error) {
	displayName = strings.TrimSpace(displayName)
	candidates, err := c.Search(ctx, displayName)
	if err != nil {
		return OwnerCandidate{}, err
	}
	matches := []OwnerCandidate{}
	for _, candidate := range candidates {
		if candidate.DisplayName == displayName {
			matches = append(matches, candidate)
		}
	}
	switch len(matches) {
	case 0:
		return OwnerCandidate{}, fmt.Errorf("%w: %s", ErrOwnerNotFound, displayName)
	case 1:
		return matches[0], nil
	default:
		return OwnerCandidate{}, &OwnerAmbiguousError{Keyword: displayName, Candidates: matches}
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
