package contractdocument

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestExternalPDFPieceInfoWithoutModDate 验证带 /PieceInfo 但缺 /ModDate 的合法原件不再被误拒。
// 参数：t为离线准入测试上下文。
// 返回值：无；WPS 等打印管道导出形态被拒或活动内容同时被放行时失败。
func TestExternalPDFPieceInfoWithoutModDate(t *testing.T) {
	body := admissionPDFWithEmptyInfo([]string{
		"<< /Type /Catalog /Pages 2 0 R /PieceInfo 4 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 5 0 R >>",
		"<< /WPS_ICV << /LastModified (D:20260102000000+08'00') >> >>",
		"<< /Length 0 >>\nstream\n\nendstream",
	})
	source, err := ValidateExternalSource("GTSR-261122.pdf", "application/pdf", body)
	if err != nil {
		t.Fatalf("带 PieceInfo 但缺 ModDate 的合法 PDF 被误拒: %v", err)
	}
	if source.Format != FormatPDF || source.MediaType != PDFMediaType {
		t.Fatalf("规范格式被改写: %+v", source)
	}
}

// TestExternalPDFPieceInfoWithoutModDateStillRejectsActiveContent 验证放宽规范元数据后仍拦截活动内容。
// 参数：t为离线准入测试上下文。
// 返回值：无；带 JavaScript 打开动作的同类原件被接受时失败。
func TestExternalPDFPieceInfoWithoutModDateStillRejectsActiveContent(t *testing.T) {
	body := admissionPDFWithEmptyInfo([]string{
		"<< /Type /Catalog /Pages 2 0 R /PieceInfo 4 0 R " +
			"/OpenAction << /S /JavaScript /JS (app.alert\\(1\\)) >> >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 5 0 R >>",
		"<< /WPS_ICV << /LastModified (D:20260102000000+08'00') >> >>",
		"<< /Length 0 >>\nstream\n\nendstream",
	})
	_, err := ValidateExternalSource("active.pdf", "application/pdf", body)
	if err == nil {
		t.Fatal("带 JavaScript 打开动作的原件被接受")
	}
	var rejection *RejectionError
	if !errors.As(err, &rejection) || rejection.Code != CodePDFActiveContent {
		t.Fatalf("活动内容未被稳定原因码拒绝: %v", err)
	}
}

// admissionPDFWithEmptyInfo 生成文档信息字典为空、Catalog 可带 /PieceInfo 的单页PDF。
// 参数：objects为按对象号排列的静态对象；Info 字典固定追加在最后一个对象。
// 返回值：交叉引用与 trailer /Info 引用齐备的完整PDF。
func admissionPDFWithEmptyInfo(objects []string) []byte {
	infoObject := len(objects) + 1
	objects = append(objects, "<< >>")
	var output strings.Builder
	output.WriteString("%PDF-1.7\n")
	offsets := []int{0}
	for index, object := range objects {
		offsets = append(offsets, output.Len())
		fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	start := output.Len()
	fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R /Info %d 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, infoObject, start)
	return []byte(output.String())
}
