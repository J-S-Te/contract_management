package config

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 承载合同子系统的全部运行配置：HTTP 监听、MySQL DSN、平台与 OIDC 对接，
// 以及各跨系统机器接入（数据看板/结算/项目审批/CRM 等）的开关与凭据。
// 分组注释标明每组配置归属的子系统；字段取值合法性由 validate 统一校验，
// 校验失败聚合返回全部问题，便于一次性修正部署配置。
type Config struct {
	CounterpartySource              string
	CategorySource                  string
	CentralSessionEnabled           bool
	CentralSessionPublicBaseURL     string
	ExternalPDFEnabled              bool
	ContractImportEnabled           bool
	ContractImportHistoryEnabled    bool
	ContractImportConfirmationRoles []string
	// ContractImportAutoApproveHistorical 是历史导入自动审批的能力开关
	//（CONTRACT_IMPORT_AUTO_APPROVE_HISTORICAL，默认 false）：开启后接口与前端
	// 才允许按租户开启运行开关，且仍须租户开关为真才自动确认并按声明状态归档。
	// 关闭时逐字节保持既有行为（仍需人工确认链路），接口按能力未开启拒绝。
	ContractImportAutoApproveHistorical bool
	ImportProject                       ImportTargetConfig
	ImportFinance                       ImportTargetConfig
	ImportReport                        ImportTargetConfig
	ImportReportRead                    ImportReportReadConfig
	ContractImportMaxItems              int
	ContractImportMaxConcurrency        int
	ContractImportStagingTTL            time.Duration
	HTTPAddress                         string
	// TrustedProxies 是唯一允许采信 X-Real-IP / X-Forwarded-For 的直接对端集合。
	TrustedProxies         []string
	MySQLDSN               string
	PlatformBaseURL        string
	OIDCIssuer             string
	OIDCBackchannelBaseURL string
	OIDCClientID           string
	// DashboardMachine* 数据看板系统机器接入（internal/v1/dashboard）
	DashboardMachineEnabled       bool
	DashboardMachineRequireBearer bool
	DashboardMachineClientID      string
	DashboardMachineAudience      string
	DashboardMachineIssuer        string
	DashboardMachinePublicKeyPath string
	DashboardMachineCallerApp     string
	DashboardMachineCallerEnv     string
	DashboardMachineScope         string
	// SettlementMachine* 结算系统读取已完成合同的机器接入配置。
	SettlementMachineEnabled            bool
	SettlementMachineRequireBearer      bool
	SettlementMachineClientID           string
	SettlementMachineAudience           string
	SettlementMachineRequiredScope      string
	ProjectApprovalMachineEnabled       bool
	ProjectApprovalMachineRequireBearer bool
	ProjectApprovalMachineClientID      string
	ProjectApprovalMachineAudience      string
	ProjectApprovalMachineIssuer        string
	ProjectApprovalMachinePublicKeyPath string
	ProjectApprovalMachineCallerApp     string
	ProjectApprovalMachineCallerEnv     string
	ProjectApprovalMachineScope         string
	CRMSignedCountMachineEnabled        bool
	CRMSignedCountMachineRequireBearer  bool
	CRMSignedCountMachineClientID       string
	CRMSignedCountMachineAudience       string
	// HandoverMachine* 配置基础平台调用的人员交接机器协议端点。
	// 信任域是平台自身 ApplicationJWTManager 签发的 application JWT
	//（issuer/audience/client_id/caller 应用与环境/唯一 scope），不再是 Keycloak OIDC 令牌。
	HandoverMachineEnabled                   bool
	HandoverMachineRequireBearer             bool
	HandoverMachineClientID                  string
	HandoverMachineAudience                  string
	HandoverMachineIssuer                    string
	HandoverMachinePublicKeyPath             string
	HandoverMachineCallerApp                 string
	HandoverMachineCallerEnv                 string
	HandoverMachineScope                     string
	CRMSignedCountMachineIssuer              string
	CRMSignedCountMachinePublicKeyPath       string
	CRMSignedCountMachineCallerApp           string
	CRMSignedCountMachineCallerEnv           string
	CRMSignedCountMachineScope               string
	CRMOpportunityIntakeMachineEnabled       bool
	CRMOpportunityIntakeMachineRequireBearer bool
	CRMOpportunityIntakeMachineClientID      string
	CRMOpportunityIntakeMachineAudience      string
	CRMOpportunityIntakeMachineIssuer        string
	CRMOpportunityIntakeMachinePublicKeyPath string
	CRMOpportunityIntakeMachineCallerApp     string
	CRMOpportunityIntakeMachineCallerEnv     string
	CRMOpportunityIntakeMachineScope         string
	OIDCClientSecret                         string
	OIDCRedirectURI                          string
	OIDCPostLogoutRedirectURI                string
	OIDCIDPHint                              string
	OIDCScopes                               []string
	OIDCTenantID                             string
	OIDCSessionCookieName                    string
	// OIDCSessionIdleTTL 是浏览器会话的空闲期限：只有显式活动探测会推进基准，
	// 普通业务请求（含轮询）只记录 last_seen_at，不能延长空闲时间窗。
	OIDCSessionIdleTTL time.Duration
	// OIDCSessionAbsoluteTTL 是浏览器会话的绝对期限，任何活动都不能延长；
	// 旧 OIDC_SESSION_TTL 仅作为该值的兼容兜底。
	OIDCSessionAbsoluteTTL         time.Duration
	OIDCAuthorizationRefresh       time.Duration
	OIDCAuthorizationTimeout       time.Duration
	OIDCAuthorizationMaxStale      time.Duration
	OIDCSessionEncryptionKey       []byte
	OIDCSessionSecure              bool
	AppPublicURL                   string
	AppPathPrefix                  string
	PlatformAuditClientID          string
	PlatformAuditClientSecret      string
	PlatformApplicationCode        string
	PlatformEnvironmentCode        string
	PlatformApplicationID          string
	PlatformCatalogSync            bool
	PlatformCatalogClientID        string
	PlatformCatalogSecret          string
	PlatformPersonnelClientID      string
	PlatformPersonnelSecret        string
	PlatformNotificationClientID   string
	PlatformNotificationSecret     string
	TemporalAddress                string
	TemporalNamespace              string
	TemporalTaskQueue              string
	TemporalWorkerBuildID          string
	TemporalWorkerDeploymentName   string
	TemporalWorkerVersioning       bool
	TemporalWorkerVersioningPolicy string
	TemporalMetricsAddress         string
	TemporalAPIKey                 string
	TemporalTLS                    bool
	NodeTimeout                    time.Duration
	ReminderInterval               time.Duration
	// ArchiveCron 是过期合同自动归档的 cron 表达式。
	// 注意：Temporal 按 UTC 解析 cron，默认值 "0 16 * * *" 表示 16:00 UTC = 北京 00:00，
	// 恰好是想要的业务时间。修改时必须按 UTC 换算，不要按北京时间理解成“下午 4 点”。
	ArchiveCron string
	// NumberAuditCron 是合同编号登记表漂移的每日只读巡检 cron 表达式。
	// 默认 "0 17 * * *" 同样是 UTC 表达式（17:00 UTC = 北京 01:00，比归档 cron 的
	// 北京 00:00 错开一小时），不要按北京时间理解；显式配置为空串表示禁用
	// 该周期巡检，worker 见空值不注册 cron。巡检只读，不主动通知任何人。
	NumberAuditCron                 string
	ProjectCategoryDirectoryEnabled bool
	ProjectIntegrationEnabled       bool
	ProjectAPIBaseURL               string
	ProjectIntegrationPoll          time.Duration
	ProjectIntegrationRetries       uint

	// ContractSummaryMachine* 是 CRM 商机签约前调用 GET /internal/contract-summary
	// 的只读机器接入（scope 固定 contract.summary.read，caller 固定 customer_and_opportunity）。
	ContractSummaryMachineEnabled       bool
	ContractSummaryMachineRequireBearer bool
	ContractSummaryMachineClientID      string
	ContractSummaryMachineAudience      string
	ContractSummaryMachineIssuer        string
	ContractSummaryMachinePublicKeyPath string
	ContractSummaryMachineCallerApp     string
	ContractSummaryMachineCallerEnv     string
	ContractSummaryMachineScope         string

	// H4：内部投递机器令牌（项目侧来源校验强制开启后必配）。
	ProjectIntegrationTokenURL     string
	ProjectIntegrationClientID     string
	ProjectIntegrationClientSecret string
	ProjectIntegrationAudience     string
	// 出站：合同 → 结算 财务生效事件（contract.financial_effective.v1）。
	// 仅在付款计划已人工维护且合计等于合同金额时才产生事件（见 payment_terms.go）。
	SettlementIntegrationEnabled      bool
	SettlementAPIBaseURL              string
	SettlementIntegrationPoll         time.Duration
	SettlementIntegrationRetries      uint
	SettlementIntegrationTokenURL     string
	SettlementIntegrationClientID     string
	SettlementIntegrationClientSecret string
	SettlementIntegrationAudience     string
	// 受控“合同财务变更”入口（BIZ-XEVT2-05 合同侧第一波）。默认关闭：
	// 关闭时写接口直接拒绝，不得因配置缺失而默认开放。
	// 开启后仍需 contract.edit 权限、合同已审批且没有在途审批、审计留痕；
	// 变更成功同事务 version+1、替换付款计划并写入新版本财务事件。
	ContractFinancialChangeEnabled bool
	CRMReferenceEnabled            bool
	CRMReferenceBaseURL            string
	CRMReferenceTokenURL           string
	CRMReferenceClientID           string
	CRMReferenceClientSecret       string
	CRMReferenceScope              string
	// ContractImportCRMProvision* 是历史导入自动开立 CRM 客户/商机的机器接入
	//（设计定稿 3.2，默认关闭）。平台侧一用途一 client 一 scope，因此客户链路
	// 与商机链路各用一对专用凭据，BASE_URL 缺省回退 CRM_REFERENCE_BASE_URL。
	// 开启时要求历史导入已开启且两对凭据完整，否则启动失败关闭。
	ContractImportCRMProvisionEnabled   bool
	CRMProvisionBaseURL                 string
	CRMProvisionTokenURL                string
	CRMProvisionClientID                string
	CRMProvisionClientSecret            string
	CRMProvisionScope                   string
	CRMProvisionOpportunityClientID     string
	CRMProvisionOpportunityClientSecret string
	CRMProvisionOpportunityScope        string
	// CRMLink*：合同→CRM 签约关联回写（crm.Dispatcher / crm.LinkNotifier）。
	// 启用条件：CRM_LINK_ENABLED显式选择；缺省按原URL非空兼容。启用时必须通过平台 client_credentials
	// 取得专用机器令牌（scope 固定 opportunity.signed.write），
	// TOKEN_URL/CLIENT_ID/CLIENT_SECRET 缺一即启动失败关闭。
	// BaseURL 为空表示该边未配置：dispatcher 不运行，启动日志明确说明，
	// 不再出现“审核完成却静默不回写”。
	CRMLinkEnabled      bool
	CRMLinkBaseURL      string
	CRMLinkTokenURL     string
	CRMLinkClientID     string
	CRMLinkClientSecret string
	CRMLinkScope        string
	// StampedFile* 控制盖章 PDF 到平台文件网关的渐进迁移；默认 旧版模式保留旧 BLOB。
	StampedFileMode          string
	StampedFileGatewayURL    string
	StampedFileApplicationID string
	StampedFileClientID      string
	StampedFileClientSecret  string
	StampedFileScope         string
}

