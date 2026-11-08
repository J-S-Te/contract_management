package application

import (
	"context"
	"errors"
)

// CRMProvisionCustomerCandidate 是开立前客户查重的候选，status 供调用方识别已停用客户。
type CRMProvisionCustomerCandidate struct {
	ID              uint64 `json:"id"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	OwnerIdentityID string `json:"owner_identity_id,omitempty"`
}

// CRMProvisionCustomerInput 是创建客户的白名单输入；类型/行业/地区允许为空，
// 由 CRM 侧按 IMPORT_PROVISION 通道补默认值。
type CRMProvisionCustomerInput struct {
	Name            string                        `json:"name"`
	CustomerType    string                        `json:"customer_type"`
	Industry        string                        `json:"industry"`
	Region          string                        `json:"region"`
	OwnerIdentityID string                        `json:"owner_identity_id"`
	Contacts        []CRMProvisionCustomerContact `json:"contacts"`
}

// CRMProvisionCustomerContact 是清单中真实客户登记联系人。
type CRMProvisionCustomerContact struct {
	Name           string `json:"name"`
	Phone          string `json:"phone"`
	IsRegistration bool   `json:"is_registration"`
}

// CRMProvisionCustomer 是创建客户的结果投影。
type CRMProvisionCustomer struct {
	ID     uint64 `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Origin string `json:"origin,omitempty"`
}

// CRMProvisionOpportunityInput 是创建商机的白名单输入；Amount 为十进制字符串，
// ExpectedSignDate 为 YYYY-MM-DD，历史合同允许过去日期（仅本通道放开）。
type CRMProvisionOpportunityInput struct {
	CustomerID         uint64 `json:"customer_id"`
	OwnerIdentityID    string `json:"owner_identity_id"`
	Name               string `json:"name"`
	Amount             string `json:"expected_amount"`
	RequirementSummary string `json:"requirement_summary"`
	ExpectedSignDate   string `json:"expected_sign_date"`
}

// CRMProvisionOpportunity 是创建/推进商机的结果投影。
type CRMProvisionOpportunity struct {
	ID         uint64 `json:"id"`
	Name       string `json:"name"`
	CustomerID uint64 `json:"customer_id"`
	Status     string `json:"opp_status"`
	Stage      string `json:"current_stage,omitempty"`
	Version    uint64 `json:"version"`
}

// OwnerDirectoryCandidate 是平台负责人目录返回的最小人员候选。
type OwnerDirectoryCandidate struct {
	IdentityID   string `json:"identity_id"`
	DisplayName  string `json:"display_name"`
	EmployeeNo   string `json:"employee_no"`
	PrimaryOrgID string `json:"primary_org_id"`
}

// CRMProvisionCustomerClient 是客户链路（customer.import_provision 专用凭据）的注入点。
// 查重与创建分开暴露：0 命中才创建，多命中必须阻断该行。
type CRMProvisionCustomerClient interface {
	LookupCustomer(context.Context, string, string, string) ([]CRMProvisionCustomerCandidate, error)
	CreateCustomer(context.Context, string, string, CRMProvisionCustomerInput) (CRMProvisionCustomer, error)
}

// CRMProvisionOpportunityClient 是商机链路（opportunity.import_provision 专用凭据）的注入点。
// 先创建再推进签约，两步都以幂等键重放。
type CRMProvisionOpportunityClient interface {
	CreateOpportunity(context.Context, string, string, CRMProvisionOpportunityInput) (CRMProvisionOpportunity, error)
	// SignOpportunity 按实际CRM版本幂等签约，并保留合同归属验真。
	// 参数：上下文、操作者身份、幂等键、商机ID、合同引用及CRM返回的预期版本。
	// 返回值：签约后的商机；版本、验真或依赖失败时返回错误。
	SignOpportunity(context.Context, string, string, uint64, string, uint64) (CRMProvisionOpportunity, error)
}

// CRMProvisionOwnerDirectory 把 Excel 中的销售姓名解析成平台唯一身份；
// 0 命中与多命中由实现区分，调用方按行阻断。
type CRMProvisionOwnerDirectory interface {
	ResolveOwnerByName(context.Context, string) (OwnerDirectoryCandidate, error)
}

// ErrCRMProvisionNotConfigured 表示开关已开启但注入点不全（客户/商机/目录任一缺失）。
var ErrCRMProvisionNotConfigured = errors.New("CRM import provision integration is not fully configured")

// CRMProvisionClients 汇总历史导入开立所需的三条依赖；零值表示未装配。
type CRMProvisionClients struct {
	Customer    CRMProvisionCustomerClient
	Opportunity CRMProvisionOpportunityClient
	Owners      CRMProvisionOwnerDirectory
}

// ConfigureCRMProvision 按开关装配历史导入开立 CRM 的注入点。
// 参数：enabled 为 CONTRACT_IMPORT_CRM_PROVISION_ENABLED，clients 为已按专用凭据构造的依赖。
// 返回值：关闭时清空三个字段并返回 nil；开启时要求三者齐备，否则失败关闭且不写入任何字段。
func (s *Service) ConfigureCRMProvision(enabled bool, clients CRMProvisionClients) error {
	s.CRMProvisionCustomerClient, s.CRMProvisionOpportunityClient, s.CRMProvisionOwnerDirectory = nil, nil, nil
	if !enabled {
		return nil
	}
	if clients.Customer == nil || clients.Opportunity == nil || clients.Owners == nil {
		return ErrCRMProvisionNotConfigured
	}
	s.CRMProvisionCustomerClient = clients.Customer
	s.CRMProvisionOpportunityClient = clients.Opportunity
	s.CRMProvisionOwnerDirectory = clients.Owners
	return nil
}
