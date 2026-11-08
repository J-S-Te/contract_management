package crm

import (
	"os"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
)

// TestOpportunityLinkExportProductionWire 导出生产编码的合成合同关联回调。
// 参数：t为测试上下文，CRM_LINK_CONTRACT_FILE指定交换文件。
// 返回值：无；生产编码失败或交换文件无法写入时失败。
func TestOpportunityLinkExportProductionWire(t *testing.T) {
	path := os.Getenv("CRM_LINK_CONTRACT_FILE")
	if path == "" {
		t.Skip("仅由跨仓契约驱动脚本执行")
	}
	linked := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	body, err := EncodeOpportunityLinkCallback(application.OpportunityIntake{IntakeID: "intake-7", EventID: "opp-signed-e31252cb65ab8cc928bd7945a153a670", OpportunityID: 7, ContractID: "01K00000000000000000000000", ContractRef: "01K00000000000000000000000", ContractNumber: "HT-7", Status: application.OpportunityIntakeLinkConfirmed, ReviewedAt: &linked, Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
}
