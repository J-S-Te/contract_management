package application

import (
	"fmt"
	"strings"

	"github.com/j-s-te/contract-management/internal/contractdocument"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// prepareExternalSource 验证外部文件并冻结规范类型和原件哈希。
// 参数：c为待创建外部合同，旧内部调用缺文件名时只兼容DOCX。
// 返回值：原件非法或PDF入口未开启时返回校验错误。
func (s *Service) prepareExternalSource(c *contract.Contract) error {
	name := c.SourceFilename
	if name == "" {
		name = "external-contract.docx"
	}
	validated, err := contractdocument.ValidateExternalSource(name, c.SourceMediaType, c.Document)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if validated.Format == contractdocument.FormatPDF && !s.ExternalPDFEnabled {
		return fmt.Errorf("%w: 外部PDF源文件入口尚未开启", ErrValidation)
	}
	if c.DocumentFormat != "" && c.DocumentFormat != validated.Format {
		return fmt.Errorf("%w: 文件声明与真实格式不一致", ErrValidation)
	}
	c.DocumentFormat, c.SourceMediaType = validated.Format, validated.MediaType
	c.SourceFilename, c.SourceSHA256, c.Content = validated.Filename, validated.SHA256, validated.Text
	return nil
}

// sourceGatewayMetadata 选择源文件的正式用途和类型，兼容旧DOCX。
// 参数：c为具有持久化来源元数据的合同。
// 返回值：正式用途、文件名及MIME；不将PDF伪装成DOCX上传。
func sourceGatewayMetadata(c contract.Contract) (string, string, string) {
	if c.DocumentFormat == contractdocument.FormatPDF {
		name := c.SourceFilename
		if name == "" {
			name = "external-contract.pdf"
		}
		return "CONTRACT_EXTERNAL_PDF_SOURCE", name, contractdocument.PDFMediaType
	}
	name := c.SourceFilename
	if strings.TrimSpace(name) == "" {
		name = "external-contract.docx"
	}
	return "CONTRACT_EXTERNAL_SOURCE", name, contractdocument.DOCXMediaType
}