// Load 加载并验证合同进程的运行与导入资源配置。
// 参数：无；从当前进程环境读取，不读取业务数据库。
// 返回值：完整配置；缺失必填项或非法值返回错误。
// Load 装载合同进程配置并校验来源与实际启用的联动能力。
// 参数：无，读取当前进程环境。
// 返回值：有效配置或失败关闭错误，不输出凭据。
func Load() (Config, error) {
	c := Config{
		HTTPAddress: env("HTTP_ADDRESS", ":8081"), PlatformBaseURL: env("PLATFORM_BASE_URL", "http://localhost:8080"),
		TrustedProxies: commaList(envOr("APP_TRUSTED_PROXIES", "127.0.0.1/32,::1/128")),
		OIDCIssuer:     os.Getenv("OIDC_ISSUER"), OIDCClientID: os.Getenv("OIDC_CLIENT_ID"),
		OIDCBackchannelBaseURL:                   os.Getenv("OIDC_BACKCHANNEL_BASE_URL"),
		DashboardMachineEnabled:                  envBool("DASHBOARD_MACHINE_ENABLED", false),
		DashboardMachineRequireBearer:            envBool("DASHBOARD_MACHINE_REQUIRE_BEARER", false),
		DashboardMachineClientID:                 os.Getenv("DASHBOARD_MACHINE_CLIENT_ID"),
		DashboardMachineAudience:                 os.Getenv("DASHBOARD_MACHINE_AUDIENCE"),
		DashboardMachineIssuer:                   os.Getenv("DASHBOARD_MACHINE_ISSUER"),
		DashboardMachinePublicKeyPath:            os.Getenv("DASHBOARD_MACHINE_PUBLIC_KEY_PATH"),
		DashboardMachineCallerApp:                os.Getenv("DASHBOARD_MACHINE_CALLER_APPLICATION_CODE"),
		DashboardMachineCallerEnv:                os.Getenv("DASHBOARD_MACHINE_CALLER_ENVIRONMENT_CODE"),
		DashboardMachineScope:                    os.Getenv("DASHBOARD_MACHINE_REQUIRED_SCOPE"),
		SettlementMachineEnabled:                 envBool("SETTLEMENT_MACHINE_ENABLED", false),
		SettlementMachineRequireBearer:           envBool("SETTLEMENT_MACHINE_REQUIRE_BEARER", false),
		SettlementMachineClientID:                os.Getenv("SETTLEMENT_MACHINE_CLIENT_ID"),
		SettlementMachineAudience:                os.Getenv("SETTLEMENT_MACHINE_AUDIENCE"),
		SettlementMachineRequiredScope:           os.Getenv("SETTLEMENT_MACHINE_REQUIRED_SCOPE"),
		HandoverMachineEnabled:                   envBool("HANDOVER_MACHINE_ENABLED", false),
		HandoverMachineRequireBearer:             envBool("HANDOVER_MACHINE_REQUIRE_BEARER", false),
		HandoverMachineClientID:                  os.Getenv("HANDOVER_MACHINE_CLIENT_ID"),
		HandoverMachineAudience:                  os.Getenv("HANDOVER_MACHINE_AUDIENCE"),
		HandoverMachineIssuer:                    os.Getenv("HANDOVER_MACHINE_ISSUER"),
		HandoverMachinePublicKeyPath:             os.Getenv("HANDOVER_MACHINE_PUBLIC_KEY_PATH"),
		HandoverMachineCallerApp:                 os.Getenv("HANDOVER_MACHINE_CALLER_APPLICATION_CODE"),
		HandoverMachineCallerEnv:                 os.Getenv("HANDOVER_MACHINE_CALLER_ENVIRONMENT_CODE"),
		HandoverMachineScope:                     os.Getenv("HANDOVER_MACHINE_REQUIRED_SCOPE"),
		ProjectApprovalMachineEnabled:            envBool("PROJECT_APPROVAL_MACHINE_ENABLED", false),
		ProjectApprovalMachineRequireBearer:      envBool("PROJECT_APPROVAL_MACHINE_REQUIRE_BEARER", false),
		ProjectApprovalMachineClientID:           os.Getenv("PROJECT_APPROVAL_MACHINE_CLIENT_ID"),
		ProjectApprovalMachineAudience:           os.Getenv("PROJECT_APPROVAL_MACHINE_AUDIENCE"),
		ProjectApprovalMachineIssuer:             os.Getenv("PROJECT_APPROVAL_MACHINE_ISSUER"),
		ProjectApprovalMachinePublicKeyPath:      os.Getenv("PROJECT_APPROVAL_MACHINE_PUBLIC_KEY_PATH"),
		ProjectApprovalMachineCallerApp:          os.Getenv("PROJECT_APPROVAL_MACHINE_CALLER_APPLICATION_CODE"),
		ProjectApprovalMachineCallerEnv:          os.Getenv("PROJECT_APPROVAL_MACHINE_CALLER_ENVIRONMENT_CODE"),
		ProjectApprovalMachineScope:              os.Getenv("PROJECT_APPROVAL_MACHINE_REQUIRED_SCOPE"),
		CRMSignedCountMachineEnabled:             envBool("CRM_SIGNED_COUNT_MACHINE_ENABLED", false),
		CRMSignedCountMachineRequireBearer:       envBool("CRM_SIGNED_COUNT_MACHINE_REQUIRE_BEARER", false),
		CRMSignedCountMachineClientID:            os.Getenv("CRM_SIGNED_COUNT_MACHINE_CLIENT_ID"),
		CRMSignedCountMachineAudience:            os.Getenv("CRM_SIGNED_COUNT_MACHINE_AUDIENCE"),
		CRMSignedCountMachineIssuer:              os.Getenv("CRM_SIGNED_COUNT_MACHINE_ISSUER"),
		CRMSignedCountMachinePublicKeyPath:       os.Getenv("CRM_SIGNED_COUNT_MACHINE_PUBLIC_KEY_PATH"),
		CRMSignedCountMachineCallerApp:           os.Getenv("CRM_SIGNED_COUNT_MACHINE_CALLER_APPLICATION_CODE"),
		CRMSignedCountMachineCallerEnv:           os.Getenv("CRM_SIGNED_COUNT_MACHINE_CALLER_ENVIRONMENT_CODE"),
		CRMSignedCountMachineScope:               os.Getenv("CRM_SIGNED_COUNT_MACHINE_REQUIRED_SCOPE"),
		CRMOpportunityIntakeMachineEnabled:       envBool("CRM_OPPORTUNITY_INTAKE_MACHINE_ENABLED", false),
		CRMOpportunityIntakeMachineRequireBearer: envBool("CRM_OPPORTUNITY_INTAKE_MACHINE_REQUIRE_BEARER", false),
		CRMOpportunityIntakeMachineClientID:      os.Getenv("CRM_OPPORTUNITY_INTAKE_MACHINE_CLIENT_ID"),
		CRMOpportunityIntakeMachineAudience:      os.Getenv("CRM_OPPORTUNITY_INTAKE_MACHINE_AUDIENCE"),
		CRMOpportunityIntakeMachineIssuer:        os.Getenv("CRM_OPPORTUNITY_INTAKE_MACHINE_ISSUER"),
		CRMOpportunityIntakeMachinePublicKeyPath: os.Getenv("CRM_OPPORTUNITY_INTAKE_MACHINE_PUBLIC_KEY_PATH"),
		CRMOpportunityIntakeMachineCallerApp:     os.Getenv("CRM_OPPORTUNITY_INTAKE_MACHINE_CALLER_APPLICATION_CODE"),
		CRMOpportunityIntakeMachineCallerEnv:     os.Getenv("CRM_OPPORTUNITY_INTAKE_MACHINE_CALLER_ENVIRONMENT_CODE"),
		CRMOpportunityIntakeMachineScope:         os.Getenv("CRM_OPPORTUNITY_INTAKE_MACHINE_REQUIRED_SCOPE"),
		ContractSummaryMachineEnabled:            envBool("CONTRACT_SUMMARY_MACHINE_ENABLED", false),
		ContractSummaryMachineRequireBearer:      envBool("CONTRACT_SUMMARY_MACHINE_REQUIRE_BEARER", false),
		ContractSummaryMachineClientID:           os.Getenv("CONTRACT_SUMMARY_MACHINE_CLIENT_ID"),
		ContractSummaryMachineAudience:           os.Getenv("CONTRACT_SUMMARY_MACHINE_AUDIENCE"),
		ContractSummaryMachineIssuer:             os.Getenv("CONTRACT_SUMMARY_MACHINE_ISSUER"),
		ContractSummaryMachinePublicKeyPath:      os.Getenv("CONTRACT_SUMMARY_MACHINE_PUBLIC_KEY_PATH"),
		ContractSummaryMachineCallerApp:          os.Getenv("CONTRACT_SUMMARY_MACHINE_CALLER_APPLICATION_CODE"),
		ContractSummaryMachineCallerEnv:          os.Getenv("CONTRACT_SUMMARY_MACHINE_CALLER_ENVIRONMENT_CODE"),
		ContractSummaryMachineScope:              os.Getenv("CONTRACT_SUMMARY_MACHINE_REQUIRED_SCOPE"),
		OIDCClientSecret:                         os.Getenv("OIDC_CLIENT_SECRET"), OIDCRedirectURI: os.Getenv("OIDC_REDIRECT_URI"),
		OIDCPostLogoutRedirectURI: os.Getenv("OIDC_POST_LOGOUT_REDIRECT_URI"),
		OIDCIDPHint:               strings.TrimSpace(os.Getenv("OIDC_IDP_HINT")),
		OIDCScopes:                fields(env("OIDC_SCOPES", "openid profile")), OIDCTenantID: os.Getenv("OIDC_TENANT_ID"),
		OIDCSessionCookieName: env("OIDC_SESSION_COOKIE_NAME", "contract_management_session"),
		AppPublicURL:          os.Getenv("APP_PUBLIC_URL"), AppPathPrefix: env("APP_PATH_PREFIX", "/contract_management"),
		PlatformAuditClientID: os.Getenv("PLATFORM_AUDIT_CLIENT_ID"), PlatformAuditClientSecret: os.Getenv("PLATFORM_AUDIT_CLIENT_SECRET"), PlatformApplicationCode: os.Getenv("PLATFORM_APPLICATION_CODE"), PlatformEnvironmentCode: os.Getenv("PLATFORM_ENVIRONMENT_CODE"),
		PlatformApplicationID:          os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID"),
		PlatformCatalogClientID:        os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID"),
		PlatformCatalogSecret:          os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET"),
		PlatformPersonnelClientID:      os.Getenv("PLATFORM_PERSONNEL_DIRECTORY_CLIENT_ID"),
		PlatformPersonnelSecret:        os.Getenv("PLATFORM_PERSONNEL_DIRECTORY_CLIENT_SECRET"),
		PlatformNotificationClientID:   os.Getenv("PLATFORM_NOTIFICATION_CLIENT_ID"),
		PlatformNotificationSecret:     os.Getenv("PLATFORM_NOTIFICATION_CLIENT_SECRET"),
		TemporalAddress:                env("TEMPORAL_ADDRESS", "localhost:7233"),
		TemporalNamespace:              env("TEMPORAL_NAMESPACE", "default"),
		TemporalTaskQueue:              env("TEMPORAL_TASK_QUEUE", "contract-management"),
		TemporalWorkerBuildID:          env("TEMPORAL_WORKER_BUILD_ID", "contract-worker-v1"),
		TemporalWorkerDeploymentName:   env("TEMPORAL_WORKER_DEPLOYMENT_NAME", "contract-management"),
		TemporalWorkerVersioningPolicy: strings.ToUpper(env("TEMPORAL_WORKER_VERSIONING_POLICY", "PINNED")),
		TemporalMetricsAddress:         env("TEMPORAL_METRICS_ADDRESS", ":9091"),
		TemporalAPIKey:                 os.Getenv("TEMPORAL_API_KEY"), ArchiveCron: env("ARCHIVE_CRON_SCHEDULE", "0 16 * * *"),
		// 编号巡检 cron 与归档 cron 的读取方式刻意不同：env() 把空串当“未配置”回落默认值，
		// 因此归档 cron 实际上无法用空串关闭；本功能按运维约定把显式空串解释为禁用，
		// 需要区分“未设置”和“显式置空”，故使用 cronEnv。
		NumberAuditCron:      cronEnv("NUMBER_AUDIT_CRON_SCHEDULE", "0 17 * * *"),
		ProjectAPIBaseURL:    env("PROJECT_API_BASE_URL", "http://localhost:8082"),
		SettlementAPIBaseURL: env("SETTLEMENT_API_BASE_URL", "http://localhost:8085"),
		CRMReferenceBaseURL:  env("CRM_REFERENCE_BASE_URL", env("CRM_API_BASE_URL", "http://localhost:8090/customer-opportunity")),
		CRMReferenceTokenURL: os.Getenv("CRM_REFERENCE_TOKEN_URL"), CRMReferenceClientID: os.Getenv("CRM_REFERENCE_CLIENT_ID"),
		CRMReferenceClientSecret: os.Getenv("CRM_REFERENCE_CLIENT_SECRET"), CRMReferenceScope: env("CRM_REFERENCE_SCOPE", "customer.contract_reference.read"),
		CRMProvisionBaseURL: os.Getenv("CRM_PROVISION_BASE_URL"), CRMProvisionTokenURL: os.Getenv("CRM_PROVISION_TOKEN_URL"),
		CRMProvisionClientID: os.Getenv("CRM_PROVISION_CLIENT_ID"), CRMProvisionClientSecret: os.Getenv("CRM_PROVISION_CLIENT_SECRET"),
		CRMProvisionScope:                   env("CRM_PROVISION_SCOPE", "customer.import_provision"),
		CRMProvisionOpportunityClientID:     os.Getenv("CRM_PROVISION_OPPORTUNITY_CLIENT_ID"),
		CRMProvisionOpportunityClientSecret: os.Getenv("CRM_PROVISION_OPPORTUNITY_CLIENT_SECRET"),
		CRMProvisionOpportunityScope:        env("CRM_PROVISION_OPPORTUNITY_SCOPE", "opportunity.import_provision"),
		CRMLinkBaseURL:                      os.Getenv("CRM_LINK_BASE_URL"), CRMLinkTokenURL: os.Getenv("CRM_LINK_TOKEN_URL"),
		CRMLinkClientID: os.Getenv("CRM_LINK_CLIENT_ID"), CRMLinkClientSecret: os.Getenv("CRM_LINK_CLIENT_SECRET"),
		CRMLinkScope:    env("CRM_LINK_SCOPE", "opportunity.signed.write"),
		StampedFileMode: env("STAMPED_FILE_GATEWAY_MODE", "legacy"), StampedFileGatewayURL: env("FILE_GATEWAY_BASE_URL", ""),
		StampedFileApplicationID: os.Getenv("FILE_GATEWAY_APPLICATION_ID"), StampedFileClientID: os.Getenv("FILE_GATEWAY_CLIENT_ID"), StampedFileClientSecret: os.Getenv("FILE_GATEWAY_CLIENT_SECRET"), StampedFileScope: env("FILE_GATEWAY_SCOPE", "platform:file:upload platform:file:bind"),
	}
	// CRM_PROVISION_BASE_URL 缺省复用 CRM 引用边地址：两者指向同一 CRM 服务，
	// 回退只覆盖地址，令牌端点与专用 scope 仍必须显式配置（失败关闭）。
	if strings.TrimSpace(c.CRMProvisionBaseURL) == "" {
		c.CRMProvisionBaseURL = c.CRMReferenceBaseURL
	}
	var err error
	if err = loadImportConfig(&c); err != nil {
		return c, err
	}
	if c.NodeTimeout, err = duration("APPROVAL_NODE_TIMEOUT", 72*time.Hour); err != nil {
		return c, err
	}
	if c.ReminderInterval, err = duration("APPROVAL_REMINDER_INTERVAL", 24*time.Hour); err != nil {
		return c, err
	}
	if c.OIDCSessionIdleTTL, err = duration("OIDC_SESSION_IDLE_TTL", 30*time.Minute); err != nil {
		return c, err
	}
	// 旧 OIDC_SESSION_TTL 只作为绝对期限的兼容兜底：新键显式设置时以新键为准。
	// 这样旧环境显式配置的短绝对期限不会被静默放大到 8h；上线时由运维显式改用新键。
	absoluteRaw := strings.TrimSpace(os.Getenv("OIDC_SESSION_ABSOLUTE_TTL"))
	if absoluteRaw == "" {
		absoluteRaw = strings.TrimSpace(os.Getenv("OIDC_SESSION_TTL"))
	}
	if c.OIDCSessionAbsoluteTTL, err = parseDurationValue("OIDC_SESSION_ABSOLUTE_TTL", absoluteRaw, 8*time.Hour); err != nil {
		return c, err
	}
	c.CentralSessionEnabled, err = strconv.ParseBool(env("CENTRAL_SESSION_ENABLED", "false"))
	if err != nil {
		return c, fmt.Errorf("CENTRAL_SESSION_ENABLED: %w", err)
	}
	c.CentralSessionPublicBaseURL = strings.TrimSpace(os.Getenv("CENTRAL_SESSION_PUBLIC_BASE_URL"))
	if c.OIDCAuthorizationRefresh, err = duration("OIDC_AUTHORIZATION_REFRESH_INTERVAL", time.Minute); err != nil {
		return c, err
	}
	if c.OIDCAuthorizationTimeout, err = duration("OIDC_AUTHORIZATION_CONTEXT_TIMEOUT", 10*time.Second); err != nil {
		return c, err
	}
	if c.OIDCAuthorizationMaxStale, err = duration("OIDC_AUTHORIZATION_MAX_STALE", 2*time.Minute); err != nil {
		return c, err
	}
	encodedKey := strings.TrimSpace(os.Getenv("OIDC_SESSION_ENCRYPTION_KEY_BASE64"))
	if encodedKey == "" {
		return c, fmt.Errorf("OIDC_SESSION_ENCRYPTION_KEY_BASE64 is required")
	}
	c.OIDCSessionEncryptionKey, err = base64.StdEncoding.DecodeString(encodedKey)
	if err != nil || len(c.OIDCSessionEncryptionKey) != 32 {
		return c, fmt.Errorf("OIDC_SESSION_ENCRYPTION_KEY_BASE64 must decode to exactly 32 bytes")
	}
	if c.OIDCSessionSecure, err = strconv.ParseBool(env("OIDC_SESSION_COOKIE_SECURE", "true")); err != nil {
		return c, fmt.Errorf("OIDC_SESSION_COOKIE_SECURE: %w", err)
	}
	c.MySQLDSN = os.Getenv("MYSQL_DSN")
	if c.MySQLDSN == "" {
		return c, fmt.Errorf("MYSQL_DSN is required")
	}
	if c.TemporalTLS, err = strconv.ParseBool(env("TEMPORAL_TLS", "false")); err != nil {
		return c, fmt.Errorf("TEMPORAL_TLS: %w", err)
	}
	// Deployment Versioning 必须在注册并提升 Current 版本后显式开启。默认关闭可
	// 保证升级前创建的 UNVERSIONED 长工作流仍有兼容 Worker 消费。
	if c.TemporalWorkerVersioning, err = strconv.ParseBool(env("TEMPORAL_WORKER_VERSIONING_ENABLED", "false")); err != nil {
		return c, fmt.Errorf("TEMPORAL_WORKER_VERSIONING_ENABLED: %w", err)
	}
	if c.PlatformCatalogSync, err = strconv.ParseBool(env("PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED", "false")); err != nil {
		return c, fmt.Errorf("PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED: %w", err)
	}
	if c.ProjectIntegrationEnabled, err = strconv.ParseBool(env("PROJECT_INTEGRATION_ENABLED", "false")); err != nil {
		return c, fmt.Errorf("PROJECT_INTEGRATION_ENABLED: %w", err)
	}
	if c.ProjectIntegrationPoll, err = duration("PROJECT_INTEGRATION_POLL_INTERVAL", 2*time.Second); err != nil {
		return c, err
	}
	retries, parseErr := strconv.ParseUint(env("PROJECT_INTEGRATION_MAX_ATTEMPTS", "20"), 10, 32)
	if parseErr != nil || retries == 0 {
		return c, fmt.Errorf("PROJECT_INTEGRATION_MAX_ATTEMPTS must be a positive integer")
	}
	c.ProjectIntegrationRetries = uint(retries)
	c.ProjectIntegrationTokenURL = os.Getenv("PROJECT_INTEGRATION_TOKEN_URL")
	c.ProjectIntegrationClientID = os.Getenv("PROJECT_INTEGRATION_CLIENT_ID")
	c.ProjectIntegrationClientSecret = os.Getenv("PROJECT_INTEGRATION_CLIENT_SECRET")
	c.ProjectIntegrationAudience = os.Getenv("PROJECT_INTEGRATION_AUDIENCE")
	if c.SettlementIntegrationEnabled, err = strconv.ParseBool(env("SETTLEMENT_INTEGRATION_OUTBOUND_ENABLED", "false")); err != nil {
		return c, fmt.Errorf("SETTLEMENT_INTEGRATION_OUTBOUND_ENABLED: %w", err)
	}
	if c.SettlementIntegrationPoll, err = duration("SETTLEMENT_INTEGRATION_POLL_INTERVAL", 2*time.Second); err != nil {
		return c, err
	}
	settlementRetries, settlementParseErr := strconv.ParseUint(env("SETTLEMENT_INTEGRATION_MAX_ATTEMPTS", "20"), 10, 32)
	if settlementParseErr != nil || settlementRetries == 0 {
		return c, fmt.Errorf("SETTLEMENT_INTEGRATION_MAX_ATTEMPTS must be a positive integer")
	}
	c.SettlementIntegrationRetries = uint(settlementRetries)
	c.SettlementIntegrationTokenURL = os.Getenv("SETTLEMENT_INTEGRATION_TOKEN_URL")
	c.SettlementIntegrationClientID = os.Getenv("SETTLEMENT_INTEGRATION_CLIENT_ID")
	c.SettlementIntegrationClientSecret = os.Getenv("SETTLEMENT_INTEGRATION_CLIENT_SECRET")
	c.SettlementIntegrationAudience = os.Getenv("SETTLEMENT_INTEGRATION_AUDIENCE")
	// 受控财务变更入口默认关闭：部署方必须显式开启，且开启后仍由
	// 权限、审批前置与审计三重门禁把守（失败关闭）。
	if c.ContractFinancialChangeEnabled, err = strconv.ParseBool(env("CONTRACT_FINANCIAL_CHANGE_ENABLED", "false")); err != nil {
		return c, fmt.Errorf("CONTRACT_FINANCIAL_CHANGE_ENABLED: %w", err)
	}
	if c.CRMReferenceEnabled, err = strconv.ParseBool(env("CRM_REFERENCE_ENABLED", "false")); err != nil {
		return c, fmt.Errorf("CRM_REFERENCE_ENABLED: %w", err)
	}
	// 历史导入 CRM 开立默认关闭：显式开启后由 validate 校验历史模式与专用凭据。
	if c.ContractImportCRMProvisionEnabled, err = strconv.ParseBool(env("CONTRACT_IMPORT_CRM_PROVISION_ENABLED", "false")); err != nil {
		return c, fmt.Errorf("CONTRACT_IMPORT_CRM_PROVISION_ENABLED must be a boolean")
	}
	c.CRMLinkEnabled, err = strconv.ParseBool(env("CRM_LINK_ENABLED", strconv.FormatBool(strings.TrimSpace(c.CRMLinkBaseURL) != "")))
	if err != nil {
		return c, fmt.Errorf("invalid CRM_LINK_ENABLED")
	}
	if !c.CRMLinkEnabled {
		c.CRMLinkBaseURL = ""
	}
	c.ProjectCategoryDirectoryEnabled, err = strconv.ParseBool(env("PROJECT_CATEGORY_DIRECTORY_ENABLED", strconv.FormatBool(c.ProjectIntegrationEnabled)))
	if err != nil {
		return c, fmt.Errorf("invalid PROJECT_CATEGORY_DIRECTORY_ENABLED")
	}
	c.CounterpartySource = env("CONTRACT_COUNTERPARTY_SOURCE", "crm")
	c.CategorySource = env("CONTRACT_CATEGORY_SOURCE", "project")
	if c.CounterpartySource != "crm" && c.CounterpartySource != "local" {
		return c, fmt.Errorf("invalid CONTRACT_COUNTERPARTY_SOURCE")
	}
	if c.CategorySource != "project" && c.CategorySource != "local" {
		return c, fmt.Errorf("invalid CONTRACT_CATEGORY_SOURCE")
	}
	if (c.CounterpartySource == "local" || c.CategorySource == "local") && (c.ProjectIntegrationEnabled || c.SettlementIntegrationEnabled || c.CRMLinkEnabled) {
		return c, fmt.Errorf("LOCAL contract source conflicts with enabled project, settlement or CRM link integration")
	}
	if c.CounterpartySource == "local" && c.CategorySource == "project" && !c.ProjectCategoryDirectoryEnabled {
		return c, fmt.Errorf("PROJECT_CATEGORY_DIRECTORY_ENABLED is required for local counterparties with project categories")
	}
	if c.CategorySource == "local" && c.CounterpartySource == "crm" && !c.CRMReferenceEnabled {
		return c, fmt.Errorf("CRM_REFERENCE_ENABLED is required for CRM counterparties with local categories")
	}
	if err := loadImportHistoryConfig(&c); err != nil {
		return c, err
	}
	if err := c.validate(); err != nil {
		return c, err
	}
	return c, nil
}

