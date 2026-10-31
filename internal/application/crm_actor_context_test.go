package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/contractdocument"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/testfixture"
)

// TestCRMActorContextSurvivesValidation 验证授权上下文错误能贯穿应用校验和批量分类。
// 参数：t为测试执行器。
// 返回值：无；错误被吞成字段错误或失去旧接口兼容性时失败。
func TestCRMActorContextSurvivesValidation(t *testing.T) {
	s := &Service{CRMContractReferences: crmReferenceDirectoryStub{err: ErrCRMActorContextRequired}}
	_, err := s.validateExternalCRMReference(context.Background(), 7, "", "actor-1")
	if !errors.Is(err, ErrCRMActorContextRequired) || !errors.Is(err, ErrValidation) {
		t.Fatalf("授权上下文分类或旧422兼容性丢失: %v", err)
	}
	if code := importErrorCode(err); code != "IMPORT_CRM_SESSION_REQUIRED" {
		t.Fatalf("code=%s want=IMPORT_CRM_SESSION_REQUIRED", code)
	}
}

// TestPreviewCRMContextRecoveryKeepsBatch 验证重连前阻断、重连后原批次重新预检。
// 参数：t为测试执行器。
// 返回值：无；错误定位、同一批次恢复或无业务副作用的约束不符时失败。
func TestPreviewCRMContextRecoveryKeepsBatch(t *testing.T) {
	s, actor, repo, gateway := newCRMContextPreview(t)
	s.CRMContractReferences = crmReferenceDirectoryStub{err: ErrCRMActorContextRequired}
	result, err := s.PreviewImport(context.Background(), actor, "batch", 2)
	if err != nil || result.State != "BLOCKED" || len(result.Items[0].Issues) != 1 {
		t.Fatalf("缺上下文必须阻断: result=%#v err=%v", result, err)
	}
	issue := result.Items[0].Issues[0]
	if issue.Code != "IMPORT_CRM_SESSION_REQUIRED" || !issue.Retryable ||
		issue.Field != "metadata.crm_customer_id" {
		t.Fatalf("需要明确授权恢复提示，不能误报字段无效: %#v", issue)
	}
	s.CRMContractReferences = validCRMReference(7)
	result, err = s.PreviewImport(context.Background(), actor, "batch", 2)
	if err != nil || result.State != "PREVIEWED" || result.ID != "batch" ||
		result.Items[0].ID != "item" || len(result.Items[0].Issues) != 0 {
		t.Fatalf("原批次不能正常恢复: %#v err=%v", result, err)
	}
	if repo.created.ID != "" || gateway.writes != 0 {
		t.Fatal("重连预检不应创建合同或重复上传原件")
	}
}

// newCRMContextPreview 构造独立PDF输入及禁止业务写入的预检依赖。
// 参数：t为测试执行器，原件仅来自测试夹具。
// 返回值：真实应用服务、调用者及可检查副作用的仓储和网关替身。
func newCRMContextPreview(t *testing.T) (*Service, Principal, *importPreviewRepository, *importPreviewGateway) {
	t.Helper()
	pdf := testfixture.PDF("CRM context recovery")
	source, err := contractdocument.ValidateExternalSource("one.pdf", "application/pdf", pdf)
	if err != nil {
		t.Fatal(err)
	}
	scopes := allowAllScope("contract.create")
	scopes["contract.import.preview"] = contract.ScopeFilter{AllowAll: true}
	actor := Principal{TenantID: "tenant", UserID: "actor", IdentityID: "actor",
		Permissions:      map[string]bool{"contract.create": true, "contract.import.preview": true},
		PermissionScopes: scopes}
	repo := &importPreviewRepository{batch: contract.ImportBatch{ID: "batch", TenantID: "tenant",
		ActorID: "actor", Revision: 2, Version: 4, State: "DRAFT", SourceSystem: "legacy",
		ExpiresAt: time.Now().Add(time.Hour), Sources: []contract.ImportSource{{ID: "source",
			ItemKey: "one", Filename: "one.pdf", MediaType: "application/pdf", SHA256: source.SHA256,
			GatewayID: "object", State: "READY"}}, Items: []contract.ImportItemState{{ID: "item",
			Item: contract.ImportItem{Key: "one", ExternalRecordID: "old-1", Mode: contract.ImportNormal,
				SourceFileRef: "source", Metadata: contract.ImportMetadata{Title: "预检测试",
					ContractType: "直签", CRMCustomerID: 7, CustomerName: "历史客户", AmountMinor: 10000,
					Currency: "CNY", ServiceItems: []contract.ServiceItem{{ServiceType: "软件测试",
						Site: "测试场所", Batch: "测试批次", Category: "软件测试", TestMode: "STANDARD"}}}}}}}}
	gateway := &importPreviewGateway{body: pdf}
	s := &Service{Repo: repo, StampedFileGateway: gateway, ExternalPDFEnabled: true,
		ContractImportEnabled: true, DetectionCategories: enabledDetectionCategories("软件测试")}
	return s, actor, repo, gateway
}
