package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/contract-management/internal/businessclock"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/oklog/ulid/v2"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 本文件只包含手动触发的死锁压测，不参与默认测试套件。
//
// 默认 go test ./... 不会设置 CONTRACT_DEADLOCK_STRESS，因此三个用例都会 Skip；
// 只有在显式设置该变量且 CONTRACT_TEST_DSN 指向 contract_test_* 回环库时才真正连库。
// 用例只创建本文件独占的 STRESS- 租户/合同/编号，并严格按这些标识清理，
// 不触碰 GTSR-/HIMPB-/HT- 等其它用例的流水桶与登记表数据。

const (
	deadlockStressGateEnv  = "CONTRACT_DEADLOCK_STRESS"
	deadlockStressLoopsEnv = "CONTRACT_DEADLOCK_STRESS_LOOPS"

	deadlockStressLockWaitSeconds = 5
	deadlockStressSessionTimeout  = 20 * time.Second
	deadlockStressOverallTimeout  = 50 * time.Second
	deadlockStressSequenceYear    = 2099
)

// deadlockStressRepository 打开隔离测试库并返回仓储与底层连接池。
//
// 与 draftE2ERepository 一样复用 draftE2ETestDSN 的库名校验/迁移纪律，
// 但本文件需要多个租户和独立的 *sql.Conn，因此单独建连、单独清理。
func deadlockStressRepository(t *testing.T) (*Repository, *sql.DB) {
	t.Helper()
	if os.Getenv(deadlockStressGateEnv) == "" {
		t.Skipf("%s is not set; manual deadlock stress suite skipped", deadlockStressGateEnv)
	}
	dsn := draftE2ETestDSN(t)
	db, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("dedicated deadlock-stress database connection failed")
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(32)
	sqlDB.SetMaxIdleConns(32)
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Error(err)
		}
	})
	return NewRepository(db), sqlDB
}

// deadlockStressTenantID 生成 26 位、带 STRESS- 独占前缀的测试租户。
func deadlockStressTenantID() string {
	id := ulid.Make().String()
	return "STRESS-" + strings.ToUpper(id[len(id)-19:])
}

// deadlockStressRunTag 生成本次运行独占的编号前缀片段，避免同一隔离库上的
// 并发/重复运行互相清理对方的 STRESS- 数据。
func deadlockStressRunTag() string {
	id := strings.ToUpper(ulid.Make().String())
	return id[len(id)-6:]
}

// deadlockStressContracts 按 id 前缀精确清理本用例造的合同行。
func deadlockStressCleanupContracts(t *testing.T, r *Repository, tenantIDs []string) {
	t.Helper()
	if len(tenantIDs) == 0 {
		return
	}
	if err := r.db.Where("tenant_id IN ?", tenantIDs).Delete(&contractRecord{}).Error; err != nil {
		t.Errorf("deadlock-stress contract cleanup failed: %v", err)
	}
}

// seedDeadlockStressContracts 直接插入本用例独占的合同行。
//
// 这里不调用 CreateContract：该入口会额外写审计/责任 outbox，而本文件只关心
// con_contract 行锁与编号分配。合同号先留 NULL，避免触发 uk_con_contract_number。
func seedDeadlockStressContracts(t *testing.T, r *Repository, tenantIDs []string, idsByTenant map[string][]string) {
	t.Helper()
	now := time.Now().UTC()
	var records []contractRecord
	for _, tenantID := range tenantIDs {
		for _, id := range idsByTenant[tenantID] {
			records = append(records, contractRecord{
				ID: id, TenantID: tenantID, Title: "deadlock stress", ContractType: "stress",
				ServiceType: "stress", OwnerUserID: "stress-owner", AmountMinor: 1, Currency: "CNY",
				Content: "{}", Status: string(contract.StatusDraft), Version: 1,
				CreatedAt: now, CreatedBy: "stress", UpdatedAt: now, UpdatedBy: "stress",
			})
		}
	}
	if len(records) == 0 {
		return
	}
	if err := r.db.CreateInBatches(records, 100).Error; err != nil {
		t.Fatalf("seed deadlock-stress contracts: %v", err)
	}
}

