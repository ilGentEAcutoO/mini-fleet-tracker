// Package migrations embeds the project's *.sql migration files so they
// travel inside the compiled binary. Keeping the embed declaration in a tiny
// sibling package (rather than in the migrator) cleanly separates "where the
// SQL lives" from "what code applies it", and lets tests inject their own
// fs.FS without touching production code.
package migrations

import "embed"

// FS holds every migration file in this directory. Both *.up.sql and *.down.sql
// are embedded; the migrator filters by suffix at runtime.
//
//go:embed *.sql
var FS embed.FS
