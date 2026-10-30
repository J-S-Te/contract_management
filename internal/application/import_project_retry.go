package application

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// importProjectRequest 构造普通首次执行和拒绝恢复共用的冻结项目协议。
// 参数：b及item为批准来源，c为当前合同，task为同一目标命令。
// 返回值：完整项目命令；缺失原始起始日时返回等待错误。
func importProjectRequest(b contract.ImportBatch, item contract.ImportItemState, c contract.Contract, task contract.ImportTarget) (map[string]any, error) {
	if c.StartDate == nil {
		return nil, &ImportTargetWaitError{Code: "IMPORT_ORIGINAL_START_DATE_REQUIRED"}
	}
	policy, _ := contract.ImportDigest(item.Item.Policy)
	evidence := item.Item.HistoryEvidence
	version := strconv.FormatUint(c.Version, 10)
	projection := map[string]any{
		"contract_id": c.ID, "contract_number": c.Number, "contract_version": version,
		"contract_name": c.Title, "customer": c.CustomerName,
		"customer_id":  strconv.FormatUint(c.CRMCustomerID, 10),
		"effective_at": c.StartDate.UTC(), "services": importProjectServices(c.ID, c.ServiceItems),
	}
	request := map[string]any{
		"schema_version": 1, "command_id": task.CommandID, "tenant_id": b.TenantID,
		"contract_id": c.ID, "contract_version": version, "origin_import_item_id": item.ID,
		"source_revision": item.SourceRevision, "policy_digest": policy,
		"release_version": task.ReleaseVersion, "import_mode": item.Item.Mode,
		"project_mode": item.Item.Policy.ProjectMode, "relationship": evidence.Relationship,
		"target_owner_user_id": evidence.TargetOwnerUserID, "contract": projection,
	}
	if evidence.ExistingProjectID != "" {
		request["existing_project_id"] = evidence.ExistingProjectID
		request["expected_project_version"] = evidence.ExpectedProjectVersion
		request["original_contract_ref"] = evidence.OriginalContractRef
	}
	return request, nil
}

// prepareImportProjectRetry 仅为持久422参数拒绝准备显式同命令重试。
// 参数：ctx和actor为当前请求，b及精确目标参数为原冻结身份。
// 返回值：排队候选或围栏错误；其他目标及非阻塞任务不进入此分支。
func (s *Service) prepareImportProjectRetry(ctx context.Context, actor Principal, b contract.ImportBatch, itemID, target string, version uint64, phase string) (*contract.ImportTarget, error) {
	if target != "PROJECT" || phase != "RELEASE" {
		return nil, nil
	}
	for _, item := range b.Items {
		if item.ID != itemID {
			continue
		}
		for _, task := range item.Targets {
			if task.Target != target || task.State != "BLOCKED" {
				continue
			}
			if task.Version != version {
				return nil, ErrImportConflict
			}
			if !retryableProjectRejection(task, time.Now().UTC()) {
				return nil, ErrImportUnsupported
			}
			if err := s.validateRejectedProjectBaseline(ctx, b, item, task); err != nil {
				return nil, err
			}
			return requeueRejectedProject(actor.UserID, task), nil
		}
	}
	return nil, nil
}

// retryableProjectRejection 限定没有活动执行租约的确定参数拒绝。
// 参数：task为持久目标，now为当前服务端时间。
// 返回值：仅原命令已冻结、尚无完成回执且拒绝历史未满时允许恢复。
func retryableProjectRejection(task contract.ImportTarget, now time.Time) bool {
	return task.Phase == "RELEASE" && task.RecipientHTTPStatus == 422 &&
		task.RecipientCode == "IMPORT_VALIDATION_FAILED" && task.RejectedAt != nil &&
		!task.RejectedAt.After(now) && !task.LeaseUntil.After(now) &&
		task.CommandID != "" && task.RequestPayload != "" && task.ReceiptID == "" &&
		task.Attempts > 0 && len(task.CommandHistory) < 100
}

