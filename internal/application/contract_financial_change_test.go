package application

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// financialChangeRepository 是可观察的受控财务变更仓储桩。
// 只实现用例真正调用的方法：GetContract 与 ApplyContractFinancialChange。
type financialChangeRepository struct {
	Repository
	contract contract.Contract
	applied  []ContractFinancialChange
	tenant   string
	actor    string
	result   contract.Contract
}

func (r *financialChangeRepository) GetContract(context.Context, string, string) (contract.Contract, error) {
	return r.contract, nil
}

func (r *financialChangeRepository) ApplyContractFinancialChange(_ context.Context, tenantID string, change ContractFinancialChange, actorUserID string) (contract.Contract, error) {
	r.tenant, r.actor = tenantID, actorUserID
	r.applied = append(r.applied, change)
	return r.result, nil
}

func financialChangeFixture() (*Service, *financialChangeRepository, Principal) {
	current := contract.Contract{
		ID: "C1", TenantID: "tenant", Number: "HT-1", Status: contract.StatusActive, Version: 7,
		AmountMinor: 10000, Currency: "CNY", OwnerIdentityID: "identity",
		PaymentTerms: []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2026-12-01", Amount: "100.00"}},
	}
	repo := &financialChangeRepository{contract: current}
	repo.result = current
	actor := Principal{TenantID: "tenant", UserID: "actor", IdentityID: "identity", Roles: []string{"admin"}, Permissions: map[string]bool{"contract.edit": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.edit": {AllowAll: true}}}
	return &Service{Repo: repo, ContractFinancialChangeEnabled: true}, repo, actor
}

func validFinancialChange() ContractFinancialChange {
	return ContractFinancialChange{
		ContractID: "C1", ExpectedVersion: 7, AmountMinor: 20000, Currency: "CNY",
		PaymentTerms: []contract.PaymentTerm{
			{InstallmentNo: 1, DueDate: "2026-12-15", Amount: "80.00"},
			{InstallmentNo: 2, DueDate: "2027-01-15", Amount: "120.00"},
		},
		Reason: "客户追加范围，经财务复核",
	}
}

// TestFinancialChangeSwitchClosedRejectsEveryRequest 验证入口默认关闭时不读取合同、
// 不写任何数据：开关是受控通道的第一道失败关闭门禁。
func TestFinancialChangeSwitchClosedRejectsEveryRequest(t *testing.T) {
	service, repo, actor := financialChangeFixture()
	service.ContractFinancialChangeEnabled = false
	if _, err := service.ChangeContractFinancialTerms(context.Background(), actor, "C1", validFinancialChange()); !errors.Is(err, ErrFinancialChangeDisabled) {
		t.Fatalf("开关关闭未拒绝：%v", err)
	}
	if len(repo.applied) != 0 {
		t.Fatal("开关关闭时不得发生任何写入")
	}
}

// TestFinancialChangeRequiresPermissionAndApproval 验证权限、数据范围与审批前置
// 是三条独立门禁：任一缺失都必须拒绝且不写数据。
func TestFinancialChangeRequiresPermissionAndApproval(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Service, *financialChangeRepository, *Principal)
		want   error
	}{
		{"no permission", func(s *Service, r *financialChangeRepository, a *Principal) { a.Permissions = nil }, ErrForbidden},
		{"no permission scope", func(s *Service, r *financialChangeRepository, a *Principal) { a.PermissionScopes = nil }, ErrForbidden},
		{"no actor", func(s *Service, r *financialChangeRepository, a *Principal) { a.UserID = "" }, ErrForbidden},
		{"draft not approved", func(s *Service, r *financialChangeRepository, a *Principal) { r.contract.Status = contract.StatusDraft }, apperrors.ErrStateConflict},
		{"pending approval", func(s *Service, r *financialChangeRepository, a *Principal) {
			r.contract.Status = contract.StatusPending
		}, apperrors.ErrStateConflict},
		{"terminal completed", func(s *Service, r *financialChangeRepository, a *Principal) {
			r.contract.Status = contract.StatusCompleted
		}, apperrors.ErrStateConflict},
		{"terminal archived", func(s *Service, r *financialChangeRepository, a *Principal) {
			r.contract.Status = contract.StatusArchived
		}, apperrors.ErrStateConflict},
		{"imported baseline", func(s *Service, r *financialChangeRepository, a *Principal) { r.contract.OriginKind = "IMPORTED" }, ErrImportCorrectionRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, repo, actor := financialChangeFixture()
			tc.mutate(service, repo, &actor)
			if _, err := service.ChangeContractFinancialTerms(context.Background(), actor, "C1", validFinancialChange()); !errors.Is(err, tc.want) {
				t.Fatalf("错误=%v want=%v", err, tc.want)
			}
			if len(repo.applied) != 0 {
				t.Fatal("门禁拒绝后不得写数据")
			}
		})
	}
}

