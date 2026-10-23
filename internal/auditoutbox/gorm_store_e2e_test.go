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

type claimRow struct {
	ID             string
	DeliveryStatus string
}

// 租约过期的 processing 行必须可重领；仍在租约内的行不能被其他 worker 抢走。
func TestClaimRecoversExpiredProcessingRows(t *testing.T) {
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
	defer cancel()
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

	now := time.Now().UTC()
	tenant := ulid.Make().String()
	insert := func(status string, lockedAt *time.Time, createdAt time.Time) string {
		id := ulid.Make().String()
		if err := db.Exec(`INSERT INTO con_audit_outbox
			(id, tenant_id, event_id, action, resource_type, resource_id, ip_source, result, delivery_status, attempts, next_attempt_at, locked_at, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, tenant, ulid.Make().String(), "CONTRACT_MANAGEMENT:STATUS_CHANGE", "contract", "c-1", "UNKNOWN", "SUCCESS",
			status, 1, now.Add(-time.Minute), lockedAt, createdAt).Error; err != nil {
			t.Fatal(err)
		}
		return id
	}
	t.Cleanup(func() {
		if err := db.Exec("DELETE FROM con_audit_outbox WHERE tenant_id = ?", tenant).Error; err != nil {
			t.Error(err)
		}
		if err := sqlDB.Close(); err != nil {
			t.Error(err)
		}
	})
	expiredLock := now.Add(-10 * time.Minute)
	expired := insert("processing", &expiredLock, now.Add(-20*time.Minute))
	activeLock := now
	active := insert("processing", &activeLock, now.Add(-20*time.Minute))
	maxed := insert("processing", &expiredLock, now.Add(-20*time.Minute))
	if err := db.Exec("UPDATE con_audit_outbox SET attempts = 20 WHERE id = ?", maxed).Error; err != nil {
		t.Fatal(err)
	}
	pending := insert("pending", nil, now.Add(-20*time.Minute))

	store := &GORMStore{DB: db, Lease: 2 * time.Minute, MaxTries: 20}
	entries, err := store.Claim(ctx, "worker-test", 10, now)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	claimed := map[string]Entry{}
	for _, entry := range entries {
		claimed[entry.ID] = entry
	}
	if _, ok := claimed[expired]; !ok {
		t.Fatalf("expired processing row was not reclaimed: %+v", entries)
	}
	if _, ok := claimed[pending]; !ok {
		t.Fatalf("pending row was not claimed: %+v", entries)
	}
	if _, ok := claimed[maxed]; !ok {
		t.Fatalf("expired max-attempt processing row was not reclaimed: %+v", entries)
	}
	if _, ok := claimed[active]; ok {
		t.Fatalf("active lease row must not be claimed: %+v", entries)
	}
	var rows []claimRow
	if err := db.Table("con_audit_outbox").Select("id, delivery_status").Where("tenant_id = ?", tenant).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == active && row.DeliveryStatus != "processing" {
			t.Fatalf("active lease row changed status: %+v", row)
		}
	}
	// 失败回写必须释放租约，下一次 Claim 才能重新领取。
	if err := store.MarkFailed(ctx, expired, "PLATFORM_AUDIT_UNAVAILABLE", now.Add(-time.Second), now); err != nil {
		t.Fatal(err)
	}
	again, err := store.Claim(ctx, "worker-test", 10, now)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range again {
		if entry.ID == expired {
			found = true
		}
	}
	if !found {
		t.Fatalf("failed row was not requeued: %+v", again)
	}
}
