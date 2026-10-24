package httpapi

import "testing"

// AUD-IP-2026：合同正文的下载路由不含 "download" 字样，必须按路径后缀识别为敏感读，
// 否则导出动作完全绕过审计（强制审计模式也保护不到）。
func TestSensitiveReadCoversDocumentDownloads(t *testing.T) {
	for _, path := range []string{
		"/approved-contracts/01J00000000000000000000001/docx",
		"/approved-contracts/01J00000000000000000000001/pdf",
		"/approved-contracts/01J00000000000000000000001/stamped-pdf",
		"/api/v1/contracts/export",
		"/embed/token",
		"/downloads/1",
	} {
		if !sensitiveRead(path) {
			t.Errorf("sensitiveRead(%q) = false，下载/导出必须审计", path)
		}
	}
	for _, path := range []string{"/contracts", "/contracts/01J00000000000000000000001", "/healthz"} {
		if sensitiveRead(path) {
			t.Errorf("sensitiveRead(%q) = true，普通读不应强制审计", path)
		}
	}
}

// 普通读跳过审计，但敏感读与写请求都必须记录。
func TestSkipAuditKeepsDocumentDownloads(t *testing.T) {
	if skipAudit("GET", "/approved-contracts/01J00000000000000000000001/pdf") {
		t.Fatal("下载路由不得被 skipAudit 跳过")
	}
	if !skipAudit("GET", "/contracts") {
		t.Fatal("普通读应跳过审计")
	}
}
