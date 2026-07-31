// Package web embeds static control-plane assets built before the Go binary.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static/*
var files embed.FS

func Static() http.Handler {
	content, _ := fs.Sub(files, "static")
	return http.FileServer(http.FS(content))
}
