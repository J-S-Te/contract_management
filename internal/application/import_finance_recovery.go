package application

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type importFinanceReleaseCommand struct {
	EventID          string              `json:"event_id"`
	EventType        string              `json:"event_type"`
	SchemaVersion    int                 `json:"schema_version"`
	Phase            string              `json:"phase"`
	TenantID         string              `json:"tenant_id"`
	ItemID           string              `json:"origin_import_item_id"`
	SourceRevision   uint64              `json:"source_revision"`
	PolicyDigest     string              `json:"policy_digest"`
	ReleaseVersion   uint64              `json:"release_version"`
	ImportMode       contract.ImportMode `json:"import_mode"`
	OccurredAt       time.Time           `json:"occurred_at"`
	ReconciliationID string              `json:"reconciliation_id"`
	SnapshotVersion  uint64              `json:"expected_snapshot_version"`
	OwnerVersion     uint64              `json:"expected_owner_version"`
}
type importFinanceRecovery struct {
	OldCommandID            string
	OldEpoch, OldGeneration uint64
	Task                    contract.ImportTarget
}

// prepareImportFinanceRecovery 仅为明确拒绝且已被同源新READY替换的旧财务命令准备受控代次。
// 参数：ctx/store/actor为受信依赖，b/item/target/version/phase为显式放行请求。
// 返回值：不可恢复时保留原任务，合法新基线返回待持锁核验的冻结新命令。
func (s *Service) prepareImportFinanceRecovery(ctx context.Context, store ImportTargetStore, actor Principal, b contract.ImportBatch, itemID, target string, version uint64, phase string) (*importFinanceRecovery, error) {
	if target != "FINANCE" || phase != "RELEASE" {
		return nil, nil
	}
	var item contract.ImportItemState
	var task contract.ImportTarget
	for _, it := range b.Items {
		if it.ID == itemID {
			item = it
			for _, t := range it.Targets {
				if t.Target == target {
					task = t
				}
			}
		}
	}
	if task.State != "BLOCKED" {
		return nil, nil
	}
	if task.Version != version {
		return nil, ErrImportConflict
	}
	if task.Phase != "RELEASE" || task.RecipientHTTPStatus != 409 || task.RecipientCode != "SETTLEMENT_CONFLICT" || task.RejectedAt == nil || task.RequestPayload == "" || len(task.CommandHistory) >= 100 {
		return nil, ErrImportUnsupported
	}
	old, e := validatedRejectedFinanceCommand(b, item, task)
	if e != nil {
		return nil, e
	}
	c, e := s.Repo.GetContract(ctx, b.TenantID, item.ContractID)
	if e != nil {
		return nil, e
	}
	var status importFinanceStatus
	if e = s.ImportFinance.Call(ctx, http.MethodGet, "/internal/v1/settlement/contract-imports/"+url.PathEscape(task.ReceiptID), nil, &status); e != nil {
		return nil, e
	}
	if e = validateFinanceRecoveryStatus(b, item, c, task, old, status); e != nil {
		return nil, e
	}
	for _, t := range item.Targets {
		if t.Target == "PROJECT" && t.State != "COMPLETE" {
			return nil, ErrImportTargetWaiting
		}
	}
	if s.ImportOwners == nil {
		return nil, ErrImportTargetWaiting
	}
	owner, e := s.ImportOwners.ResolveOwner(ctx, b.TenantID, c.OwnerUserID)
	if e != nil || owner.UserID != c.OwnerUserID {
		return nil, ErrImportTargetWaiting
	}
	if e = s.EnsureOwnerNotFrozen(ctx, b.TenantID, c.OwnerUserID); e != nil {
		return nil, e
	}
	return newFinanceRecovery(actor, item, task, old, status)
}

// validatedRejectedFinanceCommand 复核拒绝记录与本次同command的冻结来源身份。
// 参数：b/item为批准来源，task为明确409拒绝的旧命令。
// 返回值：完整原命令或不可恢复的身份冲突。
func validatedRejectedFinanceCommand(b contract.ImportBatch, item contract.ImportItemState, task contract.ImportTarget) (importFinanceReleaseCommand, error) {
	var old importFinanceReleaseCommand
	decoder := json.NewDecoder(bytes.NewReader([]byte(task.RequestPayload)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&old) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return old, ErrImportTargetReceipt
	}
	policy, _ := contract.ImportDigest(item.Item.Policy)
	if b.Confirmation == nil || b.Confirmation.State != "APPROVED" || !item.Confirmed || item.Item.Mode != contract.ImportContinueLive || contract.ValidateHistoryEvidence(item.Item) != nil || old.EventID != task.CommandID || old.EventType != "contract.import_financial.v1" || old.SchemaVersion != 1 || old.Phase != "RELEASE" || old.TenantID != b.TenantID || old.ItemID != item.ID || old.SourceRevision != item.SourceRevision || old.PolicyDigest != policy || old.ReleaseVersion != task.ReleaseVersion || old.ImportMode != item.Item.Mode || old.ReconciliationID == "" || old.SnapshotVersion == 0 || old.OwnerVersion == 0 || old.OwnerVersion != task.ResponsibilityVersion {
		return old, ErrImportTargetReceipt
	}
	return old, nil
}

