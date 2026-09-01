package migrations

import "embed"

// FS contains the versioned SQLite schema.
//
//go:embed *.sql
var FS embed.FS
