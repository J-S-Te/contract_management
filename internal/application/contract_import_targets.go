package application

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"
)

type ImportTargetStore interface {
	GetImportItemBatch(context.Context, string, string) (contract.ImportBatch, error)
	QueueImportTarget(context.Context, string, string, string, uint64, string, func(*contract.ImportBatch, *contract.ImportItemState, *contract.ImportTarget) error) error
	ClaimImportTarget(context.Context, time.Time) (contract.ImportBatch, string, string, bool, error)
	SaveImportTarget(context.Context, string, string, string, contract.ImportTarget) error
	ActivateImportResponsibility(context.Context, string, string, uint64) (uint64, error)
}

// ReleaseImportItem 保存独立目标放行请求和稳定任务，不冒充接收方完成。
// 参数：ctx及actor为当前身份，item/target/version为精确目标围栏，phase为财务PREPARE或RELEASE。
// 返回值：更新台账或权限、策略、版本错误。
func (s *Service) ReleaseImportItem(ctx context.Context, actor Principal, item, target string, version uint64, phase string) (contract.ImportBatch, error) {
	store, ok := s.Repo.(ImportTargetStore)
	if !ok {
		return contract.ImportBatch{}, ErrImportUnsupported
	}
	if version == 0 || !slices.Contains([]string{"PROJECT", "FINANCE", "REPORT"}, target) {
		return contract.ImportBatch{}, ErrValidation
	}
	if phase == "" {
		phase = "RELEASE"
	}
	if phase != "RELEASE" && (target != "FINANCE" || phase != "PREPARE") {
		return contract.ImportBatch{}, ErrValidation
	}
	if e := s.checkBusinessLicense(ctx); e != nil {
		return contract.ImportBatch{}, e
	}
	b, e := store.GetImportItemBatch(ctx, actor.TenantID, item)
	if e != nil {
		return b, e
	}
	if e = importAccess(actor, b, "contract.import.release"); e != nil {
		return b, e
	}
	projectRetry, e := s.prepareImportProjectRetry(ctx, actor, b, item, target, version, phase)
	if e != nil {
		return b, e
	}
	recovery, e := s.prepareImportFinanceRecovery(ctx, store, actor, b, item, target, version, phase)
	if e != nil {
		return b, e
	}
	e = store.QueueImportTarget(ctx, actor.TenantID, b.ID, item, version, target, func(current *contract.ImportBatch, i *contract.ImportItemState, t *contract.ImportTarget) error {
		if e := importAccess(actor, *current, "contract.import.release"); e != nil {
			return e
		}
		if current.Confirmation == nil || current.Confirmation.State != "APPROVED" || !i.Confirmed || contract.ValidateHistoryEvidence(i.Item) != nil {
			return ErrImportConflict
		}
		if projectRetry != nil {
			if t.State != "BLOCKED" || t.Epoch+1 != projectRetry.Epoch ||
				t.CommandID != projectRetry.CommandID || t.RequestPayload != projectRetry.RequestPayload ||
				t.CommandGeneration != projectRetry.CommandGeneration {
				return ErrImportConflict
			}
			*t = *projectRetry
			return nil
		}
		if recovery != nil {
			if t.State != "BLOCKED" || t.CommandID != recovery.OldCommandID || t.Epoch != recovery.OldEpoch || t.CommandGeneration != recovery.OldGeneration {
				return ErrImportConflict
			}
			*t = recovery.Task
			return nil
		}
		if t.State == "NOT_APPLICABLE" || t.State == "BLOCKED" {
			return ErrImportUnsupported
		}
		if t.State == "COMPLETE" || (t.Phase == phase && t.CommandID != "") {
			return nil
		}
		if target == "FINANCE" && phase == "RELEASE" && t.ReceiptID == "" {
			return ErrImportTargetWaiting
		}
		if target == "FINANCE" && phase == "PREPARE" && t.ReceiptID != "" {
			return nil
		}
		t.Phase, t.State, t.ErrorCode = phase, "QUEUED", ""
		t.Epoch++
		t.LeaseUntil = time.Time{}
		t.NextAttemptAt = time.Now().UTC()
		t.CommandID = stableImportCommand(i.ID, i.SourceRevision, target, phase)
		t.RequestPayload = ""
		t.OccurredAt = time.Now().UTC()
		return nil
	})
	if e != nil {
		return b, e
	}
	return store.GetImportItemBatch(ctx, actor.TenantID, item)
}

