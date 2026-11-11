package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/oklog/ulid/v2"
)

// pricingE2EActor 生成显式动作和租户全范围的隔离测试主体。
// 参数：tenant为隔离测试租户，id为身份，role为现有职责，permissions为明确授权。
// 返回值：用于实际应用服务的可信主体夹具。
func pricingE2EActor(tenant, id, role string, permissions ...string) application.Principal {
	p := application.Principal{TenantID: tenant, UserID: id, IdentityID: id, Roles: []string{role}, Permissions: map[string]bool{}, PermissionScopes: map[string]contract.ScopeFilter{}}
	for _, permission := range permissions {
		p.Permissions[permission] = true
		p.PermissionScopes[permission] = contract.ScopeFilter{AllowAll: true}
	}
	return p
}

// TestPricingLifecycleAndFrozenRevisionE2E 用最小真实MySQL数据验证标准价职责、冻结及原审批修订。
// 参数：t为测试上下文；仅显式CONTRACT_TEST_DSN指向contract_test_*回环库时执行。
// 返回值：无；迁移、事务、快照、精确分桶和隔离不符即失败并清理本租户。
func TestPricingLifecycleAndFrozenRevisionE2E(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	t.Cleanup(func() {
		for _, model := range []any{&standardPriceRecord{}, &pricingRevisionRecord{}, &approvalSnapshotRecord{}} {
			if err := r.db.Where("tenant_id=?", tenant).Delete(model).Error; err != nil {
				t.Error(err)
			}
		}
	})
	s := &application.Service{Repo: r}
	admin := pricingE2EActor(tenant, "author", "admin", "contract.pricing.manage", "contract.read")
	director := pricingE2EActor(tenant, "publisher", "sales_director", "contract.pricing.publish", "contract.read")
	p := contract.StandardPrice{Name: "隔离软件标准价", CategoryID: "测试类别", ServiceType: "软件测试", Unit: "项", UnitPriceMinor: 10000, Currency: "CNY", TaxBasis: "INCLUSIVE", TaxRateBPS: 600, EffectiveFrom: "2020-01-01"}
	saved, err := s.SaveStandardPrice(ctx, admin, p, 0, "SAVE")
	if err != nil {
		t.Fatal(err)
	}
	authorDirector := pricingE2EActor(tenant, "author", "sales_director", "contract.pricing.publish")
	if _, err := s.SaveStandardPrice(ctx, authorDirector, saved, saved.Version, "PUBLISH"); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("作者自发布未阻止：%v", err)
	}
	if _, err := s.SaveStandardPrice(ctx, admin, saved, saved.Version, "PUBLISH"); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("管理员无发布职责未阻止：%v", err)
	}
	published, err := s.SaveStandardPrice(ctx, director, saved, saved.Version, "PUBLISH")
	if err != nil {
		t.Fatal(err)
	}
	if published.Status != "PUBLISHED" || published.PublishedBy != "publisher" {
		t.Fatalf("发布事实不符：%+v", published)
	}
	if _, err := s.SaveStandardPrice(ctx, admin, published, published.Version, "SAVE"); !errors.Is(err, apperrors.ErrStateConflict) {
		t.Fatalf("已发布价格可覆写：%v", err)
	}
	// 当前有效合同只使用已发布版本冻结字段，历史无基准另留一份。
	row := financialChangeE2EContract(t, r, tenant)
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := contractFromRecord(row)
	c.AmountMinor = 8000
	c.StartDate = &day
	c.ServiceItems = []contract.ServiceItem{{SourceID: "stable-service", ServiceType: "软件测试", Category: "测试类别"}}
	c.Pricing = &contract.Pricing{BasisDate: "2026-01-01", Currency: "CNY", TaxBasis: "INCLUSIVE", TaxRateBPS: 600, FrozenAt: time.Now().UTC(), Lines: []contract.PricingLine{{LineID: "frozen-line", ServiceID: "stable-service", ServiceIndex: 0, PriceID: published.ID, PriceVersion: published.Version, Quantity: "1", Unit: "项", UnitPriceMinor: 10000, StandardMinor: 10000, AmountMinor: 8000}}}
	c.PaymentTerms = []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2026-12-01", Amount: "80.00"}}
	// 夹具仅在隔离库建立已批准版本；定价计算与原审批终决均执行真实产品仓储。
	if err := r.db.Model(&contractRecord{}).Where("tenant_id=? AND id=?", tenant, c.ID).Updates(map[string]any{"amount_minor": c.AmountMinor, "start_date": day, "service_items_json": mustPricingJSON(t, c.ServiceItems), "pricing_json": pricingJSON(c.Pricing)}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveStandardPrice(ctx, admin, published, published.Version, "DISABLE"); err != nil {
		t.Fatal(err)
	}
	frozen, err := r.GetContract(ctx, tenant, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := contract.PricingTotal(frozen); got != 10000 {
		t.Fatalf("停用追溯改变冻结额：%d", got)
	}
	unknownContract := contract.Contract{ID: ulid.Make().String(), TenantID: tenant, Title: "隔离未知合同", Type: "直签", ServiceType: "软件测试", OwnerUserID: "historical-subject", OwnerIdentityID: "historical-subject", AmountMinor: 1000, Currency: "CNY", Content: "isolated-fixture", Status: contract.StatusDraft, StartDate: &day}
	if err := r.CreateContract(ctx, unknownContract, "historical-subject"); err != nil {
		t.Fatal(err)
	}
	machine := pricingE2EActor(tenant, "machine", "", "contract.read")
	metrics, err := s.DashboardMetrics(ctx, machine)
	if err != nil {
		t.Fatal(err)
	}
	var knownCount, unknownCount int
	for _, metric := range metrics.Rows {
		if metric.RecordID == c.ID && metric.Bucket == "FROM_80_TO_90" {
			knownCount++
		}
		if metric.RecordID == unknownContract.ID && metric.Status == "UNKNOWN" && metric.UnknownReason == "MISSING_PRICING" {
			unknownCount++
		}
	}
	if len(metrics.Rows) != 2 || knownCount != 1 || unknownCount != 1 || !metrics.Complete || metrics.SourceCode != "contract_management" {
		t.Fatalf("机器事实不符：%+v", metrics)
	}
	if path := os.Getenv("CONTRACT_METRICS_FIXTURE_PATH"); path != "" {
		data, err := json.MarshalIndent(map[string]any{"code": "OK", "data": metrics}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	narrowed := machine
	narrowed.PermissionScopes = map[string]contract.ScopeFilter{"contract.read": {AllowSelf: true}}
	if _, err := s.DashboardMetrics(ctx, narrowed); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("机器窄范围扩大：%v", err)
	}
	if _, err := r.GetStandardPrice(ctx, "other-tenant", published.ID); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("跨租户价格泄漏：%v", err)
	}
	// 新价格另起版本；受理候选后旧合同仍有效，原快照显示候选价，终决后才替换。
	p.UnitPriceMinor = 12000
	next, err := s.SaveStandardPrice(ctx, admin, p, 0, "SAVE")
	if err != nil {
		t.Fatal(err)
	}
	next, err = s.SaveStandardPrice(ctx, director, next, next.Version, "PUBLISH")
	if err != nil {
		t.Fatal(err)
	}
	candidate := c
	candidate.AmountMinor = 9000
	candidate.Pricing = &contract.Pricing{BasisDate: "2026-02-01", Currency: "CNY", TaxBasis: "INCLUSIVE", TaxRateBPS: 600, Reason: "独立审批重选", FrozenAt: time.Now().UTC(), Lines: []contract.PricingLine{{LineID: "new-frozen-line", ServiceID: "stable-service", ServiceIndex: 0, PriceID: next.ID, PriceVersion: next.Version, Quantity: "1", Unit: "项", UnitPriceMinor: 12000, StandardMinor: 12000, AmountMinor: 9000}}}
	candidate.PaymentTerms = []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2026-12-01", Amount: "90.00"}}
	aid := ulid.Make().String()
	start := workflows.StartApprovalActivityInput{ApprovalID: aid, TenantID: tenant, ContractID: c.ID, ExpectedVersion: c.Version, ApplicantUserID: "requester", Kind: approval.KindStatusChange, FromStatus: c.Status, TargetStatus: c.Status, Reason: "独立审批重选", WorkflowID: "pricing-test-" + aid, Nodes: []approval.Node{{ID: "admin", RoleCode: "admin", AssigneeIDs: []string{"independent"}, Countersign: approval.CountersignAny}}}
	if err := r.AcceptPricingRevision(ctx, start, candidate, func(context.Context) (string, error) { return "isolated-start-receipt", nil }); err != nil {
		t.Fatal(err)
	}
	before, err := r.GetContract(ctx, tenant, c.ID)
	if err != nil || before.AmountMinor != 8000 {
		t.Fatalf("待审修改了有效合同：%+v %v", before, err)
	}
	snapshot, found, err := r.GetApprovalSnapshot(ctx, tenant, aid)
	if err != nil || !found || snapshot.AmountMinor != 9000 || snapshot.Pricing.Lines[0].StandardMinor != 12000 {
		t.Fatalf("候选快照错误：%+v %v", snapshot, err)
	}
	if err := r.CompleteApproval(ctx, workflows.CompleteApprovalActivityInput{ApprovalID: aid, TenantID: tenant, ContractID: c.ID, ActorUserID: "requester", Status: approval.StatusApproved, TargetStatus: c.Status}); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("仓储自审未阻止：%v", err)
	}
	if _, err := r.ApplyContractFinancialChange(ctx, tenant, application.ContractFinancialChange{ContractID: c.ID, ExpectedVersion: c.Version, AmountMinor: 9000, Currency: "CNY", PaymentTerms: candidate.PaymentTerms, Pricing: c.Pricing, Reason: "不能越过待审修订"}, "requester"); !errors.Is(err, apperrors.ErrStateConflict) {
		t.Fatalf("在途修订未阻止财务改额：%v", err)
	}
	if err := r.CompleteApproval(ctx, workflows.CompleteApprovalActivityInput{ApprovalID: aid, TenantID: tenant, ContractID: c.ID, ActorUserID: "independent", Status: approval.StatusApproved, TargetStatus: c.Status}); err != nil {
		t.Fatal(err)
	}
	after, err := r.GetContract(ctx, tenant, c.ID)
	if err != nil || after.AmountMinor != 9000 || after.Version != c.Version+1 || after.Pricing.Lines[0].StandardMinor != 12000 {
		t.Fatalf("终决结果错误：%+v %v", after, err)
	}
	frozenSnapshot, _, err := r.GetApprovalSnapshot(ctx, tenant, aid)
	if err != nil || frozenSnapshot.Pricing.Lines[0].StandardMinor != 12000 {
		t.Fatalf("快照被改写：%v", err)
	}
	events := financialEventRows(t, r, tenant, c.ID)
	if len(events) != 1 || events[0].ContractVersion != after.Version {
		t.Fatalf("未原子发送新财务版本：%+v", events)
	}
}

// mustPricingJSON 编码隔离夹具的结构化服务项。
// 参数：t为测试上下文，value为测试输入。
// 返回值：JSON字节；编码失败使测试失败。
func mustPricingJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
