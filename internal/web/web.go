// Package web also embeds service logos (assets) for the dashboard.
package web

import (
	"embed"
	"io/fs"
)

//go:embed *.html assets/*
var FS embed.FS

// Asset opens an embedded asset file by name (e.g. "assets/openwebui.png").
func Asset(name string) (fs.File, error) {
	return FS.Open(name)
}

// HasAsset reports whether an embedded asset exists.
func HasAsset(name string) bool {
	f, err := FS.Open(name)
	if err != nil {
		return false
	}
	f.Close()
	return true
}