// deadlockStressLockRow 在给定连接上执行一行加锁读，用于精确编排交错点。
func deadlockStressLockRow(ctx context.Context, conn *sql.Conn, query string, args ...any) error {
	var id string
	return conn.QueryRowContext(ctx, query, args...).Scan(&id)
}

// deadlockStressLockSequence 在当前连接/事务上对流水行加 X 锁。
func deadlockStressLockSequence(ctx context.Context, conn *sql.Conn, year int, ruleKey string) error {
	var current uint64
	return conn.QueryRowContext(ctx,
		`SELECT current_value FROM con_contract_number_sequence WHERE seq_year = ? AND rule_key = ? FOR UPDATE`,
		year, ruleKey).Scan(&current)
}

// deadlockStressBegin 开始一个事务并把会话锁等待超时调小，避免错误用例挂几十秒。
func deadlockStressBegin(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, fmt.Sprintf("SET SESSION innodb_lock_wait_timeout = %d", deadlockStressLockWaitSeconds))
	return err
}

// deadlockStressMySQLErrorNumber 提取驱动错误码；非 MySQL 错误返回 0。
func deadlockStressMySQLErrorNumber(err error) uint16 {
	var mysqlErr *mysqldriver.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number
	}
	return 0
}

// deadlockStressIsDeadlock 判断错误是否为 InnoDB 死锁（1213）。
func deadlockStressIsDeadlock(err error) bool {
	return deadlockStressMySQLErrorNumber(err) == 1213
}

// deadlockStressIsLockWaitTimeout 判断错误是否为锁等待超时（1205）。
func deadlockStressIsLockWaitTimeout(err error) bool {
	return deadlockStressMySQLErrorNumber(err) == 1205
}

// deadlockStressRecvErr 等待一个会话步骤结果，避免编排错误时永久挂起。
func deadlockStressRecvErr(t *testing.T, ch <-chan error, label string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(deadlockStressSessionTimeout):
		t.Fatalf("%s: timed out waiting for session step", label)
		return nil
	}
}

// deadlockStressEnsureSequenceRow 预建测试规则桶的流水行，让两个会话都只做加锁读。
func deadlockStressEnsureSequenceRow(t *testing.T, r *Repository, year int, ruleKey string) {
	t.Helper()
	if err := r.db.Exec(`INSERT INTO con_contract_number_sequence (seq_year, rule_key, current_value, updated_at)
		VALUES (?, ?, 0, ?) ON DUPLICATE KEY UPDATE current_value = 0, updated_at = VALUES(updated_at)`,
		year, ruleKey, time.Now().UTC()).Error; err != nil {
		t.Fatalf("ensure deadlock-stress sequence row: %v", err)
	}
}

// DeadlockStress 用例的公共前置：造两条合同行并注册精确清理。
func setupDeadlockStressContracts(t *testing.T, r *Repository, tenantID string) []string {
	t.Helper()
	ids := []string{ulid.Make().String(), ulid.Make().String()}
	seedDeadlockStressContracts(t, r, []string{tenantID}, map[string][]string{tenantID: ids})
	t.Cleanup(func() { deadlockStressCleanupContracts(t, r, []string{tenantID}) })
	return ids
}

