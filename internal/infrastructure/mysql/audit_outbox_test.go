package mysql

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/auditctx"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 批次 2：审计事件必须与业务状态变更在同一事务里落库，投递由 dispatcher 重试。
// 本地无隔离 MySQL，这里用 DryRun 断言写入语句的目标表与关键列。
func newAuditOutboxDryRun(t *testing.T) (*gorm.DB, *[]string) {
	t.Helper()
	db, err := gorm.Open(mysql.New(mysql.Config{DSN: "test:test@tcp(localhost:3306)/test?parseTime=true", SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open dry-run database: %v", err)
	}
	captured := make([]string, 0, 2)
	if err := db.Callback().Create().After("gorm:create").Register("capture_audit_outbox", func(tx *gorm.DB) {
		if sql := tx.Statement.SQL.String(); sql != "" {
			captured = append(captured, sql)
		}
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}
	return db, &captured
}

func TestInsertAuditOutboxTargetsDedicatedTableWithProvenance(t *testing.T) {
	db, captured := newAuditOutboxDryRun(t)
	ctx := auditctx.WithMeta(context.Background(), auditctx.Meta{ClientIP: "10.8.0.5", IPSource: "TRUSTED_PROXY"})

	err := db.WithContext(ctx).Create(&auditOutboxRecord{
		ID: "01J00000000000000000000001", TenantID: "tenant-1", EventID: "event-1",
		Action: "CONTRACT_MANAGEMENT:STATUS_CHANGE", ResourceType: "contract", ResourceID: "contract-1",
		ClientIP: ipBytes(auditctx.FromContext(ctx).ClientIP), IPSource: "TRUSTED_PROXY",
		Result: "SUCCESS", DeliveryStatus: "pending", NextAttemptAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}).Error
	if err != nil {
		t.Fatalf("insert audit outbox: %v", err)
	}
	if len(*captured) == 0 {
		t.Fatal("未捕获到写入语句")
	}
	sql := (*captured)[0]
	if !strings.Contains(sql, "con_audit_outbox") {
		t.Fatalf("审计事件必须写入专用 outbox 表：%s", sql)
	}
	for _, column := range []string{"client_ip", "ip_source", "delivery_status", "next_attempt_at"} {
		if !strings.Contains(sql, column) {
			t.Errorf("写入语句缺少列 %q：%s", column, sql)
		}
	}
}

// 生命周期事务必须在同一 tx 内写审计：源码级守护，避免将来被挪回"业务提交后同步发送"。
func TestLifecycleWritesAuditOutboxInsideTransaction(t *testing.T) {
	source := readMySQLSource(t, "repository.go")
	start := strings.Index(source, "func insertLifecycle(")
	if start < 0 {
		t.Fatal("未找到 insertLifecycle")
	}
	end := strings.Index(source[start:], "\n}\n")
	body := source[start : start+end]
	if !strings.Contains(body, "insertAuditOutbox(tx,") {
		t.Fatalf("insertLifecycle 必须在业务事务内写审计 outbox：%s", body)
	}
	if strings.Contains(body, "platform.Report") || strings.Contains(body, "AuditReporter") {
		t.Fatal("insertLifecycle 不得同步发送审计（会破坏事务一致性）")
	}
}

func readMySQLSource(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(content)
}
