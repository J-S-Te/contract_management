package mysql

import (
	"encoding/json"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// applyContractSource 将来源和文档分型写入同一权威合同行。
// 参数：row为待持久化记录，source为已通过应用层校验的合同。
// 返回值：策略序列化错误；失败时调用者不得继续写入。
func applyContractSource(row *contractRecord, source contract.Contract) error {
	if source.OriginKind == "" {
		source.OriginKind = "NORMAL"
	}
	if source.OriginKind == "NORMAL" {
		if !contract.OperationalOrigin(source) {
			return contract.ErrImportPolicy
		}
	} else if source.OriginKind != "IMPORTED" || source.ImportMode == "" || source.ImportMode == contract.ImportNormal || contract.ValidateImportPolicy(source.ImportMode, source.ImportPolicy) != nil {
		return contract.ErrImportPolicy
	}
	row.DocumentFormat, row.SourceMediaType = source.DocumentFormat, source.SourceMediaType
	row.SourceFilename, row.SourceSHA256 = source.SourceFilename, source.SourceSHA256
	row.ExternalPaperNumber, row.OriginKind = source.ExternalPaperNumber, source.OriginKind
	if row.OriginKind == "" {
		row.OriginKind = "NORMAL"
	}
	row.ImportMode, row.OriginImportItemID = string(source.ImportMode), stringPtr(source.OriginImportItemID)
	row.HistoricalStatus, row.HistoricalCustomerName = source.HistoricalStatus, source.HistoricalCustomerName
	row.HistoricalOwnerLabel = source.HistoricalOwnerLabel
	if source.ImportPolicy != nil {
		encoded, err := json.Marshal(source.ImportPolicy)
		if err != nil {
			return err
		}
		row.ImportPolicyJSON = encoded
	}
	return nil
}

// readContractSource 恢复行内来源，损坏历史策略保持不可放行状态。
// 参数：row为完整合同行，target为将交付应用层的合同。
// 返回值：无；损坏策略以未知版本表示，由业务校验失败关闭。
func readContractSource(row contractRecord, target *contract.Contract) {
	target.DocumentFormat, target.SourceMediaType = row.DocumentFormat, row.SourceMediaType
	target.SourceFilename, target.SourceSHA256 = row.SourceFilename, row.SourceSHA256
	target.ExternalPaperNumber, target.OriginKind = row.ExternalPaperNumber, row.OriginKind
	target.ImportMode, target.OriginImportItemID = contract.ImportMode(row.ImportMode), valueOrEmpty(row.OriginImportItemID)
	target.HistoricalStatus, target.HistoricalCustomerName = row.HistoricalStatus, row.HistoricalCustomerName
	target.HistoricalOwnerLabel = row.HistoricalOwnerLabel
	if len(row.ImportPolicyJSON) > 0 {
		if err := json.Unmarshal(row.ImportPolicyJSON, &target.ImportPolicy); err != nil {
			target.ImportPolicy = &contract.ImportPolicy{Version: -1}
		}
	}
}
