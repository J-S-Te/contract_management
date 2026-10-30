package application

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/j-s-te/contract-management/internal/contractdocument"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/oklog/ulid/v2"
	"strings"
	"time"
	"unicode/utf8"
)

var importBatchReadPermissions = []string{
	"contract.import.preview", "contract.import.commit", "contract.import.release",
}

// ImportBatchListScope 汇总批次操作读取范围，审批权限仍只允许当前受派记录。
// 参数：actor为当前可信权限及范围，不从前端角色名推断。
// 返回值：是否可读取租户全范围以及是否具备候选列表访问权。
func ImportBatchListScope(actor Principal) (all bool, allowed bool) {
	for _, permission := range importBatchReadPermissions {
		scope, ok := actor.Scope(permission)
		if ok && (scope.AllowAll || scope.AllowSelf) {
			allowed = true
			all = all || scope.AllowAll
		}
	}
	if _, approve := actor.Scope("contract.import.approve"); approve {
		allowed = true
	}
	return
}

// importAccess 同时执行导入权限、业务范围及真实创建人约束。
// 参数：p为当前鉴权结果，b为目标批次，permission为操作权限。
// 返回值：越权或缺少范围时返回禁止错误。
func importAccess(p Principal, b contract.ImportBatch, permission string) error {
	scope, ok := p.Scope(permission)
	if !ok || p.TenantID != b.TenantID || (!scope.AllowAll && (!scope.AllowSelf || p.UserID != b.ActorID)) {
		return ErrForbidden
	}
	return nil
}

// CreateImportBatch 建立空台账，不创建任何业务合同。
// 参数：ctx为上下文，actor为当前身份，source为受控来源系统键。
// 返回值：空批次或校验、许可及存储错误。
func (s *Service) CreateImportBatch(ctx context.Context, actor Principal, source string) (contract.ImportBatch, error) {
	b := contract.ImportBatch{ID: ulid.Make().String(), TenantID: actor.TenantID, ActorID: actor.UserID,
		SourceSystem: source, Revision: 1, Version: 1, State: "DRAFT", Items: []contract.ImportItemState{}, Sources: []contract.ImportSource{}}
	if !s.ContractImportEnabled {
		return b, ErrImportUnsupported
	}
	if e := importAccess(actor, b, "contract.import.preview"); e != nil {
		return b, e
	}
	if len(source) > 64 || !validImportKey(source) {
		return b, ErrValidation
	}
	if e := s.checkBusinessLicense(ctx); e != nil {
		return b, e
	}
	ttl := s.ContractImportStagingTTL
	if ttl == 0 {
		ttl = 168 * time.Hour
	}
	b.ExpiresAt = time.Now().UTC().Add(ttl)
	r, e := s.importStore()
	if e != nil {
		return b, e
	}
	return b, r.CreateImportBatch(ctx, b)
}

// validImportKey 限定清单键为规范可追溯的ASCII身份。
// 参数：v为来源或单项键。
// 返回值：长度及字符均合法时为true。
func validImportKey(v string) bool {
	if len(v) == 0 || len(v) > 128 {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_.:-", c)) {
			return false
		}
	}
	return true
}

// GetImportBatch 返回受当前数据范围保护的批次台账。
// 参数：ctx为上下文，actor为当前身份，id为批次ID。
// 返回值：台账或权限及数据库错误。
func (s *Service) GetImportBatch(ctx context.Context, actor Principal, id string) (contract.ImportBatch, error) {
	r, e := s.importStore()
	if e != nil {
		return contract.ImportBatch{}, e
	}
	b, e := r.GetImportBatch(ctx, actor.TenantID, id)
	if e != nil {
		return b, e
	}
	for _, permission := range importBatchReadPermissions {
		if importAccess(actor, b, permission) == nil {
			return b, nil
		}
	}
	return b, importConfirmationAccess(actor, b)
}

// editableImport 确保在途和已建单基线不能被编辑覆盖。
// 参数：b为持锁批次，version为调用方读取版本。
// 返回值：状态、有效期或版本冲突。
func editableImport(b *contract.ImportBatch, version uint64) error {
	if b.Version != version || !b.ExpiresAt.After(time.Now().UTC()) {
		return ErrImportConflict
	}
	if b.Confirmation != nil {
		if b.Confirmation.State != "WITHDRAWN" && b.Confirmation.State != "REJECTED" && b.Confirmation.State != "EXPIRED" {
			return ErrImportConflict
		}
		b.ConfirmationHistory = append(b.ConfirmationHistory, *b.Confirmation)
		b.Confirmation = nil
		b.OperationID, b.CommitKeyHash = "", ""
		b.CommittedRevision = 0
		b.SessionHash = nil
		b.State = "DRAFT"
		b.Epoch++
	}
	if b.State == "QUEUED" || b.State == "RUNNING" || b.State == "COMPLETED" || b.State == "EXPIRED" {
		return ErrImportConflict
	}
	return nil
}

