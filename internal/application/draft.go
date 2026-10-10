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

func (s *Service) CanEditDraft(actor Principal, c contract.Contract) bool {
	filter, ok := actor.Scope("contract.edit")
	return ok && c.Status == contract.StatusDraft && canAccessContract(filter, c) && draftActorAllowed(actor, c)
}

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
	// Caller-provided service source IDs must belong to this draft and cannot
	// duplicate one another. New items have no ID until the delivery feed derives it.
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
	external := current.TemplateID == ""
	if external {
		// Metadata edits do not rewrite an externally authored DOCX. Keep its
		// financial terms frozen so an API caller cannot change approval amounts
		// while retaining a contradictory source document.
		if input.AmountMinor != current.AmountMinor || strings.ToUpper(strings.TrimSpace(input.Currency)) != strings.ToUpper(current.Currency) {
			return current, ErrValidation
		}
		if input.Content != "" && input.Content != current.Content {
			return current, ErrValidation
		}
		input.Content, input.Document = current.Content, current.Document
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
