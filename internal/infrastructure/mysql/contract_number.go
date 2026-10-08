package mysql

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/j-s-te/contract-management/internal/docx"
)

// Use the approved snapshot, never a template that an administrator may have replaced.
func approvedNumberUpdates(current contractRecord, number string) (map[string]any, error) {
	updates := map[string]any{"contract_number": number}
	if len(current.RenderedDocument) == 0 {
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
