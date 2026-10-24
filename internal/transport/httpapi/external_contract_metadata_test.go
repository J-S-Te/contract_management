package httpapi

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// frontendExternalContractPayload 还原前端 buildNewContractPayload() 发送的载荷形状
// （模板与外部合同共用同一构造函数）。该载荷经 DisallowUnknownFields 解码，任何字段
// 缺失或命名不符都会以 json: unknown field 失败，线上表现为 422「外部合同元数据不合法」。
func frontendExternalContractPayload(t *testing.T) []byte {
	t.Helper()
	payload := map[string]any{
		"opportunity_id":   "01OPP",
		"opportunity_name": "某商机",
		"crm_customer_id":  42,
		"title":            "软件测试服务项目合同",
		"contract_type":    "直签",
		"service_type":     "软件测试",
		"amount_minor":     10000050,
		"currency":         "CNY",
		"payment_terms": []map[string]any{
			{"installment_no": 1, "due_date": "2026-11-01", "amount": "100000.50"},
		},
		"customer_name":    "某客户",
		"customer_address": "某地址",
		"customer_contact": "某联系人",
		"customer_phone":   "13800000000",
		"start_date":       "2026-10-01T00:00:00Z",
		"end_date":         "2026-12-31T00:00:00Z",
		"service_items": []map[string]any{
			{
				"service_type": "软件测试", "name": "软件测试", "site": "北京", "batch": "第一批次",
				"category": "软件测试", "requirement": "体系/能力要求", "test_mode": "常规检测",
				"systems": []map[string]any{{"name": "系统A", "level": "三级"}},
			},
		},
		"content": "",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("构造前端载荷失败：%v", err)
	}
	return raw
}

func TestExternalContractMetadataDecodesFrontendPayload(t *testing.T) {
	decoder := json.NewDecoder(bytes.NewReader(frontendExternalContractPayload(t)))
	decoder.DisallowUnknownFields()
	var metadata externalContractMetadata
	if err := decoder.Decode(&metadata); err != nil {
		t.Fatalf("外部合同元数据必须能解码前端载荷，否则上传会以 422 CON_VALIDATION_ERROR 失败：%v", err)
	}
	if metadata.Title != "软件测试服务项目合同" || metadata.CRMCustomerID != 42 {
		t.Fatalf("载荷字段未正确落位：%+v", metadata)
	}
	if len(metadata.ServiceItems) != 1 || len(metadata.ServiceItems[0].Systems) != 1 {
		t.Fatalf("服务项未正确解码：%+v", metadata.ServiceItems)
	}
	if len(metadata.PaymentTerms) != 1 || metadata.PaymentTerms[0].Amount != "100000.50" {
		t.Fatalf("付款计划未正确解码：%+v", metadata.PaymentTerms)
	}
}

// 跨仓契约检查：直接读前端源码里的载荷构造函数，确保它发出的每个一级字段都能被
// externalContractMetadata 接受。前端不在当前检出时跳过（各仓独立 CI）。
func TestExternalContractMetadataCoversFrontendPayloadBuilder(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "frontend", "src", "modules",
		"contract_management", "views", "ContractManagementView.vue")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("未找到前端源码，跳过跨仓契约检查：%v", err)
	}
	keys := payloadBuilderKeys(t, string(raw))
	if len(keys) == 0 {
		t.Fatal("未能从前端 buildNewContractPayload() 解析出任何字段，契约检查失效")
	}
	accepted := jsonFieldNames(reflect.TypeOf(externalContractMetadata{}))
	for _, key := range keys {
		if !accepted[key] {
			t.Fatalf("前端发送字段 %q，但 externalContractMetadata 不接受，上传将以 422「外部合同元数据不合法」失败", key)
		}
	}
}

func jsonFieldNames(structType reflect.Type) map[string]bool {
	names := make(map[string]bool, structType.NumField())
	for index := 0; index < structType.NumField(); index++ {
		name := strings.Split(structType.Field(index).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			name = structType.Field(index).Name
		}
		names[name] = true
	}
	return names
}

// payloadBuilderKeys 取 buildNewContractPayload() 返回对象的一级字段名。
// 用花括号深度计数区分一级字段与 service_items 内的嵌套字段；函数体首行即为左花括号，
// 因此深度为 1 的行才是载荷属性行。
func payloadBuilderKeys(t *testing.T, source string) []string {
	t.Helper()
	anchor := strings.Index(source, "function buildNewContractPayload(")
	if anchor < 0 {
		t.Fatal("未在前端源码中定位到 buildNewContractPayload(")
	}
	body := source[anchor:]
	// 锚定 return { 而不是函数体左花括号：函数体是外层，载荷对象是内层，
	// 否则属性行会落在深度 2 上而被漏掉。
	returnAt := strings.Index(body, "return {")
	if returnAt < 0 {
		t.Fatal("buildNewContractPayload() 未找到 return 对象字面量")
	}
	open := returnAt + strings.Index(body[returnAt:], "{")
	if open < returnAt {
		t.Fatal("buildNewContractPayload() 缺少返回对象")
	}
	keys := []string{}
	depth := 0
	for _, line := range strings.Split(body[open:], "\n") {
		trimmed := strings.TrimSpace(line)
		if depth == 1 {
			if colon := strings.Index(trimmed, ":"); colon > 0 {
				if candidate := strings.TrimSpace(trimmed[:colon]); isPayloadKey(candidate) {
					keys = append(keys, candidate)
				}
			}
		}
		depth += strings.Count(trimmed, "{") - strings.Count(trimmed, "}")
		if depth <= 0 {
			break
		}
	}
	return keys
}

func isPayloadKey(value string) bool {
	if value == "" {
		return false
	}
	for index, char := range value {
		if char >= 'a' && char <= 'z' || char == '_' {
			continue
		}
		if index > 0 && char >= '0' && char <= '9' {
			continue
		}
		return false
	}
	return true
}
