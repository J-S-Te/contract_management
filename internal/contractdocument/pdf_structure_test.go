package contractdocument

import (
	"errors"
	"testing"
)

// TestForbiddenPDFNameRejectsActiveActions 固定禁止名单：脚本、嵌入、启动、加密之外，
// 外部跳转、表单提交、附加/打开动作与多媒体动作同样必须被拒绝。
// 参数：t为名称准入单测上下文。
// 返回值：无；任一禁止动作被放行或普通字典名被误拒绝时失败。
func TestForbiddenPDFNameRejectsActiveActions(t *testing.T) {
	for _, name := range []string{
		"JavaScript", "JS", "EmbeddedFile", "EmbeddedFiles", "Launch", "Encrypt",
		"URI", "GoToR", "GoToE", "SubmitForm", "ImportData", "AA", "OpenAction",
		"Rendition", "Sound", "Movie",
	} {
		if !forbiddenPDFName(name) {
			t.Fatalf("禁止动作 %q 被放行", name)
		}
	}
	// 名称转义按解码后的值判定（#41 为 'A'），避免用转义绕过名单。
	if !forbiddenPDFName("Open#41ction") {
		t.Fatal("转义形式的 OpenAction 被放行")
	}
	for _, name := range []string{"Font", "Pages", "MediaBox", "Type", "PieceInfo", "WPS_ICV", "Resources", "Annots"} {
		if forbiddenPDFName(name) {
			t.Fatalf("普通字典名 %q 被误拒绝", name)
		}
	}
}

// TestExternalPDFURIAndMultimediaActionsRejected 验证真实PDF中的外部跳转与多媒体动作按活动内容拒绝。
// 参数：t为离线准入测试上下文。
// 返回值：无；外部跳转或多媒体动作被接受、或原因码漂移时失败。
func TestExternalPDFURIAndMultimediaActionsRejected(t *testing.T) {
	for _, sample := range []struct {
		name, catalog string
	}{
		{"uri-action", "<< /Type /Catalog /Pages 2 0 R /OpenAction << /S /URI /URI (https://example.com) >> >>"},
		{"movie-action", "<< /Type /Catalog /Pages 2 0 R /OpenAction << /S /Movie /Movie 4 0 R >> >>"},
		{"additional-actions", "<< /Type /Catalog /Pages 2 0 R /AA << /O << /S /GoToE >> >> >>"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			body := admissionPDF([]string{
				sample.catalog,
				"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
				"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 5 0 R >>",
				"<< /Length 0 >>\nstream\n\nendstream",
			}, "")
			_, err := ValidateExternalSource("action.pdf", "application/pdf", body)
			var rejection *RejectionError
			if !errors.As(err, &rejection) || rejection.Code != CodePDFActiveContent {
				t.Fatalf("动作未被稳定原因码拒绝: %v", err)
			}
		})
	}
}