// validateRejectedProjectBaseline 拒绝利用重试更换合同、负责人或冻结政策。
// 参数：ctx为读取请求，b/item为已确认来源，task为明确被拒的原项目命令。
// 返回值：只有当前合同与完整原载荷语义一致时成功，发出字节仍保留原样。
func (s *Service) validateRejectedProjectBaseline(ctx context.Context, b contract.ImportBatch, item contract.ImportItemState, task contract.ImportTarget) error {
	if b.Confirmation == nil || b.Confirmation.State != "APPROVED" || !item.Confirmed ||
		item.Item.Mode != contract.ImportContinueLive || contract.ValidateHistoryEvidence(item.Item) != nil {
		return ErrImportConflict
	}
	c, err := s.Repo.GetContract(ctx, b.TenantID, item.ContractID)
	if err != nil {
		return err
	}
	policy, _ := contract.ImportDigest(c.ImportPolicy)
	expectedPolicy, _ := contract.ImportDigest(item.Item.Policy)
	if c.ID != item.ContractID || c.TenantID != b.TenantID || c.OriginKind != "IMPORTED" ||
		c.OriginImportItemID != item.ID || c.ImportMode != item.Item.Mode ||
		policy != expectedPolicy ||
		c.OwnerUserID != item.Item.HistoryEvidence.TargetOwnerUserID ||
		task.CommandID != stableImportCommand(item.ID, item.SourceRevision, "PROJECT", "RELEASE") {
		return ErrImportConflict
	}
	request, err := importProjectRequest(b, item, c, task)
	if err != nil {
		return err
	}
	return validateFrozenProjectRequest(request, task.RequestPayload)
}

// validateFrozenProjectRequest 复核原命令和当前完整项目投影的语义一致性。
// 参数：request为当前可信投影，payload为持久冻结载荷。
// 返回值：任一字段变化或协议异常返回冲突；不修改原载荷字节。
func validateFrozenProjectRequest(request map[string]any, payload string) error {
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	current, err := canonicalProjectRequestDigest(raw)
	if err != nil {
		return err
	}
	frozen, err := canonicalProjectRequestDigest([]byte(payload))
	if err != nil {
		return err
	}
	if current != frozen {
		return ErrImportConflict
	}
	return nil
}

// canonicalProjectRequestDigest 比较完整JSON对象并保留大整数精度。
// 参数：raw为原冻结载荷或当前投影，仅允许项目协议大小内的单一对象。
// 返回值：确定性摘要；坏对象、超限或尾随数据返回基线冲突。
func canonicalProjectRequestDigest(raw []byte) (string, error) {
	if len(raw) > 2<<20 {
		return "", ErrImportConflict
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var document map[string]any
	if decoder.Decode(&document) != nil || document == nil || decoder.Decode(&struct{}{}) != io.EOF {
		return "", ErrImportConflict
	}
	return contract.ImportDigest(document)
}

// requeueRejectedProject 保留原幂等身份、原载荷和真实拒绝证据并提升执行围栏。
// 参数：actor为当前放行人，task为已完成所有恢复校验的旧目标。
// 返回值：仅QUEUED候选；不会同步执行或标记接收方成功。
func requeueRejectedProject(actor string, task contract.ImportTarget) *contract.ImportTarget {
	now := time.Now().UTC()
	digest, _ := contract.ImportDigest(task.RequestPayload)
	history := append([]contract.ImportTargetCommandHistory(nil), task.CommandHistory...)
	task.CommandHistory = append(history, contract.ImportTargetCommandHistory{
		CommandID: task.CommandID, Generation: task.CommandGeneration,
		RequestDigest: digest, RequestPayload: task.RequestPayload,
		HTTPStatus: task.RecipientHTTPStatus, RecipientCode: task.RecipientCode,
		RejectedAt:          *task.RejectedAt,
		RecoveryRequestedBy: actor, RecoveryRequestedAt: &now,
	})
	task.Epoch++
	task.State, task.ErrorCode = "QUEUED", ""
	task.NextAttemptAt = now
	task.LeaseUntil = time.Time{}
	task.RecipientHTTPStatus, task.RecipientCode, task.RejectedAt = 0, "", nil
	task.RecoveryRequestedBy = actor
	return &task
}
