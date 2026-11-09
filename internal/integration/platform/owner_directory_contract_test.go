package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ownerWireUser 提供独立于合同域模型的平台真实用户响应夹具。
// 参数：id为平台user_id，name为权威显示姓名。
// 返回值：含组织归属的平台用户JSON结构，不伪造员工号。
func ownerWireUser(id, name string) map[string]any {
	return map[string]any{"user_id": id, "display_name": name, "organizations": []any{
		map[string]any{"organization_id": "org-1", "organization_name": "销售部", "is_primary": true},
	}}
}

// ownerWirePage 构造平台实际分页信封，避免复用客户端类型掩盖契约错误。
// 参数：t为测试上下文，items为原始候选，page和total为平台分页事实。
// 返回值：完整JSON；编码失败时终止测试。
func ownerWirePage(t *testing.T, items []map[string]any, page, total int) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"data": map[string]any{
		"items": items, "page": page, "page_size": 20, "total": total,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// pagedOwnerClient 通过真实HTTP响应模拟平台分页，核对专有令牌和查询参数。
// 参数：t为上下文，respond按请求页码输出状态与完整响应体。
// 返回值：仅指向测试服务器的客户端；不连接任何业务目录。
func pagedOwnerClient(t *testing.T, respond func(int) (int, string)) *OwnerDirectoryClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil || page < 1 || r.URL.Path != ownerDirectoryPath ||
			r.URL.Query().Get("keyword") != "张三" || r.URL.Query().Get("page_size") != "20" ||
			r.Header.Get("Authorization") != "Bearer directory-token" {
			t.Errorf("unexpected directory request: %s", r.URL)
		}
		status, body := respond(page)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return &OwnerDirectoryClient{BaseURL: server.URL, Client: server.Client(),
		TokenSource: func(context.Context) (string, error) { return "directory-token", nil }}
}

// TestOwnerDirectoryMatchesCanonicalIDAndExactName 覆盖真实字段及前缀姓名消歧。
// 参数：t为测试上下文，响应保留完整平台JSON形状。
// 返回值：无；错误身份、前缀误选或真正重名未阻断时失败。
func TestOwnerDirectoryMatchesCanonicalIDAndExactName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items []map[string]any
		want  error
	}{
		{"prefix-and-exact", []map[string]any{ownerWireUser("prefix", "张三丰"), ownerWireUser("exact", "张三")}, nil},
		{"prefix-only", []map[string]any{ownerWireUser("prefix", "张三丰")}, ErrOwnerNotFound},
		{"true-duplicate", []map[string]any{ownerWireUser("one", "张三"), ownerWireUser("two", "张三")}, ErrOwnerAmbiguous},
		{"wrong-identity-field", []map[string]any{{"identity_id": "forged", "display_name": "张三", "organizations": []any{}}}, ErrOwnerDirectoryUnavailable},
		{"wrong-organizations-shape", []map[string]any{{"user_id": "one", "display_name": "张三", "organizations": "invalid"}}, ErrOwnerDirectoryUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := pagedOwnerClient(t, func(page int) (int, string) {
				return http.StatusOK, ownerWirePage(t, tc.items, page, len(tc.items))
			})
			owner, err := client.ResolveOwnerByName(context.Background(), "张三")
			if !errors.Is(err, tc.want) {
				t.Fatalf("owner=%+v err=%v want=%v", owner, err, tc.want)
			}
			if tc.want == nil && (owner.IdentityID != "exact" || owner.EmployeeNo != "" || owner.PrimaryOrgID != "") {
				t.Fatalf("unexpected canonical owner: %+v", owner)
			}
		})
	}
}

// TestOwnerDirectoryChecksLaterPages 验证后页精确姓名、重复身份及真正重名。
// 参数：t为测试上下文，第一页始终满20条以触发实际分页。
// 返回值：无；未读取后页或按第一页错误选人时失败。
func TestOwnerDirectoryChecksLaterPages(t *testing.T) {
	for _, tc := range []struct {
		name       string
		firstExact bool
		lastID     string
		want       error
	}{
		{"exact-on-later-page", false, "exact", nil},
		{"same-identity-on-later-page", true, "exact", nil},
		{"same-name-other-identity", true, "other", ErrOwnerAmbiguous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := make([]map[string]any, 20)
			for i := range first {
				first[i] = ownerWireUser(fmt.Sprint("prefix-", i), "张三丰")
			}
			if tc.firstExact {
				first[0] = ownerWireUser("exact", "张三")
			}
			calls := 0
			client := pagedOwnerClient(t, func(page int) (int, string) {
				calls++
				items := first
				if page == 2 {
					items = []map[string]any{ownerWireUser(tc.lastID, "张三")}
				}
				return http.StatusOK, ownerWirePage(t, items, page, 21)
			})
			owner, err := client.ResolveOwnerByName(context.Background(), "张三")
			if !errors.Is(err, tc.want) || calls != 2 || (tc.want == nil && owner.IdentityID != "exact") {
				t.Fatalf("owner=%+v err=%v calls=%d", owner, err, calls)
			}
		})
	}
}

