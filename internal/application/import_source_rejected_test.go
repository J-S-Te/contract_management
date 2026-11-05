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

// TestUploadImportSourceReportsRejectionReason 验证服务层准入拒绝带出原因码而非裸校验错误。
// 参数：t为不依赖数据库的测试上下文。
// 返回值：无；原因码、文案、文件名或errors.Is(ErrValidation)语义不符时失败。
func TestUploadImportSourceReportsRejectionReason(t *testing.T) {
	ctx := context.Background()
	actor := Principal{TenantID: "tenant", UserID: "actor", IdentityID: "actor",
		Permissions:      map[string]bool{"contract.import.preview": true},
		PermissionScopes: map[string]contract.ScopeFilter{"contract.import.preview": {AllowAll: true}}}
	pdf := testfixture.PDF("original contract")
	for _, input := range []struct {
		label, key, name, mime, code, reason string
		sources                              int
		body                                 []byte
	}{
		{label: "unsupported-type", key: "one", name: "表格.xlsx",
			code: contractdocument.CodeSourceTypeUnsupported, reason: "源文件只支持DOCX或PDF"},
		{label: "pdf-structure", key: "one", name: "one.pdf", mime: "application/pdf",
			code: contractdocument.CodePDFStructureInvalid, reason: "PDF结构损坏、加密或超出解析资源上限",
			body: []byte("%PDF-1.4\n%%EOF")},
		{label: "item-key", key: "bad key", name: "one.pdf", mime: "application/pdf",
			code: CodeImportItemKeyInvalid, reason: "单项键不合法：只允许字母、数字与 _.:-，长度 1—128",
			body: pdf},
		{label: "source-limit", key: "one", name: "one.pdf", mime: "application/pdf",
			code: CodeImportSourceLimit, reason: "每批暂存原件已达上限 300 份", sources: 300, body: pdf},
	} {
		t.Run(input.label, func(t *testing.T) {
			repository := &importPreviewRepository{}
			repository.batch = contract.ImportBatch{ID: "batch", TenantID: actor.TenantID, ActorID: actor.UserID,
				Revision: 1, Version: 1, State: "DRAFT", ExpiresAt: time.Now().Add(time.Hour)}
			for index := 0; index < input.sources; index++ {
				repository.batch.Sources = append(repository.batch.Sources,
					contract.ImportSource{ID: "source-" + input.label, ItemKey: "old", Filename: "old.pdf", State: "READY"})
			}
			gateway := &importPreviewGateway{}
			service := &Service{Repo: repository, StampedFileGateway: gateway, ExternalPDFEnabled: true, ContractImportEnabled: true}
			_, err := service.UploadImportSource(ctx, actor, "batch", 1, input.key, input.name, input.mime, input.body)
			var rejected *ImportSourceRejectedError
			if !errors.As(err, &rejected) {
				t.Fatalf("错误未携带原因码: %v", err)
			}
			if rejected.ReasonCode != input.code || rejected.Reason != input.reason {
				t.Fatalf("rejection = %+v, want code %q reason %q", rejected, input.code, input.reason)
			}
			if rejected.Filename != input.name || rejected.Retryable {
				t.Fatalf("rejection = %+v, want filename %q and no retry", rejected, input.name)
			}
			if !errors.Is(err, ErrValidation) {
				t.Fatal("拒绝未保留ErrValidation身份，既有422语义被破坏")
			}
			if gateway.writes != 0 {
				t.Fatal("被拒原件不得写入文件网关")
			}
		})
	}
}

// TestImportSourceRejectedFallback 验证未知准入错误的兜底码、可重试透传与文件名规范化。
// 参数：t为测试上下文。
// 返回值：无；兜底码缺失、路径未剥离或重试标记丢失时失败。
func TestImportSourceRejectedFallback(t *testing.T) {
	fallback := importSourceRejected(errors.New("依赖返回的原始拒绝"), "../../客户/one.pdf")
	var rejected *ImportSourceRejectedError
	if !errors.As(fallback, &rejected) {
		t.Fatalf("兜底错误类型不符: %v", fallback)
	}
	if rejected.ReasonCode != CodeSourceRejected || rejected.Reason != "依赖返回的原始拒绝" {
		t.Fatalf("fallback = %+v", rejected)
	}
	if rejected.Filename != "one.pdf" {
		t.Fatalf("filename = %q, want basename only", rejected.Filename)
	}
	retryable := importSourceRejected(contractdocument.RejectRetryable(contractdocument.CodePDFValidationBusy,
		"PDF校验繁忙，请稍后重试"), "one.pdf")
	if current := retryable.(*ImportSourceRejectedError); !current.Retryable ||
		current.ReasonCode != contractdocument.CodePDFValidationBusy || current.Filename != "one.pdf" {
		t.Fatalf("retryable = %+v", current)
	}
}