// validateFinanceRecoveryStatus 要求真实同源HOLD及已替换对账凭证，不回退身份、政策或责任。
// 参数：b/item/c/task为当前源事实，old为被拒命令，status为受信结算当前查询。
// 返回值：匹配的新READY返回nil，否则保持原BLOCKED。
func validateFinanceRecoveryStatus(b contract.ImportBatch, item contract.ImportItemState, c contract.Contract, task contract.ImportTarget, old importFinanceReleaseCommand, status importFinanceStatus) error {
	policy, _ := contract.ImportDigest(c.ImportPolicy)
	if c.OriginKind != "IMPORTED" || c.OriginImportItemID != item.ID || c.ImportMode != item.Item.Mode || policy != old.PolicyDigest || c.OwnerUserID != item.Item.HistoryEvidence.TargetOwnerUserID || status.PolicyDigest != old.PolicyDigest || status.ImportMode != old.ImportMode || status.SourceContractID != c.ID || status.CustomerID != strconv.FormatUint(c.CRMCustomerID, 10) || status.Currency != c.Currency || status.ContractAmount != minorAmount(c.AmountMinor) || status.SnapshotID != task.ReceiptID || status.ItemID != item.ID || status.SourceRevision != item.SourceRevision || status.ReleaseVersion != task.ReleaseVersion || status.ExpectedOwner != c.OwnerUserID {
		return ErrImportTargetReceipt
	}
	if status.State != "READY" || status.MigrationState != "HOLD" || status.ReconciliationID == "" || status.ReconciliationID == old.ReconciliationID || status.SnapshotVersion < old.SnapshotVersion {
		return ErrImportConflict
	}
	if !status.OwnerActive || !status.OwnerMatches || status.Owner != c.OwnerUserID || status.OwnerVersion != old.OwnerVersion {
		return ErrImportTargetWaiting
	}
	return nil
}

// newFinanceRecovery 有界归档被拒命令并冻结授权的新阶段命令，不改业务release_version。
// 参数：actor为真实操作者，item/task为旧任务，old/status为已核对的新旧凭证。
// 返回值：待原子持久化的新代次或编码错误。
func newFinanceRecovery(actor Principal, item contract.ImportItemState, task contract.ImportTarget, old importFinanceReleaseCommand, status importFinanceStatus) (*importFinanceRecovery, error) {
	recovery := &importFinanceRecovery{OldCommandID: task.CommandID, OldEpoch: task.Epoch, OldGeneration: task.CommandGeneration}
	digest, _ := contract.ImportDigest(task.RequestPayload)
	task.CommandHistory = append(append([]contract.ImportTargetCommandHistory(nil), task.CommandHistory...), contract.ImportTargetCommandHistory{CommandID: task.CommandID, Generation: task.CommandGeneration, RequestDigest: digest, RequestPayload: task.RequestPayload, HTTPStatus: task.RecipientHTTPStatus, RecipientCode: task.RecipientCode, RejectedAt: *task.RejectedAt})
	task.CommandGeneration++
	hash, _ := contract.ImportDigest([]any{item.ID, item.SourceRevision, task.Target, "RELEASE", task.CommandGeneration})
	task.CommandID = hash[:32]
	now := time.Now().UTC()
	old.EventID, old.OccurredAt = task.CommandID, now
	old.ReconciliationID, old.SnapshotVersion = status.ReconciliationID, status.SnapshotVersion
	payload, e := json.Marshal(old)
	if e != nil {
		return nil, e
	}
	task.RequestPayload = string(payload)
	task.OccurredAt = now
	task.Epoch++
	task.State = "QUEUED"
	task.ErrorCode = ""
	task.NextAttemptAt = now
	task.LeaseUntil = time.Time{}
	task.RecipientHTTPStatus = 0
	task.RecipientCode = ""
	task.RejectedAt = nil
	task.RecoveryRequestedBy = actor.UserID
	recovery.Task = task
	return recovery, nil
}
