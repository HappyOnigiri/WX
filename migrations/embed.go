package migrations

import "embed"

// FS は versioned SQLite schema を格納する。
//
//go:embed *.sql
var FS embed.FS
