package mysql

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// TestImportCRMProvisionCASStateMachineE2E 在隔离测试库验证迁移 000043 的真实表结构、
// 幂等起点、state+attempt 的 CAS 推进与 BLOCKED 落账。
// 参数：t 为测试上下文，仅显式隔离 CONTRACT_TEST_DSN 运行。
// 返回值：无；未配置隔离测试库时跳过，绝不连接业务库。
func TestImportCRMProvisionCASStateMachineE2E(t *testing.T) {
	repo, tenant := draftE2ERepository(t)
	t.Cleanup(func() {
		repo.db.Where("tenant_id=?", tenant).Delete(&importCRMProvisionRecord{})
	})
	ctx := context.Background()
	key := contract.ImportCRMProvisionKey{TenantID: tenant, SourceSystem: "history-crm", ExternalRecordID: "record-1"}

	// 起点幂等：不存在时创建 PENDING/attempt=0。
	started, err := repo.UpsertStartImportCRMProvision(ctx, key, "01J0000000000000000000000B")
	if err != nil || started.State != contract.ImportCRMProvisionPending || started.Attempt != 0 {
		t.Fatalf("start=%+v err=%v", started, err)
	}
	// 顺序推进并写入客户 ID。
	customerReady, err := repo.AdvanceImportCRMProvision(ctx, key, contract.ImportCRMProvisionAdvance{
		From: contract.ImportCRMProvisionPending, To: contract.ImportCRMProvisionCustomerReady,
		ExpectedAttempt: 0, CustomerID: 42, CustomerCreated: true,
	})
	if err != nil || customerReady.State != contract.ImportCRMProvisionCustomerReady || customerReady.Attempt != 1 ||
		customerReady.CustomerID != 42 || !customerReady.CustomerCreated {
		t.Fatalf("customer ready=%+v err=%v", customerReady, err)
	}
	// 过期 attempt 的推进必须被 CAS 拒绝，不得覆盖他人结果。
	if _, err := repo.AdvanceImportCRMProvision(ctx, key, contract.ImportCRMProvisionAdvance{
		From: contract.ImportCRMProvisionCustomerReady, To: contract.ImportCRMProvisionContractReady, ExpectedAttempt: 0,
	}); !errors.Is(err, apperrors.ErrStateConflict) {
		t.Fatalf("stale advance error=%v", err)
	}
	// 重跑只增加 attempt，不回退已完成状态、不重置客户 ID。
	retried, err := repo.UpsertStartImportCRMProvision(ctx, key, "")
	if err != nil || retried.State != contract.ImportCRMProvisionCustomerReady || retried.Attempt != 2 || retried.CustomerID != 42 {
		t.Fatalf("retry=%+v err=%v", retried, err)
	}
	// 合同建好后推进 CONTRACT_READY，再建商机并推进 OPPORTUNITY_READY。
	contractReady, err := repo.AdvanceImportCRMProvision(ctx, key, contract.ImportCRMProvisionAdvance{
		From: contract.ImportCRMProvisionCustomerReady, To: contract.ImportCRMProvisionContractReady, ExpectedAttempt: 2,
	})
	if err != nil || contractReady.State != contract.ImportCRMProvisionContractReady || contractReady.Attempt != 3 {
		t.Fatalf("contract ready=%+v err=%v", contractReady, err)
	}
	// 失败落账进入 BLOCKED 并保留错误码，之后不可再自动推进。
	blocked, err := repo.RecordImportCRMProvisionError(ctx, key, contract.ImportCRMProvisionContractReady, 3,
		"IMPORT_PROVISION_CONTRACT_NOT_VERIFIED", "合同验真未通过")
	if err != nil || blocked.State != contract.ImportCRMProvisionBlocked ||
		blocked.LastErrorCode != "IMPORT_PROVISION_CONTRACT_NOT_VERIFIED" || blocked.LastErrorMessage != "合同验真未通过" {
		t.Fatalf("blocked=%+v err=%v", blocked, err)
	}
	if _, err := repo.AdvanceImportCRMProvision(ctx, key, contract.ImportCRMProvisionAdvance{
		From: contract.ImportCRMProvisionBlocked, To: contract.ImportCRMProvisionOpportunityReady,
		ExpectedAttempt: 3, OpportunityID: 92,
	}); !errors.Is(err, contract.ErrImportCRMProvisionState) {
		t.Fatalf("blocked advance error=%v", err)
	}
	// 读回与清理：台账内容稳定。
	stored, err := repo.GetImportCRMProvision(ctx, key)
	if err != nil || stored.State != contract.ImportCRMProvisionBlocked || stored.Attempt != 3 ||
		stored.LastErrorCode != "IMPORT_PROVISION_CONTRACT_NOT_VERIFIED" {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	// BLOCKED 之后重跑起点仍只提升 attempt，不改终态。
	if again, err := repo.UpsertStartImportCRMProvision(ctx, key, ""); err != nil ||
		again.State != contract.ImportCRMProvisionBlocked || again.Attempt != 4 {
		t.Fatalf("blocked retry=%+v err=%v", again, err)
	}
}
