package mysql

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestImportReportSourceCustomerIDJSONTypeMySQL 验证报告源保持冻结JSON number/null契约。
// 参数：t为显式隔离MySQL上下文，只使用本测试合成历史合同。
// 返回值：无；客户ID字符串化或NULL被伪造为数值时失败。
func TestImportReportSourceCustomerIDJSONTypeMySQL(t *testing.T) {
	for _, empty := range []bool{false, true} {
		name := "number"
		if empty {
			name = "null"
		}
		t.Run(name, func(t *testing.T) {
			r, tenant := draftE2ERepository(t)
			ctx := context.Background()
			b, _ := historyMySQLFixture(t, r, tenant, contract.ImportArchiveOnly)
			b = approveHistoryFixture(t, r, b)
			actor := application.Principal{TenantID: tenant, UserID: b.ActorID, Permissions: map[string]bool{"contract.import.release": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.import.release": {AllowAll: true}}}
			svc := &application.Service{Repo: r}
			if _, e := svc.ReleaseImportItem(ctx, actor, b.Items[0].ID, "REPORT", 1, ""); e != nil {
				t.Fatal(e)
			}
			// 合成可空历史行只验证输出契约，不放宽导入时CRM必填/ACTIVE校验。
			if empty {
				if e := r.db.Model(&contractRecord{}).Where("tenant_id=? AND id=?", tenant, b.Items[0].ContractID).Update("crm_customer_id", nil).Error; e != nil {
					t.Fatal(e)
				}
			}
			source, e := r.ImportReportSource(ctx, tenant)
			if e != nil {
				t.Fatal(e)
			}
			body, e := json.Marshal(source)
			if e != nil {
				t.Fatal(e)
			}
			var wire struct {
				Items []struct {
					CustomerID    json.RawMessage `json:"crm_customer_id"`
					OpportunityID json.RawMessage `json:"opportunity_id"`
				} `json:"items"`
			}
			if e = json.Unmarshal(body, &wire); e != nil || len(wire.Items) != 1 {
				t.Fatal("invalid source envelope", e)
			}
			expected := "42"
			if empty {
				expected = "null"
			}
			if string(wire.Items[0].CustomerID) != expected {
				t.Fatalf("crm_customer_id must be JSON %s, got %s", expected, wire.Items[0].CustomerID)
			}
			// 与T10冻结字段类型同形，不能以宽松字符串转换掩盖协议漂移。
			var receiver struct {
				Items []struct {
					CustomerID    *uint64 `json:"crm_customer_id"`
					OpportunityID *string `json:"opportunity_id"`
				} `json:"items"`
			}
			if e = json.Unmarshal(body, &receiver); e != nil {
				t.Fatal("frozen T10 type cannot decode source", e)
			}
			if empty && receiver.Items[0].CustomerID != nil {
				t.Fatal("NULL customer invented")
			}
			if !empty && (receiver.Items[0].CustomerID == nil || *receiver.Items[0].CustomerID != 42) {
				t.Fatal("real numeric customer lost")
			}
			if string(wire.Items[0].OpportunityID) != "null" {
				t.Fatal("nullable string opportunity contract changed")
			}
		})
	}
}

// TestImportProjectServicesStrictDecoderMySQL 验证源实际JSON符合T08严格合同服务白名单。
// 参数：t为隔离MySQL上下文；接收HTTP仅是冻结DTO严格解码替身。
// 返回值：无；任何额外服务字段导致源项目任务被接收方拒绝时失败。
func TestImportProjectServicesStrictDecoderMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	b, _ := historyMySQLFixture(t, r, tenant, contract.ImportContinueLive)
	b = approveHistoryFixture(t, r, b)
	item := b.Items[0]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var outer struct {
			Contract json.RawMessage `json:"contract"`
		}
		if e := json.Unmarshal(body, &outer); e != nil {
			t.Error(e)
			w.WriteHeader(422)
			return
		}
		// 与T08 ContractActivation/ContractService字段同形，测试不依赖相邻Go模块。
		var incoming struct {
			OriginKind              string    `json:"origin_kind,omitempty"`
			ImportMode              string    `json:"import_mode,omitempty"`
			OriginImportItemID      string    `json:"origin_import_item_id,omitempty"`
			ContractID              string    `json:"contract_id"`
			ContractNumber          string    `json:"contract_number,omitempty"`
			ContractVersion         string    `json:"contract_version"`
			ContractName            string    `json:"contract_name"`
			Customer                string    `json:"customer"`
			CustomerID              string    `json:"customer_id,omitempty"`
			EffectiveAt             time.Time `json:"effective_at"`
			StampedContractUploaded bool      `json:"stamped_contract_uploaded"`
			Services                []struct {
				SourceID    string `json:"source_id"`
				Name        string `json:"name"`
				Site        string `json:"site"`
				SiteCode    string `json:"site_code,omitempty"`
				Batch       string `json:"batch"`
				Category    string `json:"category"`
				System      string `json:"system"`
				SystemLevel string `json:"system_level"`
				Requirement string `json:"requirement"`
				TestMode    string `json:"test_mode"`
			} `json:"services"`
		}
		decoder := json.NewDecoder(bytes.NewReader(outer.Contract))
		decoder.DisallowUnknownFields()
		if e := decoder.Decode(&incoming); e != nil {
			t.Error("T08 strict decode rejects actual source JSON:", e)
			w.WriteHeader(422)
			return
		}
		if e := decoder.Decode(&struct{}{}); e != io.EOF {
			t.Error("invalid trailing contract JSON")
			w.WriteHeader(422)
			return
		}
		if len(incoming.Services) != 1 || incoming.Services[0].Category != "软件测试" || incoming.Services[0].SourceID == "" {
			t.Error("service mapping lost")
			w.WriteHeader(422)
			return
		}
		var command map[string]any
		_ = json.Unmarshal(body, &command)
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"schema_version": 1, "command_id": command["command_id"], "tenant_id": tenant, "origin_import_item_id": item.ID, "source_revision": item.SourceRevision, "policy_digest": command["policy_digest"], "release_version": 1, "contract_id": item.ContractID, "project_id": "strict-test-project", "project_version": 1, "status": "PROJECT_BOUND"}})
	}))
	defer server.Close()
	svc := &application.Service{Repo: r, ImportProject: &application.ImportTargetTransport{BaseURL: server.URL, TokenSource: func(context.Context) (string, error) { return "isolated-test", nil }}}
	actor := application.Principal{TenantID: tenant, UserID: b.ActorID, Permissions: map[string]bool{"contract.import.release": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.import.release": {AllowAll: true}}}
	if _, e := svc.ReleaseImportItem(ctx, actor, item.ID, "PROJECT", 1, ""); e != nil {
		t.Fatal(e)
	}
	if e := svc.DispatchImportTargetOnce(ctx); e != nil {
		t.Fatal(e)
	}
	b, _ = r.GetImportBatch(ctx, tenant, b.ID)
	if b.Items[0].Targets[0].State != "COMPLETE" {
		t.Fatalf("strict protocol target failed: %#v", b.Items[0].Targets[0])
	}
}
