// Package ui serves the single-page web UI for the Agent Orchestrator.
// The entire UI is a single embedded HTML file; no external CDN or build step needed.
package ui

import (
	"embed"
	"net/http"
)

//go:embed static
var staticFiles embed.FS

// NewHandler returns an http.Handler that serves the embedded UI.
func NewHandler() http.Handler {
	return http.FileServer(http.FS(staticFiles))
}
