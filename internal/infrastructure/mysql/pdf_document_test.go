package mysql

import (
	"bytes"
	"github.com/j-s-te/contract-management/internal/testfixture"
	"testing"
)

// TestPDFApprovalDoesNotRewriteSource 验证PDF编号更新不触发DOCX解析。
// 参数：t 为审批编号测试上下文。
// 返回值：无；原件被改写、未知类型被接受或编号丢失时测试失败。
func TestPDFApprovalDoesNotRewriteSource(t *testing.T) {
	pdf := testfixture.PDF("original contract")
	current := contractRecord{DocumentFormat: "PDF", RenderedDocument: append([]byte(nil), pdf...)}
	updates, err := approvedNumberUpdates(current, "HT-20261029-12345678")
	if err != nil {
		t.Fatal(err)
	}
	if updates["contract_number"] != "HT-20261029-12345678" || len(updates) != 1 {
		t.Fatalf("PDF不得被写入正文或哈希更新: %+v", updates)
	}
	if !bytes.Equal(current.RenderedDocument, pdf) {
		t.Fatal("PDF原件改变")
	}
	current.DocumentFormat = "UNRECOGNIZED"
	if _, err := approvedNumberUpdates(current, "HT-1"); err == nil {
		t.Fatal("未知格式不应完成审批")
	}
}

// TestContractSourceMetadataReadsFrozenIdentity 验证来源元数据读取不丢失类型。
// 参数：t 为合同行映射测试上下文。
// 返回值：无；格式、原号或历史策略丢失时测试失败。
func TestContractSourceMetadataReadsFrozenIdentity(t *testing.T) {
	row := contractRecord{DocumentFormat: "PDF", SourceFilename: "原件.pdf",
		SourceMediaType: "application/pdf", SourceSHA256: "original-sha",
		ExternalPaperNumber: "旧合同2020-001", OriginKind: "IMPORTED", ImportMode: "ARCHIVE_ONLY",
		ImportPolicyJSON: []byte(`{"policy_version":1,"project_mode":"NONE","finance_mode":"NONE","report_mode":"ARCHIVE_SEPARATE","responsibility_mode":"INACTIVE","signing_mode":"DECLARED_ONLY"}`)}
	got := contractFromRecord(row)
	if got.DocumentFormat != "PDF" || got.SourceFilename != "原件.pdf" ||
		got.ExternalPaperNumber != "旧合同2020-001" || got.SourceSHA256 != "original-sha" ||
		got.ImportPolicy == nil || got.ImportPolicy.ProjectMode != "NONE" {
		t.Fatalf("来源映射丢失: %+v", got)
	}
}