// UploadImportSource 使用稳定请求身份暂存原件，并保留所有代次供精确清理。
// 参数：ctx、actor及batch限定访问；version为批次版本，key/name/media/body为一份原件。
// 返回值：不透明file_ref或上传、绑定、围栏错误。
func (s *Service) UploadImportSource(ctx context.Context, actor Principal, batch string, version uint64, key, name, media string, body []byte) (source contract.ImportSource, err error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if !s.ContractImportEnabled || s.StampedFileGateway == nil {
		return source, ErrImportUnsupported
	}
	if !validImportKey(key) {
		return source, ErrValidation
	}
	validated, e := contractdocument.ValidateExternalSource(name, media, body)
	if e != nil {
		return source, ErrValidation
	}
	if validated.Format == "PDF" && !s.ExternalPDFEnabled {
		return source, ErrImportUnsupported
	}
	if e = s.checkBusinessLicense(ctx); e != nil {
		return source, e
	}
	r, e := s.importStore()
	if e != nil {
		return source, e
	}
	e = r.UpdateImportBatch(ctx, actor.TenantID, batch, func(b *contract.ImportBatch) error {
		if e := importAccess(actor, *b, "contract.import.preview"); e != nil {
			return e
		}
		if e := editableImport(b, version); e != nil {
			return e
		}
		for _, item := range b.Items {
			if item.Item.Key == key && item.ContractID != "" {
				return ErrImportCorrectionRequired
			}
		}
		for _, old := range b.Sources {
			if old.ItemKey == key && old.SHA256 == validated.SHA256 && old.Filename == validated.Filename {
				source = old
				return nil
			}
		}
		if len(b.Sources) >= 300 {
			return ErrValidation
		}
		source = contract.ImportSource{ID: ulid.Make().String(), ItemKey: key, Filename: validated.Filename,
			MediaType: validated.MediaType, SHA256: validated.SHA256, State: "UPLOADING"}
		b.Sources = append(b.Sources, source)
		b.Revision++
		b.PreviewToken = ""
		b.State = "DRAFT"
		b.Epoch++
		return nil
	})
	if e != nil {
		return source, e
	}
	if source.State == "READY" {
		return source, nil
	}
	fileID, e := s.StampedFileGateway.Upload(ctx, "contract-import-"+source.ID, s.StampedFileApplicationID,
		"CONTRACT_IMPORT_SOURCE", source.Filename, source.MediaType, bytes.NewReader(body))
	if e != nil {
		return source, e
	}
	if e = s.StampedFileGateway.Bind(ctx, s.StampedFileApplicationID, fileID, "contract_import_item", source.ID, "IMPORT_SOURCE", source.Filename); e != nil {
		return source, e
	}
	source.GatewayID, source.State = fileID, "READY"
	e = r.UpdateImportBatch(ctx, actor.TenantID, batch, func(b *contract.ImportBatch) error {
		// 上传回执只改变原代次，不覆盖迟到期间已更新的文件或清单。
		if b.State == "EXPIRED" || !b.ExpiresAt.After(time.Now().UTC()) {
			return ErrImportConflict
		}
		for i := range b.Sources {
			if b.Sources[i].ID == source.ID {
				if b.Sources[i].State == "CLEANED" {
					return ErrImportConflict
				}
				b.Sources[i] = source
				return nil
			}
		}
		return ErrImportConflict
	})
	if e != nil {
		if cleanup, ok := s.StampedFileGateway.(importCleanupGateway); ok {
			finish, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer stop()
			if cleanupErr := cleanup.ReleaseBinding(finish, s.StampedFileApplicationID, fileID, "contract_import_item", source.ID, "IMPORT_SOURCE"); cleanupErr != nil {
				return source, fmt.Errorf("%w: staging compensation failed", e)
			}
		}
	}
	return source, e
}

