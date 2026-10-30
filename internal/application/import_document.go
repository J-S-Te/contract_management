package application

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/j-s-te/contract-management/internal/contractdocument"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// GetImportSourceDocument 按批次创建人或受派审批权限读取同一冻结原件。
// 参数：ctx及actor为当前身份，batch/itemID限定冻结来源。
// 返回值：经原SHA核验并持久审计的文档，不要求审批人拥有普通合同读取范围。
func (s *Service) GetImportSourceDocument(ctx context.Context, actor Principal, batch, itemID string) (contract.Contract, error) {
	b, e := s.GetImportBatch(ctx, actor, batch)
	if e != nil {
		return contract.Contract{}, e
	}
	var item contract.ImportItemState
	for _, i := range b.Items {
		if i.ID == itemID {
			item = i
		}
	}
	if item.ID == "" {
		return contract.Contract{}, ErrValidation
	}
	var source contract.ImportSource
	for _, src := range b.Sources {
		if src.ID == item.Item.SourceFileRef {
			source = src
		}
	}
	if source.ID == "" || source.SHA256 == "" {
		return contract.Contract{}, ErrImportConflict
	}
	c := contract.Contract{ID: item.ID, SourceFilename: source.Filename, SourceMediaType: source.MediaType, SourceSHA256: source.SHA256}
	if item.ContractID != "" {
		c, e = s.Repo.GetContract(ctx, b.TenantID, item.ContractID)
	} else {
		if s.StampedFileGateway == nil {
			return c, ErrImportUnsupported
		}
		c.Document, e = s.StampedFileGateway.Download(ctx, source.GatewayID)
	}
	if e != nil {
		return c, e
	}
	if fmt.Sprintf("%x", sha256.Sum256(c.Document)) != source.SHA256 {
		return c, ErrImportIdentityConflict
	}
	c.DocumentFormat, e = contractdocument.EffectiveDocumentFormat(c.DocumentFormat, c.Document)
	if e != nil {
		return c, ErrValidation
	}
	if e = s.RecordDocumentAccess(ctx, actor, c.ID, "IMPORT_SOURCE_"+c.DocumentFormat); e != nil {
		return contract.Contract{}, e
	}
	return c, nil
}
