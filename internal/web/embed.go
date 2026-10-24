package web

import (
	"embed"
	"io/fs"
	"mime"
)

//go:generate go run gen_icons.go

//go:embed static
var embedded embed.FS

// staticFS is what gets served: static/ as the site root.
var staticFS, _ = fs.Sub(embedded, "static")

// Not in Go's built-in table; system tables vary.
func init() { mime.AddExtensionType(".webmanifest", "application/manifest+json") }
