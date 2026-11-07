package config

import (
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
)

type ImportTargetConfig struct{ BaseURL, TokenURL, ClientID, ClientSecret, Scope string }
type ImportReportReadConfig struct{ Issuer, Audience, PublicKeyPath, ClientID, CallerEnvironmentCode string }

// loadImportHistoryConfig 校验显式确认角色与独立机器能力，不自动启用外部服务。
// 参数：c为当前进程配置。
// 返回值：缺失、未知角色、URL或scope错误，均只含配置项名称。
func loadImportHistoryConfig(c *Config) error {
	c.ContractImportConfirmationRoles = strings.FieldsFunc(os.Getenv("CONTRACT_IMPORT_CONFIRMATION_ROLES"), func(r rune) bool { return r == ',' || r == ' ' })
	known := []string{"admin", "sales_director", "tech_director", "finance_director"}
	seen := map[string]bool{}
	for _, role := range c.ContractImportConfirmationRoles {
		if !slices.Contains(known, role) || seen[role] {
			return fmt.Errorf("CONTRACT_IMPORT_CONFIRMATION_ROLES contains unknown role")
		}
		seen[role] = true
	}
	if c.ContractImportHistoryEnabled && len(c.ContractImportConfirmationRoles) == 0 {
		return fmt.Errorf("CONTRACT_IMPORT_CONFIRMATION_ROLES requires explicit business approval template")
	}
	// 历史导入自动审批默认关闭，且只是部署层能力开关：开启后仍须由合同系统超级
	// 管理员（角色 admin）按租户开启运行开关（DB 持久化、默认关闭），两者同时开启
	// 才自动确认并按清单声明状态归档。这是"跳过人工会签"的例外，必须显式配置，
	// 不能随历史/开立开关一起被打开；依赖历史模式已验证、开立通道已装配。
	autoApprove, autoApproveErr := strconv.ParseBool(env("CONTRACT_IMPORT_AUTO_APPROVE_HISTORICAL", "false"))
	if autoApproveErr != nil {
		return fmt.Errorf("CONTRACT_IMPORT_AUTO_APPROVE_HISTORICAL must be a boolean")
	}
	c.ContractImportAutoApproveHistorical = autoApprove
	if autoApprove {
		if !c.ContractImportHistoryEnabled {
			return fmt.Errorf("CONTRACT_IMPORT_AUTO_APPROVE_HISTORICAL requires CONTRACT_IMPORT_HISTORY_ENABLED=true")
		}
		if !c.ContractImportCRMProvisionEnabled {
			return fmt.Errorf("CONTRACT_IMPORT_AUTO_APPROVE_HISTORICAL requires CONTRACT_IMPORT_CRM_PROVISION_ENABLED=true")
		}
	}
	targets := []struct {
		prefix, scope string
		out           *ImportTargetConfig
	}{{"CONTRACT_IMPORT_PROJECT_", "project.contract.import", &c.ImportProject}, {"CONTRACT_IMPORT_FINANCE_", "settlement.contract.ingest", &c.ImportFinance}, {"CONTRACT_IMPORT_REPORT_", "analysis.contract-import.report-status", &c.ImportReport}}
	for _, target := range targets {
		p, out := target.prefix, target.out
		out.BaseURL = os.Getenv(p + "BASE_URL")
		out.TokenURL = os.Getenv(p + "TOKEN_URL")
		out.ClientID = os.Getenv(p + "CLIENT_ID")
		out.ClientSecret = os.Getenv(p + "CLIENT_SECRET")
		out.Scope = env(p+"SCOPE", target.scope)
		if out.BaseURL == "" && out.TokenURL == "" && out.ClientID == "" && out.ClientSecret == "" {
			continue
		}
		if !validImportURL(out.BaseURL) || !validImportURL(out.TokenURL) || out.ClientID == "" || out.ClientSecret == "" || out.Scope != target.scope {
			return fmt.Errorf("%s requires complete URL, token client and exact SCOPE", p)
		}
		// BASE_URL 是服务 origin：调用方按 origin + 绝对契约路径拼接请求。
		// 带路径的 BASE_URL 会静默改写目标端点（例如请求落到
		// /prefix/internal/v1/...），使“实际调用的接口”与配置看起来不同，
		// 因此启动期即拒绝。错误只含键名，不回显配置值。
		if !validImportBaseURL(out.BaseURL) {
			return fmt.Errorf("%sBASE_URL must be a service origin without path", p)
		}
	}
	p := "CONTRACT_IMPORT_REPORT_READ_"
	read := &c.ImportReportRead
	read.Issuer = os.Getenv(p + "ISSUER")
	read.Audience = os.Getenv(p + "AUDIENCE")
	read.PublicKeyPath = os.Getenv(p + "PUBLIC_KEY_PATH")
	read.ClientID = os.Getenv(p + "CLIENT_ID")
	read.CallerEnvironmentCode = os.Getenv(p + "CALLER_ENVIRONMENT_CODE")
	if read.ClientID != "" && (strings.TrimSpace(read.Issuer) == "" || read.Audience == "" || read.PublicKeyPath == "" || read.CallerEnvironmentCode == "") {
		return fmt.Errorf("CONTRACT_IMPORT_REPORT_READ requires complete verifier configuration")
	}
	if c.ImportReport.ClientID != "" && (c.ImportReport.ClientID == c.ImportProject.ClientID || c.ImportReport.ClientID == c.ImportFinance.ClientID || c.ImportReport.ClientID == c.DashboardMachineClientID) {
		return fmt.Errorf("CONTRACT_IMPORT_REPORT_CLIENT_ID requires dedicated identity")
	}
	return nil
}

// validImportURL 校验可配置HTTP目标，拒绝凭据、片段及查询注入。
// 参数：value为完整服务origin或token地址。
// 返回值：合法且无内嵌身份信息时为true。
func validImportURL(value string) bool {
	u, e := url.Parse(value)
	return e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

// validImportBaseURL 要求 *_BASE_URL 是不带路径的服务 origin。
// 参数：value为已通过 validImportURL 的 BASE_URL。
// 返回值：只有 scheme+host（允许末尾单个 /）时为true。
//
// TOKEN_URL 通常自带 /oauth2/token 之类的路径，因此该约束只适用于 BASE_URL：
// 调用方用 BASE_URL + 绝对契约路径拼请求，带路径的 origin 会把契约路径挪到
// 前缀之后，导致调用目标与审计/网关心智模型不一致。
func validImportBaseURL(value string) bool {
	u, e := url.Parse(value)
	return e == nil && (u.Path == "" || u.Path == "/") && u.RawPath == ""
}
