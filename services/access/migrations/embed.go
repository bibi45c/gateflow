// Package migrations holds the access service's SQL migrations, which
// internal/store applies to the access schema with pkg/pgmigrate.
package migrations

import "embed"

// FS holds the NNNN_name.sql files at its root.
//
//go:embed *.sql
var FS embed.FS
