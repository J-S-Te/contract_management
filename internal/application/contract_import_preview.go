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
			return importBatchForReader(b, actor), nil
		}
	}
	if e := importConfirmationAccess(actor, b); e != nil {
		return b, e
	}
	return importBatchForReader(b, actor), nil
}

// importBatchForReader 按读取人收窄台账响应。
// 参数：b为已通过权限复核的台账，actor为当前可信身份。
// 返回值：创建人拿到原样台账；其他读取人（如独立审批人）拿到清空 preview_token 的副本。
// 取舍：preview_token 是创建人提交预检的唯一凭据，审批人只需要台账事实，
// 因此不把提交凭据序列化给非创建人，也不改变创建人的现有字段。
func importBatchForReader(b contract.ImportBatch, actor Principal) contract.ImportBatch {
	if b.ActorID != actor.UserID {
		b.PreviewToken = ""
	}
	return b
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
		return source, &ImportSourceRejectedError{ReasonCode: CodeImportItemKeyInvalid,
			Reason: "单项键不合法：只允许字母、数字与 _.:-，长度 1—128", Filename: importSourceFilename(name)}
	}
	validated, e := contractdocument.ValidateExternalSource(name, media, body)
	if e != nil {
		return source, importSourceRejected(e, name)
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
			return &ImportSourceRejectedError{ReasonCode: CodeImportSourceLimit, Reason: "每批暂存原件已达上限 300 份",
				Filename: importSourceFilename(name)}
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
		// 清单变更后上一轮开立计划失效，必须由下一次预检重新生成。
		b.ProvisionPlan = nil
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

// prepareImportItem 复核暂存原件及来源引用并复用普通合同的实时目录/字段校验；
// LOCAL 仅允许 NORMAL 普通合同，不创建任何合同。
// 参数：ctx为请求，actor为可信主体，b为批次，item为冻结前条目。
// 返回值：规范合同及摘要，或校验、来源和依赖错误。
func (s *Service) prepareImportItem(ctx context.Context, actor Principal, b contract.ImportBatch, item contract.ImportItemState) (contract.Contract, string, error) {
	return s.prepareImportItemWith(ctx, actor, b, item, false)
}

// prepareImportItemWith 是预检与执行期共用的原件/字段复核实现。
// 参数：provision 为 true 时使用开立通道准备函数（不做 CRM 客户引用回读，
// 客户由编排在建单前查重/创建），其余行为与普通导入完全一致。
// 返回值：规范合同、规范摘要或错误；不创建合同。
func (s *Service) prepareImportItemWith(ctx context.Context, actor Principal, b contract.ImportBatch, item contract.ImportItemState, provision bool) (c contract.Contract, digest string, err error) {
	if item.Item.Mode != contract.ImportNormal && (item.Item.Metadata.CounterpartySource == contract.CounterpartyLocal || item.Item.Metadata.CategorySource == contract.CategoryLocal) {
		return c, "", ErrImportUnsupported
	}
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
		return c, "", &ImportIdentityConflictError{Kind: ImportConflictKindBaselineDrift}
	}
	if utf8.RuneCountInString(c.ExternalPaperNumber) > 128 {
		return c, "", ErrValidation
	}
	if provision {
		c, e = validator.prepareProvisionContract(ctx, actor, c, true, "contract.create")
	} else {
		c, e = validator.prepareContract(ctx, actor, c, true, "contract.create")
	}
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
	// provisionPlan 每轮重算：开关关闭或模式不适用时为空，避免上一轮计划残留。
	provisionPlan := []contract.ImportProvisionPlanItem{}
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
		// 历史开立通道：该行凭清单名称自动开立客户/商机，因此不再要求清单声明
		// crm_customer_id（其余字段规则完全不变），预检状态改为 NEEDS_PROVISION。
		provision := s.importProvisionItemApplies(item.Item)
		issues := importFieldIssues(item.Item)
		if provision {
			issues = withoutProvisionCustomerRequirement(issues)
			// 客户类型/行业/地区只在开立通道透传给 CRM，超长必须在预览期按字段定位，
			// 否则用户填了也会被静默忽略或到执行期才失败。
			if profileIssue := importProvisionCustomerProfileIssue(item.Item.Metadata); profileIssue != nil {
				issues = append(issues, *profileIssue)
			}
		}
		if len(issues) > 0 {
			ready = false
			item.State, item.Issues = "BLOCKED", issues
			continue
		}
		if provision {
			plan, issue := s.planImportProvision(ctx, actor, b, item.Item)
			provisionPlan = append(provisionPlan, plan)
			if issue != nil {
				ready = false
				item.State, item.Issues = "BLOCKED", []contract.ImportIssue{*issue}
				continue
			}
			normalized, digest, err := s.prepareProvisionImportItem(ctx, actor, b, *item)
			item.Issues = nil
			if err != nil {
				ready = false
				item.State = "BLOCKED"
				item.Issues = []contract.ImportIssue{previewImportItemIssue(err)}
			} else {
				item.State, item.Digest = contract.ImportItemStateNeedsProvision, digest
				item.NormalizedMetadata = normalizedImportMetadata(normalized)
			}
			continue
		}
		normalized, digest, err := s.prepareImportItem(ctx, actor, b, *item)
		item.Issues = nil
		if err != nil {
			ready = false
			item.State = "BLOCKED"
			item.Issues = []contract.ImportIssue{previewImportItemIssue(err)}
		} else {
			item.State, item.Digest = "READY", digest
			item.NormalizedMetadata = normalizedImportMetadata(normalized)
		}
	}
	b.ProvisionPlan = provisionPlan
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
		current.ProvisionPlan = b.ProvisionPlan
		return nil
	})
	if e != nil {
		return b, e
	}
	return s.GetImportBatch(ctx, actor, id)
}

