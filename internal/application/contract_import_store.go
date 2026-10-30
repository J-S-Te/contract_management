package application

import (
	"context"
	"errors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"time"
)

var ErrImportMixedLifecycle = contract.ErrImportMixedLifecycle
var ErrImportConflict = errors.New("IMPORT_REVISION_CONFLICT")
var ErrImportIdentityConflict = errors.New("IMPORT_SOURCE_ID_CONFLICT")
var ErrImportUnsupported = errors.New("IMPORT_POLICY_UNSUPPORTED")
var ErrImportCorrectionRequired = errors.New("IMPORT_CORRECTION_REQUIRED")
var ErrImportNeedsReview = errors.New("IMPORT_NEEDS_REVIEW")

type ContractImportStore interface {
	CreateImportBatch(context.Context, contract.ImportBatch) error
	GetImportBatch(context.Context, string, string) (contract.ImportBatch, error)
	UpdateImportBatch(context.Context, string, string, func(*contract.ImportBatch) error) error
	ClaimImportBatch(context.Context, time.Time) (contract.ImportBatch, bool, error)
	CommitImportDraft(context.Context, string, string, uint64, uint64, string, contract.Contract, string) (string, error)
}

// importStore 取得独立持久化能力，缺实现时不降级为无幂等创建。
// 参数：无；读取服务仓储。
// 返回值：批次仓储或依赖错误。
func (s *Service) importStore() (ContractImportStore, error) {
	r, ok := s.Repo.(ContractImportStore)
	if !ok {
		return nil, ErrImportUnsupported
	}
	return r, nil
}
