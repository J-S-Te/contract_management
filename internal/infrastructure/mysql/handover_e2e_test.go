package mysql

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/migration"
	"github.com/j-s-te/contract-management/migrations"
	"github.com/oklog/ulid/v2"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// handoverE2ERepository 复用合同仓既有的 显式启用约定：DSN 必须指向 回环地址上
// 明确命名的 contract_test_* 数据库，绝不从业务配置推导。
func handoverE2ERepository(t *testing.T) (*Repository, *gorm.DB, string) {
	t.Helper()
	dsn := os.Getenv("CONTRACT_TEST_DSN")
	if dsn == "" {
		t.Skip("CONTRACT_TEST_DSN is not configured; no database connection attempted")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil || !strings.HasPrefix(cfg.DBName, "contract_test_") || cfg.Net != "tcp" ||
		!(strings.HasPrefix(cfg.Addr, "127.0.0.1:") || strings.HasPrefix(cfg.Addr, "localhost:")) {
		t.Fatal("CONTRACT_TEST_DSN must target an explicitly named contract_test_* database on loopback TCP")
	}
	cfg.ParseTime = true
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := migration.Run(ctx, cfg.FormatDSN(), migrations.Files); err != nil {
		t.Fatalf("dedicated test database migrations failed: %v", err)
	}
	db, err := gorm.Open(gormmysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"con_handover_receipt", "con_handover_freeze"} {
		if err := db.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
	tenant := ulid.Make().String()
	if err := db.Exec("DELETE FROM con_contract WHERE tenant_id = ?", tenant).Error; err != nil {
		t.Fatal(err)
	}
	return &Repository{db: db}, db, tenant
}

func seedHandoverContract(t *testing.T, db *gorm.DB, tenantID, ownerID, status string) string {
	t.Helper()
	now := time.Now().UTC()
	contractID := ulid.Make().String()
	record := map[string]any{
		"id": contractID, "tenant_id": tenantID, "contract_number": "HANDOVER-" + contractID,
		"title": "交接测试合同", "contract_type": "sales", "service_type": "integration",
		"owner_user_id": ownerID, "owner_identity_id": ownerID, "owner_display_name": "原负责人",
		"amount_minor": 100000, "currency": "CNY", "content": "{}",
		"status": status, "version": 3, "created_at": now, "created_by": ownerID, "updated_at": now, "updated_by": ownerID,
	}
	if err := db.Exec("INSERT INTO con_contract (id, tenant_id, contract_number, title, contract_type, service_type, owner_user_id, owner_identity_id, owner_display_name, amount_minor, currency, content, status, version, created_at, created_by, updated_at, updated_by) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		record["id"], record["tenant_id"], record["contract_number"], record["title"], record["contract_type"], record["service_type"],
		record["owner_user_id"], record["owner_identity_id"], record["owner_display_name"],
		record["amount_minor"], record["currency"], record["content"], record["status"], record["version"],
		record["created_at"], record["created_by"], record["updated_at"], record["updated_by"]).Error; err != nil {
		t.Fatalf("seed contract: %v", err)
	}
	return contractID
}

func handoverOwnerVersion(t *testing.T, db *gorm.DB, contractID string) (string, uint64, string, string) {
	t.Helper()
	var row struct {
		OwnerUserID      string
		OwnerIdentityID  string
		OwnerDisplayName string
		Version          uint64
	}
	if err := db.Table("con_contract").Select("owner_user_id, owner_identity_id, owner_display_name, version").Where("id = ?", contractID).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row.OwnerUserID, row.Version, row.OwnerIdentityID, row.OwnerDisplayName
}

func TestHandoverRepositoryFreezeAndResponsibilitiesOnMySQL(t *testing.T) {
	repository, db, tenant := handoverE2ERepository(t)
	ctx := context.Background()
	leaver := ulid.Make().String()
	active := seedHandoverContract(t, db, tenant, leaver, "active")
	seedHandoverContract(t, db, tenant, leaver, "archived")

	now := time.Now().UTC()
	freeze, created, err := repository.CreateFreeze(ctx, application.HandoverFreeze{
		ID: ulid.Make().String(), TenantID: tenant, BatchID: ulid.Make().String(), RequestID: ulid.Make().String(),
		UserID: leaver, Status: application.HandoverFreezeFrozen, FrozenAt: now,
	})
	if err != nil || !created {
		t.Fatalf("freeze = %+v created=%v err=%v", freeze, created, err)
	}
	again, created, err := repository.CreateFreeze(ctx, application.HandoverFreeze{
		ID: ulid.Make().String(), TenantID: tenant, BatchID: freeze.BatchID, RequestID: freeze.RequestID,
		UserID: leaver, Status: application.HandoverFreezeFrozen, FrozenAt: now,
	})
	if err != nil || created || again.ID != freeze.ID {
		t.Fatalf("repeated freeze must be idempotent: %+v created=%v err=%v", again, created, err)
	}
	if active, found, err := repository.ActiveFreeze(ctx, tenant, leaver); err != nil || !found || active.ID != freeze.ID {
		t.Fatalf("active freeze = %+v found=%v err=%v", active, found, err)
	}

	items, err := repository.ListOwnedResponsibilities(ctx, tenant, leaver, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ResourceID != active || items[0].Category != application.HandoverCategoryContractOwner {
		t.Fatalf("responsibilities = %+v, want only the unfinished owned contract", items)
	}

	if err := repository.ReleaseFreeze(ctx, tenant, freeze.BatchID, "COMPLETED", now); err != nil {
		t.Fatal(err)
	}
	if _, found, err := repository.ActiveFreeze(ctx, tenant, leaver); err != nil || found {
		t.Fatalf("freeze must be released: found=%v err=%v", found, err)
	}
	if err := repository.ReleaseFreeze(ctx, tenant, freeze.BatchID, "COMPLETED", now); err != nil {
		t.Fatalf("repeated release must be idempotent: %v", err)
	}
}

func TestHandoverRepositoryTransferOwnershipOnMySQL(t *testing.T) {
	repository, db, tenant := handoverE2ERepository(t)
	ctx := context.Background()
	leaver, successor := ulid.Make().String(), ulid.Make().String()
	contractID := seedHandoverContract(t, db, tenant, leaver, "active")
	input := application.HandoverTransferInput{
		TenantID: tenant, BatchID: ulid.Make().String(), ItemID: ulid.Make().String(), RequestID: ulid.Make().String(),
		ResourceType: "contract", ResourceID: contractID, FromUserID: leaver, ToUserID: successor,
		ToDisplayName: "承接人", ExpectedVersion: 3, IdempotencyKey: "handover:" + contractID + ":item",
	}
	now := time.Now().UTC()
	receipt, err := repository.TransferOwnership(ctx, input, now)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != application.HandoverReceiptApplied || receipt.AppliedVersion != 4 || receipt.EvidenceRef == "" {
		t.Fatalf("receipt = %+v", receipt)
	}
	owner, version, identity, displayName := handoverOwnerVersion(t, db, contractID)
	if owner != successor || version != 4 || identity != successor || displayName != "承接人" {
		t.Fatalf("contract owner=%s identity=%s name=%s version=%d, want successor identity/name version 4", owner, identity, displayName, version)
	}

	// 幂等重放：返回既有回执，且不再改动合同。
	replayed, err := repository.TransferOwnership(ctx, input, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Status != application.HandoverReceiptApplied || replayed.AppliedVersion != 4 {
		t.Fatalf("replay receipt = %+v", replayed)
	}
	if owner, version, identity, displayName := handoverOwnerVersion(t, db, contractID); owner != successor || version != 4 || identity != successor || displayName != "承接人" {
		t.Fatalf("replay must not change the contract: owner=%s identity=%s name=%s version=%d", owner, identity, displayName, version)
	}

	// 陈旧版本：写入 REJECTED 回执，合同保持不变。
	stale := input
	stale.IdempotencyKey = input.IdempotencyKey + "-stale"
	stale.ExpectedVersion = 2
	rejected, err := repository.TransferOwnership(ctx, stale, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != application.HandoverReceiptRejected || rejected.ReasonCode != "CONTRACT_VERSION_CONFLICT" {
		t.Fatalf("stale transfer receipt = %+v", rejected)
	}
	if owner, version, identity, displayName := handoverOwnerVersion(t, db, contractID); owner != successor || version != 4 || identity != successor || displayName != "承接人" {
		t.Fatalf("rejected transfer must not change the contract: owner=%s identity=%s name=%s version=%d", owner, identity, displayName, version)
	}
	if _, found, err := repository.FindReceipt(ctx, tenant, stale.IdempotencyKey); err != nil || !found {
		t.Fatalf("rejected receipt must be persisted: found=%v err=%v", found, err)
	}
	if _, found, err := repository.FindReceipt(ctx, tenant, "missing-key"); err != nil || found {
		t.Fatalf("unknown key must report not found: found=%v err=%v", found, err)
	}
}

var _ = errors.Is

// seedHandoverApproval 写入一条审批实例及其任务，用于验证"未完成审批节点"也进入责任清单。
func seedHandoverApproval(t *testing.T, db *gorm.DB, tenantID, contractID, status string, tasks []map[string]any) string {
	t.Helper()
	now := time.Now().UTC()
	approvalID := ulid.Make().String()
	if err := db.Exec("INSERT INTO con_approval_instance (id, tenant_id, contract_id, kind, status, applicant_user_id, from_status, target_status, nodes_json, current_node_index, temporal_workflow_id, temporal_run_id, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,CAST(? AS JSON),0,?,?,?,?)",
		approvalID, tenantID, contractID, "contract", status, "user-applicant", "draft", "pending", "[]", "wf-"+approvalID, "run-"+approvalID, now, now).Error; err != nil {
		t.Fatalf("seed approval instance: %v", err)
	}
	for index, task := range tasks {
		if err := db.Exec("INSERT INTO con_approval_task (approval_id, node_id, assignee_user_id, node_name, node_index, status, approved, started_at, completed_at) VALUES (?,?,?,?,?,?,?,?,NULL)",
			approvalID, task["node_id"], task["assignee"], "合同审批节点", index, task["status"], false, now).Error; err != nil {
			t.Fatalf("seed approval task: %v", err)
		}
	}
	return approvalID
}

func TestHandoverRepositoryIncludesApprovalResponsibilitiesOnMySQL(t *testing.T) {
	repository, db, tenant := handoverE2ERepository(t)
	ctx := context.Background()
	leaver, other := ulid.Make().String(), ulid.Make().String()
	contractID := seedHandoverContract(t, db, tenant, leaver, "active")

	// 同一租户：leaver 的 active 任务（应返回）与其他用户/已完成任务（不应返回）。
	approvalID := seedHandoverApproval(t, db, tenant, contractID, "running", []map[string]any{
		{"node_id": "node-1", "assignee": leaver, "status": "active"},
		{"node_id": "node-2", "assignee": other, "status": "active"},
		{"node_id": "node-3", "assignee": leaver, "status": "completed"},
	})
	// 另一租户：即便同一个人有待办也不能跨租户返回。
	otherTenant := ulid.Make().String()
	otherContract := seedHandoverContract(t, db, otherTenant, leaver, "active")
	seedHandoverApproval(t, db, otherTenant, otherContract, "running", []map[string]any{
		{"node_id": "node-9", "assignee": leaver, "status": "active"},
	})
	// 已结束的审批实例不得计入。
	finished := seedHandoverApproval(t, db, tenant, contractID, "approved", []map[string]any{
		{"node_id": "node-8", "assignee": leaver, "status": "active"},
	})

	items, err := repository.ListOwnedResponsibilities(ctx, tenant, leaver, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]application.HandoverResponsibility{}
	for _, item := range items {
		seen[item.ResourceID] = item
	}
	if len(items) != 2 {
		t.Fatalf("items = %d (%v), want the contract owner plus one approval node", len(items), seen)
	}
	if _, ok := seen[contractID]; !ok {
		t.Fatalf("owned contract missing from responsibilities: %v", seen)
	}
	approvalItem, ok := seen[approvalID+":node-1"]
	if !ok {
		t.Fatalf("active approval node missing: %v", seen)
	}
	if approvalItem.Category != application.HandoverCategoryApprovalAssignee || approvalItem.ResourceType != application.HandoverResourceApprovalTask {
		t.Fatalf("approval responsibility mislabelled: %+v", approvalItem)
	}
	for _, unexpected := range []string{approvalID + ":node-2", approvalID + ":node-3", finished + ":node-8"} {
		if _, present := seen[unexpected]; present {
			t.Fatalf("unexpected responsibility leaked: %s", unexpected)
		}
	}
}