// withoutProvisionCustomerRequirement 去掉「清单必须声明 crm_customer_id」这一条，
// 仅用于历史开立通道：客户由编排先查重/创建，清单不声明客户 ID 才是正常输入。
// 参数：issues 为字段形状校验结果。
// 返回值：过滤后的逐项问题（保持原顺序，不含该条 REQUIRED）。
func withoutProvisionCustomerRequirement(issues []contract.ImportIssue) []contract.ImportIssue {
	filtered := make([]contract.ImportIssue, 0, len(issues))
	for _, issue := range issues {
		if issue.Field == "metadata.crm_customer_id" && issue.Code == "REQUIRED" {
			continue
		}
		filtered = append(filtered, issue)
	}
	return filtered
}

// previewImportItemIssue 把预检失败映射为可定位的逐项 Issue。
// 参数：err 为预检错误。
// 返回值：字段路径、稳定错误码与可重试标记（不泄露业务正文）；
// 来源身份冲突单列一支，避免落到「可重试的依赖故障」上诱使操作者反复重试。
func previewImportItemIssue(err error) contract.ImportIssue {
	switch {
	case errors.Is(err, ErrCRMActorContextRequired):
		return contract.ImportIssue{Field: "metadata.crm_customer_id", Code: "IMPORT_CRM_SESSION_REQUIRED", Retryable: true}
	case errors.Is(err, ErrImportUnsupported):
		return contract.ImportIssue{Field: "metadata", Code: "IMPORT_POLICY_UNSUPPORTED"}
	case errors.Is(err, ErrForbidden):
		return contract.ImportIssue{Field: "metadata", Code: "IMPORT_AUTHORIZATION_REQUIRED"}
	case errors.Is(err, ErrImportIdentityConflict):
		// 必须在「非校验错误 → 可重试依赖故障」之前判定：
		// 身份冲突是终态，重试只会复现同一冲突。
		if issue, conflict := importIdentityConflictIssue(err, "metadata"); conflict {
			return issue
		}
		return contract.ImportIssue{Field: "metadata", Code: "IMPORT_SOURCE_ID_CONFLICT"}
	case !errors.Is(err, ErrValidation):
		return contract.ImportIssue{Field: "metadata", Code: "IMPORT_DEPENDENCY_UNAVAILABLE", Retryable: true}
	default:
		return contract.ImportIssue{Field: "metadata", Code: "IMPORT_VALIDATION_FAILED"}
	}
}