// SetImportManifest 保存白名单清单并使旧预检失效。
// 参数：ctx、actor、id限定批次，version为读取版本，m为严格解码的清单。
// 返回值：校验、版本或存储错误。
func (s *Service) SetImportManifest(ctx context.Context, actor Principal, id string, version uint64, m contract.ImportManifest) error {
	if !s.ContractImportEnabled {
		return ErrImportUnsupported
	}
	max := s.ContractImportMaxItems
	if max == 0 {
		max = 100
	}
	if e := m.Validate(max); e != nil {
		if errors.Is(e, contract.ErrImportMixedLifecycle) {
			return ErrImportMixedLifecycle
		}
		return ErrValidation
	}
	r, e := s.importStore()
	if e != nil {
		return e
	}
	correctionRequired := false
	e = r.UpdateImportBatch(ctx, actor.TenantID, id, func(b *contract.ImportBatch) error {
		if e := importAccess(actor, *b, "contract.import.preview"); e != nil {
			return e
		}
		if e := editableImport(b, version); e != nil {
			return e
		}
		if m.SourceSystem != b.SourceSystem {
			return ErrValidation
		}
		previous := map[string]contract.ImportItemState{}
		for _, item := range b.Items {
			previous[item.Item.Key] = item
		}
		items := make([]contract.ImportItemState, 0, len(m.Items))
		for _, item := range m.Items {
			found := false
			for _, src := range b.Sources {
				if src.ID == item.SourceFileRef && src.ItemKey == item.Key && src.State == "READY" {
					found = true
				}
			}
			if !found {
				return ErrValidation
			}
			old, exists := previous[item.Key]
			if exists && old.ContractID != "" {
				oldHash, _ := contract.ImportDigest(old.Item)
				newHash, _ := contract.ImportDigest(item)
				if oldHash != newHash {
					markImportCorrection(b, old.ID)
					correctionRequired = true
					return nil
				}
				items = append(items, old)
			} else {
				identity := ulid.Make().String()
				if exists {
					identity = old.ID
				}
				items = append(items, contract.ImportItemState{ID: identity, Item: item, SourceRevision: b.Revision + 1, State: "PENDING", Issues: []contract.ImportIssue{}})
			}
			delete(previous, item.Key)
		}
		for _, old := range previous {
			if old.ContractID != "" {
				markImportCorrection(b, old.ID)
				correctionRequired = true
				return nil
			}
		}
		b.Items = items
		b.Epoch++
		b.Revision++
		b.PreviewToken = ""
		b.State = "DRAFT"
		return nil
	})
	if e != nil {
		return e
	}
	if correctionRequired {
		return ErrImportCorrectionRequired
	}
	return nil
}

// markImportCorrection 保留原合同基线，只标识需要业务更正的提议。
// 参数：b为持锁批次，itemID为不得被覆盖的已建单项。
// 返回值：无；不改变清单修订或已创建合同。
func markImportCorrection(b *contract.ImportBatch, itemID string) {
	b.State = "PARTIAL"
	b.Epoch++
	b.PreviewToken = ""
	for i := range b.Items {
		if b.Items[i].ID == itemID {
			b.Items[i].State = "CORRECTION_REQUIRED"
			b.Items[i].Issues = []contract.ImportIssue{{Field: "metadata", Code: "IMPORT_CORRECTION_REQUIRED"}}
			return
		}
	}
}

// prepareImportItem 读取暂存原件并复用普通合同的实时目录和字段校验。
// 参数：ctx为上下文，actor为当前身份，b为冻结批次，item为待校验项。
// 返回值：规范合同、规范摘要或错误；不创建合同。
func (s *Service) prepareImportItem(ctx context.Context, actor Principal, b contract.ImportBatch, item contract.ImportItemState) (c contract.Contract, digest string, err error) {
	if item.Item.Mode != contract.ImportNormal {
		if (!s.ContractImportHistoryEnabled && b.Confirmation == nil) || contract.ValidateHistoryEvidence(item.Item) != nil {
			return c, "", ErrImportUnsupported
		}
		if b.Confirmation == nil && !s.importHistoryModeAvailable(item.Item.Mode) {
			return c, "", ErrImportUnsupported
		}
		if _, e := s.importHistoryOwner(ctx, actor, item.Item); e != nil {
			return c, "", e
		}
	}
	var source contract.ImportSource
	for _, src := range b.Sources {
		if src.ID == item.Item.SourceFileRef {
			source = src
		}
	}
	if source.State != "READY" || s.StampedFileGateway == nil {
		return c, "", ErrImportUnsupported
	}
	body, e := s.StampedFileGateway.Download(ctx, source.GatewayID)
	if e != nil {
		return c, "", e
	}
	c = item.Item.Metadata.Contract()
	c.Document, c.SourceFilename, c.SourceMediaType = body, source.Filename, source.MediaType
	validator := *s
	if b.OperationID != "" {
		validator.ExternalPDFEnabled = true
	}
	if e = validator.prepareExternalSource(&c); e != nil {
		return c, "", e
	}
	if c.SourceSHA256 != source.SHA256 {
		return c, "", ErrImportIdentityConflict
	}
	if utf8.RuneCountInString(c.ExternalPaperNumber) > 128 {
		return c, "", ErrValidation
	}
	c, e = s.prepareContract(ctx, actor, c, true, "contract.create")
	if e != nil {
		return c, "", e
	}
	if item.Item.Metadata.ServiceType != "" && item.Item.Metadata.ServiceType != c.ServiceType {
		return c, "", ErrValidation
	}
	if len(item.Item.Metadata.Systems) > 0 {
		declared, _ := contract.ImportDigest(item.Item.Metadata.Systems)
		actual, _ := contract.ImportDigest(c.Systems)
		if declared != actual {
			return c, "", ErrValidation
		}
	}
	if c.StartDate != nil {
		normalized := c.StartDate.UTC()
		c.StartDate = &normalized
	}
	if c.EndDate != nil {
		normalized := c.EndDate.UTC()
		c.EndDate = &normalized
	}
	baseline := c
	baseline.Content = "" // 摘要只绑定源SHA；合同仍保留DOCX提取正文。
	digest, e = contract.ImportDigest(struct {
		Version                                   int
		SourceSystem, ExternalRecordID, SourceSHA string
		Metadata                                  contract.Contract
		History                                   *contract.ImportHistoryEvidence
		Policy                                    *contract.ImportPolicy
		Mode                                      contract.ImportMode
	}{1, b.SourceSystem, item.Item.ExternalRecordID, source.SHA256, baseline, item.Item.HistoryEvidence, item.Item.Policy, item.Item.Mode})
	return c, digest, e
}

