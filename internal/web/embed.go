// Package web embeds the built single-page UI so the server ships as one
// self-contained binary with no external asset files.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// FS returns the embedded UI file system rooted at the dist directory.
func FS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("web: embedded dist directory missing: " + err.Error())
	}
	return sub
}
