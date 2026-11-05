package platform

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ownerDirectoryTestServer 启动假平台：令牌端点固定返回 owner_directory.read，
// 目录端点按 items 返回响应并校验查询参数。
// 参数：t 为测试上下文，status 为目录端点状态码，body 为目录端点响应体。
// 返回值：平台 origin（可直接作为 BaseURL）。
func ownerDirectoryTestServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/oauth2/token" {
			if clientID, secret, ok := request.BasicAuth(); !ok || clientID != "contract-directory" || secret != "secret" ||
				request.FormValue("grant_type") != "client_credentials" || request.FormValue("scope") != OwnerDirectoryScope {
				t.Errorf("token request auth=%q form=%v", request.Header.Get("Authorization"), request.Form)
			}
			_, _ = io.WriteString(response, `{"access_token":"directory-token","token_type":"Bearer","scope":"owner_directory.read"}`)
			return
		}
		if request.URL.Path != "/api/v1/internal/owner-directory" {
			t.Errorf("unexpected path=%s", request.URL.Path)
			response.WriteHeader(http.StatusNotFound)
			return
		}
		if request.Method != http.MethodGet || request.URL.Query().Get("keyword") != "张三" ||
			request.URL.Query().Get("page") != "1" || request.URL.Query().Get("page_size") != "20" {
			t.Errorf("directory request method=%s query=%v", request.Method, request.URL.Query())
		}
		if request.Header.Get("Authorization") != "Bearer directory-token" || request.Header.Get("Accept") != "application/json" {
			t.Errorf("directory headers=%v", request.Header)
		}
		response.WriteHeader(status)
		_, _ = io.WriteString(response, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// ownerDirectoryTestClient 组装指向假平台的目录客户端。
// 参数：server 为假平台。
// 返回值：可调用的客户端。
func ownerDirectoryTestClient(server *httptest.Server) *OwnerDirectoryClient {
	return NewOwnerDirectoryClient(server.URL, server.URL+"/oauth2/token", "contract-directory", "secret")
}

// TestResolveOwnerByNameDistinguishesZeroAndMultipleMatches 验证 0 命中与多命中可区分。
// 参数：t 为测试上下文。
// 返回值：无；分类错误或候选明细丢失时失败。
func TestResolveOwnerByNameDistinguishesZeroAndMultipleMatches(t *testing.T) {
	empty := ownerDirectoryTestServer(t, http.StatusOK, `{"data":{"items":[],"total":0,"page":1,"page_size":20}}`)
	if _, err := ownerDirectoryTestClient(empty).ResolveOwnerByName(context.Background(), "张三"); !errors.Is(err, ErrOwnerNotFound) {
		t.Fatalf("zero match error=%v", err)
	}

	single := ownerDirectoryTestServer(t, http.StatusOK, `{"data":{"items":[{"identity_id":"identity-1","display_name":"张三","employee_no":"E001","primary_org_id":"org-9"}],"total":1}}`)
	candidate, err := ownerDirectoryTestClient(single).ResolveOwnerByName(context.Background(), "张三")
	if err != nil || candidate.IdentityID != "identity-1" || candidate.DisplayName != "张三" ||
		candidate.EmployeeNo != "E001" || candidate.PrimaryOrgID != "org-9" {
		t.Fatalf("candidate=%+v err=%v", candidate, err)
	}

	multiple := ownerDirectoryTestServer(t, http.StatusOK, `{"data":{"items":[{"identity_id":"identity-1","display_name":"张三"},{"identity_id":"identity-2","display_name":"张三"}],"total":2}}`)
	_, err = ownerDirectoryTestClient(multiple).ResolveOwnerByName(context.Background(), "张三")
	if !errors.Is(err, ErrOwnerAmbiguous) {
		t.Fatalf("multiple match error=%v", err)
	}
	var ambiguous *OwnerAmbiguousError
	if !errors.As(err, &ambiguous) || len(ambiguous.Candidates) != 2 {
		t.Fatalf("ambiguous=%+v", ambiguous)
	}
}

// TestSearchOwnerDirectoryDeduplicatesAndFailsClosed 验证同身份重复返回不会误判多命中，
// 而协议异常、依赖故障与非法关键字失败关闭。
// 参数：t 为测试上下文。
// 返回值：无；去重或失败分类不符时失败。
func TestSearchOwnerDirectoryDeduplicatesAndFailsClosed(t *testing.T) {
	duplicated := ownerDirectoryTestServer(t, http.StatusOK, `{"data":{"items":[{"identity_id":"identity-1","display_name":"张三"},{"identity_id":"identity-1","display_name":"张三"}],"total":2}}`)
	candidates, err := ownerDirectoryTestClient(duplicated).Search(context.Background(), "张三")
	if err != nil || len(candidates) != 1 || candidates[0].IdentityID != "identity-1" {
		t.Fatalf("dedupe candidates=%+v err=%v", candidates, err)
	}

	unavailable := ownerDirectoryTestServer(t, http.StatusServiceUnavailable, `{}`)
	if _, err := ownerDirectoryTestClient(unavailable).Search(context.Background(), "张三"); !errors.Is(err, ErrOwnerDirectoryUnavailable) {
		t.Fatalf("5xx error=%v", err)
	}

	incomplete := ownerDirectoryTestServer(t, http.StatusOK, `{"data":{"items":[{"identity_id":"","display_name":"张三"}],"total":1}}`)
	if _, err := ownerDirectoryTestClient(incomplete).Search(context.Background(), "张三"); !errors.Is(err, ErrOwnerDirectoryUnavailable) {
		t.Fatalf("incomplete candidate error=%v", err)
	}

	// 空关键字在发请求前失败关闭。
	if _, err := ownerDirectoryTestClient(duplicated).Search(context.Background(), " "); err == nil || errors.Is(err, ErrOwnerDirectoryUnavailable) {
		t.Fatalf("empty keyword error=%v", err)
	}
	// 未配置的客户端不得发出请求。
	if _, err := (&OwnerDirectoryClient{}).Search(context.Background(), "张三"); err == nil {
		t.Fatal("unconfigured client accepted a call")
	}
}

// TestOwnerDirectoryTokenRequiresDedicatedScope 验证令牌缺少 owner_directory.read 时失败关闭。
// 参数：t 为测试上下文。
// 返回值：无；缺 scope 的令牌被接受时失败。
func TestOwnerDirectoryTokenRequiresDedicatedScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/oauth2/token" {
			_, _ = io.WriteString(response, `{"access_token":"token","token_type":"Bearer","scope":"customer.contract_reference.read"}`)
			return
		}
		t.Errorf("directory endpoint must not be called without the dedicated scope")
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := NewOwnerDirectoryClient(server.URL, server.URL+"/oauth2/token", "id", "secret")
	if _, err := client.Search(context.Background(), "张三"); !errors.Is(err, ErrOwnerDirectoryUnavailable) {
		t.Fatalf("missing scope error=%v", err)
	}
	if NewOwnerDirectoryClient("", server.URL+"/oauth2/token", "id", "secret") != nil {
		t.Fatal("client built without base url")
	}
	if NewOwnerDirectoryClient(server.URL, "", "id", "secret") != nil {
		t.Fatal("client built without token url")
	}
}