// PreviewImport 冻结指定revision的预检结果和时效令牌。
// 参数：ctx及actor为当前鉴权，id/revision限定清单代次。
// 返回值：逐项结果或版本、目录与权限错误。
func (s *Service) PreviewImport(ctx context.Context, actor Principal, id string, revision uint64) (contract.ImportBatch, error) {
	b, e := s.GetImportBatch(ctx, actor, id)
	if e != nil {
		return b, e
	}
	if e = importAccess(actor, b, "contract.import.preview"); e != nil {
		return b, e
	}
	if !s.ContractImportEnabled {
		return b, ErrImportUnsupported
	}
	if b.Revision != revision || len(b.Items) == 0 || b.State == "QUEUED" || b.State == "RUNNING" || b.State == "COMPLETED" {
		return b, ErrImportConflict
	}
	if e = s.checkBusinessLicense(ctx); e != nil {
		return b, e
	}
	version := b.Version
	mixed := false
	normal := b.Items[0].Item.Mode == contract.ImportNormal
	for _, item := range b.Items {
		if (item.Item.Mode == contract.ImportNormal) != normal {
			mixed = true
		}
	}
	ready := !mixed
	for index := range b.Items {
		item := &b.Items[index]
		if item.ContractID != "" {
			continue
		}
		if mixed {
			item.State = "BLOCKED"
			item.Issues = []contract.ImportIssue{{Field: "mode", Code: "IMPORT_MIXED_LIFECYCLE"}}
			continue
		}
		if issues := importFieldIssues(item.Item); len(issues) > 0 {
			ready = false
			item.State, item.Issues = "BLOCKED", issues
			continue
		}
		normalized, digest, err := s.prepareImportItem(ctx, actor, b, *item)
		item.Issues = nil
		if err != nil {
			ready = false
			item.State = "BLOCKED"
			code := "IMPORT_VALIDATION_FAILED"
			retry := false
			if errors.Is(err, ErrImportUnsupported) {
				code = "IMPORT_POLICY_UNSUPPORTED"
			} else if errors.Is(err, ErrForbidden) {
				code = "IMPORT_AUTHORIZATION_REQUIRED"
			} else if !errors.Is(err, ErrValidation) {
				code = "IMPORT_DEPENDENCY_UNAVAILABLE"
				retry = true
			}
			item.Issues = []contract.ImportIssue{{Field: "metadata", Code: code, Retryable: retry}}
		} else {
			item.State, item.Digest = "READY", digest
			item.NormalizedMetadata = normalizedImportMetadata(normalized)
		}
	}
	b.State = "BLOCKED"
	if ready {
		b.State = "PREVIEWED"
	}
	b.PreviewToken = ulid.Make().String()
	b.PreviewExpiresAt = time.Now().UTC().Add(30 * time.Minute)
	r, _ := s.importStore()
	e = r.UpdateImportBatch(ctx, actor.TenantID, id, func(current *contract.ImportBatch) error {
		if e := importAccess(actor, *current, "contract.import.preview"); e != nil {
			return e
		}
		if e := editableImport(current, version); e != nil {
			return e
		}
		current.Items, current.State = b.Items, b.State
		current.PreviewToken, current.PreviewExpiresAt = b.PreviewToken, b.PreviewExpiresAt
		return nil
	})
	if e != nil {
		return b, e
	}
	return s.GetImportBatch(ctx, actor, id)
}
