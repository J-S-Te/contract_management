package crm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/j-s-te/contract-management/internal/application"
)

type provisionWireCase struct {
	Path     string            `json:"path"`
	Body     json.RawMessage   `json:"body"`
	Headers  map[string]string `json:"headers"`
	Status   int               `json:"status"`
	Response json.RawMessage   `json:"response"`
}

// TestCRMImportProvisionCrossRepository 把真实客户端字节交给CRM真实路由再消费其响应。
// 参数：t为测试上下文；环境变量指定交换文件与导出或校验阶段。
// 返回值：请求被CRM拒绝或真实响应无法消费时失败。
func TestCRMImportProvisionCrossRepository(t *testing.T) {
	path := os.Getenv("CRM_PROVISION_CONTRACT_FILE")
	if path == "" {
		t.Skip("需要跨仓契约驱动脚本")
	}
	var cases []provisionWireCase
	verify := os.Getenv("CRM_PROVISION_CONTRACT_PHASE") == "verify"
	if verify {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, &cases); err != nil {
			t.Fatal(err)
		}
	}
	index := 0
	c := provisionTestClient("https://crm.contract.test", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if verify {
			current := cases[index]
			index++
			if current.Path != r.URL.Path {
				t.Fatalf("路径=%s want=%s", r.URL.Path, current.Path)
			}
			return &http.Response{StatusCode: current.Status, Body: io.NopCloser(bytes.NewReader(current.Response)), Header: make(http.Header)}, nil
		}
		cases = append(cases, provisionWireCase{Path: r.URL.Path, Body: body, Headers: map[string]string{"X-Actor-Identity-ID": r.Header.Get("X-Actor-Identity-ID"), "Idempotency-Key": r.Header.Get("Idempotency-Key")}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader([]byte(`{"code":"OK","data":{"id":7,"version":1,"opp_status":"CLOSED","current_stage":"已签约","origin":"IMPORT_PROVISION"}}`))), Header: make(http.Header)}, nil
	}))
	ctx := context.Background()
	customer, err := c.CreateCustomer(ctx, "actor-9", "cross-contract-key", ProvisionCustomerInput{Name: "历史客户甲", OwnerIdentityID: "owner-x", Contacts: []application.CRMProvisionCustomerContact{{Name: "张三", Phone: "13800138000", IsRegistration: true}}})
	if verify && (err != nil || customer.ID != 7 || customer.Status != "ACTIVE") {
		t.Fatalf("CRM客户真实响应: %+v err=%v", customer, err)
	}
	opportunity, err := c.CreateOpportunity(ctx, "actor-9", "cross-contract-key", ProvisionOpportunityInput{CustomerID: 7, OwnerIdentityID: "owner-x", Name: "历史商机", Amount: "100.00", ExpectedSignDate: "2020-01-01", RequirementSummary: "历史合同软件测试"})
	if verify && (err != nil || opportunity.ID != 7 || opportunity.CustomerID != 7 || opportunity.Version != 3) {
		t.Fatalf("CRM商机真实响应: %+v err=%v", opportunity, err)
	}
	signed, err := c.SignOpportunity(ctx, "actor-9", "cross-contract-key", 7, "01K00000000000000000000000", 3)
	if verify && (err != nil || signed.ID != 7 || signed.Version != 4 || signed.Status != "CLOSED" || signed.Stage != "已签约") {
		t.Fatalf("CRM签约真实响应: %+v err=%v", signed, err)
	}
	if verify && index != 3 {
		t.Fatalf("跨仓响应数量=%d", index)
	}
	if !verify {
		if len(cases) != 3 {
			t.Fatalf("跨仓请求数量=%d", len(cases))
		}
		raw, _ := json.MarshalIndent(cases, "", "  ")
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