// TestFinancialChangeValidatesVersionCurrencyAndSchedule 验证并发版本、
// 币种锁定与付款计划校验都在进入仓储事务前失败关闭。
func TestFinancialChangeValidatesVersionCurrencyAndSchedule(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ContractFinancialChange)
		want   error
	}{
		{"missing version", func(c *ContractFinancialChange) { c.ExpectedVersion = 0 }, ErrValidation},
		{"stale version", func(c *ContractFinancialChange) { c.ExpectedVersion = 6 }, apperrors.ErrVersionConflict},
		{"currency change", func(c *ContractFinancialChange) { c.Currency = "USD" }, ErrFinancialChangeCurrencyLocked},
		{"zero amount", func(c *ContractFinancialChange) { c.AmountMinor = 0 }, ErrValidation},
		{"missing reason", func(c *ContractFinancialChange) { c.Reason = "  " }, ErrValidation},
		{"missing schedule", func(c *ContractFinancialChange) { c.PaymentTerms = nil }, ErrValidation},
		{"schedule total mismatch", func(c *ContractFinancialChange) {
			c.PaymentTerms = []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2026-12-15", Amount: "199.99"}}
		}, ErrValidation},
		{"duplicate installment", func(c *ContractFinancialChange) {
			c.PaymentTerms = []contract.PaymentTerm{
				{InstallmentNo: 1, DueDate: "2026-12-15", Amount: "80.00"},
				{InstallmentNo: 1, DueDate: "2027-01-15", Amount: "120.00"},
			}
		}, ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, repo, actor := financialChangeFixture()
			change := validFinancialChange()
			tc.mutate(&change)
			if _, err := service.ChangeContractFinancialTerms(context.Background(), actor, "C1", change); !errors.Is(err, tc.want) {
				t.Fatalf("错误=%v want=%v", err, tc.want)
			}
			if len(repo.applied) != 0 {
				t.Fatal("校验失败后不得写数据")
			}
		})
	}
}

// TestFinancialChangeAppliesVersionedChange 验证通过门禁后交给仓储的变更
// 使用可信租户/操作者、归一化后的计划与自增版本语义。
func TestFinancialChangeAppliesVersionedChange(t *testing.T) {
	service, repo, actor := financialChangeFixture()
	updated := repo.contract
	updated.AmountMinor, updated.Version = 20000, 8
	updated.PaymentTerms = []contract.PaymentTerm{
		{InstallmentNo: 1, DueDate: "2026-12-15", Amount: "80.00"},
		{InstallmentNo: 2, DueDate: "2027-01-15", Amount: "120.00"},
	}
	repo.result = updated
	change := validFinancialChange()
	got, err := service.ChangeContractFinancialTerms(context.Background(), actor, "C1", change)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 8 || got.AmountMinor != 20000 || len(repo.applied) != 1 {
		t.Fatalf("变更结果异常：%+v applied=%d", got, len(repo.applied))
	}
	applied := repo.applied[0]
	if repo.tenant != "tenant" || repo.actor != "actor" || applied.ContractID != "C1" || applied.ExpectedVersion != 7 {
		t.Fatalf("可信身份或版本未传递：tenant=%q actor=%q change=%+v", repo.tenant, repo.actor, applied)
	}
	if applied.AmountMinor != 20000 || applied.Currency != "CNY" || len(applied.PaymentTerms) != 2 || applied.Reason == "" {
		t.Fatalf("变更内容未归一化：%+v", applied)
	}
}

// TestFinancialChangeFailsClosedWhenRepositoryCannotQueueEvent 验证仓储不支持
// 受控变更（无法在同事务内写事件/审计）时失败关闭，不退化为只改合同。
func TestFinancialChangeFailsClosedWhenRepositoryCannotQueueEvent(t *testing.T) {
	current := contract.Contract{ID: "C1", TenantID: "tenant", Status: contract.StatusActive, Version: 3, AmountMinor: 10000, Currency: "CNY", OwnerIdentityID: "identity"}
	actor := Principal{TenantID: "tenant", UserID: "actor", IdentityID: "identity", Permissions: map[string]bool{"contract.edit": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.edit": {AllowAll: true}}}
	service := &Service{Repo: &recordingRepository{contract: current}, ContractFinancialChangeEnabled: true}
	change := validFinancialChange()
	change.ExpectedVersion = 3
	if _, err := service.ChangeContractFinancialTerms(context.Background(), actor, "C1", change); !errors.Is(err, apperrors.ErrStateConflict) {
		t.Fatalf("不支持的仓储必须失败关闭：%v", err)
	}
}
