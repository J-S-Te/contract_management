package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/mock"
	temporalmocks "go.temporal.io/sdk/mocks"
)

// fix2PricingContract 在隔离库建立已对项目投影公开但尚未定价的普通合同。
// 参数：t为测试上下文，items为既有服务事实。
// 返回值：真实仓储、已发布价格服务、编辑主体、旧合同及可用价格身份。
func fix2PricingContract(t *testing.T, items []contract.ServiceItem) (*Repository, *application.Service, application.Principal, contract.Contract, string) {
	t.Helper()
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
	if err := r.db.Model(&contractRecord{}).Where("tenant_id=? AND id=?", tenant, row.ID).Updates(map[string]any{"service_items_json": mustPricingJSON(t, items), "start_date": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}).Error; err != nil {
		t.Fatal(err)
	}
	old, err := r.GetContract(ctx, tenant, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	temporal := temporalmocks.NewClient(t)
	run := temporalmocks.NewWorkflowRun(t)
	run.On("GetID").Return("isolated-compatible-pricing").Maybe()
	run.On("GetRunID").Return("isolated-compatible-run").Maybe()
	temporal.On("ExecuteWorkflow", mock.Anything, mock.Anything, workflows.StatusChangeWorkflowName, mock.Anything).Return(run, nil).Maybe()
	s := &application.Service{Repo: r, Temporal: temporal, Personnel: fix1PricingPersonnel{}, ContractFinancialChangeEnabled: true}
	actor := pricingE2EActor(tenant, "author", "admin", "contract.pricing.manage", "contract.edit")
	p, err := s.SaveStandardPrice(ctx, actor, contract.StandardPrice{Name: "项目身份兼容价", CategoryID: "软件测试", ServiceType: "软件测试", Unit: "项", UnitPriceMinor: 1000000, Currency: "CNY", TaxBasis: "INCLUSIVE", TaxRateBPS: 600, EffectiveFrom: "2020-01-01"}, 0, "SAVE")
	if err != nil {
		t.Fatal(err)
	}
	p, err = s.SaveStandardPrice(ctx, pricingE2EActor(tenant, "publisher", "sales_director", "contract.pricing.publish"), p, p.Version, "PUBLISH")
	if err != nil {
		t.Fatal(err)
	}
	return r, s, actor, old, p.ID
}

// fix2SubmitPricing 通过真实应用服务和MySQL受理首次定价修订。
// 参数：t为测试上下文，s/actor为服务边界，old为生效合同，priceID为已发布版本。
// 返回值：原审批身份；启动端使用SDK替身，不代表真实Temporal服务。
func fix2SubmitPricing(t *testing.T, s *application.Service, actor application.Principal, old contract.Contract, priceID string) application.StartResult {
	t.Helper()
	lines := make([]contract.PricingLine, len(old.ServiceItems))
	for i := range lines {
		lines[i] = contract.PricingLine{ServiceIndex: i, PriceID: priceID, Quantity: "1", AmountMinor: 800000}
	}
	amount := int64(len(lines)) * 800000
	result, err := s.SubmitPricingRevision(context.Background(), actor, old.ID, application.ContractFinancialChange{ExpectedVersion: old.Version, AmountMinor: amount, Currency: "CNY", Reason: "补基准保持既有项目身份", PaymentTerms: []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2026-12-01", Amount: formatAmountMinor(amount)}}, Pricing: &contract.Pricing{Reselect: true, BasisDate: "2026-01-01", TaxBasis: "INCLUSIVE", TaxRateBPS: 600, Lines: lines}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// TestFix2FirstPricingPreservesPublishedProjectServiceIDsE2E 验证首次定价不重命名已公开的项目服务引用。
// 参数：t为测试上下文，覆盖单系统及空/非空身份、多系统混合合同。
// 返回值：无；修订前后投影身份逐项完全相同且金额、事件仍正确。
func TestFix2FirstPricingPreservesPublishedProjectServiceIDsE2E(t *testing.T) {
	cases := map[string][]contract.ServiceItem{
		"single":                 {{ServiceType: "软件测试"}},
		"mixed-multiple-systems": {{ServiceType: "软件测试", Systems: []contract.SystemInfo{{Name: "系统甲", Level: "一级"}, {Name: "系统乙", Level: "二级"}}}, {SourceID: "existing-source", ServiceType: "软件测试", Systems: []contract.SystemInfo{{Name: "系统丙", Level: "一级"}, {Name: "系统丁", Level: "二级"}}}, {ServiceType: "软件测试"}},
	}
	for name, items := range cases {
		t.Run(name, func(t *testing.T) {
			r, s, actor, old, priceID := fix2PricingContract(t, items)
			ctx := context.Background()
			before := contract.ProjectServiceItems(old.ID, old.ServiceItems)
			expected := []string{old.ID + "-01"}
			if name == "mixed-multiple-systems" {
				expected = []string{old.ID + "-01-01", old.ID + "-01-02", "existing-source-01", "existing-source-02", old.ID + "-03"}
			}
			for i, item := range before {
				if i >= len(expected) || item.SourceID != expected[i] {
					t.Fatalf("原投影算法改变：%+v", before)
				}
			}
			if len(before) != len(expected) {
				t.Fatalf("原投影条目数改变：%+v", before)
			}
			result := fix2SubmitPricing(t, s, actor, old, priceID)
			if err := r.CompleteApproval(ctx, workflows.CompleteApprovalActivityInput{ApprovalID: result.ApprovalID, TenantID: old.TenantID, ContractID: old.ID, ActorUserID: "independent", Status: approval.StatusApproved, TargetStatus: old.Status}); err != nil {
				t.Fatal(err)
			}
			after, err := r.GetContract(ctx, old.TenantID, old.ID)
			if err != nil {
				t.Fatal(err)
			}
			projected := contract.ProjectServiceItems(after.ID, after.ServiceItems)
			if !reflect.DeepEqual(before, projected) {
				t.Fatalf("已公开项目身份漂移：before=%+v after=%+v", before, projected)
			}
			if _, err := contract.PricingTotal(after); err != nil {
				t.Fatal(err)
			}
			metrics, err := r.DiscountMetrics(ctx, old.TenantID)
			if err != nil || len(metrics.Rows) != 1 || metrics.Rows[0].Bucket != "FROM_80_TO_90" {
				t.Fatalf("指标不符：%+v %v", metrics, err)
			}
			events := financialEventRows(t, r, old.TenantID, old.ID)
			if len(events) != 1 || events[0].ContractVersion != after.Version {
				t.Fatalf("财务事件不符：%+v", events)
			}
		})
	}
}

// TestFix2LegacyULIDProposalRejectedWithoutIdentityDriftE2E 验证升级前ULID候选不能静默改写项目身份或已批依据。
// 参数：t为测试上下文，旧候选以原仓储受理格式在隔离库建立。
// 返回值：无；批准应用明确冲突且不写业务，原撤回可关闭后重新提交。
func TestFix2LegacyULIDProposalRejectedWithoutIdentityDriftE2E(t *testing.T) {
	r, s, actor, old, priceID := fix2PricingContract(t, []contract.ServiceItem{{ServiceType: "软件测试", Systems: []contract.SystemInfo{{Name: "甲", Level: "一级"}, {Name: "乙", Level: "二级"}}}})
	ctx := context.Background()
	before := contract.ProjectServiceItems(old.ID, old.ServiceItems)
	candidate := old
	candidate.ServiceItems = append([]contract.ServiceItem(nil), old.ServiceItems...)
	candidate.ServiceItems[0].SourceID = ulid.Make().String()
	candidate.AmountMinor = 800000
	candidate.PaymentTerms = []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2026-12-01", Amount: "8000.00"}}
	candidate.Pricing = &contract.Pricing{BasisDate: "2026-01-01", Currency: "CNY", TaxBasis: "INCLUSIVE", TaxRateBPS: 600, FrozenAt: time.Now().UTC(), Lines: []contract.PricingLine{{ServiceIndex: 0, ServiceID: candidate.ServiceItems[0].SourceID, LineID: ulid.Make().String(), PriceID: priceID, PriceVersion: 2, Quantity: "1", Unit: "项", UnitPriceMinor: 1000000, StandardMinor: 1000000, AmountMinor: 800000}}}
	aid := ulid.Make().String()
	start := workflows.StartApprovalActivityInput{ApprovalID: aid, TenantID: old.TenantID, ContractID: old.ID, ExpectedVersion: old.Version, ApplicantUserID: actor.UserID, Kind: approval.KindStatusChange, FromStatus: old.Status, TargetStatus: old.Status, Reason: "旧ULID候选", WorkflowID: "legacy-pricing-" + aid, Nodes: []approval.Node{{ID: "admin", RoleCode: "admin", AssigneeIDs: []string{"independent"}, Countersign: approval.CountersignAny}}}
	if err := r.AcceptPricingRevision(ctx, start, candidate, func(context.Context) (string, error) { return "isolated-legacy-run", nil }); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := r.GetApprovalSnapshot(ctx, old.TenantID, aid)
	if err != nil {
		t.Fatal(err)
	}
	frozenJSON, _ := json.Marshal(snapshot)
	err = r.CompleteApproval(ctx, workflows.CompleteApprovalActivityInput{ApprovalID: aid, TenantID: old.TenantID, ContractID: old.ID, ActorUserID: "independent", Status: approval.StatusApproved, TargetStatus: old.Status})
	if !errors.Is(err, apperrors.ErrStateConflict) {
		t.Fatalf("旧ULID候选应明确冲突，得到%v", err)
	}
	unchanged, err := r.GetContract(ctx, old.TenantID, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, _, _ := r.GetApprovalSnapshot(ctx, old.TenantID, aid)
	againJSON, _ := json.Marshal(again)
	if unchanged.Version != old.Version || unchanged.Pricing != nil || !reflect.DeepEqual(before, contract.ProjectServiceItems(unchanged.ID, unchanged.ServiceItems)) || string(againJSON) != string(frozenJSON) || len(financialEventRows(t, r, old.TenantID, old.ID)) != 0 {
		t.Fatal("旧候选错误改变了有效合同、快照或财务事件")
	}
	if err := r.CompleteApproval(ctx, workflows.CompleteApprovalActivityInput{ApprovalID: aid, TenantID: old.TenantID, ContractID: old.ID, ActorUserID: actor.UserID, Status: approval.StatusWithdrawn}); err != nil {
		t.Fatal(err)
	}
	meta, err := r.GetApprovalMeta(ctx, old.TenantID, aid)
	if err != nil || meta.Status != approval.StatusWithdrawn || !meta.CompletionApplied {
		t.Fatalf("旧候选未按原撤回终态关闭：%+v %v", meta, err)
	}
	fresh := fix2SubmitPricing(t, s, actor, unchanged, priceID)
	if err := r.CompleteApproval(ctx, workflows.CompleteApprovalActivityInput{ApprovalID: fresh.ApprovalID, TenantID: old.TenantID, ContractID: old.ID, ActorUserID: "independent", Status: approval.StatusApproved, TargetStatus: old.Status}); err != nil {
		t.Fatal(err)
	}
	final, err := r.GetContract(ctx, old.TenantID, old.ID)
	if err != nil || !reflect.DeepEqual(before, contract.ProjectServiceItems(final.ID, final.ServiceItems)) {
		t.Fatalf("撤回重提后身份不一致：%+v %v", final, err)
	}
}
