package contractdocument

import (
	"crypto/sha256"
	"fmt"
	"mime"
	"path/filepath"
	"strings"

	"github.com/j-s-te/contract-management/internal/docx"
	contractpdf "github.com/j-s-te/contract-management/internal/pdf"
)

const (
	FormatDOCX    = "DOCX"
	FormatPDF     = "PDF"
	DOCXMediaType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	PDFMediaType  = "application/pdf"
	MaxPDFSize    = 20 << 20
)

type ValidatedSource struct {
	Format    string
	Filename  string
	MediaType string
	SHA256    string
	Text      string
}

// ValidateExternalSource 统一校验外部原件类型、体积和可用正文。
// 参数：filename和mediaType为上传声明，body为尚未改写的源文件字节。
// 返回值：格式、规范MIME、原件哈希及可提取正文；非法原件返回错误。
func ValidateExternalSource(filename, mediaType string, body []byte) (ValidatedSource, error) {
	if strings.ContainsAny(filename, "\x00\r\n") {
		return ValidatedSource{}, fmt.Errorf("源文件名称无效")
	}
	name := filepath.Base(strings.ReplaceAll(strings.TrimSpace(filename), "\\", "/"))
	if name == "." || len([]rune(name)) > 255 {
		return ValidatedSource{}, fmt.Errorf("源文件名称无效")
	}
	declared := ""
	if mediaType != "" {
		var err error
		declared, _, err = mime.ParseMediaType(mediaType)
		if err != nil {
			return ValidatedSource{}, fmt.Errorf("源文件MIME无效")
		}
	}
	source := ValidatedSource{Filename: name, SHA256: fmt.Sprintf("%x", sha256.Sum256(body))}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".pdf":
		source.Format, source.MediaType = FormatPDF, PDFMediaType
		if err := validatePDF(body); err != nil {
			return ValidatedSource{}, err
		}
	case ".docx":
		source.Format, source.MediaType = FormatDOCX, DOCXMediaType
		if err := docx.ValidateExternalDocument(body); err != nil {
			return ValidatedSource{}, err
		}
		text, err := docx.PlainText(body)
		if err != nil || strings.TrimSpace(text) == "" {
			return ValidatedSource{}, fmt.Errorf("外部DOCX正文不能为空")
		}
		source.Text = text
	default:
		return ValidatedSource{}, fmt.Errorf("源文件只支持DOCX或PDF")
	}
	if declared != "" && declared != "application/octet-stream" && declared != source.MediaType {
		return ValidatedSource{}, fmt.Errorf("源文件MIME与格式不一致")
	}
	return source, nil
}

// EffectiveDocumentFormat 解析明确类型并兼容旧DOCX及无文档记录。
// 参数：format为持久类型，document为当前行或同一审批快照的文档字节。
// 返回值：可信类型；未知类型、非DOCX旧字节及类型错配返回错误。
func EffectiveDocumentFormat(format string, document []byte) (string, error) {
	if format == "" && len(document) == 0 {
		return "", nil
	}
	if format == "" || format == FormatDOCX {
		if err := docx.ValidateExternalDocument(document); err != nil {
			return "", err
		}
		return FormatDOCX, nil
	}
	if format == FormatPDF {
		if err := validatePDF(document); err != nil {
			return "", err
		}
		return FormatPDF, nil
	}
	return "", fmt.Errorf("未知合同文档类型")
}

// validatePDF 对PDF执行有界信封、静态结构及危险字典检查。
// 参数：body为源PDF字节，最大20MiB；不执行内容或重写签名原件。
// 返回值：通过离线结构准入时为nil；不执行脚本、不验签或改写源字节。
func validatePDF(body []byte) error {
	if !contractpdf.Valid(body) {
		return fmt.Errorf("PDF为空、损坏或超过20MiB")
	}
	// 名称策略交给解码后的对象检查，原始流、注释和字符串可能合法包含同名字节。
	return validatePDFStructure(body)
}
