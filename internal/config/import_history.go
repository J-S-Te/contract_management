package config

import (
	"fmt"
	"net/url"
	"os"
	"slices"
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
