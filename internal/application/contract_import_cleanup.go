package application

import (
	"context"
	"fmt"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"time"
)

type importCleanupStore interface {
	ListImportCleanup(context.Context, time.Time) ([]contract.ImportBatch, error)
}
type importCleanupGateway interface {
	RetireUploadRequest(context.Context, string) (string, string, error)
	ReleaseBinding(context.Context, string, string, string, string, string) error
}

// CleanupImportSources 只退役本批次的精确暂存引用，不删除合同永久引用或原件字节。
// 参数：ctx控制有限批次清理；网关负责对象保留和容量回收。
// 返回值：查询、退役或落库失败；失败项保留原身份供下轮补偿。
func (s *Service) CleanupImportSources(ctx context.Context) error {
	store, ok := s.Repo.(importCleanupStore)
	if !ok {
		return nil
	}
	gateway, ok := s.StampedFileGateway.(importCleanupGateway)
	if !ok {
		return nil
	}
	batches, e := store.ListImportCleanup(ctx, time.Now().UTC())
	if e != nil {
		return e
	}
	repo, _ := s.importStore()
	for _, b := range batches {
		if b.ExpiresAt.After(time.Now().UTC()) {
			continue
		}
		e = repo.UpdateImportBatch(ctx, b.TenantID, b.ID, func(current *contract.ImportBatch) error {
			if current.OperationID != "" && current.State != "COMPLETED" {
				return ErrImportConflict
			}
			if current.OperationID == "" {
				current.State = "EXPIRED"
				current.Epoch++
				current.PreviewToken = ""
			}
			return nil
		})
		if e != nil {
			return e
		}
		for _, source := range b.Sources {
			if source.State == "CLEANED" {
				continue
			}
			fileID, _, err := gateway.RetireUploadRequest(ctx, "contract-import-"+source.ID)
			if err != nil {
				return err
			}
			if source.GatewayID != "" && fileID != "" && source.GatewayID != fileID {
				return fmt.Errorf("import upload request identity mismatch")
			}
			if fileID == "" {
				fileID = source.GatewayID
			}
			if fileID != "" {
				if err = gateway.ReleaseBinding(ctx, s.StampedFileApplicationID, fileID, "contract_import_item", source.ID, "IMPORT_SOURCE"); err != nil {
					return err
				}
			}
			if err = repo.UpdateImportBatch(ctx, b.TenantID, b.ID, func(current *contract.ImportBatch) error {
				for i := range current.Sources {
					if current.Sources[i].ID == source.ID {
						current.Sources[i].State = "CLEANED"
						return nil
					}
				}
				return ErrImportConflict
			}); err != nil {
				return err
			}
		}
	}
	return nil
}