// TestOwnerDirectoryRejectsIncompletePages 验证目录故障和不完整分页不能产生唯一结论。
// 参数：t为测试上下文，覆盖后页故障、空页、缺字段和分页边界。
// 返回值：无；任何不完整目录被当作可信候选时失败。
func TestOwnerDirectoryRejectsIncompletePages(t *testing.T) {
	first := make([]map[string]any, 20)
	for i := range first {
		first[i] = ownerWireUser(fmt.Sprint("identity-", i), "张三")
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"later-failure", 503, `{}`},
		{"later-empty", 200, ownerWirePage(t, []map[string]any{}, 2, 21)},
		{"changed-total", 200, ownerWirePage(t, []map[string]any{ownerWireUser("extra", "张三")}, 2, 22)},
		{"wrong-page", 200, ownerWirePage(t, []map[string]any{ownerWireUser("extra", "张三")}, 1, 21)},
		{"missing-pagination", 200, `{"data":{"items":[],"total":0}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := pagedOwnerClient(t, func(page int) (int, string) {
				if page == 1 {
					return 200, ownerWirePage(t, first, page, 21)
				}
				return tc.status, tc.body
			})
			if _, err := client.ResolveOwnerByName(context.Background(), "张三"); !errors.Is(err, ErrOwnerDirectoryUnavailable) {
				t.Fatalf("incomplete directory accepted: %v", err)
			}
		})
	}
	for _, body := range []string{
		`{"data":{"items":[],"page":1,"page_size":20}}`,
		`{"data":{"items":{},"total":0,"page":1,"page_size":20}}`,
		`{"data":{"items":[],"total":0,"page":1,"page_size":50}}`,
		`{"data":{"total":0,"page":1,"page_size":20}}`,
		ownerWirePage(t, first, 1, 1001),
		strings.Repeat("x", (1<<20)+1),
	} {
		client := pagedOwnerClient(t, func(int) (int, string) { return 200, body })
		if _, err := client.Search(context.Background(), "张三"); !errors.Is(err, ErrOwnerDirectoryUnavailable) {
			t.Fatalf("invalid response accepted: %v", err)
		}
	}
}

// TestOwnerDirectoryKeywordRuneBoundary 验证关键字遵守平台100个Unicode字符边界。
// 参数：t为测试上下文，使用多字节姓名边界并核对超长请求不外发。
// 返回值：无；按字节误拒合法输入或接受超长关键字时失败。
func TestOwnerDirectoryKeywordRuneBoundary(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("keyword") != strings.Repeat("张", 100) {
			t.Error("unexpected keyword")
		}
		_, _ = io.WriteString(w, `{"data":{"items":[],"page":1,"page_size":20,"total":0}}`)
	}))
	t.Cleanup(server.Close)
	client := &OwnerDirectoryClient{BaseURL: server.URL,
		TokenSource: func(context.Context) (string, error) { return "token", nil }}
	if _, err := client.Search(context.Background(), strings.Repeat("张", 100)); err != nil {
		t.Fatalf("valid keyword rejected: %v", err)
	}
	if _, err := client.Search(context.Background(), strings.Repeat("张", 101)); err == nil || calls != 1 {
		t.Fatalf("oversized keyword accepted: err=%v calls=%d", err, calls)
	}
}

// TestOwnerDirectoryPreservesTransportGuards 验证自定义客户端也拒绝重定向并遵守调用超时。
// 参数：t为测试上下文，服务器仅提供可观察的重定向或延迟响应。
// 返回值：无；令牌请求跟随重定向或忽略上层截止时间时失败。
func TestOwnerDirectoryPreservesTransportGuards(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Store(true)
		_, _ = io.WriteString(w, `{"data":{"items":[],"page":1,"page_size":20,"total":0}}`)
	}))
	t.Cleanup(target.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)
	redirect := &OwnerDirectoryClient{BaseURL: server.URL, Client: server.Client(),
		TokenSource: func(context.Context) (string, error) { return "token", nil }}
	if _, err := redirect.Search(context.Background(), "张三"); !errors.Is(err, ErrOwnerDirectoryUnavailable) || redirected.Load() {
		t.Fatalf("redirect accepted: err=%v followed=%v", err, redirected.Load())
	}
	client := pagedOwnerClient(t, func(int) (int, string) {
		time.Sleep(25 * time.Millisecond)
		return 200, `{"data":{"items":[],"page":1,"page_size":20,"total":0}}`
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := client.Search(ctx, "张三"); !errors.Is(err, ErrOwnerDirectoryUnavailable) {
		t.Fatalf("deadline ignored: %v", err)
	}
}