// TestDeadlockStressLegacyOrderFormsCycle 确定性复现旧顺序的 1213。
//
// 交错时序（每一步都由 channel 精确放行，不依赖 sleep 碰运气）：
//
//	会话 A                         会话 B
//	BEGIN                          （等待 A 锁住 c2）
//	SELECT ... c2 FOR UPDATE  ✔
//	  ── 通知主协程 ──             BEGIN
//	                                SELECT ... 流水行 FOR UPDATE ✔
//	  ── 通知主协程 ──             ── 双方同时放行 ──
//	SELECT ... 流水行 FOR UPDATE ⏳  SELECT ... c2 FOR UPDATE ⏳
//	        A 持 c2 等流水            B 持流水等 c2
//	                  └── 环闭合，InnoDB 立即回滚一方（1213）
//
// 断言：恰好一方拿到 1213，另一方成功；不允许退化成 1205 锁等待超时。
func TestDeadlockStressLegacyOrderFormsCycle(t *testing.T) {
	r, sqlDB := deadlockStressRepository(t)
	tenantID := deadlockStressTenantID()
	ids := setupDeadlockStressContracts(t, r, tenantID)
	c1, c2 := ids[0], ids[1]
	ruleKey := "stress-old-" + deadlockStressRunTag()
	deadlockStressEnsureSequenceRow(t, r, deadlockStressSequenceYear, ruleKey)
	t.Cleanup(func() {
		if err := r.db.Exec(`DELETE FROM con_contract_number_sequence WHERE seq_year = ? AND rule_key = ?`,
			deadlockStressSequenceYear, ruleKey).Error; err != nil {
			t.Errorf("deadlock-stress old-order sequence cleanup failed: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), deadlockStressSessionTimeout)
	defer cancel()
	connA, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connA.Close()
	connB, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connB.Close()

	proceed := make(chan struct{})
	aPhase1, bPhase1 := make(chan error, 1), make(chan error, 1)
	aPhase2, bPhase2 := make(chan error, 1), make(chan error, 1)
	aRollback, bRollback := make(chan error, 1), make(chan error, 1)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := deadlockStressBegin(ctx, connA); err != nil {
			aPhase1 <- err
			return
		}
		if err := deadlockStressLockRow(ctx, connA, `SELECT id FROM con_contract WHERE id = ? FOR UPDATE`, c2); err != nil {
			aPhase1 <- err
			return
		}
		aPhase1 <- nil
		select {
		case <-proceed:
		case <-ctx.Done():
			aPhase2 <- ctx.Err()
			return
		}
		aPhase2 <- deadlockStressLockSequence(ctx, connA, deadlockStressSequenceYear, ruleKey)
		_, rbErr := connA.ExecContext(context.Background(), "ROLLBACK")
		aRollback <- rbErr
	}()

	if err := deadlockStressRecvErr(t, aPhase1, "session A lock c2"); err != nil {
		t.Fatalf("session A failed to lock c2: %v", err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := deadlockStressBegin(ctx, connB); err != nil {
			bPhase1 <- err
			return
		}
		if err := deadlockStressLockSequence(ctx, connB, deadlockStressSequenceYear, ruleKey); err != nil {
			bPhase1 <- err
			return
		}
		bPhase1 <- nil
		select {
		case <-proceed:
		case <-ctx.Done():
			bPhase2 <- ctx.Err()
			return
		}
		bPhase2 <- deadlockStressLockRow(ctx, connB, `SELECT id FROM con_contract WHERE id = ? FOR UPDATE`, c2)
		_, rbErr := connB.ExecContext(context.Background(), "ROLLBACK")
		bRollback <- rbErr
	}()

	if err := deadlockStressRecvErr(t, bPhase1, "session B lock sequence"); err != nil {
		t.Fatalf("session B failed to lock sequence: %v", err)
	}
	close(proceed)

	errA := deadlockStressRecvErr(t, aPhase2, "session A lock sequence")
	errB := deadlockStressRecvErr(t, bPhase2, "session B lock c2")
	if err := deadlockStressRecvErr(t, aRollback, "session A rollback"); err != nil {
		t.Errorf("session A rollback failed: %v", err)
	}
	if err := deadlockStressRecvErr(t, bRollback, "session B rollback"); err != nil {
		t.Errorf("session B rollback failed: %v", err)
	}
	wg.Wait()

	deadlocks := 0
	if deadlockStressIsDeadlock(errA) {
		deadlocks++
		t.Logf("session A received InnoDB deadlock: %v", errA)
	} else if errA != nil {
		t.Fatalf("session A returned non-deadlock error: %v", errA)
	}
	if deadlockStressIsDeadlock(errB) {
		deadlocks++
		t.Logf("session B received InnoDB deadlock: %v", errB)
	} else if errB != nil {
		t.Fatalf("session B returned non-deadlock error: %v", errB)
	}
	if deadlocks != 1 {
		t.Fatalf("旧顺序交叉必须恰好一方被 InnoDB 回滚(1213)，实际 deadlocks=%d (A=%v B=%v); c1=%s c2=%s",
			deadlocks, errA, errB, c1, c2)
	}
}

// TestDeadlockStressNewOrderDoesNotCycle 证明两阶段新顺序在同样的“持锁等待”交错下不成环。
//
// 这里刻意把并发审批型事务放在下面的位置：
//   - A（批次，新顺序）先锁 c1，随后尝试按 id 升序锁 c2；在拿到 c2 前绝不碰流水行。
//   - B（审批型）先锁 c2，再锁流水行。它在整个过程中持有 c2。
//
// 因此 A 只能停在 c2 的等待队列上，此时 A 手上只有 c1、没有流水行；B 可以顺利拿到
// 流水行，不构成环。B 释放后 A 才拿到 c2，再取流水行。
//
// 注意：如果把 B 写成“先锁流水、再锁 c2”，而 A 已经持有 c2，那么即使 A 使用新顺序，
// 也会和这个“反向加锁”的旧对端成环。新顺序保证的是与审批同向（合同行→流水行），
// 不是对任意反向加锁事务免疫；因此本回归用例的 B 必须模拟真实审批的加锁方向。
func TestDeadlockStressNewOrderDoesNotCycle(t *testing.T) {
	r, sqlDB := deadlockStressRepository(t)
	tenantID := deadlockStressTenantID()
	ids := setupDeadlockStressContracts(t, r, tenantID)
	sort.Strings(ids)
	c1, c2 := ids[0], ids[1]
	ruleKey := "stress-new-" + deadlockStressRunTag()
	deadlockStressEnsureSequenceRow(t, r, deadlockStressSequenceYear, ruleKey)
	t.Cleanup(func() {
		if err := r.db.Exec(`DELETE FROM con_contract_number_sequence WHERE seq_year = ? AND rule_key = ?`,
			deadlockStressSequenceYear, ruleKey).Error; err != nil {
			t.Errorf("deadlock-stress new-order sequence cleanup failed: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), deadlockStressSessionTimeout)
	defer cancel()
	connA, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connA.Close()
	connB, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connB.Close()

	allowAtoC2 := make(chan struct{})
	allowBtoSequence := make(chan struct{})
	allowBrollback := make(chan struct{})
	aPhase1, bPhase1 := make(chan error, 1), make(chan error, 1)
	aGotC2, aGotSequence := make(chan error, 1), make(chan error, 1)
	bGotSequence, bDone := make(chan error, 1), make(chan error, 1)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := deadlockStressBegin(ctx, connA); err != nil {
			aPhase1 <- err
			return
		}
		if err := deadlockStressLockRow(ctx, connA, `SELECT id FROM con_contract WHERE id = ? FOR UPDATE`, c1); err != nil {
			aPhase1 <- err
			return
		}
		aPhase1 <- nil
		select {
		case <-allowAtoC2:
		case <-ctx.Done():
			aGotC2 <- ctx.Err()
			aGotSequence <- ctx.Err()
			return
		}
		// 新顺序：先把本批合同行按 id 升序拿全，再进入取号。
		if err := deadlockStressLockRow(ctx, connA, `SELECT id FROM con_contract WHERE id = ? FOR UPDATE`, c2); err != nil {
			aGotC2 <- err
			aGotSequence <- err
			_, _ = connA.ExecContext(context.Background(), "ROLLBACK")
			return
		}
		aGotC2 <- nil
		aGotSequence <- deadlockStressLockSequence(ctx, connA, deadlockStressSequenceYear, ruleKey)
		_, _ = connA.ExecContext(context.Background(), "ROLLBACK")
	}()

	if err := deadlockStressRecvErr(t, aPhase1, "batch session lock c1"); err != nil {
		t.Fatalf("batch session failed to lock c1: %v", err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := deadlockStressBegin(ctx, connB); err != nil {
			bPhase1 <- err
			return
		}
		// 审批型：先锁自己的合同行。
		if err := deadlockStressLockRow(ctx, connB, `SELECT id FROM con_contract WHERE id = ? FOR UPDATE`, c2); err != nil {
			bPhase1 <- err
			return
		}
		bPhase1 <- nil
		select {
		case <-allowBtoSequence:
		case <-ctx.Done():
			bGotSequence <- ctx.Err()
			bDone <- ctx.Err()
			return
		}
		// 审批型：再取流水行。此时 A 尚未持有流水行，所以这里不应被 A 阻塞。
		bGotSequence <- deadlockStressLockSequence(ctx, connB, deadlockStressSequenceYear, ruleKey)
		select {
		case <-allowBrollback:
		case <-ctx.Done():
			bDone <- ctx.Err()
			return
		}
		_, rbErr := connB.ExecContext(context.Background(), "ROLLBACK")
		bDone <- rbErr
	}()

	if err := deadlockStressRecvErr(t, bPhase1, "approval session lock c2"); err != nil {
		t.Fatalf("approval session failed to lock c2: %v", err)
	}

	started := time.Now()
	close(allowAtoC2)
	close(allowBtoSequence)
	if err := deadlockStressRecvErr(t, bGotSequence, "approval session lock sequence"); err != nil {
		t.Fatalf("approval session 在 A 只持有 c1 时取流水失败: %v", err)
	}

	// B 仍持有 c2，A 不可能越过 c2 去碰流水行；若这里提前拿到 c2，说明编排不成立。
	select {
	case err := <-aGotC2:
		t.Fatalf("batch session acquired c2 before approval session released it: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	close(allowBrollback)
	if err := deadlockStressRecvErr(t, bDone, "approval session rollback"); err != nil {
		t.Fatalf("approval session rollback failed: %v", err)
	}
	if err := deadlockStressRecvErr(t, aGotC2, "batch session lock c2 after approval release"); err != nil {
		t.Fatalf("batch session 在审批释放后仍未拿到 c2: %v", err)
	}
	if err := deadlockStressRecvErr(t, aGotSequence, "batch session lock sequence after c2"); err != nil {
		t.Fatalf("batch session 拿到 c2 后取流水失败: %v", err)
	}
	wg.Wait()

	t.Logf("new order completed without deadlock: approval-first, batch waited on c2 then sequence, elapsed=%s", time.Since(started))
}

// deadlockStressAllocationTask 描述一次真实取号调用（审批型或导入型）。
type deadlockStressAllocationTask struct {
	kind        string
	tenantID    string
	contractIDs []string
}

// deadlockStressAllocationResult 记录一次任务分配到的全部编号或首个错误。
type deadlockStressAllocationResult struct {
	task    deadlockStressAllocationTask
	numbers []string
	err     error
}

// runDeadlockStressApprovalTask 复刻审批路径：锁合同行 → allocateContractNumberInTx。
func runDeadlockStressApprovalTask(ctx context.Context, r *Repository, task deadlockStressAllocationTask) ([]string, error) {
	tx := r.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return nil, tx.Error
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.Exec(fmt.Sprintf("SET SESSION innodb_lock_wait_timeout = %d", deadlockStressLockWaitSeconds)).Error; err != nil {
		return nil, err
	}
	var locked string
	if err := tx.Raw(`SELECT id FROM con_contract WHERE tenant_id = ? AND id = ? FOR UPDATE`,
		task.tenantID, task.contractIDs[0]).Scan(&locked).Error; err != nil {
		return nil, err
	}
	if locked != task.contractIDs[0] {
		return nil, fmt.Errorf("approval lock missed contract %s", task.contractIDs[0])
	}
	number, err := allocateContractNumberInTx(tx, task.tenantID, task.contractIDs[0], "", time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if err := tx.Model(&contractRecord{}).Where("tenant_id = ? AND id = ?", task.tenantID, task.contractIDs[0]).
		Update("contract_number", number).Error; err != nil {
		return nil, err
	}
	if err := tx.Commit().Error; err != nil {
		return nil, err
	}
	return []string{number}, nil
}

// runDeadlockStressImportTask 复刻两阶段导入路径：按 id 升序锁全部目标合同行 → 逐条取号。
func runDeadlockStressImportTask(ctx context.Context, r *Repository, task deadlockStressAllocationTask) ([]string, error) {
	ids := append([]string(nil), task.contractIDs...)
	sort.Strings(ids)
	tx := r.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return nil, tx.Error
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.Exec(fmt.Sprintf("SET SESSION innodb_lock_wait_timeout = %d", deadlockStressLockWaitSeconds)).Error; err != nil {
		return nil, err
	}
	var locked []string
	if err := tx.Raw(`SELECT id FROM con_contract WHERE tenant_id = ? AND id IN ? ORDER BY id ASC FOR UPDATE`,
		task.tenantID, ids).Scan(&locked).Error; err != nil {
		return nil, err
	}
	if len(locked) != len(ids) {
		return nil, fmt.Errorf("import pre-lock found %d/%d contracts", len(locked), len(ids))
	}
	numbers := make([]string, 0, len(ids))
	for _, id := range ids {
		number, err := allocateContractNumberInTx(tx, task.tenantID, id, "", time.Now().UTC())
		if err != nil {
			return nil, err
		}
		if err := tx.Model(&contractRecord{}).Where("tenant_id = ? AND id = ?", task.tenantID, id).
			Update("contract_number", number).Error; err != nil {
			return nil, err
		}
		numbers = append(numbers, number)
	}
	if err := tx.Commit().Error; err != nil {
		return nil, err
	}
	return numbers, nil
}

// deadlockStressSaveRule 写入本次压测规则。
func deadlockStressSaveRule(t *testing.T, r *Repository, rule contract.ContractNumberRule) {
	t.Helper()
	normalized, err := contract.NormalizeContractNumberRule(rule)
	if err != nil {
		t.Fatalf("normalize deadlock-stress rule: %v", err)
	}
	normalized.UpdatedBy = "deadlock-stress"
	normalized.UpdatedAt = time.Now().UTC()
	if err := r.SaveContractNumberRule(context.Background(), normalized); err != nil {
		t.Fatalf("save deadlock-stress rule: %v", err)
	}
}

// deadlockStressRestoreDefaultRule 在用例结束恢复迁移种子默认规则。
func deadlockStressRestoreDefaultRule(t *testing.T, r *Repository) {
	t.Helper()
	t.Cleanup(func() {
		rule := contract.DefaultContractNumberRule()
		rule.UpdatedBy = "deadlock-stress"
		rule.UpdatedAt = time.Now().UTC()
		if err := r.SaveContractNumberRule(context.Background(), rule); err != nil {
			t.Errorf("restore default contract number rule: %v", err)
		}
	})
}

// deadlockStressLoops 读取压测轮数；默认 30 轮，显式环境变量可覆盖。
func deadlockStressLoops(t *testing.T) int {
	t.Helper()
	raw := os.Getenv(deadlockStressLoopsEnv)
	if raw == "" {
		return 30
	}
	loops, err := strconv.Atoi(raw)
	if err != nil || loops <= 0 {
		t.Fatalf("%s must be a positive integer, got %q", deadlockStressLoopsEnv, raw)
	}
	return loops
}

// TestDeadlockStressRealAllocation 用真实 allocateContractNumberInTx 做并发正向压力。
//
// 每轮同时发起 4 个审批型任务（锁 1 行）和 2 个导入型任务（锁 3 行），两类事务
// 都遵循“全部合同行（id 升序）→ 流水行”的全局顺序。默认 30 轮，共 300 次分配。
//
// 断言：没有 1213/1205；编号全系统唯一；登记表每个编号恰好一行；
// 流水水位精确等于成功分配数（回滚的取号不占水位）。
func TestDeadlockStressRealAllocation(t *testing.T) {
	r, _ := deadlockStressRepository(t)
	deadlockStressRestoreDefaultRule(t, r)
	tag := deadlockStressRunTag()
	rule := contract.ContractNumberRule{Prefix: "STRESS-" + tag + "-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 4}
	deadlockStressSaveRule(t, r, rule)
	ruleKey, err := contract.ContractNumberRuleKey(rule)
	if err != nil {
		t.Fatal(err)
	}
	// 生产取号按北京日历日取年，断言必须同口径。
	year := businessclock.YearOf(time.Now())

	// 前置清理：只限本次运行独占的编号前缀/租户/规则桶。
	numberPattern := rule.Prefix + "%"
	if err := r.db.Where("contract_number LIKE ?", numberPattern).Delete(&contractNumberRegistryRecord{}).Error; err != nil {
		t.Fatalf("pre-clean deadlock-stress registry: %v", err)
	}
	if err := r.db.Exec(`DELETE FROM con_contract_number_sequence WHERE seq_year = ? AND rule_key = ?`, year, ruleKey).Error; err != nil {
		t.Fatalf("pre-clean deadlock-stress sequence: %v", err)
	}
	tenants := []string{deadlockStressTenantID(), deadlockStressTenantID(), deadlockStressTenantID()}
	t.Cleanup(func() {
		if err := r.db.Where("contract_number LIKE ?", numberPattern).Delete(&contractNumberRegistryRecord{}).Error; err != nil {
			t.Errorf("deadlock-stress registry cleanup failed: %v", err)
		}
		deadlockStressCleanupContracts(t, r, tenants)
		if err := r.db.Exec(`DELETE FROM con_contract_number_sequence WHERE seq_year = ? AND rule_key = ?`, year, ruleKey).Error; err != nil {
			t.Errorf("deadlock-stress sequence cleanup failed: %v", err)
		}
	})

	loops := deadlockStressLoops(t)
	const (
		approvalsPerRound     = 4
		importBatchesPerRound = 2
		importBatchSize       = 3
	)
	idsByTenant := map[string][]string{}
	var rounds [][]deadlockStressAllocationTask
	for round := 0; round < loops; round++ {
		var tasks []deadlockStressAllocationTask
		for i := 0; i < approvalsPerRound; i++ {
			tenantID := tenants[(round+i)%len(tenants)]
			id := ulid.Make().String()
			idsByTenant[tenantID] = append(idsByTenant[tenantID], id)
			tasks = append(tasks, deadlockStressAllocationTask{kind: "approval", tenantID: tenantID, contractIDs: []string{id}})
		}
		for i := 0; i < importBatchesPerRound; i++ {
			tenantID := tenants[(round+i+approvalsPerRound)%len(tenants)]
			ids := make([]string, 0, importBatchSize)
			for j := 0; j < importBatchSize; j++ {
				id := ulid.Make().String()
				ids = append(ids, id)
				idsByTenant[tenantID] = append(idsByTenant[tenantID], id)
			}
			sort.Strings(ids)
			tasks = append(tasks, deadlockStressAllocationTask{kind: "import", tenantID: tenantID, contractIDs: ids})
		}
		rounds = append(rounds, tasks)
	}
	seedDeadlockStressContracts(t, r, tenants, idsByTenant)

	ctx, cancel := context.WithTimeout(context.Background(), deadlockStressOverallTimeout)
	defer cancel()
	started := time.Now()
	totalTasks := loops * (approvalsPerRound + importBatchesPerRound)
	results := make(chan deadlockStressAllocationResult, totalTasks)

	for _, tasks := range rounds {
		var roundWG sync.WaitGroup
		start := make(chan struct{})
		for _, task := range tasks {
			roundWG.Add(1)
			go func(task deadlockStressAllocationTask) {
				defer roundWG.Done()
				<-start
				var numbers []string
				var runErr error
				if task.kind == "approval" {
					numbers, runErr = runDeadlockStressApprovalTask(ctx, r, task)
				} else {
					numbers, runErr = runDeadlockStressImportTask(ctx, r, task)
				}
				results <- deadlockStressAllocationResult{task: task, numbers: numbers, err: runErr}
			}(task)
		}
		close(start)
		roundWG.Wait()
	}
	close(results)

	var failures, deadlocks, lockWaitTimeouts []string
	numbers := make(map[string]string)
	for result := range results {
		if result.err != nil {
			failures = append(failures, fmt.Sprintf("%s/%v: %v", result.task.kind, result.task.contractIDs, result.err))
			if deadlockStressIsDeadlock(result.err) {
				deadlocks = append(deadlocks, result.err.Error())
			}
			if deadlockStressIsLockWaitTimeout(result.err) {
				lockWaitTimeouts = append(lockWaitTimeouts, result.err.Error())
			}
			continue
		}
		for _, number := range result.numbers {
			if previous, ok := numbers[number]; ok {
				failures = append(failures, fmt.Sprintf("duplicate number %s assigned to %s and %s", number, previous, result.task.contractIDs))
				continue
			}
			numbers[number] = result.task.contractIDs[0]
		}
	}
	if len(failures) > 0 {
		t.Fatalf("real allocation stress failed: %d failures, 1213=%d, 1205=%d; first=%v",
			len(failures), len(deadlocks), len(lockWaitTimeouts), failures[0])
	}
	if len(deadlocks) != 0 || len(lockWaitTimeouts) != 0 {
		t.Fatalf("real allocation stress saw deadlocks=%v lock-wait-timeouts=%v", deadlocks, lockWaitTimeouts)
	}

	numberList := make([]string, 0, len(numbers))
	for number := range numbers {
		numberList = append(numberList, number)
		if !strings.HasPrefix(number, rule.Prefix) || len(number) != len(rule.Prefix)+2+rule.SeqWidth {
			t.Fatalf("unexpected contract number format: %s", number)
		}
	}
	sort.Strings(numberList)

	var registryRows []contractNumberRegistryRecord
	if err := r.db.Where("contract_number IN ?", numberList).Find(&registryRows).Error; err != nil {
		t.Fatalf("read deadlock-stress registry: %v", err)
	}
	if len(registryRows) != len(numberList) {
		t.Fatalf("registry rows=%d want=%d (每个编号必须恰好一行)", len(registryRows), len(numberList))
	}
	registered := make(map[string]bool, len(registryRows))
	for _, row := range registryRows {
		if registered[row.ContractNumber] {
			t.Fatalf("registry contains duplicate row for %s", row.ContractNumber)
		}
		registered[row.ContractNumber] = true
	}

	var water uint64
	waterErr := r.db.Raw(`SELECT current_value FROM con_contract_number_sequence WHERE seq_year = ? AND rule_key = ?`,
		year, ruleKey).Row().Scan(&water)
	if errors.Is(waterErr, sql.ErrNoRows) {
		water = 0
		waterErr = nil
	}
	if waterErr != nil {
		t.Fatalf("read deadlock-stress sequence water: %v", waterErr)
	}
	if water != uint64(len(numberList)) {
		t.Fatalf("sequence water=%d want successful allocations=%d（回滚的取号不应占水位）", water, len(numberList))
	}

	t.Logf("real allocation stress stats: loops=%d tasks=%d allocations=%d unique=%d registry=%d water=%d deadlocks=%d lockWaitTimeouts=%d elapsed=%s",
		loops, totalTasks, len(numberList), len(numbers), len(registryRows), water, len(deadlocks), len(lockWaitTimeouts), time.Since(started))
}
