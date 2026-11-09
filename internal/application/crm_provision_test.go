package application

import (
	"context"
	"errors"
	"testing"
)

// stubProvisionCustomerClient 是客户链路的假客户端，只用于装配测试，不访问网络。
type stubProvisionCustomerClient struct{}

// LookupCustomer 返回空候选，表示客户不存在。
// 参数：ctx 为上下文，actor/idempotencyKey/name 为契约字段。
// 返回值：空候选与 nil 错误。
func (stubProvisionCustomerClient) LookupCustomer(context.Context, string, string, string) ([]CRMProvisionCustomerCandidate, error) {
	return nil, nil
}

// CreateCustomer 返回固定客户投影。
// 参数：ctx 为上下文，actor/idempotencyKey 为契约字段，input 为客户输入。
// 返回值：固定客户与 nil 错误。
func (stubProvisionCustomerClient) CreateCustomer(context.Context, string, string, CRMProvisionCustomerInput) (CRMProvisionCustomer, error) {
	return CRMProvisionCustomer{ID: 1, Name: "客户A", Status: "ACTIVE"}, nil
}

// stubProvisionOpportunityClient 是商机链路的假客户端。
type stubProvisionOpportunityClient struct{}

// CreateOpportunity 返回固定商机投影。
// 参数：ctx 为上下文，actor/idempotencyKey 为契约字段，input 为商机输入。
// 返回值：固定商机与 nil 错误。
func (stubProvisionOpportunityClient) CreateOpportunity(context.Context, string, string, CRMProvisionOpportunityInput) (CRMProvisionOpportunity, error) {
	return CRMProvisionOpportunity{ID: 2, CustomerID: 1, Status: "FOLLOWING"}, nil
}

// SignOpportunity 返回固定已签约商机。
// 参数：ctx 为上下文，actor/idempotencyKey 为契约字段，opportunityID/contractRef 为推进目标。
// 返回值：固定商机与 nil 错误。
func (stubProvisionOpportunityClient) SignOpportunity(context.Context, string, string, uint64, string, uint64) (CRMProvisionOpportunity, error) {
	return CRMProvisionOpportunity{ID: 2, CustomerID: 1, Status: "SIGNED", Stage: "CLOSED"}, nil
}

// stubProvisionOwnerDirectory 是负责人目录的假实现。
type stubProvisionOwnerDirectory struct{}

// ResolveOwnerByName 返回固定身份。
// 参数：ctx 为上下文，displayName 为销售姓名。
// 返回值：固定候选与 nil 错误。
func (stubProvisionOwnerDirectory) ResolveOwnerByName(context.Context, string) (OwnerDirectoryCandidate, error) {
	return OwnerDirectoryCandidate{IdentityID: "identity-1", DisplayName: "张三"}, nil
}

// completeProvisionClients 返回三条依赖齐全的注入集合。
// 参数：无。
// 返回值：假客户端集合。
func completeProvisionClients() CRMProvisionClients {
	return CRMProvisionClients{
		Customer:    stubProvisionCustomerClient{},
		Opportunity: stubProvisionOpportunityClient{},
		Owners:      stubProvisionOwnerDirectory{},
	}
}

// TestConfigureCRMProvisionKeepsInjectionPointsEmptyWhenDisabled 验证开关关闭时
// Service 装配成功且三个注入点为 nil（含清掉残留注入）。
// 参数：t 为测试上下文。
// 返回值：无；关闭态报错或残留非 nil 字段时失败。
func TestConfigureCRMProvisionKeepsInjectionPointsEmptyWhenDisabled(t *testing.T) {
	service := &Service{}
	if err := service.ConfigureCRMProvision(false, CRMProvisionClients{}); err != nil {
		t.Fatalf("disabled configuration must not fail: %v", err)
	}
	if service.CRMProvisionCustomerClient != nil || service.CRMProvisionOpportunityClient != nil ||
		service.CRMProvisionOwnerDirectory != nil {
		t.Fatalf("disabled configuration injected clients: %+v", service)
	}
	// 关闭态也必须清掉残留注入，避免上一轮装配在半关闭配置下继续生效。
	service.CRMProvisionCustomerClient = stubProvisionCustomerClient{}
	service.CRMProvisionOpportunityClient = stubProvisionOpportunityClient{}
	service.CRMProvisionOwnerDirectory = stubProvisionOwnerDirectory{}
	if err := service.ConfigureCRMProvision(false, completeProvisionClients()); err != nil {
		t.Fatalf("disabled configuration must not fail: %v", err)
	}
	if service.CRMProvisionCustomerClient != nil || service.CRMProvisionOpportunityClient != nil ||
		service.CRMProvisionOwnerDirectory != nil {
		t.Fatalf("disabled configuration kept stale clients: %+v", service)
	}
}

// TestConfigureCRMProvisionRequiresAllClientsWhenEnabled 验证开启后任一依赖缺失即失败关闭，
// 且不写入部分注入。
// 参数：t 为测试上下文。
// 返回值：无；残缺配置被接受或写入部分字段时失败。
func TestConfigureCRMProvisionRequiresAllClientsWhenEnabled(t *testing.T) {
	complete := completeProvisionClients()
	for _, testCase := range []struct {
		name    string
		clients CRMProvisionClients
	}{
		{name: "missing-customer", clients: CRMProvisionClients{Opportunity: complete.Opportunity, Owners: complete.Owners}},
		{name: "missing-opportunity", clients: CRMProvisionClients{Customer: complete.Customer, Owners: complete.Owners}},
		{name: "missing-owners", clients: CRMProvisionClients{Customer: complete.Customer, Opportunity: complete.Opportunity}},
		{name: "missing-all", clients: CRMProvisionClients{}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service := &Service{}
			err := service.ConfigureCRMProvision(true, testCase.clients)
			if !errors.Is(err, ErrCRMProvisionNotConfigured) {
				t.Fatalf("enabled partial configuration error=%v", err)
			}
			if service.CRMProvisionCustomerClient != nil || service.CRMProvisionOpportunityClient != nil ||
				service.CRMProvisionOwnerDirectory != nil {
				t.Fatalf("partial configuration was injected: %+v", service)
			}
		})
	}
}

// TestConfigureCRMProvisionInjectsClientsWhenEnabled 验证开启且配置齐全时三个字段非 nil。
// 参数：t 为测试上下文。
// 返回值：无；注入缺失或类型不符时失败。
func TestConfigureCRMProvisionInjectsClientsWhenEnabled(t *testing.T) {
	service := &Service{}
	if err := service.ConfigureCRMProvision(true, completeProvisionClients()); err != nil {
		t.Fatalf("complete configuration rejected: %v", err)
	}
	if service.CRMProvisionCustomerClient == nil || service.CRMProvisionOpportunityClient == nil ||
		service.CRMProvisionOwnerDirectory == nil {
		t.Fatalf("complete configuration not injected: %+v", service)
	}
	if _, err := service.CRMProvisionCustomerClient.CreateCustomer(context.Background(), "identity-1", "key-1",
		CRMProvisionCustomerInput{Name: "客户A", OwnerIdentityID: "identity-1"}); err != nil {
		t.Fatalf("injected customer client unusable: %v", err)
	}
	if _, err := service.CRMProvisionOpportunityClient.SignOpportunity(context.Background(), "identity-1", "key-1", 2, "CM-1", 1); err != nil {
		t.Fatalf("injected opportunity client unusable: %v", err)
	}
	if _, err := service.CRMProvisionOwnerDirectory.ResolveOwnerByName(context.Background(), "张三"); err != nil {
		t.Fatalf("injected owner directory unusable: %v", err)
	}
}
