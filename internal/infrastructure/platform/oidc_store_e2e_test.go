package platform

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/migration"
	"github.com/j-s-te/contract-management/migrations"
	"github.com/oklog/ulid/v2"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 本文件是空闲会话存储的 E2E 用例：只在显式隔离的 contract_test_* 回环库上运行，
// 未配置 CONTRACT_TEST_DSN 时整体跳过，绝不连接业务库或从业务配置推导 DSN。

// oidcIdleE2EStore 校验并迁移隔离测试库，返回 OIDC 会话存储与测试租户。
// 参数：t 为测试上下文；要求 CONTRACT_TEST_DSN 指向 contract_test_* 回环 TCP 库。
// 返回值：GORM OIDC 存储、底层 GORM 句柄与本次测试专用租户；测试结束按租户清理。
func oidcIdleE2EStore(t *testing.T) (*GORMOIDCStore, *gorm.DB, string) {
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
	defer cancel()
	if err := migration.Run(ctx, cfg.FormatDSN(), migrations.Files); err != nil {
		t.Fatalf("dedicated test database migrations failed: %v", err)
	}
	db, err := gorm.Open(gormmysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("dedicated test database connection failed: %v", err)
	}
	tenant := ulid.Make().String()
	t.Cleanup(func() {
		if err := db.Where("tenant_id = ?", tenant).Delete(&gormSession{}).Error; err != nil {
			t.Errorf("dedicated test session cleanup failed: %v", err)
		}
	})
	store, err := NewGORMOIDCStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return store, db, tenant
}

// oidcIdleE2ESession 构造待插入的会话行；lastInteractive 为 nil 模拟未回填的旧行。
func oidcIdleE2ESession(t *testing.T, tenant string, now time.Time, lastInteractive *time.Time) SessionRecord {
	t.Helper()
	principalJSON, err := json.Marshal(application.Principal{
		Subject: "kc-e2e-user", TenantID: tenant, UserID: "identity-e2e", IdentityID: "identity-e2e",
		AuthorizationRevision: 1,
	})
	if err != nil {
		t.Fatalf("marshal principal: %v", err)
	}
	lastSeen := now
	return SessionRecord{
		SessionIDHash: tokenHash(ulid.Make().String()), TenantID: tenant, IdentityID: "identity-e2e",
		PrincipalJSON: principalJSON, AccessTokenCiphertext: []byte("access"),
		RefreshTokenCiphertext: []byte("refresh"), IDTokenCiphertext: []byte("id"),
		AuthorizationRevision: 1, AuthorizationCheckedAt: now, TokenExpiresAt: now.Add(time.Hour),
		SessionExpiresAt: now.Add(8 * time.Hour), LastSeenAt: &lastSeen, LastInteractiveAt: lastInteractive,
		CreatedAt: now, UpdatedAt: now,
	}
}

func loadE2ESession(t *testing.T, store *GORMOIDCStore, hash []byte, now time.Time) SessionRecord {
	t.Helper()
	record, err := store.FindSession(context.Background(), hash, now)
	if err != nil {
		t.Fatalf("FindSession() error = %v", err)
	}
	return record
}

// TestGORMOIDCStoreSessionColumnsExistAfterMigration 验证 000039 迁移已落地到隔离库：
// 空闲基准列与普通请求观测列都必须存在，避免缺少列时所有已认证请求直接 401。
func TestGORMOIDCStoreSessionColumnsExistAfterMigration(t *testing.T) {
	_, db, _ := oidcIdleE2EStore(t)
	var count int64
	if err := db.Raw("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name IN (?, ?)",
		"con_oidc_session", "last_seen_at", "last_interactive_at").Scan(&count).Error; err != nil {
		t.Fatalf("query information_schema: %v", err)
	}
	if count != 2 {
		t.Fatalf("con_oidc_session idle columns = %d, want 2", count)
	}
}

