package migration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	driver "github.com/go-sql-driver/mysql"
)

const metadataTable = "con_schema_migration"

var migrationName = regexp.MustCompile(`^([0-9]{6})_([a-z0-9_]+)\.sql$`)

// preflights 按迁移版本号注册执行前预检：命中的迁移在写入任何语句前先跑预检，
// 预检失败直接中止，不执行该迁移的 SQL、也不写 con_schema_migration。
// 预检挂在唯一入口 Run 上，因此所有调用方（cmd/migrate、测试、未来的启动期迁移）
// 都自动带上，不需要运维额外执行手工 SQL。
//
// 键刻意用版本号而不是迁移名：迁移名写错一个字母时，按名字查表只会静默跳过预检，
// 没有任何测试会红；按版本号查表则会在 Run 里被 verifyPreflights 拦下并报错
// （版本号不存在就不再“跳过”，而是直接失败）。
// 并发写 map 是数据竞争，因此只在初始化阶段构建，运行期只读。
var preflights = map[uint64]func(context.Context, *sql.DB) error{
	38: PreflightContractNumbers,
}

type item struct {
	version  uint64
	name     string
	sql      string
	checksum [sha256.Size]byte
}

// Run 按数字顺序应用全部待执行嵌入迁移，并拒绝历史变更。
//
// 对注册了预检的迁移（见 preflights），在执行该迁移的任何语句之前先跑预检：
// 预检失败返回包装后的中文错误并立即中止，既不执行该迁移的 SQL，也不写入
// con_schema_migration。已应用版本只做 checksum 校验，不重复预检。
func Run(ctx context.Context, dsn string, files embed.FS) error {
	config, err := driver.ParseDSN(dsn)
	if err != nil {
		return fmt.Errorf("parse MySQL DSN: %w", err)
	}
	config.MultiStatements = true
	database, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		return fmt.Errorf("open MySQL: %w", err)
	}
	defer database.Close()
	database.SetConnMaxLifetime(5 * time.Minute)
	database.SetMaxOpenConns(2)
	database.SetMaxIdleConns(2)
	if err := database.PingContext(ctx); err != nil {
		return fmt.Errorf("ping MySQL: %w", err)
	}
	if _, err := database.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+metadataTable+` (
		version BIGINT UNSIGNED NOT NULL,
		name VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
		checksum BINARY(32) NOT NULL,
		applied_at DATETIME(3) NOT NULL,
		PRIMARY KEY (version)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`); err != nil {
		return fmt.Errorf("create migration metadata: %w", err)
	}
	items, err := load(files)
	if err != nil {
		return err
	}
	// 预检注册表与嵌入迁移必须严格对应：注册了不存在的版本就是配置错误，
	// 直接失败而不是悄悄跳过预检。
	if err := verifyPreflights(preflights, items); err != nil {
		return err
	}
	applied, err := appliedChecksums(ctx, database)
	if err != nil {
		return err
	}
	for _, migration := range items {
		if checksum, exists := applied[migration.version]; exists {
			if checksum != migration.checksum {
				return fmt.Errorf("migration %06d_%s checksum differs from applied version", migration.version, migration.name)
			}
			continue
		}
		// 预检必须早于本迁移的任何语句：命中即先检查存量数据，失败时本迁移
		// 不产生任何写入（连 con_schema_migration 记录也不会落库）。
		if preflight, exists := preflights[migration.version]; exists {
			if err := preflight(ctx, database); err != nil {
				return fmt.Errorf("preflight for migration %06d_%s: %w", migration.version, migration.name, err)
			}
		}
		if _, err := database.ExecContext(ctx, migration.sql); err != nil {
			return fmt.Errorf("apply migration %06d_%s: %w", migration.version, migration.name, err)
		}
		if _, err := database.ExecContext(ctx,
			`INSERT INTO `+metadataTable+` (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
			migration.version, migration.name, migration.checksum[:], time.Now().UTC(),
		); err != nil {
			return fmt.Errorf("record migration %06d_%s: %w", migration.version, migration.name, err)
		}
	}
	return nil
}

func load(files embed.FS) ([]item, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	items := make([]item, 0, len(entries))
	versions := make(map[uint64]string)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		match := migrationName.FindStringSubmatch(entry.Name())
		if match == nil {
			return nil, fmt.Errorf("invalid migration filename %q", entry.Name())
		}
		version, err := strconv.ParseUint(match[1], 10, 64)
		if err != nil || version == 0 {
			return nil, fmt.Errorf("invalid migration version in %q", entry.Name())
		}
		if previous, exists := versions[version]; exists {
			return nil, fmt.Errorf("migration version %d is duplicated by %q and %q", version, previous, entry.Name())
		}
		body, err := files.ReadFile(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		if strings.TrimSpace(string(body)) == "" {
			return nil, fmt.Errorf("migration %q is empty", entry.Name())
		}
		versions[version] = entry.Name()
		items = append(items, item{
			version: version, name: match[2], sql: string(body), checksum: sha256.Sum256(body),
		})
	}
	sort.Slice(items, func(left, right int) bool { return items[left].version < items[right].version })
	for index, migration := range items {
		expected := uint64(index + 1)
		if migration.version != expected {
			return nil, fmt.Errorf("migration versions must be contiguous: got %d, want %d", migration.version, expected)
		}
	}
	return items, nil
}

// verifyPreflights 校验预检注册表与已加载迁移严格对应。
//
// 注册表的键是迁移版本号：只要注册了嵌入迁移里不存在的版本（改名、拼错、删文件后忘记
// 同步），这里就返回中文错误让 Run 直接失败，而不是让预检静默失效。把注册表作为参数
// 传入是为了让测试能注入“带非法版本”的注册表验证这条路径。
// 参数：registered 为版本号到预检函数的映射，items 为 load 出的迁移列表。
// 返回值：全部对应时返回 nil，否则返回带 %w 原因的中文错误。
func verifyPreflights(registered map[uint64]func(context.Context, *sql.DB) error, items []item) error {
	known := make(map[uint64]struct{}, len(items))
	for _, migration := range items {
		known[migration.version] = struct{}{}
	}
	versions := make([]uint64, 0, len(registered))
	for version := range registered {
		versions = append(versions, version)
	}
	sort.Slice(versions, func(left, right int) bool { return versions[left] < versions[right] })
	for _, version := range versions {
		if _, exists := known[version]; !exists {
			return fmt.Errorf("preflight registered for unknown migration version %d: %w", version, errUnknownPreflightVersion)
		}
	}
	return nil
}

// errUnknownPreflightVersion 标记预检注册表里的版本号在嵌入迁移中不存在。
var errUnknownPreflightVersion = errors.New("preflight version does not match any embedded migration")

func appliedChecksums(ctx context.Context, database *sql.DB) (map[uint64][sha256.Size]byte, error) {
	rows, err := database.QueryContext(ctx, `SELECT version, checksum FROM `+metadataTable)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()
	result := make(map[uint64][sha256.Size]byte)
	for rows.Next() {
		var version uint64
		var raw []byte
		if err := rows.Scan(&version, &raw); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		if len(raw) != sha256.Size {
			return nil, fmt.Errorf("migration %d has an invalid checksum", version)
		}
		var checksum [sha256.Size]byte
		copy(checksum[:], raw)
		result[version] = checksum
	}
	return result, rows.Err()
}