// validate 检查合同运行配置及新增导入开关的完整性和边界。
// 参数：无；使用接收者配置c。
// 返回值：配置有效返回nil，缺失、冲突或非法值返回可定位错误。
func (c Config) validate() error {
	if c.ContractImportEnabled {
		if c.StampedFileMode == "legacy" {
			return fmt.Errorf("CONTRACT_IMPORT_ENABLED requires configured file gateway storage")
		}
		for _, requiredScope := range []string{"platform:file:upload", "platform:file:bind", "platform:file:download"} {
			found := false
			for _, scope := range strings.Fields(c.StampedFileScope) {
				if scope == requiredScope {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("CONTRACT_IMPORT_ENABLED requires FILE_GATEWAY_SCOPE upload, bind and download")
			}
		}
	}
	if c.StampedFileMode != "legacy" && c.StampedFileMode != "dual" && c.StampedFileMode != "required" {
		return fmt.Errorf("STAMPED_FILE_GATEWAY_MODE must be legacy, dual or required")
	}
	if c.StampedFileMode != "legacy" {
		if !validHTTPOrigin(c.StampedFileGatewayURL) || strings.TrimSpace(c.StampedFileApplicationID) == "" || strings.TrimSpace(c.StampedFileClientID) == "" || strings.TrimSpace(c.StampedFileClientSecret) == "" || strings.TrimSpace(c.StampedFileScope) == "" {
			return fmt.Errorf("file gateway URL, application ID, client ID, client secret and scope are required when stamped file gateway is enabled")
		}
	}
	if strings.TrimSpace(c.HTTPAddress) == "" {
		return fmt.Errorf("HTTP_ADDRESS must not be empty")
	}
	if c.ProjectIntegrationEnabled || c.ProjectCategoryDirectoryEnabled {
		if !validHTTPOrigin(c.ProjectAPIBaseURL) {
			return fmt.Errorf("PROJECT_API_BASE_URL must be an HTTP(S) origin")
		}
		// H4：项目侧来源校验强制开启，机器令牌为必配；audience 可空（依赖项目侧配置）。
		for name, value := range map[string]string{
			"PROJECT_INTEGRATION_TOKEN_URL": c.ProjectIntegrationTokenURL, "PROJECT_INTEGRATION_CLIENT_ID": c.ProjectIntegrationClientID,
			"PROJECT_INTEGRATION_CLIENT_SECRET": c.ProjectIntegrationClientSecret,
		} {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("%s is required when project integration or category directory is enabled", name)
			}
		}
		tokenURL, tokenErr := url.ParseRequestURI(c.ProjectIntegrationTokenURL)
		if tokenErr != nil || (tokenURL.Scheme != "http" && tokenURL.Scheme != "https") || tokenURL.Host == "" ||
			tokenURL.User != nil || tokenURL.RawQuery != "" || tokenURL.Fragment != "" {
			return fmt.Errorf("PROJECT_INTEGRATION_TOKEN_URL must be a valid HTTP(S) URL without credentials, query or fragment")
		}
	}
	if c.SettlementIntegrationEnabled {
		if !validHTTPOrigin(c.SettlementAPIBaseURL) {
			return fmt.Errorf("SETTLEMENT_API_BASE_URL must be an HTTP(S) origin")
		}
		// 结算侧入站强制机器 Bearer 且要求令牌绑定可信租户（AUD-2026-001），令牌配置为必配。
		for name, value := range map[string]string{
			"SETTLEMENT_INTEGRATION_TOKEN_URL": c.SettlementIntegrationTokenURL, "SETTLEMENT_INTEGRATION_CLIENT_ID": c.SettlementIntegrationClientID,
			"SETTLEMENT_INTEGRATION_CLIENT_SECRET": c.SettlementIntegrationClientSecret,
		} {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("%s is required when SETTLEMENT_INTEGRATION_OUTBOUND_ENABLED=true", name)
			}
		}
		tokenURL, tokenErr := url.ParseRequestURI(c.SettlementIntegrationTokenURL)
		if tokenErr != nil || (tokenURL.Scheme != "http" && tokenURL.Scheme != "https") || tokenURL.Host == "" ||
			tokenURL.User != nil || tokenURL.RawQuery != "" || tokenURL.Fragment != "" {
			return fmt.Errorf("SETTLEMENT_INTEGRATION_TOKEN_URL must be a valid HTTP(S) URL without credentials, query or fragment")
		}
	}
	if c.SettlementIntegrationPoll <= 0 {
		return fmt.Errorf("SETTLEMENT_INTEGRATION_POLL_INTERVAL must be positive")
	}
	if c.ProjectIntegrationPoll <= 0 {
		return fmt.Errorf("PROJECT_INTEGRATION_POLL_INTERVAL must be positive")
	}
	if c.CRMReferenceEnabled {
		if !validCRMReferenceBaseURL(c.CRMReferenceBaseURL) {
			return fmt.Errorf("CRM_REFERENCE_BASE_URL must be an HTTP(S) origin with optional /customer-opportunity prefix, without credentials, query or fragment")
		}
		for _, item := range []struct{ name, value string }{
			{"CRM_REFERENCE_TOKEN_URL", c.CRMReferenceTokenURL},
			{"CRM_REFERENCE_CLIENT_ID", c.CRMReferenceClientID},
			{"CRM_REFERENCE_CLIENT_SECRET", c.CRMReferenceClientSecret},
		} {
			if strings.TrimSpace(item.value) == "" {
				return fmt.Errorf("%s is required when CRM_REFERENCE_ENABLED=true", item.name)
			}
		}
		tokenURL, tokenErr := url.ParseRequestURI(c.CRMReferenceTokenURL)
		if tokenErr != nil || (tokenURL.Scheme != "http" && tokenURL.Scheme != "https") || tokenURL.Host == "" || tokenURL.User != nil || tokenURL.RawQuery != "" || tokenURL.Fragment != "" {
			return fmt.Errorf("CRM_REFERENCE_TOKEN_URL must be a valid HTTP(S) URL without credentials, query or fragment")
		}
		if c.CRMReferenceScope != "customer.contract_reference.read" {
			return fmt.Errorf("CRM_REFERENCE_SCOPE must be customer.contract_reference.read")
		}
	}
	// 历史导入开立 CRM 会写业务数据（客户/商机），失败关闭口径比只读引用更严：
	// 必须历史模式已开启、地址与令牌端点合法、两对机器凭据非占位值，
	// 且客户链路与商机链路的 scope 各自精确等于专用用途（平台侧一用途一 client
	// 一 scope，多 scope 会因 PrimaryScope 为空在令牌端点 invalid_scope）。
	if c.ContractImportCRMProvisionEnabled {
		if !c.ContractImportHistoryEnabled {
			return fmt.Errorf("CONTRACT_IMPORT_CRM_PROVISION_ENABLED requires CONTRACT_IMPORT_HISTORY_ENABLED=true")
		}
		if !validCRMReferenceBaseURL(c.CRMProvisionBaseURL) {
			return fmt.Errorf("CRM_PROVISION_BASE_URL must be an HTTP(S) origin with optional /customer-opportunity prefix, without credentials, query or fragment")
		}
		for _, item := range []struct{ name, value string }{
			{"CRM_PROVISION_TOKEN_URL", c.CRMProvisionTokenURL},
			{"CRM_PROVISION_CLIENT_ID", c.CRMProvisionClientID},
			{"CRM_PROVISION_CLIENT_SECRET", c.CRMProvisionClientSecret},
			{"CRM_PROVISION_OPPORTUNITY_CLIENT_ID", c.CRMProvisionOpportunityClientID},
			{"CRM_PROVISION_OPPORTUNITY_CLIENT_SECRET", c.CRMProvisionOpportunityClientSecret},
		} {
			if strings.TrimSpace(item.value) == "" || placeholder(item.value) {
				return fmt.Errorf("%s is required when CONTRACT_IMPORT_CRM_PROVISION_ENABLED=true", item.name)
			}
		}
		tokenURL, tokenErr := url.ParseRequestURI(c.CRMProvisionTokenURL)
		if tokenErr != nil || (tokenURL.Scheme != "http" && tokenURL.Scheme != "https") || tokenURL.Host == "" || tokenURL.User != nil || tokenURL.RawQuery != "" || tokenURL.Fragment != "" {
			return fmt.Errorf("CRM_PROVISION_TOKEN_URL must be a valid HTTP(S) URL without credentials, query or fragment")
		}
		// scope 必须单值精确匹配：带空格拼多值会被平台拒绝（invalid_scope）。
		if c.CRMProvisionScope != "customer.import_provision" {
			return fmt.Errorf("CRM_PROVISION_SCOPE must be customer.import_provision")
		}
		if c.CRMProvisionOpportunityScope != "opportunity.import_provision" {
			return fmt.Errorf("CRM_PROVISION_OPPORTUNITY_SCOPE must be opportunity.import_provision")
		}
	}
	// 合同→CRM 关联回写的凭据必须经启动校验：BaseURL 为空表示该边关闭；
	// 一旦配置，CRM 要求平台 client_credentials 令牌并校验 scope/时间戳/nonce，
	// 因此令牌端点与客户端凭据缺一即失败关闭，避免静态令牌或无令牌请求
	// 被 401 反复拒绝直至死信。
	if c.CRMLinkEnabled {
		if !validCRMReferenceBaseURL(c.CRMLinkBaseURL) {
			return fmt.Errorf("CRM_LINK_BASE_URL must be an HTTP(S) origin with optional /customer-opportunity prefix, without credentials, query or fragment")
		}
		for _, item := range []struct{ name, value string }{
			{"CRM_LINK_TOKEN_URL", c.CRMLinkTokenURL},
			{"CRM_LINK_CLIENT_ID", c.CRMLinkClientID},
			{"CRM_LINK_CLIENT_SECRET", c.CRMLinkClientSecret},
		} {
			if strings.TrimSpace(item.value) == "" || placeholder(item.value) {
				return fmt.Errorf("%s is required when CRM_LINK_BASE_URL is set", item.name)
			}
		}
		tokenURL, tokenErr := url.ParseRequestURI(c.CRMLinkTokenURL)
		if tokenErr != nil || (tokenURL.Scheme != "http" && tokenURL.Scheme != "https") || tokenURL.Host == "" || tokenURL.User != nil || tokenURL.RawQuery != "" || tokenURL.Fragment != "" {
			return fmt.Errorf("CRM_LINK_TOKEN_URL must be a valid HTTP(S) URL without credentials, query or fragment")
		}
		if strings.TrimSpace(c.CRMLinkScope) != "opportunity.signed.write" {
			return fmt.Errorf("CRM_LINK_SCOPE must be opportunity.signed.write")
		}
	}
	if c.ProjectApprovalMachineEnabled {
		if !c.ProjectApprovalMachineRequireBearer {
			return fmt.Errorf("PROJECT_APPROVAL_MACHINE_REQUIRE_BEARER must be true when project approval integration is enabled")
		}
		for name, value := range map[string]string{
			"PROJECT_APPROVAL_MACHINE_CLIENT_ID":               c.ProjectApprovalMachineClientID,
			"PROJECT_APPROVAL_MACHINE_AUDIENCE":                c.ProjectApprovalMachineAudience,
			"PROJECT_APPROVAL_MACHINE_ISSUER":                  c.ProjectApprovalMachineIssuer,
			"PROJECT_APPROVAL_MACHINE_PUBLIC_KEY_PATH":         c.ProjectApprovalMachinePublicKeyPath,
			"PROJECT_APPROVAL_MACHINE_CALLER_APPLICATION_CODE": c.ProjectApprovalMachineCallerApp,
			"PROJECT_APPROVAL_MACHINE_CALLER_ENVIRONMENT_CODE": c.ProjectApprovalMachineCallerEnv,
			"PROJECT_APPROVAL_MACHINE_REQUIRED_SCOPE":          c.ProjectApprovalMachineScope,
		} {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("%s is required when project approval integration is enabled", name)
			}
		}
	}
	if c.CRMSignedCountMachineEnabled {
		if !c.CRMSignedCountMachineRequireBearer {
			return fmt.Errorf("CRM_SIGNED_COUNT_MACHINE_REQUIRE_BEARER must be true when CRM signed count integration is enabled")
		}
		for name, value := range map[string]string{
			"CRM_SIGNED_COUNT_MACHINE_CLIENT_ID":               c.CRMSignedCountMachineClientID,
			"CRM_SIGNED_COUNT_MACHINE_AUDIENCE":                c.CRMSignedCountMachineAudience,
			"CRM_SIGNED_COUNT_MACHINE_ISSUER":                  c.CRMSignedCountMachineIssuer,
			"CRM_SIGNED_COUNT_MACHINE_PUBLIC_KEY_PATH":         c.CRMSignedCountMachinePublicKeyPath,
			"CRM_SIGNED_COUNT_MACHINE_CALLER_APPLICATION_CODE": c.CRMSignedCountMachineCallerApp,
			"CRM_SIGNED_COUNT_MACHINE_CALLER_ENVIRONMENT_CODE": c.CRMSignedCountMachineCallerEnv,
			"CRM_SIGNED_COUNT_MACHINE_REQUIRED_SCOPE":          c.CRMSignedCountMachineScope,
		} {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("%s is required when CRM signed count integration is enabled", name)
			}
		}
		if c.CRMSignedCountMachineScope != "contract.opportunity_signed_count.read" {
			return fmt.Errorf("CRM_SIGNED_COUNT_MACHINE_REQUIRED_SCOPE must be contract.opportunity_signed_count.read")
		}
	}
	if c.CRMOpportunityIntakeMachineEnabled {
		if !c.CRMOpportunityIntakeMachineRequireBearer {
			return fmt.Errorf("CRM_OPPORTUNITY_INTAKE_MACHINE_REQUIRE_BEARER must be true when CRM opportunity intake is enabled")
		}
		for name, value := range map[string]string{
			"CRM_OPPORTUNITY_INTAKE_MACHINE_CLIENT_ID":               c.CRMOpportunityIntakeMachineClientID,
			"CRM_OPPORTUNITY_INTAKE_MACHINE_AUDIENCE":                c.CRMOpportunityIntakeMachineAudience,
			"CRM_OPPORTUNITY_INTAKE_MACHINE_ISSUER":                  c.CRMOpportunityIntakeMachineIssuer,
			"CRM_OPPORTUNITY_INTAKE_MACHINE_PUBLIC_KEY_PATH":         c.CRMOpportunityIntakeMachinePublicKeyPath,
			"CRM_OPPORTUNITY_INTAKE_MACHINE_CALLER_APPLICATION_CODE": c.CRMOpportunityIntakeMachineCallerApp,
			"CRM_OPPORTUNITY_INTAKE_MACHINE_CALLER_ENVIRONMENT_CODE": c.CRMOpportunityIntakeMachineCallerEnv,
			"CRM_OPPORTUNITY_INTAKE_MACHINE_REQUIRED_SCOPE":          c.CRMOpportunityIntakeMachineScope,
		} {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("%s is required when CRM opportunity intake is enabled", name)
			}
		}
		if c.CRMOpportunityIntakeMachineScope != "opportunity.signed.write" {
			return fmt.Errorf("CRM_OPPORTUNITY_INTAKE_MACHINE_REQUIRED_SCOPE must be opportunity.signed.write")
		}
	}
	// 合同摘要只暴露当前租户的“客户归属合同”最小投影；启用后必须完整绑定机器身份，
	// 且 scope/caller 固定，不允许部署侧改成更宽用途。
	if c.ContractSummaryMachineEnabled {
		if !c.ContractSummaryMachineRequireBearer {
			return fmt.Errorf("CONTRACT_SUMMARY_MACHINE_REQUIRE_BEARER must be true when contract summary integration is enabled")
		}
		for _, item := range []struct{ name, value string }{
			{"CONTRACT_SUMMARY_MACHINE_CLIENT_ID", c.ContractSummaryMachineClientID},
			{"CONTRACT_SUMMARY_MACHINE_AUDIENCE", c.ContractSummaryMachineAudience},
			{"CONTRACT_SUMMARY_MACHINE_ISSUER", c.ContractSummaryMachineIssuer},
			{"CONTRACT_SUMMARY_MACHINE_PUBLIC_KEY_PATH", c.ContractSummaryMachinePublicKeyPath},
			{"CONTRACT_SUMMARY_MACHINE_CALLER_APPLICATION_CODE", c.ContractSummaryMachineCallerApp},
			{"CONTRACT_SUMMARY_MACHINE_CALLER_ENVIRONMENT_CODE", c.ContractSummaryMachineCallerEnv},
			{"CONTRACT_SUMMARY_MACHINE_REQUIRED_SCOPE", c.ContractSummaryMachineScope},
		} {
			if strings.TrimSpace(item.value) == "" || placeholder(item.value) {
				return fmt.Errorf("%s is required when contract summary integration is enabled", item.name)
			}
		}
		if c.ContractSummaryMachineScope != "contract.summary.read" {
			return fmt.Errorf("CONTRACT_SUMMARY_MACHINE_REQUIRED_SCOPE must be contract.summary.read")
		}
		if c.ContractSummaryMachineCallerApp != "customer_and_opportunity" {
			return fmt.Errorf("CONTRACT_SUMMARY_MACHINE_CALLER_APPLICATION_CODE must be customer_and_opportunity")
		}
	}
	platformURL, err := url.ParseRequestURI(c.PlatformBaseURL)
	if err != nil || (platformURL.Scheme != "http" && platformURL.Scheme != "https") ||
		platformURL.Host == "" || platformURL.User != nil || platformURL.RawQuery != "" ||
		platformURL.Fragment != "" || (platformURL.Path != "" && platformURL.Path != "/") {
		return fmt.Errorf("PLATFORM_BASE_URL must be an HTTP(S) origin without credentials, path, query or fragment")
	}
	issuerURL, err := url.ParseRequestURI(c.OIDCIssuer)
	if err != nil || (issuerURL.Scheme != "http" && issuerURL.Scheme != "https") || issuerURL.Host == "" ||
		issuerURL.User != nil || issuerURL.RawQuery != "" || issuerURL.Fragment != "" {
		return fmt.Errorf("OIDC_ISSUER must be a valid HTTP(S) URL")
	}
	if c.OIDCBackchannelBaseURL != "" {
		backchannelURL, parseErr := url.ParseRequestURI(c.OIDCBackchannelBaseURL)
		if parseErr != nil || (backchannelURL.Scheme != "http" && backchannelURL.Scheme != "https") ||
			backchannelURL.Host == "" || backchannelURL.User != nil || backchannelURL.RawQuery != "" ||
			backchannelURL.Fragment != "" || (backchannelURL.Path != "" && backchannelURL.Path != "/") {
			return fmt.Errorf("OIDC_BACKCHANNEL_BASE_URL must be an HTTP(S) origin")
		}
	}
	if strings.TrimSpace(c.OIDCClientID) == "" || strings.TrimSpace(c.OIDCClientSecret) == "" ||
		strings.TrimSpace(c.OIDCRedirectURI) == "" || strings.TrimSpace(c.OIDCTenantID) == "" {
		return fmt.Errorf("OIDC_CLIENT_ID, OIDC_CLIENT_SECRET, OIDC_REDIRECT_URI and OIDC_TENANT_ID are required")
	}
	if len(c.OIDCScopes) == 0 || !contains(c.OIDCScopes, "openid") {
		return fmt.Errorf("OIDC_SCOPES must include openid")
	}
	// 空闲期限必须显式可解析且在允许区间；绝对期限必须严格大于空闲期限，
	// 否则空闲机制无法生效。配置非法时启动失败关闭，不在运行期产生歧义语义。
	if c.OIDCSessionIdleTTL < time.Minute || c.OIDCSessionIdleTTL > 24*time.Hour {
		return fmt.Errorf("OIDC_SESSION_IDLE_TTL must be between 60s and 24h")
	}
	// 中央模式由平台裁定真实空闲与原认证截止，本地期限关系只约束旧模式。
	if !c.CentralSessionEnabled && c.OIDCSessionAbsoluteTTL <= c.OIDCSessionIdleTTL {
		return fmt.Errorf("OIDC_SESSION_ABSOLUTE_TTL must be greater than OIDC_SESSION_IDLE_TTL")
	}
	if strings.TrimSpace(c.OIDCSessionCookieName) == "" ||
		c.OIDCAuthorizationRefresh <= 0 || c.OIDCAuthorizationRefresh >= c.OIDCSessionAbsoluteTTL ||
		c.OIDCAuthorizationTimeout <= 0 || c.OIDCAuthorizationMaxStale < 0 || c.OIDCAuthorizationMaxStale >= c.OIDCSessionAbsoluteTTL {
		return fmt.Errorf("OIDC session cookie name and authorization refresh intervals shorter than the absolute session TTL are required")
	}
	if strings.TrimSpace(c.PlatformApplicationCode) == "" || strings.TrimSpace(c.PlatformEnvironmentCode) == "" {
		return fmt.Errorf("PLATFORM_APPLICATION_CODE and PLATFORM_ENVIRONMENT_CODE are required for OIDC authorization binding")
	}
	if c.AppPathPrefix == "/" || !strings.HasPrefix(c.AppPathPrefix, "/") ||
		(c.AppPathPrefix != "" && strings.HasSuffix(c.AppPathPrefix, "/")) {
		return fmt.Errorf("APP_PATH_PREFIX must be empty or a non-root absolute path without trailing slash")
	}
	if strings.TrimSpace(c.AppPublicURL) == "" {
		return fmt.Errorf("APP_PUBLIC_URL is required")
	}
	if strings.TrimSpace(c.TemporalAddress) == "" || strings.TrimSpace(c.TemporalNamespace) == "" || strings.TrimSpace(c.TemporalTaskQueue) == "" || strings.TrimSpace(c.TemporalWorkerBuildID) == "" {
		return fmt.Errorf("Temporal address, namespace and task queue must not be empty")
	}
	if c.TemporalWorkerVersioning && strings.TrimSpace(c.TemporalWorkerDeploymentName) == "" {
		return fmt.Errorf("TEMPORAL_WORKER_DEPLOYMENT_NAME is required when worker versioning is enabled")
	}
	if c.TemporalWorkerVersioningPolicy != "PINNED" && c.TemporalWorkerVersioningPolicy != "AUTO_UPGRADE" {
		return fmt.Errorf("TEMPORAL_WORKER_VERSIONING_POLICY must be PINNED or AUTO_UPGRADE")
	}
	if strings.TrimSpace(c.TemporalMetricsAddress) == "" {
		return fmt.Errorf("TEMPORAL_METRICS_ADDRESS is required")
	}
	if c.NodeTimeout <= 0 || c.ReminderInterval <= 0 || c.ReminderInterval >= c.NodeTimeout {
		return fmt.Errorf("approval durations must be positive and reminder interval must be shorter than node timeout")
	}
	hasAuditClientID := strings.TrimSpace(c.PlatformAuditClientID) != ""
	hasAuditClientSecret := strings.TrimSpace(c.PlatformAuditClientSecret) != ""
	if hasAuditClientID != hasAuditClientSecret {
		return fmt.Errorf("platform audit configuration must provide client ID and client secret together")
	}
	if hasAuditClientID && (strings.TrimSpace(c.PlatformApplicationCode) == "" || strings.TrimSpace(c.PlatformEnvironmentCode) == "") {
		return fmt.Errorf("platform audit configuration must provide client ID, client secret, application code and environment code together")
	}
	if c.PlatformCatalogSync && (strings.TrimSpace(c.PlatformApplicationID) == "" ||
		strings.TrimSpace(c.PlatformCatalogClientID) == "" || strings.TrimSpace(c.PlatformCatalogSecret) == "") {
		return fmt.Errorf("platform authorization catalog synchronization requires application ID, client ID and client secret")
	}
	if (strings.TrimSpace(c.PlatformPersonnelClientID) == "") != (strings.TrimSpace(c.PlatformPersonnelSecret) == "") {
		return fmt.Errorf("platform personnel directory configuration must provide client ID and secret together")
	}
	if (strings.TrimSpace(c.PlatformNotificationClientID) == "") != (strings.TrimSpace(c.PlatformNotificationSecret) == "") {
		return fmt.Errorf("platform notification configuration must provide client ID and secret together")
	}
	// 看板机器接口返回全租户合同数据，启用后必须校验调用方机器身份；租户边界必须由
	// 验签令牌提供，不能退化为“无 Bearer + 请求头租户”的组合。与 project 侧对齐。
	if c.DashboardMachineRequireBearer && !c.DashboardMachineEnabled {
		return fmt.Errorf("DASHBOARD_MACHINE_REQUIRE_BEARER requires DASHBOARD_MACHINE_ENABLED")
	}
	if c.DashboardMachineEnabled && !c.DashboardMachineRequireBearer {
		return fmt.Errorf("DASHBOARD_MACHINE_REQUIRE_BEARER must be true when DASHBOARD_MACHINE_ENABLED=true (internal dashboard source verification is mandatory)")
	}
	if c.DashboardMachineRequireBearer {
		// 使用有序校验，确保配置错误稳定地先指出客户端身份，再指出受众，
		// 避免 Go map 随机遍历导致测试和运维诊断结果不确定。
		for _, item := range []struct {
			name, value string
		}{
			{"DASHBOARD_MACHINE_CLIENT_ID", c.DashboardMachineClientID},
			{"DASHBOARD_MACHINE_AUDIENCE", c.DashboardMachineAudience},
			{"DASHBOARD_MACHINE_ISSUER", c.DashboardMachineIssuer},
			{"DASHBOARD_MACHINE_PUBLIC_KEY_PATH", c.DashboardMachinePublicKeyPath},
			{"DASHBOARD_MACHINE_CALLER_APPLICATION_CODE", c.DashboardMachineCallerApp},
			{"DASHBOARD_MACHINE_CALLER_ENVIRONMENT_CODE", c.DashboardMachineCallerEnv},
			{"DASHBOARD_MACHINE_REQUIRED_SCOPE", c.DashboardMachineScope},
		} {
			if strings.TrimSpace(item.value) == "" || placeholder(item.value) {
				return fmt.Errorf("%s is required when dashboard bearer authentication is enabled", item.name)
			}
		}
	}
	if c.SettlementMachineRequireBearer && !c.SettlementMachineEnabled {
		return fmt.Errorf("SETTLEMENT_MACHINE_REQUIRE_BEARER requires SETTLEMENT_MACHINE_ENABLED")
	}
	if c.HandoverMachineRequireBearer && !c.HandoverMachineEnabled {
		return fmt.Errorf("HANDOVER_MACHINE_REQUIRE_BEARER requires HANDOVER_MACHINE_ENABLED")
	}
	// 交接协议只服务平台编排器：启用后必须用完整信任域验签，否则端点只能是 503，
	// 不能退化为“无 Bearer + 请求头租户”。与看板/结算集成同一失败关闭口径。
	if c.HandoverMachineEnabled && !c.HandoverMachineRequireBearer {
		return fmt.Errorf("HANDOVER_MACHINE_REQUIRE_BEARER must be true when HANDOVER_MACHINE_ENABLED=true (internal handover source verification is mandatory)")
	}
	if c.HandoverMachineRequireBearer {
		for _, item := range []struct {
			name, value string
		}{
			{"HANDOVER_MACHINE_CLIENT_ID", c.HandoverMachineClientID},
			{"HANDOVER_MACHINE_AUDIENCE", c.HandoverMachineAudience},
			{"HANDOVER_MACHINE_ISSUER", c.HandoverMachineIssuer},
			{"HANDOVER_MACHINE_PUBLIC_KEY_PATH", c.HandoverMachinePublicKeyPath},
			{"HANDOVER_MACHINE_CALLER_APPLICATION_CODE", c.HandoverMachineCallerApp},
			{"HANDOVER_MACHINE_CALLER_ENVIRONMENT_CODE", c.HandoverMachineCallerEnv},
			{"HANDOVER_MACHINE_REQUIRED_SCOPE", c.HandoverMachineScope},
		} {
			if strings.TrimSpace(item.value) == "" || placeholder(item.value) {
				return fmt.Errorf("%s is required when handover machine bearer authentication is enabled", item.name)
			}
		}
	}
	if c.SettlementMachineEnabled && !c.SettlementMachineRequireBearer {
		return fmt.Errorf("SETTLEMENT_MACHINE_REQUIRE_BEARER must be true when SETTLEMENT_MACHINE_ENABLED=true")
	}
	if c.SettlementMachineRequireBearer {
		for _, item := range []struct {
			name, value string
		}{
			{"SETTLEMENT_MACHINE_CLIENT_ID", c.SettlementMachineClientID},
			{"SETTLEMENT_MACHINE_AUDIENCE", c.SettlementMachineAudience},
			// 结算读取接口必须绑定唯一机器用途：缺失范围会让端点退回只校验
			// client_id/audience 的弱断言，因此在启动期失败关闭。
			{"SETTLEMENT_MACHINE_REQUIRED_SCOPE", c.SettlementMachineRequiredScope},
		} {
			if strings.TrimSpace(item.value) == "" || placeholder(item.value) {
				return fmt.Errorf("%s is required when settlement bearer authentication is enabled", item.name)
			}
		}
	}
	return nil
}

