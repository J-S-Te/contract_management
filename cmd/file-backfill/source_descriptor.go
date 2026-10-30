package main

import (
	"crypto/sha256"
	"fmt"

	"github.com/j-s-te/contract-management/internal/contractdocument"
)

// sourceBackfillDescriptor 按持久类型选择正式源文件补偿用途。
// 参数：row为限定租户内的合同源记录，PDF原件哈希已知时必须一致。
// 返回值：用途、原名、MIME；非法类型或损坏原件返回错误且不上传。
func sourceBackfillDescriptor(row contractRow) (string, string, string, error) {
	format, err := contractdocument.EffectiveDocumentFormat(row.DocumentFormat, row.RenderedDocument)
	if err != nil {
		return "", "", "", err
	}
	name := row.SourceFilename
	if format == contractdocument.FormatPDF {
		if row.SourceSHA256 != "" && row.SourceSHA256 != fmt.Sprintf("%x", sha256.Sum256(row.RenderedDocument)) {
			return "", "", "", fmt.Errorf("PDF source SHA-256 mismatch")
		}
		if name == "" {
			name = safeName(row.Title, row.ID) + ".pdf"
		}
		return "CONTRACT_EXTERNAL_PDF_SOURCE", name, contractdocument.PDFMediaType, nil
	}
	if format != contractdocument.FormatDOCX {
		return "", "", "", fmt.Errorf("source document missing")
	}
	if name == "" {
		name = safeName(row.Title, row.ID) + ".docx"
	}
	return "CONTRACT_EXTERNAL_SOURCE", name, contractdocument.DOCXMediaType, nil
}
