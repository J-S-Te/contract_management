package contractdocument

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/j-s-te/contract-management/internal/testfixture"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"testing"
)

// TestExternalPDFValidation 验证源PDF边界及危险内容失败关闭。
// 参数：t 为文档准入测试上下文。
// 返回值：无；误收非法文件或改变原件时测试失败。
func TestExternalPDFValidation(t *testing.T) {
	pdf := testfixture.PDF("original contract")
	validated, err := ValidateExternalSource("原件.pdf", "application/pdf", pdf)
	if err != nil {
		t.Fatal(err)
	}
	if validated.Format != "PDF" || validated.MediaType != "application/pdf" ||
		validated.SHA256 != fmt.Sprintf("%x", sha256.Sum256(pdf)) || validated.Text != "" {
		t.Fatalf("unexpected PDF source: %+v", validated)
	}
	for _, marker := range []string{"/JavaScript", "/JS", "/EmbeddedFile", "/Launch", "/Encrypt"} {
		body := []byte("%PDF-1.4\n" + marker + "\n%%EOF")
		if _, err := ValidateExternalSource("unsafe.pdf", "application/pdf", body); err == nil {
			t.Fatalf("危险标记 %q 被接受", marker)
		}
	}
	for _, input := range []struct {
		name, mime string
		body       []byte
	}{
		{"wrong.docx", "application/pdf", pdf}, {"wrong.pdf", "text/plain", pdf},
		{"broken.pdf", "application/pdf", []byte("%PDF-1.4\ntruncated")},
		{"empty.pdf", "application/pdf", nil},
	} {
		if _, err := ValidateExternalSource(input.name, input.mime, input.body); err == nil {
			t.Fatalf("非法文件被接受: %s/%s", input.name, input.mime)
		}
	}
	padding := (20 << 20) - len(pdf) - 2
	limit := testfixture.PDFWithPadding("original contract", padding)
	padding += (20 << 20) - len(limit)
	limit = testfixture.PDFWithPadding("original contract", padding)
	if _, err := ValidateExternalSource("limit.pdf", "application/octet-stream", limit); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateExternalSource("large.pdf", "application/pdf", append(limit, ' ')); err == nil {
		t.Fatal("超20MiB的PDF被接受")
	}
}

// TestExternalPDFRejectMalformedStructure 拒绝信封完整但没有页面树及交叉引用的伪原件。
// 参数：t为测试上下文。
// 返回值：无；不完整结构被接受时失败。
func TestExternalPDFRejectMalformedStructure(t *testing.T) {
	for _, body := range [][]byte{[]byte("%PDF-1.4\n%%EOF"), []byte("%PDF-1.4\n1 0 obj<</Type /Catalog>>endobj\n%%EOF")} {
		if _, err := ValidateExternalSource("fake.pdf", "application/pdf", body); err == nil {
			t.Fatal("伪PDF被接受")
		}
	}
}

// TestExternalPDFEncodedAndCompressedActions 验证名称转义及压缩对象流不能绕过活动内容策略。
// 参数：t为真实解析器测试上下文。
// 返回值：无；活动对象被接受或正常压缩PDF被误拒时失败。
func TestExternalPDFEncodedAndCompressedActions(t *testing.T) {
	encoded := testfixture.PDFAction("Java#53cript")
	if bytes.Contains(encoded, []byte("/JavaScript")) || bytes.Contains(encoded, []byte("/JS")) {
		t.Fatal("夹具未隐藏原始标记")
	}
	if _, err := ValidateExternalSource("encoded.pdf", "application/pdf", encoded); err == nil {
		t.Fatal("名称转义活动对象被接受")
	}
	for _, unsafe := range []bool{false, true} {
		input := testfixture.PDF("compressed normal")
		if unsafe {
			input = encoded
		}
		configuration := model.NewStatelessConfiguration()
		configuration.Offline = true
		configuration.WriteObjectStream, configuration.WriteXRefStream = true, true
		var output bytes.Buffer
		if err := api.Optimize(context.Background(), bytes.NewReader(input), &output, configuration, nil); err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(output.Bytes(), []byte("/ObjStm")) {
			t.Fatal("未生成真实压缩对象流")
		}
		_, err := ValidateExternalSource("compressed.pdf", "application/pdf", output.Bytes())
		if unsafe && err == nil {
			t.Fatal("压缩活动对象被接受")
		}
		if !unsafe && err != nil {
			t.Fatalf("普通压缩PDF被拒绝: %v", err)
		}
	}
}

// TestDocumentFormatLegacy 验证旧DOCX兼容而不会把未知旧字节猜为PDF。
// 参数：t 为历史文档兼容测试上下文。
// 返回值：无；错误格式推断或DOCX正文丢失时测试失败。
func TestDocumentFormatLegacy(t *testing.T) {
	document := documentFixture(t)
	format, err := EffectiveDocumentFormat("", document)
	if err != nil || format != "DOCX" {
		t.Fatalf("legacy format=%q error=%v", format, err)
	}
	source, err := ValidateExternalSource("外部.docx", "application/octet-stream", document)
	if err != nil || source.Text != "合同正文" {
		t.Fatalf("DOCX text=%q error=%v", source.Text, err)
	}
	if _, err := EffectiveDocumentFormat("", []byte("%PDF-1.4\n%%EOF")); err == nil {
		t.Fatal("旧无类型数据不应猜测为PDF")
	}
	if _, err := EffectiveDocumentFormat("UNKNOWN", document); err == nil {
		t.Fatal("未知类型未拒绝")
	}
	if format, err := EffectiveDocumentFormat("", nil); err != nil || format != "" {
		t.Fatalf("legacy text-only format=%q error=%v", format, err)
	}
}

// documentFixture 生成经过真实DOCX校验的最小正文包。
// 参数：t 为夹具创建测试上下文。
// 返回值：包含内容类型和正文的DOCX字节；写入失败直接终止测试。
func documentFixture(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	w := zip.NewWriter(&buffer)
	files := map[string]string{
		"[Content_Types].xml": `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`,
		"word/document.xml":   `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>合同正文</w:t></w:r></w:p></w:body></w:document>`,
	}
	for name, content := range files {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
