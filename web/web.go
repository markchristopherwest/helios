// Package web embeds the SPA so the binary ships as a single artifact.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var staticFS embed.FS

// UI returns the static tree rooted at its contents (index.html at "/").
func UI() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // impossible: path is compiled in
	}
	return sub
}