// TestGORMOIDCStoreCreateSessionPersistsIdleTimestamps 验证新会话同时写入绝对期限、
// last_seen_at 与 last_interactive_at。
func TestGORMOIDCStoreCreateSessionPersistsIdleTimestamps(t *testing.T) {
	store, _, tenant := oidcIdleE2EStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := oidcIdleE2ESession(t, tenant, now, &now)
	if err := store.CreateSession(context.Background(), record); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	loaded := loadE2ESession(t, store, record.SessionIDHash, now)
	if loaded.LastSeenAt == nil || !loaded.LastSeenAt.Equal(now) {
		t.Fatalf("LastSeenAt = %v, want %v", loaded.LastSeenAt, now)
	}
	if loaded.LastInteractiveAt == nil || !loaded.LastInteractiveAt.Equal(now) {
		t.Fatalf("LastInteractiveAt = %v, want %v", loaded.LastInteractiveAt, now)
	}
	if !loaded.SessionExpiresAt.Equal(now.Add(8 * time.Hour)) {
		t.Fatalf("SessionExpiresAt = %v, want absolute 8h", loaded.SessionExpiresAt)
	}
}

// TestGORMOIDCStoreTouchLastSeenDoesNotAdvanceIdle 验证普通请求只更新 last_seen_at，
// 空闲基准 last_interactive_at 必须保持不变（轮询不能续命）。
func TestGORMOIDCStoreTouchLastSeenDoesNotAdvanceIdle(t *testing.T) {
	store, _, tenant := oidcIdleE2EStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	lastInteractive := now.Add(-20 * time.Minute)
	record := oidcIdleE2ESession(t, tenant, now, &lastInteractive)
	if err := store.CreateSession(context.Background(), record); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if err := store.TouchSessionLastSeen(context.Background(), record.SessionIDHash, now); err != nil {
		t.Fatalf("TouchSessionLastSeen() error = %v", err)
	}
	loaded := loadE2ESession(t, store, record.SessionIDHash, now)
	if loaded.LastSeenAt == nil || !loaded.LastSeenAt.Equal(now) {
		t.Fatalf("LastSeenAt = %v, want %v", loaded.LastSeenAt, now)
	}
	if loaded.LastInteractiveAt == nil || !loaded.LastInteractiveAt.Equal(lastInteractive) {
		t.Fatalf("LastInteractiveAt = %v, want unchanged %v", loaded.LastInteractiveAt, lastInteractive)
	}
}

// TestGORMOIDCStoreRecordInteractionExtendsIdleOnly 验证显式活动推进空闲基准，
// 但不修改绝对到期时间。
func TestGORMOIDCStoreRecordInteractionExtendsIdleOnly(t *testing.T) {
	store, _, tenant := oidcIdleE2EStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	lastInteractive := now.Add(-29 * time.Minute)
	record := oidcIdleE2ESession(t, tenant, now, &lastInteractive)
	absoluteBefore := record.SessionExpiresAt
	if err := store.CreateSession(context.Background(), record); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if err := store.RecordInteraction(context.Background(), record.SessionIDHash, now, 30*time.Minute); err != nil {
		t.Fatalf("RecordInteraction() error = %v", err)
	}
	loaded := loadE2ESession(t, store, record.SessionIDHash, now)
	if loaded.LastInteractiveAt == nil || !loaded.LastInteractiveAt.Equal(now) {
		t.Fatalf("LastInteractiveAt = %v, want %v", loaded.LastInteractiveAt, now)
	}
	if !loaded.SessionExpiresAt.Equal(absoluteBefore) {
		t.Fatalf("absolute expiry changed by activity: %v -> %v", absoluteBefore, loaded.SessionExpiresAt)
	}
}

