// Package migrations 嵌入合同管理系统不可变的 SQL 迁移历史。
package migrations

import "embed"

// Files contains all numbered SQL migrations.
//
//go:embed *.sql
var Files embed.FS
