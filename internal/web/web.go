// Package web embeds the dashboard templates (with on-disk override).
package web

import (
	"embed"
)

//go:embed *.html
var FS embed.FS
