package web

import (
	"embed"
	"io/fs"
	"mime"
)

//go:generate go run gen_icons.go

//go:embed static
var embedded embed.FS

// staticRoot is static/ as the site root.
var staticRoot, _ = fs.Sub(embedded, "static")

// staticFS is what gets served: static/ without input.css (the Tailwind
// source). Hiding it at the fs level covers every spelling of the path,
// since http.FileServer opens the cleaned name.
var staticFS fs.FS = servedFS{staticRoot}

// Not in Go's built-in table; system tables vary.
func init() { mime.AddExtensionType(".webmanifest", "application/manifest+json") }

type servedFS struct{ fs.FS }

func (f servedFS) Open(name string) (fs.File, error) {
	if name == "input.css" {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return f.FS.Open(name)
}
