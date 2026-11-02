package config

import (
	"strings"
	"testing"
)

// TestImportBaseURLRejectsPath 验证三个 *_BASE_URL 必须是不带路径的服务 origin
// （BIZ-XAPI-13）：调用方按 origin + 绝对契约路径拼请求，带路径的 origin 会把
// 契约路径挪到前缀之后，使实际调用端点与配置语义不一致。
// 错误只含配置项名称，不回显配置值。
func TestImportBaseURLRejectsPath(t *testing.T) {
	for _, tt := range []struct{ prefix, key, baseURL string }{
		{"CONTRACT_IMPORT_PROJECT_", "CONTRACT_IMPORT_PROJECT_BASE_URL", "http://project-management:8082/project_management"},
		{"CONTRACT_IMPORT_FINANCE_", "CONTRACT_IMPORT_FINANCE_BASE_URL", "http://settlement:8083/api/"},
		{"CONTRACT_IMPORT_REPORT_", "CONTRACT_IMPORT_REPORT_BASE_URL", "http://data-analysis:8080/internal/v1/contract-imports/report-status"},
	} {
		t.Run(tt.key, func(t *testing.T) {
			validEnvironment(t)
			t.Setenv(tt.prefix+"BASE_URL", tt.baseURL)
			t.Setenv(tt.prefix+"TOKEN_URL", "http://platform:8080/oauth2/token")
			t.Setenv(tt.prefix+"CLIENT_ID", "dedicated-import-client")
			t.Setenv(tt.prefix+"CLIENT_SECRET", "test-client-secret")
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tt.key) {
				t.Fatalf("Load() error = %v, want %s origin error", err, tt.key)
			}
			// 错误只含键名：不得回显配置值（避免把内网地址/端口写进日志）。
			if strings.Contains(err.Error(), tt.baseURL) || strings.Contains(err.Error(), "data-analysis:8080") {
				t.Fatalf("错误泄露配置值: %v", err)
			}
		})
	}
}

// TestImportBaseURLAcceptsServiceOrigin 验证 origin 形式（含末尾 /）仍被接受，
// 且 TOKEN_URL 保留自身路径不被该约束影响。
func TestImportBaseURLAcceptsServiceOrigin(t *testing.T) {
	for _, tt := range []struct {
		prefix, baseURL string
	}{
		{"CONTRACT_IMPORT_PROJECT_", "http://project-management:8082"},
		{"CONTRACT_IMPORT_FINANCE_", "http://settlement:8083/"},
		{"CONTRACT_IMPORT_REPORT_", "http://data-analysis:8080"},
	} {
		t.Run(tt.prefix, func(t *testing.T) {
			validEnvironment(t)
			t.Setenv(tt.prefix+"BASE_URL", tt.baseURL)
			t.Setenv(tt.prefix+"TOKEN_URL", "http://platform:8080/oauth2/token")
			t.Setenv(tt.prefix+"CLIENT_ID", "dedicated-import-client")
			t.Setenv(tt.prefix+"CLIENT_SECRET", "test-client-secret")
			if _, err := Load(); err != nil {
				t.Fatalf("Load() error = %v, want origin accepted", err)
			}
		})
	}
}
