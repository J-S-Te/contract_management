package mysql

import (
	"bytes"
	"context"
	"github.com/j-s-te/contract-management/internal/testfixture"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/oklog/ulid/v2"
)

// TestPDFApprovalSourceSnapshotMySQL 验证真实事务、快照和编号保持PDF原件。
// 参数：t为隔离MySQL测试上下文，只接受专用回环CONTRACT_TEST_DSN。
// 返回值：无；迁移、创建、审批或不可变原件校验失败时测试失败。
func TestPDFApprovalSourceSnapshotMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	t.Cleanup(func() {
		if err := r.db.Exec("DELETE FROM con_audit_outbox WHERE tenant_id = ?", tenant).Error; err != nil {
			t.Error(err)
		}
	})
	pdf := testfixture.PDF("original contract")
	owner := ulid.Make().String()
	c := contract.Contract{ID: ulid.Make().String(), TenantID: tenant, Title: "历史源PDF测试",
		Type: "直签", ServiceType: "等保测评", OwnerUserID: owner, OwnerIdentityID: owner,
		AmountMinor: 10000, Currency: "CNY", Document: pdf, DocumentFormat: "PDF",
		SourceFilename: "原始已签合同.pdf", SourceMediaType: "application/pdf",
		SourceSHA256: draftE2EHash(string(pdf)), ContentHash: draftE2EHash(string(pdf)),
		ExternalPaperNumber: "纸面-2020-001", Status: contract.StatusDraft,
		ServiceItems: []contract.ServiceItem{{ServiceType: "等保测评", Site: "北京", Batch: "第一批", Category: "等保测评", TestMode: "STANDARD"}}}
	if err := r.CreateContract(ctx, c, owner); err != nil {
		t.Fatal(err)
	}
	stored, err := r.GetContract(ctx, tenant, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DocumentFormat != "PDF" || stored.ExternalPaperNumber != c.ExternalPaperNumber {
		t.Fatal("来源元数据未持久化")
	}
	approvalID := ulid.Make().String()
	start := workflows.StartApprovalActivityInput{ApprovalID: approvalID, TenantID: tenant,
		ContractID: c.ID, ApplicantUserID: owner, ExpectedVersion: 1, Kind: approval.KindContract,
		FromStatus: contract.StatusDraft, TargetStatus: contract.StatusPending,
		ContentHash: c.ContentHash, WorkflowID: "pdf-source-" + c.ID, RunID: ulid.Make().String()}
	if err := r.StartApproval(ctx, start); err != nil {
		t.Fatal(err)
	}
	snapshot, found, err := r.GetApprovalSnapshot(ctx, tenant, approvalID)
	if err != nil || !found || snapshot.DocumentFormat != "PDF" || !bytes.Equal(snapshot.Document, pdf) {
		t.Fatalf("PDF审批快照丢失: found=%v error=%v", found, err)
	}
	complete := workflows.CompleteApprovalActivityInput{ApprovalID: approvalID, TenantID: tenant,
		ContractID: c.ID, ActorUserID: owner, Status: approval.StatusApproved, TargetStatus: contract.StatusActive}
	for range 2 {
		if err := r.CompleteApproval(ctx, complete); err != nil {
			t.Fatal(err)
		}
	}
	stored, err = r.GetContract(ctx, tenant, c.ID)
	if err != nil || stored.Status != contract.StatusActive || stored.Number == "" ||
		stored.ContentHash != c.ContentHash || !bytes.Equal(stored.Document, pdf) {
		t.Fatalf("PDF审批没有正确生效或改写了原件: status=%s error=%v", stored.Status, err)
	}
}
