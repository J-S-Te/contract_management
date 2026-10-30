package mysql

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestImportFinanceReleaseControlledRecoveryMySQL 验证确定409可受控重核，未知响应保持原命令与载荷。
// 参数：t为显式隔离MySQL上下文；结算HTTP为明确协议替身。
// 返回值：无；泛化BLOCKED恢复、迟到覆盖、无权重试或未知响应换载荷时失败。
func TestImportFinanceReleaseControlledRecoveryMySQL(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		name := "known_409_reconciled"
		if unknown {
			name = "unknown_response_immutable"
		}
		t.Run(name, func(t *testing.T) {
			r, tenant := draftE2ERepository(t)
			ctx := context.Background()
			b, _ := historyMySQLFixture(t, r, tenant, contract.ImportContinueLive)
			b = approveHistoryFixture(t, r, b)
			item := b.Items[0]
			policy, _ := contract.ImportDigest(item.Item.Policy)
			if e := r.UpdateImportBatch(ctx, tenant, b.ID, func(current *contract.ImportBatch) error {
				current.Items[0].Targets[0].State = "COMPLETE"
				current.Items[0].Targets[1].State = "PREPARED"
				current.Items[0].Targets[1].ReceiptID = "snapshot"
				current.Items[0].Targets[1].Phase = "PREPARE"
				return nil
			}); e != nil {
				t.Fatal(e)
			}
			snapshotVersion := uint64(3)
			reconciliation := "old-local-proof"
			first := true
			received := [][]byte{}
			statusPolicy := policy
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodGet {
					var stream contractResponsibilityStream
					if e := r.db.Where("tenant_id=? AND contract_id=?", tenant, item.ContractID).Take(&stream).Error; e != nil {
						t.Error(e)
						w.WriteHeader(500)
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"policy_digest": statusPolicy, "import_mode": item.Item.Mode, "contract_snapshot_id": "snapshot", "state": "READY", "migration_state": "HOLD", "origin_import_item_id": item.ID, "source_revision": item.SourceRevision, "release_version": 1, "snapshot_version": snapshotVersion, "reconciliation_id": reconciliation, "source_contract_id": item.ContractID, "customer_id": "42", "currency": "CNY", "contract_amount": "100.00", "expected_owner_user_id": b.ActorID, "owner_user_id": b.ActorID, "owner_version": stream.Version, "owner_active": stream.Active, "owner_matches": stream.Active}})
					return
				}
				body, _ := io.ReadAll(req.Body)
				received = append(received, append([]byte(nil), body...))
				if first {
					first = false
					if unknown {
						w.WriteHeader(503)
						return
					}
					w.WriteHeader(409)
					w.Write([]byte(`{"code":"SETTLEMENT_CONFLICT"}`))
					return
				}
				var command map[string]any
				_ = json.Unmarshal(body, &command)
				json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"event_id": command["event_id"], "contract_snapshot_id": "snapshot", "status": "FINANCE_RELEASED", "migration_state": "RELEASED"}})
			}))
			defer server.Close()
			svc := &application.Service{Repo: r, ImportOwners: historyOwnerDirectory{}, ImportFinance: &application.ImportTargetTransport{BaseURL: server.URL, TokenSource: func(context.Context) (string, error) { return "isolated-test", nil }}}
			actor := application.Principal{TenantID: tenant, UserID: b.ActorID, Permissions: map[string]bool{"contract.import.release": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.import.release": {AllowAll: true}}}
			release := func(p application.Principal) error {
				t.Helper()
				b, _ = r.GetImportBatch(ctx, tenant, b.ID)
				_, e := svc.ReleaseImportItem(ctx, p, item.ID, "FINANCE", b.Items[0].Targets[1].Version, "RELEASE")
				return e
			}
			dispatch := func() {
				t.Helper()
				if e := svc.DispatchImportTargetOnce(ctx); e != nil {
					t.Fatal(e)
				}
			}
			if e := release(actor); e != nil {
				t.Fatal(e)
			}
			dispatch()
			makeTargetDue(t, r, b, "FINANCE")
			dispatch()
			b, _ = r.GetImportBatch(ctx, tenant, b.ID)
			old := b.Items[0].Targets[1]
			if len(received) != 1 || old.RequestPayload == "" {
				t.Fatal("old frozen release not sent")
			}
			snapshotVersion, reconciliation = 4, "new-local-proof"
			if unknown {
				if old.State != "WAITING" || old.RecipientHTTPStatus != 0 {
					t.Fatal("unknown response misclassified")
				}
				if e := release(actor); e != nil {
					t.Fatal(e)
				}
				b, _ = r.GetImportBatch(ctx, tenant, b.ID)
				current := b.Items[0].Targets[1]
				if current.CommandID != old.CommandID || current.RequestPayload != old.RequestPayload || current.CommandGeneration != old.CommandGeneration || len(current.CommandHistory) != 0 {
					t.Fatal("unknown response allowed payload replacement")
				}
				makeTargetDue(t, r, b, "FINANCE")
				dispatch()
				if len(received) != 2 || !bytes.Equal(received[0], received[1]) {
					t.Fatal("unknown response retry changed frozen command")
				}
				return
			}
			if old.State != "BLOCKED" || old.RecipientHTTPStatus != 409 || old.RecipientCode != "SETTLEMENT_CONFLICT" {
				t.Fatal("known rejection not persisted")
			}
			unauthorized := actor
			unauthorized.Permissions = nil
			if e := release(unauthorized); e != application.ErrForbidden {
				t.Fatal("release permission not required", e)
			}
			// 旧凭证及政策不符均不能通过单次READY查询重开。
			reconciliation = "old-local-proof"
			if e := release(actor); e == nil {
				t.Fatal("unchanged reconciliation accepted")
			}
			reconciliation = "new-local-proof"
			statusPolicy = "wrong-policy"
			if e := release(actor); e == nil {
				t.Fatal("policy conflict recovered")
			}
			statusPolicy = policy
			if e := release(actor); e != nil {
				t.Fatal("new READY not recoverable", e)
			}
			b, _ = r.GetImportBatch(ctx, tenant, b.ID)
			current := b.Items[0].Targets[1]
			if current.CommandID == old.CommandID || current.CommandGeneration != 1 || current.ReleaseVersion != old.ReleaseVersion || len(current.CommandHistory) != 1 || current.CommandHistory[0].CommandID != old.CommandID || current.CommandHistory[0].RequestPayload != old.RequestPayload || current.Epoch <= old.Epoch {
				t.Fatal("recovery history/generation not atomic")
			}
			late := old
			late.State = "COMPLETE"
			if e := r.SaveImportTarget(ctx, tenant, b.ID, item.ID, late); e != application.ErrImportConflict {
				t.Fatal("late old response overwrote new generation", e)
			}
			dispatch()
			b, _ = r.GetImportBatch(ctx, tenant, b.ID)
			if b.Items[0].Targets[1].State != "COMPLETE" || len(received) != 2 {
				t.Fatal("recovered release did not complete")
			}
			var auditCount int64
			if e := r.db.Table("con_audit_outbox").Where("tenant_id=? AND action IN ?", tenant, []string{"CONTRACT_MANAGEMENT:IMPORT_TARGET_REJECTED", "CONTRACT_MANAGEMENT:IMPORT_FINANCE_RELEASE_RETRY_AUTHORIZED"}).Count(&auditCount).Error; e != nil || auditCount != 2 {
				t.Fatal("rejection/recovery audit missing", auditCount, e)
			}
			var command struct {
				ID             string `json:"event_id"`
				Reconciliation string `json:"reconciliation_id"`
				Version        uint64 `json:"expected_snapshot_version"`
			}
			_ = json.Unmarshal(received[1], &command)
			if command.ID != current.CommandID || command.Reconciliation != "new-local-proof" || command.Version != 4 {
				t.Fatal("new baseline not sent")
			}
		})
	}
}
