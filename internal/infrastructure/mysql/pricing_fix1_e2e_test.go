package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/stretchr/testify/mock"
	temporalmocks "go.temporal.io/sdk/mocks"
)

type fix1PricingPersonnel struct{}

// ListEligibleUsers 为真实受理服务提供明确独立审批人目录替身。
// 参数：ctx/actor为可信边界，roles为所需职责。
// 返回值：隔离测试审批人，不替代真实平台目录验收。
func (fix1PricingPersonnel) ListEligibleUsers(context.Context, application.Principal, []string) ([]application.UserReference, error) {
	return []application.UserReference{{UserID: "independent", Roles: []string{"admin"}}}, nil
}

// TestFix1FirstPricingRevisionPersistsNewServiceIdentityE2E 覆盖无价无服务身份NORMAL合同首次基准真实事务链。
// 参数：t为测试上下文，仅复用严格隔离CONTRACT_TEST_DSN。
// 返回值：无；受理、独立终决、回读及机器事实必须一致。
func TestFix1FirstPricingRevisionPersistsNewServiceIdentityE2E(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	t.Cleanup(func() {
		for _, model := range []any{&standardPriceRecord{}, &pricingRevisionRecord{}, &approvalSnapshotRecord{}} {
			if err := r.db.Where("tenant_id=?", tenant).Delete(model).Error; err != nil {
				t.Error(err)
			}
		}
	})
	row := financialChangeE2EContract(t, r, tenant)
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := r.db.Model(&contractRecord{}).Where("tenant_id=? AND id=?", tenant, row.ID).Update("start_date", day).Error; err != nil {
		t.Fatal(err)
	}
	old, err := r.GetContract(ctx, tenant, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Pricing != nil || len(old.ServiceItems) != 1 || old.ServiceItems[0].SourceID != "" {
		t.Fatalf("夹具不是无价无ID：%+v", old)
	}
	temporal := temporalmocks.NewClient(t)
	run := temporalmocks.NewWorkflowRun(t)
	run.On("GetID").Return("isolated-first-pricing")
	run.On("GetRunID").Return("isolated-first-pricing-run")
	temporal.On("ExecuteWorkflow", mock.Anything, mock.Anything, workflows.StatusChangeWorkflowName, mock.Anything).Return(run, nil).Once()
	s := &application.Service{Repo: r, Temporal: temporal, Personnel: fix1PricingPersonnel{}, ContractFinancialChangeEnabled: true}
	admin := pricingE2EActor(tenant, "author", "admin", "contract.pricing.manage", "contract.edit")
	director := pricingE2EActor(tenant, "publisher", "sales_director", "contract.pricing.publish")
	price, err := s.SaveStandardPrice(ctx, admin, contract.StandardPrice{Name: "首次软件价", CategoryID: "软件测试", ServiceType: "软件测试", Unit: "项", UnitPriceMinor: 1000000, Currency: "CNY", TaxBasis: "INCLUSIVE", TaxRateBPS: 600, EffectiveFrom: "2020-01-01"}, 0, "SAVE")
	if err != nil {
		t.Fatal(err)
	}
	price, err = s.SaveStandardPrice(ctx, director, price, price.Version, "PUBLISH")
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.SubmitPricingRevision(ctx, admin, row.ID, application.ContractFinancialChange{ExpectedVersion: old.Version, AmountMinor: 800000, Currency: "CNY", Reason: "首次建立独立标准基准", PaymentTerms: []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2026-12-01", Amount: "8000.00"}}, Pricing: &contract.Pricing{Reselect: true, BasisDate: "2026-01-01", TaxBasis: "INCLUSIVE", TaxRateBPS: 600, Lines: []contract.PricingLine{{ServiceIndex: 0, PriceID: price.ID, Quantity: "1", AmountMinor: 800000}}}})
	if err != nil {
		t.Fatal(err)
	}
	before, err := r.GetContract(ctx, tenant, row.ID)
	if err != nil || before.Pricing != nil || before.ServiceItems[0].SourceID != "" || before.AmountMinor != old.AmountMinor {
		t.Fatalf("受理改写了生效版本：%+v %v", before, err)
	}
	snapshot, found, err := r.GetApprovalSnapshot(ctx, tenant, result.ApprovalID)
	if err != nil || !found || snapshot.ServiceItems[0].SourceID == "" {
		t.Fatalf("候选缺稳定身份：%+v %v", snapshot, err)
	}
	if err := r.CompleteApproval(ctx, workflows.CompleteApprovalActivityInput{ApprovalID: result.ApprovalID, TenantID: tenant, ContractID: row.ID, ActorUserID: "independent", Status: approval.StatusApproved, TargetStatus: old.Status}); err != nil {
		t.Fatalf("首次基准终决失败：%v", err)
	}
	after, err := r.GetContract(ctx, tenant, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != old.Version+1 || after.ServiceItems[0].SourceID != snapshot.ServiceItems[0].SourceID || after.Pricing == nil || after.Pricing.Lines[0].ServiceID != after.ServiceItems[0].SourceID {
		t.Fatalf("终决未原子保存身份：%+v", after)
	}
	metrics, err := r.DiscountMetrics(ctx, tenant)
	if err != nil || len(metrics.Rows) != 1 || metrics.Rows[0].Status != "KNOWN" || metrics.Rows[0].Bucket != "FROM_80_TO_90" {
		t.Fatalf("回读/机器事实不符：%+v %v", metrics, err)
	}
	events := financialEventRows(t, r, tenant, row.ID)
	if len(events) != 1 || events[0].ContractVersion != after.Version {
		t.Fatalf("财务事件不一致：%+v", events)
	}
}

// TestFix1IdentityAdoptionRejectsServiceMutation 验证首次身份补齐不能借审批替换服务内容或既有身份。
// 参数：t为测试上下文。
// 返回值：无；仅空ID变为稳定ID且内容顺序不变可通过。
func TestFix1IdentityAdoptionRejectsServiceMutation(t *testing.T) {
	current := []contract.ServiceItem{{ServiceType: "软件测试", Category: "软件测试"}}
	for _, proposed := range [][]contract.ServiceItem{{{SourceID: "C-01", ServiceType: "渗透测试", Category: "软件测试"}}, {{SourceID: "C-01", ServiceType: "软件测试", Category: "另一个类别"}}, {{ServiceType: "软件测试", Category: "软件测试"}}} {
		if _, err := pricingRevisionServiceIdentities("C", current, proposed); err == nil {
			t.Fatalf("非法服务变更被接受：%+v", proposed)
		}
	}
	current[0].SourceID = "existing"
	if _, err := pricingRevisionServiceIdentities("C", current, []contract.ServiceItem{{SourceID: "replacement", ServiceType: "软件测试", Category: "软件测试"}}); err == nil {
		t.Fatal("既有稳定身份被替换")
	}
}
