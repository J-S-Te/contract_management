package contract

import "errors"

type ImportMode string

const (
	ImportNormal           ImportMode = "NORMAL"
	ImportArchiveOnly      ImportMode = "ARCHIVE_ONLY"
	ImportContinueLive     ImportMode = "CONTINUE_LIVE"
	ImportHistoricalClosed ImportMode = "HISTORICAL_CLOSED"
)

var ErrImportPolicy = errors.New("invalid contract import policy")

type ImportPolicy struct {
	Version            int    `json:"policy_version"`
	ProjectMode        string `json:"project_mode"`
	FinanceMode        string `json:"finance_mode"`
	ReportMode         string `json:"report_mode"`
	ResponsibilityMode string `json:"responsibility_mode"`
	SigningMode        string `json:"signing_mode"`
}

// ValidateImportPolicy 验证模式及持久策略，阻止历史记录回退普通业务。
// 参数：mode 为接入模式，policy 为服务端冻结的历史业务处理策略。
// 返回值：合法组合返回nil；缺策略、未知版本或不安全组合返回ErrImportPolicy。
func ValidateImportPolicy(mode ImportMode, policy *ImportPolicy) error {
	if mode == "" || mode == ImportNormal {
		if policy != nil {
			return ErrImportPolicy
		}
		return nil
	}
	if policy == nil || policy.Version != 1 || !validImportPresentation(policy) {
		return ErrImportPolicy
	}
	valid := false
	switch mode {
	case ImportArchiveOnly:
		valid = policy.ProjectMode == "NONE" && policy.FinanceMode == "NONE" &&
			policy.ReportMode == "ARCHIVE_SEPARATE" && policy.ResponsibilityMode == "INACTIVE"
	case ImportContinueLive:
		valid = (policy.ProjectMode == "CREATE_NEW" || policy.ProjectMode == "LINK_EXISTING") &&
			policy.FinanceMode == "FULL_REPLAY" && policy.ResponsibilityMode == "ACTIVATE_ON_RELEASE"
	case ImportHistoricalClosed:
		valid = policy.ProjectMode == "NONE" && policy.ResponsibilityMode == "INACTIVE" &&
			(policy.FinanceMode == "EVIDENCE_ONLY" || policy.FinanceMode == "FULL_REPLAY")
	}
	if !valid {
		return ErrImportPolicy
	}
	return nil
}

// validImportPresentation 限定历史统计和签署策略的已知枚举。
// 参数：policy 为已确认非空的历史策略。
// 返回值：统计与签署策略均受支持时为true。
func validImportPresentation(policy *ImportPolicy) bool {
	report := policy.ReportMode == "ARCHIVE_SEPARATE" ||
		policy.ReportMode == "HISTORICAL_SEPARATE"
	signing := policy.SigningMode == "DECLARED_ONLY"
	return report && signing
}