// stableImportCommand 派生兼容接收方32hex约束的稳定阶段命令。
// 参数：item/revision/target/phase为不可变放行身份。
// 返回值：稳定32位小写hex命令ID。
func stableImportCommand(item string, revision uint64, target, phase string) string {
	hash, _ := contract.ImportDigest([]any{item, revision, target, phase})
	return hash[:32]
}

// DispatchImportTargetOnce 认领有限租约任务并保存精确远端回执或退避原因。
// 参数：ctx为进程生命周期。
// 返回值：本地持久化错误，远端等待保留在目标台账。
func (s *Service) DispatchImportTargetOnce(ctx context.Context) error {
	store, ok := s.Repo.(ImportTargetStore)
	if !ok {
		return nil
	}
	if e := s.checkBusinessLicense(ctx); e != nil {
		return e
	}
	b, itemID, target, found, e := store.ClaimImportTarget(ctx, time.Now().UTC())
	if e != nil || !found {
		return e
	}
	var item contract.ImportItemState
	var task contract.ImportTarget
	for _, i := range b.Items {
		if i.ID == itemID {
			item = i
			for _, t := range i.Targets {
				if t.Target == target {
					task = t
				}
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	e = s.executeImportTarget(ctx, store, b, item, &task)
	task.LeaseUntil = time.Time{}
	if e == nil {
		task.ErrorCode = ""
	} else {
		task.ErrorCode = "IMPORT_TARGET_WAITING"
		var wait *ImportTargetWaitError
		if errors.As(e, &wait) {
			task.ErrorCode = wait.Code
		}
		if errors.Is(e, ErrImportTargetReceipt) {
			var rejected *ImportTargetHTTPError
			if errors.As(e, &rejected) {
				task.RecipientHTTPStatus = rejected.Status
				task.RecipientCode = rejected.Code
				now := time.Now().UTC()
				task.RejectedAt = &now
			}
			task.State = "BLOCKED"
			task.ErrorCode = "IMPORT_TARGET_RECEIPT_INVALID"
		} else {
			task.State = "WAITING"
			task.NextAttemptAt = time.Now().UTC().Add(time.Duration(min(task.Attempts, 20)) * 15 * time.Second)
		}
	}
	finish, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	return store.SaveImportTarget(finish, b.TenantID, b.ID, item.ID, task)
}

// executeImportTarget 按PROJECT、财务暂存或数析水位协议推进单项。
// 参数：ctx/store限定执行，b/item为冻结源基线，task为持租约目标。
// 返回值：真实阶段回执校验结果或等待错误。
func (s *Service) executeImportTarget(ctx context.Context, store ImportTargetStore, b contract.ImportBatch, item contract.ImportItemState, t *contract.ImportTarget) error {
	if b.Confirmation == nil || b.Confirmation.State != "APPROVED" || contract.ValidateHistoryEvidence(item.Item) != nil {
		return ErrImportTargetReceipt
	}
	var c contract.Contract
	var e error
	if metadata, ok := s.Repo.(interface {
		GetImportTargetContract(context.Context, string, string) (contract.Contract, error)
	}); ok {
		c, e = metadata.GetImportTargetContract(ctx, b.TenantID, item.ContractID)
	} else {
		c, e = s.Repo.GetContract(ctx, b.TenantID, item.ContractID)
	}
	if e != nil {
		return e
	}
	expected, _ := contract.ImportDigest(item.Item.Policy)
	actual, _ := contract.ImportDigest(c.ImportPolicy)
	if c.OriginKind != "IMPORTED" || c.OriginImportItemID != item.ID || c.ImportMode != item.Item.Mode || expected != actual || c.OwnerUserID != item.Item.HistoryEvidence.TargetOwnerUserID {
		return ErrImportTargetReceipt
	}
	switch t.Target {
	case "PROJECT":
		return s.executeImportProject(ctx, b, item, c, t)
	case "FINANCE":
		return s.executeImportFinance(ctx, store, b, item, c, t)
	case "REPORT":
		return s.executeImportReport(ctx, b, item, t)
	}
	return ErrImportTargetReceipt
}

// freezeImportRequest 先保存确定载荷，再允许远端调用，恢复不重算发生时间或事实。
// 参数：ctx、b/item为目标身份，t为当前任务，request为待冻结的协议DTO。
// 返回值：序列化或持久化错误。
func (s *Service) freezeImportRequest(ctx context.Context, b contract.ImportBatch, item contract.ImportItemState, t *contract.ImportTarget, request any) error {
	if t.RequestPayload != "" {
		return nil
	}
	payload, e := json.Marshal(request)
	if e != nil {
		return e
	}
	t.RequestPayload = string(payload)
	return s.Repo.(ImportTargetStore).SaveImportTarget(ctx, b.TenantID, b.ID, item.ID, *t)
}

// executeImportProject 调用新历史项目协议并核验完整绑定身份。
// 参数：ctx为上下文，b/item/c为冻结源事实，t为目标任务。
// 返回值：PROJECT_BOUND精确匹配才成功。
func (s *Service) executeImportProject(ctx context.Context, b contract.ImportBatch, item contract.ImportItemState, c contract.Contract, t *contract.ImportTarget) error {
	policy, _ := contract.ImportDigest(item.Item.Policy)
	request, e := importProjectRequest(b, item, c, *t)
	if e != nil {
		return e
	}
	if t.RequestPayload != "" && validateFrozenProjectRequest(request, t.RequestPayload) != nil {
		return ErrImportTargetReceipt
	}
	if e := s.freezeImportRequest(ctx, b, item, t, request); e != nil {
		return e
	}
	var reply struct {
		CommandID      string `json:"command_id"`
		SchemaVersion  int    `json:"schema_version"`
		TenantID       string `json:"tenant_id"`
		ItemID         string `json:"origin_import_item_id"`
		SourceRevision uint64 `json:"source_revision"`
		PolicyDigest   string `json:"policy_digest"`
		ReleaseVersion uint64 `json:"release_version"`
		ContractID     string `json:"contract_id"`
		ProjectID      string `json:"project_id"`
		ProjectVersion uint64 `json:"project_version"`
		Status         string `json:"status"`
	}
	if e := s.ImportProject.Call(ctx, http.MethodPost, "/internal/v1/contract-imports/project-bindings", []byte(t.RequestPayload), &reply); e != nil {
		return e
	}
	if reply.CommandID != t.CommandID || reply.SchemaVersion != 1 || reply.TenantID != b.TenantID || reply.ItemID != item.ID || reply.SourceRevision != item.SourceRevision || reply.PolicyDigest != policy || reply.ReleaseVersion != t.ReleaseVersion || reply.ContractID != c.ID || reply.ProjectID == "" || reply.ProjectVersion == 0 || reply.Status != "PROJECT_BOUND" {
		return ErrImportTargetReceipt
	}
	if expected := item.Item.HistoryEvidence.ExistingProjectID; expected != "" && reply.ProjectID != expected {
		return ErrImportTargetReceipt
	}
	t.State, t.ReceiptID = "COMPLETE", reply.ProjectID
	return nil
}

type importFinanceStatus struct {
	PolicyDigest     string              `json:"policy_digest"`
	ImportMode       contract.ImportMode `json:"import_mode"`
	SourceContractID string              `json:"source_contract_id"`
	CustomerID       string              `json:"customer_id"`
	Currency         string              `json:"currency"`
	ContractAmount   string              `json:"contract_amount"`
	SnapshotID       string              `json:"contract_snapshot_id"`
	State            string              `json:"state"`
	MigrationState   string              `json:"migration_state"`
	ItemID           string              `json:"origin_import_item_id"`
	SnapshotVersion  uint64              `json:"snapshot_version"`
	ReleaseVersion   uint64              `json:"release_version"`
	SourceRevision   uint64              `json:"source_revision"`
	ReconciliationID string              `json:"reconciliation_id"`
	ExpectedOwner    string              `json:"expected_owner_user_id"`
	Owner            string              `json:"owner_user_id"`
	OwnerVersion     uint64              `json:"owner_version"`
	OwnerActive      bool                `json:"owner_active"`
	OwnerMatches     bool                `json:"owner_matches"`
}

// executeImportFinance 执行PREPARE-HOLD-本地核对-责任ACK-RELEASE顺序。
// 参数：ctx/store为执行依赖，b/item/c为批准源事实，t为当前阶段。
// 返回值：PREPARED仅暂存，独立FINANCE_RELEASED才完成。
func (s *Service) executeImportFinance(ctx context.Context, store ImportTargetStore, b contract.ImportBatch, item contract.ImportItemState, c contract.Contract, t *contract.ImportTarget) error {
	if item.Item.Policy.FinanceMode != "FULL_REPLAY" || item.Item.Mode != contract.ImportContinueLive {
		return ErrImportTargetReceipt
	}
	policy, _ := contract.ImportDigest(item.Item.Policy)
	request := map[string]any{"event_id": t.CommandID,
		"event_type":            "contract.import_financial.v1",
		"schema_version":        1,
		"phase":                 t.Phase,
		"tenant_id":             b.TenantID,
		"origin_import_item_id": item.ID,
		"source_revision":       item.SourceRevision,
		"policy_digest":         policy,
		"release_version":       t.ReleaseVersion,
		"import_mode":           item.Item.Mode,
		"occurred_at":           t.OccurredAt}
	path := "/internal/v1/settlement/events/contract-imports"
	if t.Phase == "PREPARE" {
		terms := []map[string]any{}
		for _, term := range c.PaymentTerms {
			terms = append(terms, map[string]any{"installment_no": term.InstallmentNo, "due_date": term.DueDate, "amount": term.Amount})
		}
		request["expected_facts"] = *item.Item.HistoryEvidence.ExpectedFacts
		request["contract"] = map[string]any{"id": c.ID,
			"no":      c.Number,
			"version": c.Version,
			"customer_id": strconv.FormatUint(c.CRMCustomerID,
				10),
			"customer_name": c.CustomerName,
			"owner_user_id": c.OwnerUserID,
			"currency":      c.Currency,
			"amount":        minorAmount(c.AmountMinor),
			"payment_terms": terms}
	} else {
		path = "/internal/v1/settlement/contract-imports/" + url.PathEscape(t.ReceiptID) + "/release"
		if t.RequestPayload == "" {
			var status importFinanceStatus
			if e := s.ImportFinance.Call(ctx, http.MethodGet, "/internal/v1/settlement/contract-imports/"+url.PathEscape(t.ReceiptID), nil, &status); e != nil {
				return e
			}
			if status.PolicyDigest != policy || status.ImportMode != item.Item.Mode || status.SourceContractID != c.ID || status.CustomerID != strconv.FormatUint(c.CRMCustomerID, 10) || status.Currency != c.Currency || status.ContractAmount != minorAmount(c.AmountMinor) || status.SnapshotID != t.ReceiptID || status.ItemID != item.ID || status.SourceRevision != item.SourceRevision || status.ReleaseVersion != t.ReleaseVersion || status.ExpectedOwner != c.OwnerUserID {
				return ErrImportTargetReceipt
			}
			if status.State != "READY" || status.ReconciliationID == "" || status.SnapshotVersion == 0 {
				return &ImportTargetWaitError{Code: "IMPORT_FINANCE_RECONCILIATION_REQUIRED"}
			}
			for _, other := range item.Targets {
				if other.Target == "PROJECT" && other.State != "COMPLETE" {
					return &ImportTargetWaitError{Code: "IMPORT_PROJECT_BINDING_REQUIRED"}
				}
			}
			if s.ImportOwners == nil {
				return &ImportTargetWaitError{Code: "IMPORT_OWNER_DIRECTORY_REQUIRED"}
			}
			owner, ownerErr := s.ImportOwners.ResolveOwner(ctx, b.TenantID, c.OwnerUserID)
			if ownerErr != nil || owner.UserID != c.OwnerUserID {
				return &ImportTargetWaitError{Code: "IMPORT_OWNER_DIRECTORY_REQUIRED"}
			}
			if e := s.EnsureOwnerNotFrozen(ctx, b.TenantID, c.OwnerUserID); e != nil {
				return e
			}
			version, e := store.ActivateImportResponsibility(ctx, b.TenantID, item.ID, item.SourceRevision)
			if e != nil {
				return e
			}
			t.ResponsibilityVersion = version
			if !status.OwnerActive || !status.OwnerMatches || status.Owner != c.OwnerUserID || status.OwnerVersion != version {
				return &ImportTargetWaitError{Code: "IMPORT_RESPONSIBILITY_ACK_REQUIRED"}
			}
			request["reconciliation_id"] = status.ReconciliationID
			request["expected_snapshot_version"] = status.SnapshotVersion
			request["expected_owner_version"] = version
			path = "/internal/v1/settlement/contract-imports/" + url.PathEscape(t.ReceiptID) + "/release"
		}
	}
	if e := s.freezeImportRequest(ctx, b, item, t, request); e != nil {
		return e
	}
	var reply struct {
		EventID        string `json:"event_id"`
		SnapshotID     string `json:"contract_snapshot_id"`
		Status         string `json:"status"`
		MigrationState string `json:"migration_state"`
	}
	if e := s.ImportFinance.Call(ctx, http.MethodPost, path, []byte(t.RequestPayload), &reply); e != nil {
		return e
	}
	if reply.EventID != t.CommandID || reply.SnapshotID == "" || (t.ReceiptID != "" && reply.SnapshotID != t.ReceiptID) {
		return ErrImportTargetReceipt
	}
	if t.Phase == "PREPARE" {
		if reply.Status != "FINANCE_PREPARED" || (reply.MigrationState != "HOLD" && reply.MigrationState != "RELEASED") {
			return ErrImportTargetReceipt
		}
		t.State = "PREPARED"
	} else {
		if reply.Status != "FINANCE_RELEASED" || reply.MigrationState != "RELEASED" {
			return ErrImportTargetReceipt
		}
		t.State = "COMPLETE"
	}
	t.ReceiptID = reply.SnapshotID
	return nil
}

// minorAmount 将合法分单位原金额精确转换为接收方十进制。
// 参数：minor为非负原金额分。
// 返回值：固定两位小数金额，不使用浮点数。
func minorAmount(minor int64) string { return fmt.Sprintf("%d.%02d", minor/100, minor%100) }

// executeImportReport 只接受数析精确版本和全部必需通道的实际应用证明。
// 参数：ctx为上下文，b/item为源身份，t为REPORT任务。
// 返回值：APPLIED完整证明才完成，否则保留等待。
func (s *Service) executeImportReport(ctx context.Context, b contract.ImportBatch, item contract.ImportItemState, t *contract.ImportTarget) error {
	var reply struct {
		SchemaVersion        int        `json:"schema_version"`
		TenantID             string     `json:"tenant_id"`
		ItemID               string     `json:"item_id"`
		SourceRevision       uint64     `json:"source_revision"`
		ReleaseVersion       uint64     `json:"report_release_version"`
		RequiredChannels     string     `json:"required_channels"`
		AppliedChannels      []string   `json:"applied_channels"`
		State                string     `json:"state"`
		SnapshotID           string     `json:"source_snapshot_id"`
		RunID                string     `json:"aggregation_run_id"`
		AppliedAt            *time.Time `json:"applied_at"`
		SourceFactsConnected bool       `json:"source_facts_connected"`
	}
	path := "/internal/v1/contract-imports/report-status?item_id=" + url.QueryEscape(item.ID) + "&source_revision=" + strconv.FormatUint(item.SourceRevision, 10) + "&report_release_version=" + strconv.FormatUint(t.ReleaseVersion, 10)
	if e := s.ImportReport.Call(ctx, http.MethodGet, path, nil, &reply); e != nil {
		return e
	}
	if reply.SchemaVersion != 1 || reply.TenantID != b.TenantID || reply.ItemID != item.ID || reply.SourceRevision != item.SourceRevision || reply.ReleaseVersion != t.ReleaseVersion || reply.RequiredChannels != item.Item.HistoryEvidence.ReportRequiredChannels {
		return ErrImportTargetReceipt
	}
	if reply.State != "APPLIED" {
		return &ImportTargetWaitError{Code: "IMPORT_REPORT_APPLICATION_PENDING"}
	}
	hash, hashErr := hex.DecodeString(reply.SnapshotID)
	if !slices.Contains(reply.AppliedChannels, "API_APPLIED") || hashErr != nil || len(hash) != 32 || reply.RunID == "" || reply.AppliedAt == nil {
		return ErrImportTargetReceipt
	}
	if reply.RequiredChannels == "API_ONLY" && reply.SourceFactsConnected {
		return ErrImportTargetReceipt
	}
	if reply.RequiredChannels == "API_AND_SOURCE_FACTS" && (!reply.SourceFactsConnected || !slices.Contains(reply.AppliedChannels, "SOURCE_FACTS_APPLIED")) {
		return &ImportTargetWaitError{Code: "IMPORT_REPORT_SOURCE_FACTS_PENDING"}
	}
	t.State, t.ReceiptID = "COMPLETE", reply.RunID
	return nil
}