// envOr 返回环境变量值，缺失时使用回退值。
func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// commaList 按逗号拆分并去除空白项。
func commaList(value string) []string {
	items := make([]string, 0)
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			items = append(items, trimmed)
		}
	}
	return items
}

func placeholder(value string) bool {
	upper := strings.ToUpper(strings.TrimSpace(value))
	return strings.Contains(upper, "PENDING") || strings.Contains(upper, "CHANGEME") || strings.Contains(upper, "EXAMPLE.COM")
}

func fields(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool { return r == ' ' || r == ',' })
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

// CRM 将引用 API 挂载在此前缀下；对于网关会移除该前缀的部署，
// 保留仅根路径的兼容，但不接受任意路径。
func validCRMReferenceBaseURL(value string) bool {
	parsed, err := url.ParseRequestURI(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && !parsed.ForceQuery && parsed.Fragment == "" && parsed.RawPath == "" && (parsed.Path == "" || parsed.Path == "/" || parsed.Path == "/customer-opportunity" || parsed.Path == "/customer-opportunity/")
}

func validHTTPOrigin(value string) bool {
	parsed, err := url.ParseRequestURI(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && (parsed.Path == "" || parsed.Path == "/")
}

func envBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// cronEnv 读取“可禁用的周期任务”cron 表达式：未设置（含未出现在环境里）时取默认值，
// 显式设置为空串或纯空白时返回空串，表示禁用该周期任务，由调用方跳过注册。
//
// 与 env 的差别只在空值语义：env 把空串当“未配置”回落默认值，归档 cron 因此无法用
// 空串关闭；合同编号巡检需要“默认每天跑、必要时一键关闭”的运维语义，故单独提供本函数。
func cronEnv(key, fallback string) string {
	value, exists := os.LookupEnv(key)
	if !exists {
		return fallback
	}
	return strings.TrimSpace(value)
}

func duration(key string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	return parseDurationValue(key, value, fallback)
}

// parseDurationValue 解析已经取值完成的时长字符串，空值回落默认值。
// 用于需要兼容旧环境变量键的配置项（如 OIDC_SESSION_ABSOLUTE_TTL / OIDC_SESSION_TTL），
// 参数：key 用于错误定位，raw 为已按优先级选出的原值，fallback 为未配置时的默认值。
// 返回值：解析后的时长；格式非法返回带键名的错误。
func parseDurationValue(key, raw string, fallback time.Duration) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return parsed, nil
}
