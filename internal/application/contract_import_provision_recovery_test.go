package application

import (
	"context"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/testfixture"
)

// TestProvisionUnavailableIsNotBusinessRejection 验证依赖故障不会误报为表格业务数据错误。
// 参数：t 为测试上下文，使用客户端与应用层共用的依赖故障哨兵。
// 返回值：5xx 分类、恢复标记或业务拒绝边界不符时失败。
func TestProvisionUnavailableIsNotBusinessRejection(t *testing.T) {
	unavailable := classifyImportProvisionCRMError(ErrCRMProvisionUnavailable)
	if unavailable.Code != "IMPORT_PROVISION_CRM_UNAVAILABLE" || !unavailable.Retryable {
		t.Fatalf("依赖故障误分类: %+v", unavailable)
	}
	rejected := classifyImportProvisionCRMError(ErrCRMProvisionRejected)
	if rejected.Code != CodeImportProvisionCRMRejected || rejected.Retryable {
		t.Fatalf("业务拒绝边界变化: %+v", rejected)
	}
}

// TestOpportunityFailureResumesOriginalDraft 验证商机失败后通过正式恢复入口续做同一合同。
// 参数：t 为测试上下文；仅 CRM 和持久化边界使用既有替身。
// 返回值：重复创建客户或合同、改变编号、提前确认或无法恢复时失败。
func TestOpportunityFailureResumesOriginalDraft(t *testing.T) {
	pdf := testfixture.PDF("opportunity recovery")
	actor := provisionImportActor()
	calls, customer, opportunity, owners := newProvisionStubs()
	item := provisionHistoryItem("one", "old-1", "source-one", "历史客户", "张三", 5000000)
	repo := newProvisionImportRepository(provisionImportBatch(
		[]contract.ImportItem{item}, provisionTestSource("one", pdf)))
	repo.autoApproveSetting = contract.ImportAutoApproveSetting{Enabled: true, Version: 1}
	service := provisionTestService(repo, pdf, customer, opportunity, owners)
	service.ContractImportAutoApproveHistorical = true
	previewProvisionBatch(t, service, repo, actor)
	resetProvisionCalls(calls)
	opportunity.createErr = ErrCRMProvisionUnavailable
	dispatchUntilSettled(t, service)
	key := contract.ImportCRMProvisionKey{TenantID: provisionTestTenant,
		SourceSystem: "legacy", ExternalRecordID: "old-1"}
	before := repo.records[key]
	if before.State != contract.ImportCRMProvisionBlocked || before.CustomerID != 501 ||
		before.OpportunityID != 0 || repo.commits != 1 || repo.contract.Status != contract.StatusDraft ||
		repo.batch.Items[0].Confirmed || calls.sign != 0 || len(repo.autoApproveCalls) != 0 {
		t.Fatalf("商机失败后越过确认门禁: ledger=%+v calls=%+v", before, calls)
	}
	id, number := repo.contract.ID, repo.contract.Number
	if id == "" || number == "" {
		t.Fatal("未保留草稿与预留编号")
	}
	failedKeys := append([]string(nil), opportunity.createKeys...)
	opportunity.createErr = nil
	repo.batch.OperationID, repo.batch.CommittedRevision = "operation-test", repo.batch.Revision
	if err := service.ControlImport(context.Background(), actor, repo.batch.ID,
		repo.batch.Version, true, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	dispatchUntilSettled(t, service)
	if repo.contract.ID != id || repo.contract.Number != number || repo.commits != 1 ||
		calls.createCustomer != 1 || calls.sign != 1 || !repo.batch.Items[0].Confirmed ||
		repo.records[key].State != contract.ImportCRMProvisionCompleted {
		t.Fatalf("恢复未复用断点: ledger=%+v calls=%+v", repo.records[key], calls)
	}
	for _, key := range opportunity.createKeys {
		if key != failedKeys[0] {
			t.Fatal("商机恢复更换了创建幂等键")
		}
	}
}
