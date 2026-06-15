package api

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.json
var openapiSpec []byte

// handleOpenAPI serves the embedded OpenAPI 3.1 document (the public API
// contract). Unauthenticated, like the health/metrics endpoints.
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(openapiSpec)
}

// docsHTML renders the API reference from the embedded spec using Redoc (loaded
// from a CDN, consistent with the UI's web-font loading).
const docsHTML = `<!doctype html>
<html>
<head>
  <meta charset="utf-8"/>
  <title>Omni-logging API</title>
  <meta name="viewport" content="width=device-width, initial-scale=1"/>
  <style>body { margin: 0; }</style>
</head>
<body>
  <redoc spec-url="/openapi.json"></redoc>
  <script src="https://cdn.redoc.ly/redoc/latest/bundles/redoc.standalone.js"></script>
</body>
</html>`

// handleDocs serves a minimal API reference page backed by /openapi.json.
func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(docsHTML))
}
