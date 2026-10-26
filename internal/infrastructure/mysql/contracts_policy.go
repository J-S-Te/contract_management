package mysql

import (
	"fmt"
	"strings"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// validateStampedUploadPolicy 保持上传策略可独立测试：归档合同不得直接覆盖；
// 已完成签署的合同必须走带原因的正式更换流程。真正的并发保护由事务内行锁提供。
func validateStampedUploadPolicy(status contract.Status, signingStatus contract.SigningStatus, hasSigning bool, replacementReason string) error {
	if status == contract.StatusArchived {
		return fmt.Errorf("%w: 已归档合同不能直接更换盖章文件，请先按归档恢复/重审流程处理", apperrors.ErrStateConflict)
	}
	if hasSigning && signingStatus == contract.SigningCompleted && strings.TrimSpace(replacementReason) == "" {
		return fmt.Errorf("%w: 已确认签署的合同必须通过正式版本更换流程重新核验", apperrors.ErrStateConflict)
	}
	return nil
}
