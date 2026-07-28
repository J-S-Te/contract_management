package migration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
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

type item struct {
	version  uint64
	name     string
	sql      string
	checksum [sha256.Size]byte
}

// Run applies every pending embedded migration in numeric order and rejects changed history.
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
