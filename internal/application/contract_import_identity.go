package application

import (
	"errors"
	"fmt"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// 来源身份冲突的两种成因：来源单号此前已导入（con_import_identity 已有行），
// 或预检后冻结基线（来源文件字节/权威字段）发生变化。
const (
	// ImportConflictKindAlreadyImported 表示同一来源单号已有导入身份记录且摘要不同。
	ImportConflictKindAlreadyImported = "ALREADY_IMPORTED"
	// ImportConflictKindBaselineDrift 表示预检后来源文件或权威字段发生变化，基线已失效。
	ImportConflictKindBaselineDrift = "BASELINE_DRIFT"
	// ImportConflictKindLedgerMismatch 表示历史开立台账与批次进度自相矛盾（如台账称合同已建，
	// 批次却没有合同 ID），必须人工核账，不能猜测继续。
	ImportConflictKindLedgerMismatch = "LEDGER_MISMATCH"
)

// ImportIdentityConflictError 在 ErrImportIdentityConflict 之外携带可定位的既有合同/导入项，
// 使逐项 Issue 能告诉操作者「冲突的是哪一条」；Unwrap 保证错误分类、HTTP 映射
// 与既有 errors.Is 判定完全不变（摘要语义不动，只是把冲突说出来）。
type ImportIdentityConflictError struct {
	ContractID string
	ItemID     string
	Kind       string
}

// Error 返回与 ErrImportIdentityConflict 一致的稳定错误码文本。
// 参数：无。
// 返回值：稳定错误码文本。
func (e *ImportIdentityConflictError) Error() string { return ErrImportIdentityConflict.Error() }

// Unwrap 让 errors.Is(err, ErrImportIdentityConflict) 继续成立。
// 参数：无。
// 返回值：哨兵错误。
func (e *ImportIdentityConflictError) Unwrap() error { return ErrImportIdentityConflict }

// importIdentityConflictIssue 把来源身份冲突投影为可行动的逐项 Issue。
// 参数：err 为执行期或预检期错误，field 为该链路约定的字段路径
// （执行期 "commit"，预检沿用 "metadata"）。
// 返回值：Issue 及是否为身份冲突；非冲突错误返回 false，由调用方按原分类处理。
func importIdentityConflictIssue(err error, field string) (contract.ImportIssue, bool) {
	var conflict *ImportIdentityConflictError
	if !errors.As(err, &conflict) {
		return contract.ImportIssue{}, false
	}
	return contract.ImportIssue{Field: field, Code: importErrorCode(err), Retryable: false,
		Message:    importIdentityConflictMessage(conflict),
		ContractID: conflict.ContractID, ItemID: conflict.ItemID, ConflictKind: conflict.Kind}, true
}

// importIdentityConflictMessage 生成可操作提示。
// 参数：conflict 为带定位信息的来源身份冲突。
// 返回值：只含平台内部 ID 与静态说明的提示，不回显来源文件内容或外部系统原文。
func importIdentityConflictMessage(conflict *ImportIdentityConflictError) string {
	if conflict.Kind == ImportConflictKindAlreadyImported {
		if conflict.ContractID != "" {
			return fmt.Sprintf("该来源单号此前已导入为合同 %s（导入项 %s），不会重复建单；请核对原合同并走更正流程。",
				conflict.ContractID, conflict.ItemID)
		}
		return "该来源单号已有导入身份记录，但未关联合同；请核对导入台账后再处理。"
	}
	if conflict.Kind == ImportConflictKindLedgerMismatch {
		return "CRM 开立台账与批次进度不一致（台账称合同/商机已建，但批次缺少合同 ID）；请核对开立记录后再处理。"
	}
	return "预检后来源文件或权威字段（客户/商机/分类等）发生变化，请重新预检后再提交。"
}
