package mysql

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/j-s-te/contract-management/internal/contractdocument"
	"github.com/j-s-te/contract-management/internal/docx"
)

// approvedNumberUpdates 仅为DOCX回写正文编号，PDF保持原字节与哈希。
// 参数：current为审批锁定的合同行，number为本次正式系统编号。
// 返回值：应更新的字段；未知类型或无效文档返回错误，事务不得完成。
func approvedNumberUpdates(current contractRecord, number string) (map[string]any, error) {
	updates := map[string]any{"contract_number": number}
	if len(current.RenderedDocument) == 0 {
		return updates, nil
	}
	format, err := contractdocument.EffectiveDocumentFormat(current.DocumentFormat, current.RenderedDocument)
	if err != nil {
		return nil, err
	}
	if format == contractdocument.FormatPDF {
		return updates, nil
	}
	document, err := docx.ReplaceSystemContractNumber(current.RenderedDocument, number)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(document, current.RenderedDocument) {
		return updates, nil
	}
	content, err := docx.PlainText(document)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(document)
	updates["rendered_document"], updates["content"], updates["content_hash"] = document, content, hex.EncodeToString(hash[:])
	if len(current.TemplateValuesJSON) > 0 {
		values := map[string]string{}
		if err := json.Unmarshal(current.TemplateValuesJSON, &values); err != nil {
			return nil, err
		}
		for _, key := range []string{"合同编号", "系统合同编号"} {
			if _, ok := values[key]; ok {
				values[key] = number
			}
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			return nil, err
		}
		updates["template_values_json"] = encoded
	}
	return updates, nil
}
