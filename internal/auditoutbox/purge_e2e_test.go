package auditoutbox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/contract-management/internal/migration"
	"github.com/j-s-te/contract-management/migrations"
	"github.com/oklog/ulid/v2"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// openPurgeTestStore 连接显式命名的隔离测试库并执行迁移；未配置 DSN 时跳过，
// 与同包其它 e2e 用例一致，绝不连接业务库。
func openPurgeTestStore(t *testing.T) (*gorm.DB, *GORMStore, context.Context) {
	t.Helper()
	dsn := os.Getenv("CONTRACT_TEST_DSN")
	if dsn == "" {
		t.Skip("CONTRACT_TEST_DSN is not configured; no database connection attempted")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil || !strings.HasPrefix(cfg.DBName, "contract_test_") || cfg.Net != "tcp" ||
		!(strings.HasPrefix(cfg.Addr, "127.0.0.1:") || strings.HasPrefix(cfg.Addr, "localhost:")) {
		t.Fatal("CONTRACT_TEST_DSN must target an explicitly named contract_test_* database on loopback TCP")
	}
	cfg.ParseTime = true
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	if err := migration.Run(ctx, cfg.FormatDSN(), migrations.Files); err != nil {
		t.Fatalf("dedicated test database migrations failed: %v", err)
	}
	db, err := gorm.Open(gormmysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	tenant := ulid.Make().String()
	t.Cleanup(func() {
		if err := db.Exec("DELETE FROM con_audit_outbox WHERE tenant_id = ?", tenant).Error; err != nil {
			t.Error(err)
		}
		if err := sqlDB.Close(); err != nil {
			t.Error(err)
		}
	})
	return db, &GORMStore{DB: db, Lease: 2 * time.Minute, MaxTries: 20}, ctx
}

// 清理只删"已投递且 delivered_at 早于 cutoff"的行；未送达与终态失败证据必须保留。
func TestPurgeDeliveredRemovesOnlyExpiredDeliveredRows(t *testing.T) {
	db, store, ctx := openPurgeTestStore(t)
	now := time.Now().UTC()
	older := now.Add(-40 * 24 * time.Hour)
	recent := now.Add(-1 * 24 * time.Hour)
	tenant := ulid.Make().String()
	insert := func(status string, deliveredAt *time.Time) string {
		id := ulid.Make().String()
		if err := db.Exec(`INSERT INTO con_audit_outbox
			(id, tenant_id, event_id, action, resource_type, resource_id, ip_source, result, delivery_status, attempts, next_attempt_at, delivered_at, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, tenant, ulid.Make().String(), "CONTRACT_MANAGEMENT:STATUS_CHANGE", "contract", "c-1", "UNKNOWN", "SUCCESS",
			status, 1, now.Add(-time.Minute), deliveredAt, now.Add(-45*24*time.Hour)).Error; err != nil {
			t.Fatal(err)
		}
		return id
	}
	expiredDelivered := insert("delivered", &older)
	recentDelivered := insert("delivered", &recent)
	pending := insert("pending", nil)
	retry := insert("retry", nil)
	failed := insert("failed", nil)

	deleted, err := store.PurgeDelivered(ctx, now.Add(-30*24*time.Hour), purgeBatchLimit)
	if err != nil {
		t.Fatalf("PurgeDelivered: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1（只应删除过期 delivered 行）", deleted)
	}
	var remaining []string
	if err := db.Table("con_audit_outbox").Where("tenant_id = ?", tenant).Order("id").Pluck("id", &remaining).Error; err != nil {
		t.Fatal(err)
	}
	survivors := map[string]bool{}
	for _, id := range remaining {
		survivors[id] = true
	}
	if survivors[expiredDelivered] {
		t.Fatal("过期的 delivered 行必须被删除")
	}
	for name, id := range map[string]string{"recent-delivered": recentDelivered, "pending": pending, "retry": retry, "failed": failed} {
		if !survivors[id] {
			t.Fatalf("%s 行被误删；只有过期的 delivered 行可以清理", name)
		}
	}
}

// 单次调用最多删 batchSize 行，循环调用直到返回 0；不允许一条语句删光整表。
func TestPurgeDeliveredRespectsBatchSize(t *testing.T) {
	db, store, ctx := openPurgeTestStore(t)
	now := time.Now().UTC()
	older := now.Add(-40 * 24 * time.Hour)
	tenant := ulid.Make().String()
	const total = 5
	for i := 0; i < total; i++ {
		if err := db.Exec(`INSERT INTO con_audit_outbox
			(id, tenant_id, event_id, action, resource_type, resource_id, ip_source, result, delivery_status, attempts, next_attempt_at, delivered_at, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			ulid.Make().String(), tenant, ulid.Make().String(), "CONTRACT_MANAGEMENT:STATUS_CHANGE", "contract", "c-1", "UNKNOWN", "SUCCESS",
			"delivered", 1, now.Add(-time.Minute), older, now.Add(-45*24*time.Hour)).Error; err != nil {
			t.Fatal(err)
		}
	}
	deletedPerBatch := []int64{}
	for batch := 0; batch < 10; batch++ {
		deleted, err := store.PurgeDelivered(ctx, now.Add(-30*24*time.Hour), 2)
		if err != nil {
			t.Fatalf("PurgeDelivered: %v", err)
		}
		if deleted > 2 {
			t.Fatalf("单批删除 %d 行，超过 batchSize=2", deleted)
		}
		if deleted == 0 {
			break
		}
		deletedPerBatch = append(deletedPerBatch, deleted)
	}
	if len(deletedPerBatch) != 3 || deletedPerBatch[0] != 2 || deletedPerBatch[1] != 2 || deletedPerBatch[2] != 1 {
		t.Fatalf("分批删除序列 = %v，want [2 2 1]", deletedPerBatch)
	}
	var left int64
	if err := db.Table("con_audit_outbox").Where("tenant_id = ?", tenant).Count(&left).Error; err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("remaining = %d，循环结束应清空过期 delivered 行", left)
	}
}
