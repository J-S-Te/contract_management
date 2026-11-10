package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
)

// TestImportOwnerNameResolution 验证当前负责人使用真实user_id，精确查重且不猜选。
// 参数：t为平台机器目录契约测试上下文。
// 返回值：无；零命中、重名、依赖失败或身份类型不符时测试失败。
func TestImportOwnerNameResolution(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []map[string]string
		code int
		want error
	}{
		{"唯一姓名", []map[string]string{{"user_id": "user-1", "display_name": "当前负责人甲"}}, 200, nil},
		{"模糊结果不能当命中", []map[string]string{{"user_id": "user-2", "display_name": "当前负责人甲乙"}}, 200, application.ErrProvisionOwnerNotFound},
		{"同名", []map[string]string{{"user_id": "user-1", "display_name": "当前负责人甲"}, {"user_id": "user-2", "display_name": "当前负责人甲"}}, 200, application.ErrProvisionOwnerAmbiguous},
		{"目录故障", nil, 503, application.ErrProvisionOwnerDirectoryUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth2/token" {
					_ = r.ParseForm()
					if r.Form.Get("scope") != "owner_directory.read" {
						t.Error("错误的机器scope")
					}
					_, _ = w.Write([]byte(`{"access_token":"machine-token","token_type":"Bearer","scope":"owner_directory.read"}`))
					return
				}
				if r.URL.Query().Get("keyword") != "当前负责人甲" || r.URL.Query().Get("role_code") != "" {
					t.Error("当前负责人不应限定销售角色或改变姓名")
				}
				w.WriteHeader(tc.code)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"items": tc.rows, "total": len(tc.rows)}})
			}))
			defer server.Close()
			directory := NewPersonnelDirectory(server.URL, "client", "secret", time.Second)
			resolver, ok := directory.(interface {
				ResolveOwnerByName(context.Context, string, string) (application.UserReference, error)
			})
			if !ok {
				t.Fatal("历史当前负责人尚未支持按姓名解析")
			}
			owner, err := resolver.ResolveOwnerByName(context.Background(), "tenant", "当前负责人甲")
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want=%v", err, tc.want)
			}
			if tc.want == nil && owner.UserID != "user-1" {
				t.Fatalf("必须返回平台user_id: %+v", owner)
			}
		})
	}
}
