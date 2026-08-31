// Package migrations embeds the SQL migration files so the service can apply
// them at startup without needing them present on the filesystem at runtime.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
