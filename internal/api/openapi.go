package api

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.json
var openapiSpec []byte

// The API reference viewer is served from these embedded assets rather than a
// CDN bundle, so the binary stays self-contained (no third-party script, works
// air-gapped) and /docs runs under the same strict CSP as the rest of the UI.
var (
	//go:embed docs.html
	docsHTML []byte
	//go:embed docs.css
	docsCSS []byte
	//go:embed docs.js
	docsJS []byte
)

// handleOpenAPI serves the embedded OpenAPI 3.1 document (the public API
// contract). Unauthenticated, like the health endpoint.
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(openapiSpec)
}

// handleDocs serves the API reference page backed by /openapi.json.
func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(docsHTML)
}

func (s *Server) handleDocsCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = w.Write(docsCSS)
}

func (s *Server) handleDocsJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(docsJS)
}
