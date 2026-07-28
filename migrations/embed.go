// Package migrations embeds the immutable contract-management SQL migration history.
package migrations

import "embed"

// Files contains all numbered SQL migrations.
//
//go:embed *.sql
var Files embed.FS
