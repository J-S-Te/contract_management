package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// TestDecodeImportManifestWhitelistsCustomerProfileColumns 验证清单严格解码白名单
// 已包含客户类型/行业/地区三列：它们在 ImportMetadata 上声明后即可被 PUT manifest 接受，
// 而拼写错误或未登记的字段仍按 DisallowUnknownFields 拒绝。
// 参数：t为轻量gin+httptest测试上下文，不依赖数据库。
// 返回值：三列被拒绝、解码后字段不符或未知字段被放行时失败。
func TestDecodeImportManifestWhitelistsCustomerProfileColumns(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manifest := func(profile string) string {
		return `{"expected_version":1,"manifest":{"schema_version":1,"source_system":"legacy","items":[` +
			`{"item_key":"one","external_record_id":"old-1","mode":"ARCHIVE_ONLY","source_file_ref":"source-one",` +
			`"metadata":{"title":"历史合同","contract_type":"直签","customer_name":"历史客户","amount_minor":100,` +
			`"currency":"CNY","service_items":[],` + profile + `}}]}}`
	}
	decode := func(t *testing.T, body string) (contract.ImportManifest, bool) {
		t.Helper()
		response := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(response)
		context.Request = httptest.NewRequest(http.MethodPut, "/api/v1/contract-import-batches/batch-1/manifest", strings.NewReader(body))
		var target struct {
			ExpectedVersion uint64                  `json:"expected_version"`
			Manifest        contract.ImportManifest `json:"manifest"`
		}
		return target.Manifest, decodeImportJSON(context, &target)
	}

	decoded, ok := decode(t, manifest(`"customer_type":"企业","industry":"制造业","region":"华东"`))
	if !ok {
		t.Fatal("manifest declaring customer_type/industry/region was rejected by strict decoding")
	}
	metadata := decoded.Items[0].Metadata
	if metadata.CustomerType != "企业" || metadata.Industry != "制造业" || metadata.Region != "华东" {
		t.Fatalf("metadata=%+v", metadata)
	}
	// 三列只用于开立编排：显式白名单转换不得把它们写进合同。
	if c := metadata.Contract(); c.CustomerName != "历史客户" || c.Title != "历史合同" {
		t.Fatalf("contract=%+v", c)
	}

	if _, ok = decode(t, manifest(`"customer_types":"企业"`)); ok {
		t.Fatal("undocumented metadata field must stay rejected by strict decoding")
	}
}