// TestGORMOIDCStoreRecordInteractionRejectsIdleExpired 验证空闲到期后活动接口返回未认证，
// 且不会把旧会话的空闲基准推进到当前时间（不复活）。
func TestGORMOIDCStoreRecordInteractionRejectsIdleExpired(t *testing.T) {
	store, _, tenant := oidcIdleE2EStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	lastInteractive := now.Add(-31 * time.Minute)
	record := oidcIdleE2ESession(t, tenant, now, &lastInteractive)
	if err := store.CreateSession(context.Background(), record); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if err := store.RecordInteraction(context.Background(), record.SessionIDHash, now, 30*time.Minute); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("RecordInteraction() error = %v, want ErrUnauthenticated", err)
	}
	loaded := loadE2ESession(t, store, record.SessionIDHash, now)
	if loaded.LastInteractiveAt == nil || !loaded.LastInteractiveAt.Equal(lastInteractive) {
		t.Fatalf("idle-expired interaction revived session: last_interactive_at = %v", loaded.LastInteractiveAt)
	}
}

// TestGORMOIDCStoreRecordInteractionRejectsRevokedOrAbsoluteExpired 验证撤销/绝对到期会话
// 不能通过活动接口恢复。
func TestGORMOIDCStoreRecordInteractionRejectsRevokedOrAbsoluteExpired(t *testing.T) {
	store, _, tenant := oidcIdleE2EStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	lastInteractive := now
	if err := store.CreateSession(context.Background(), oidcIdleE2ESession(t, tenant, now, &lastInteractive)); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	// 已撤销：用同一租户再造一条并在插入后撤销。
	revoked := oidcIdleE2ESession(t, tenant, now, &lastInteractive)
	if err := store.CreateSession(context.Background(), revoked); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if err := store.RevokeSession(context.Background(), revoked.SessionIDHash, now); err != nil {
		t.Fatalf("RevokeSession() error = %v", err)
	}
	if err := store.RecordInteraction(context.Background(), revoked.SessionIDHash, now, 30*time.Minute); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked session interaction error = %v, want ErrUnauthenticated", err)
	}
	// 绝对到期：记录本身已超过 session_expires_at。
	expired := oidcIdleE2ESession(t, tenant, now.Add(-2*time.Hour), &lastInteractive)
	expired.SessionExpiresAt = now.Add(-time.Hour)
	if err := store.CreateSession(context.Background(), expired); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if err := store.RecordInteraction(context.Background(), expired.SessionIDHash, now, 30*time.Minute); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("absolute-expired session interaction error = %v, want ErrUnauthenticated", err)
	}
}

// TestGORMOIDCStoreRecordInteractionAcceptsLegacyNullInteractiveRow 验证未回填旧行
// （last_interactive_at=NULL）按 created_at 计算空闲窗口，升级后不会被误锁死。
func TestGORMOIDCStoreRecordInteractionAcceptsLegacyNullInteractiveRow(t *testing.T) {
	store, db, tenant := oidcIdleE2EStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := oidcIdleE2ESession(t, tenant, now, nil)
	record.CreatedAt = now.Add(-10 * time.Minute)
	if err := store.CreateSession(context.Background(), record); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	// 明确清空为 NULL，模拟迁移尚未回填/历史异常行。
	if err := db.Exec("UPDATE con_oidc_session SET last_interactive_at = NULL WHERE session_id_hash = ?", record.SessionIDHash).Error; err != nil {
		t.Fatalf("clear last_interactive_at: %v", err)
	}
	loaded := loadE2ESession(t, store, record.SessionIDHash, now)
	if loaded.LastInteractiveAt != nil {
		t.Fatalf("LastInteractiveAt = %v, want NULL", loaded.LastInteractiveAt)
	}
	if err := store.RecordInteraction(context.Background(), record.SessionIDHash, now, 30*time.Minute); err != nil {
		t.Fatalf("RecordInteraction() legacy NULL row error = %v", err)
	}
	loaded = loadE2ESession(t, store, record.SessionIDHash, now)
	if loaded.LastInteractiveAt == nil || !loaded.LastInteractiveAt.Equal(now) {
		t.Fatalf("LastInteractiveAt = %v, want %v", loaded.LastInteractiveAt, now)
	}
}
