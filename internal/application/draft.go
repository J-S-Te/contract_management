package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

type DraftRepository interface {
	UpdateContractDraft(context.Context, contract.Contract, uint64, string) error
}

func draftActorAllowed(actor Principal, c contract.Contract) bool {
	if actor.TenantID != c.TenantID || actor.UserID == "" {
		return false
	}
	for _, role := range actor.Roles {
		if role == "admin" {
			return true
		}
	}
	for _, role := range actor.Roles {
		if role == "sales" {
			return c.CreatedBy != "" && c.CreatedBy == actor.UserID
		}
	}
	return false
}

// CanEditDraft 判断当前主体是否可编辑普通来源草稿，历史基线禁止普通编辑。
// 参数：actor为当前可信主体，c为目标合同。
// 返回值：可编辑返回true，否则返回false。
func (s *Service) CanEditDraft(actor Principal, c contract.Contract) bool {
	filter, ok := actor.Scope("contract.edit")
	return ok && contract.OperationalOrigin(c) && c.Status == contract.StatusDraft && canAccessContract(filter, c) && draftActorAllowed(actor, c)
}

// UpdateContractDraft 编辑普通草稿元数据并保留外部原件与来源。
// 参数：ctx为请求上下文，actor为操作者，id及expectedVersion限定合同，input为新元数据。
// 返回值：更新后合同；权限、版本、原件财务冻结或存储冲突返回错误。
func (s *Service) UpdateContractDraft(ctx context.Context, actor Principal, id string, expectedVersion uint64, input contract.Contract) (contract.Contract, error) {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return contract.Contract{}, err
	}
	filter, ok := actor.Scope("contract.edit")
	if !ok {
		return contract.Contract{}, ErrForbidden
	}
	current, err := s.getContractScoped(ctx, filter, id)
	if err != nil {
		return current, err
	}
	if !contract.OperationalOrigin(current) {
		return current, ErrImportCorrectionRequired
	}
	if !draftActorAllowed(actor, current) {
		return current, ErrForbidden
	}
	if current.Status != contract.StatusDraft {
		return current, apperrors.ErrStateConflict
	}
	if expectedVersion == 0 {
		return current, ErrValidation
	}
	if expectedVersion != current.Version {
		return current, apperrors.ErrVersionConflict
	}
	if input.Number != "" || input.TemplateID != current.TemplateID {
		return current, ErrValidation
	}
	// 调用方提供的服务源 ID 必须属于此草稿，且不能
	// 彼此重复。新条目在交付数据流派生 ID 前没有 ID。
	owned := map[string]bool{}
	for _, item := range current.ServiceItems {
		if item.SourceID != "" {
			owned[item.SourceID] = true
		}
	}
	seen := map[string]bool{}
	for _, item := range input.ServiceItems {
		if item.SourceID != "" && (!owned[item.SourceID] || seen[item.SourceID]) {
			return current, ErrValidation
		}
		seen[item.SourceID] = item.SourceID != ""
	}
	input.OwnerIdentityID, input.OwnerOrgID, input.ProjectID = current.OwnerIdentityID, current.OwnerOrgID, current.ProjectID
	// 旧客户端会省略新字段。保留已存储计划，并根据编辑后的
	// 金额重新校验；显式空数组只能清空草稿中的计划。
	if input.PaymentTerms == nil {
		if len(current.PaymentTerms) > 0 && !strings.EqualFold(strings.TrimSpace(input.Currency), strings.TrimSpace(current.Currency)) {
			return current, ErrValidation
		}
		input.PaymentTerms = current.PaymentTerms
	}
	external := current.TemplateID == ""
	if external {
		// 元数据编辑不重写外部编写的 DOCX。冻结其
		// 财务条款，防止 API 调用方修改审批金额，
		// 却保留内容矛盾的源文档。
		if input.AmountMinor != current.AmountMinor || strings.ToUpper(strings.TrimSpace(input.Currency)) != strings.ToUpper(current.Currency) {
			return current, ErrValidation
		}
		if input.Content != "" && input.Content != current.Content {
			return current, ErrValidation
		}
		input.Content, input.Document = current.Content, current.Document
		input.DocumentFormat, input.SourceMediaType = current.DocumentFormat, current.SourceMediaType
		input.SourceFilename, input.SourceSHA256 = current.SourceFilename, current.SourceSHA256
		if len(input.TemplateValues) != 0 {
			return current, ErrValidation
		}
	}
	prepared, err := s.prepareContract(ctx, actor, input, external, "contract.edit")
	if err != nil {
		return current, err
	}
	prepared.ID, prepared.TenantID, prepared.Number, prepared.NumberFormat = current.ID, current.TenantID, current.Number, current.NumberFormat
	prepared.OwnerIdentityID, prepared.OwnerOrgID, prepared.ProjectID = current.OwnerIdentityID, current.OwnerOrgID, current.ProjectID
	prepared.OwnerUserID, prepared.OwnerDisplayName = current.OwnerUserID, current.OwnerDisplayName
	prepared.CreatedAt, prepared.CreatedBy, prepared.Status = current.CreatedAt, current.CreatedBy, current.Status
	prepared.SourceFileID, prepared.SourceFileStatus = current.SourceFileID, current.SourceFileStatus
	prepared.ExternalPaperNumber, prepared.OriginKind = current.ExternalPaperNumber, current.OriginKind
	prepared.ImportMode, prepared.OriginImportItemID = current.ImportMode, current.OriginImportItemID
	prepared.ImportPolicy = current.ImportPolicy
	source := []byte(prepared.Content)
	if len(prepared.Document) > 0 {
		source = prepared.Document
	}
	hash := sha256.Sum256(source)
	prepared.ContentHash = hex.EncodeToString(hash[:])
	repository, ok := s.Repo.(DraftRepository)
	if !ok {
		return current, apperrors.ErrStateConflict
	}
	if err := s.checkBusinessLicense(ctx); err != nil {
		return contract.Contract{}, err
	}
	if err := repository.UpdateContractDraft(ctx, prepared, expectedVersion, actor.UserID); err != nil {
		return current, err
	}
	prepared.Version = expectedVersion + 1
	prepared.UpdatedAt = time.Now().UTC()
	prepared.CanEditDraft = s.CanEditDraft(actor, prepared)
	return prepared, nil
}
