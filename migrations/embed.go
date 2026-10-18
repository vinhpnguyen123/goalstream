// Package migrations embeds the SQL migrations so every binary carries its own schema and
// applies it at startup (decision D17) — no separate migrate step on any deploy platform.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
