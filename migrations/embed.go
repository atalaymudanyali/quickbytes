// Package migrations holds the SQL files that build the database schema, step by
// step. They're embedded into the binary so a deployed service always carries the
// migrations that match its code.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
