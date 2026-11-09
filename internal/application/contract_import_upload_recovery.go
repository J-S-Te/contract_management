package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/oklog/ulid/v2"
)

var (
	ErrImportSourceStorageUnavailable = errors.New("import source storage unavailable")
	ErrImportSourceUploadPending      = errors.New("import source upload still writing")
)

type importUploadResolver interface {
	ResolveUploadRequest(context.Context, string) (string, string, error)
}

// recoverImportSource 查询旧请求的权威结果，避免失败幂等身份永久阻断或未知结果重复上传。
// 参数：ctx和actor限定调用，batch及epoch为批次围栏，source为已保存的上传代次。
// 返回值：可继续的原代次或新代次；未确认终态、内容拒绝或围栏变化时返回错误。
func (s *Service) recoverImportSource(ctx context.Context, actor Principal, batch string,
	epoch uint64, source contract.ImportSource) (contract.ImportSource, error) {
	resolver, ok := s.StampedFileGateway.(importUploadResolver)
	if !ok {
		return source, nil
	}
	fileID, status, err := resolver.ResolveUploadRequest(ctx, "contract-import-"+source.ID)
	if err != nil {
		return source, fmt.Errorf("%w: %v", ErrImportSourceStorageUnavailable, err)
	}
	if source.GatewayID != "" && source.GatewayID != fileID {
		return source, ErrImportConflict
	}
	switch status {
	case "WRITING":
		return source, ErrImportSourceUploadPending
	case "REJECTED":
		return source, &ImportSourceRejectedError{ReasonCode: "IMPORT_SOURCE_STORAGE_REJECTED",
			Reason: "原件被文件存储校验拒绝，请核对并更换原件后重新暂存。", Filename: source.Filename}
	case "CANCELLED":
		return source, ErrImportConflict
	case "READY", "FAILED":
		if fileID == "" {
			return source, ErrImportSourceStorageUnavailable
		}
	case "NOT_FOUND":
	default:
		return source, ErrImportSourceStorageUnavailable
	}
	return s.applyImportUploadResolution(ctx, actor, batch, epoch, source, fileID, status)
}

// applyImportUploadResolution 在批次锁内核对查询围栏并保存确定的上传结果。
// 参数：ctx及actor限定访问，batch/epoch/source限定原代次，fileID/status为权威网关回执。
// 返回值：可继续上传或绑定的代次；失败代次保留以供既有清理流程精确退役。
func (s *Service) applyImportUploadResolution(ctx context.Context, actor Principal, batch string,
	epoch uint64, source contract.ImportSource, fileID, status string) (contract.ImportSource, error) {
	r, err := s.importStore()
	if err != nil {
		return source, err
	}
	err = r.UpdateImportBatch(ctx, actor.TenantID, batch, func(b *contract.ImportBatch) error {
		if err := importAccess(actor, *b, "contract.import.preview"); err != nil {
			return err
		}
		if b.Epoch != epoch || !b.ExpiresAt.After(time.Now().UTC()) {
			return ErrImportConflict
		}
		if err := editableImport(b, b.Version); err != nil {
			return err
		}
		for i, old := range b.Sources {
			if old.ID != source.ID {
				continue
			}
			if old.State != "UPLOADING" {
				return ErrImportConflict
			}
			if status == "FAILED" {
				if len(b.Sources) >= 300 {
					return &ImportSourceRejectedError{
						ReasonCode: CodeImportSourceLimit, Reason: "每批暂存原件已达上限 300 份", Filename: source.Filename}
				}
				b.Sources[i].State, b.Sources[i].GatewayID = "FAILED", fileID
				source.ID, source.GatewayID = ulid.Make().String(), ""
				b.Sources = append(b.Sources, source)
				b.Revision++
				b.Epoch++
				b.PreviewToken = ""
				b.State = "DRAFT"
			} else if status == "READY" {
				source.GatewayID = fileID
				b.Sources[i] = source
			}
			return nil
		}
		return ErrImportConflict
	})
	return source, err
}
