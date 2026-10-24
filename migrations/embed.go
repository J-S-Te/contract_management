// Package migrations 嵌入合同管理系统不可变的 SQL 迁移历史。
package migrations

import "embed"

// Files 包含所有带编号的 SQL 迁移。
//
//go:embed *.sql
var Files embed.FS
