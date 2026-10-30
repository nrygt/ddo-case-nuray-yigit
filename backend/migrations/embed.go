// Package migrations SQL şema dosyalarını binary içine gömer.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
