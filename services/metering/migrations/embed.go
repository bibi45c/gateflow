// Package migrations holds the SQL migrations of the accounting schema, which the
// metering service applies at startup with pkg/pgmigrate.
package migrations

import "embed"

// FS holds the migration files, NNNN_name.sql, at its root.
//
//go:embed *.sql
var FS embed.FS
