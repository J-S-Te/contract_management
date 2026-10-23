package auditoutbox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/j-s-te/contract-management/internal/migration"
	"github.com/j-s-te/contract-management/migrations"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestFailedAuditOutboxListAndRetryOnMySQL(t *testing.T) {
	dsn := os.Getenv("CONTRACT_TEST_DSN")
	if dsn == "" {
		t.Skip("CONTRACT_TEST_DSN is required")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil || !strings.HasPrefix(cfg.DBName, "contract_test_") || cfg.Net != "tcp" || !(strings.HasPrefix(cfg.Addr, "127.0.0.1:") || strings.HasPrefix(cfg.Addr, "localhost:")) {
		t.Fatal("CONTRACT_TEST_DSN must target contract_test_* on loopback TCP")
	}
	cfg.ParseTime = true
	ctx := context.Background()
	if err := migration.Run(ctx, cfg.FormatDSN(), migrations.Files); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(gormmysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	tenant := "AUDITFAILEDTEST00000000001"
	_ = db.Exec(`DELETE FROM con_audit_outbox WHERE tenant_id=?`, tenant).Error
	t.Cleanup(func() { _ = db.Exec(`DELETE FROM con_audit_outbox WHERE tenant_id=?`, tenant).Error })
	if err := db.Exec(`INSERT INTO con_audit_outbox(id,tenant_id,event_id,action,resource_type,resource_id,ip_source,result,delivery_status,attempts,next_attempt_at,last_error,created_at) VALUES('01AUDITFAILEDTEST00000001',?,'01AUDITEVENTTEST0000000001','CONTRACT:test','contract','c1','CLIENT','SUCCESS','failed',20,UTC_TIMESTAMP(3),'PLATFORM_AUDIT_UNAVAILABLE',UTC_TIMESTAMP(3))`, tenant).Error; err != nil {
		t.Fatal(err)
	}
	store := &GORMStore{DB: db}
	items, err := store.ListFailed(ctx, tenant, 100)
	if err != nil || len(items) != 1 || items[0].LastError != "PLATFORM_AUDIT_UNAVAILABLE" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	ok, err := store.RetryFailed(ctx, tenant, items[0].ID, time.Now().UTC())
	if err != nil || !ok {
		t.Fatalf("retry ok=%v err=%v", ok, err)
	}
	var status string
	if err := db.Raw(`SELECT delivery_status FROM con_audit_outbox WHERE tenant_id=? AND id=?`, tenant, items[0].ID).Row().Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "retry" {
		t.Fatalf("status=%s, want retry", status)
	}
}
