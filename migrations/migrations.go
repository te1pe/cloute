// Package migrations embeds the SQL migrations applied by goose at startup.
package migrations

import "embed"

// FS holds the migration files.
//
//go:embed *.sql
var FS embed.FS
