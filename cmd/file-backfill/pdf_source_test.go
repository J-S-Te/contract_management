package main

import (
	"github.com/j-s-te/contract-management/internal/testfixture"
	"testing"
)

// TestBackfillSourcePreservesPDFType 验证补偿工具不再把PDF标成DOCX。
// 参数：t为源文件补偿元数据测试上下文。
// 返回值：无；用途、原名、MIME错配或非法文档被接受时测试失败。
func TestBackfillSourcePreservesPDFType(t *testing.T) {
	row := contractRow{ID: "source-1", Title: "纸面合同", DocumentFormat: "PDF",
		SourceFilename: "原件.pdf", RenderedDocument: testfixture.PDF("original contract")}
	purpose, filename, mediaType, err := sourceBackfillDescriptor(row)
	if err != nil || purpose != "CONTRACT_EXTERNAL_PDF_SOURCE" || filename != "原件.pdf" || mediaType != "application/pdf" {
		t.Fatalf("purpose=%s filename=%s mime=%s error=%v", purpose, filename, mediaType, err)
	}
	row.DocumentFormat = "UNKNOWN"
	if _, _, _, err := sourceBackfillDescriptor(row); err == nil {
		t.Fatal("未知格式不应补传")
	}
}
