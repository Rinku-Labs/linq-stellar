package api

import (
	"embed"
	"net/http"
)

// The test console (/console) is a page for trying the API on the test
// network without a terminal: sign in with a test key, create an order, see
// the exact request a developer would send, watch it pay out, rotate the key
// and hit the rate limit. It is a client of /v1 and nothing else: every action
// is a /v1 call from the browser with the key the person typed, so it can do
// nothing that key couldn't do with curl.
//
// Served on the test network only, beside PUT /v1/webhook. The page keeps a
// key in the browser tab, and a live key belongs in a server, not a browser.

//go:embed console/index.html console/app.js console/app.css
var consoleFiles embed.FS

// consoleCSP lets the page run its own script and call this origin, and
// nothing else: no inline script, no third-party code that could read the key.
// Unlike the reconciliation page, this one needs JavaScript; the CSP is
// changed on purpose, for this page only.
const consoleCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func (s *Server) consoleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /console", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/console/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /console/{$}", serveConsoleFile("console/index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /console/app.js", serveConsoleFile("console/app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /console/app.css", serveConsoleFile("console/app.css", "text/css; charset=utf-8"))
}

func serveConsoleFile(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := consoleFiles.ReadFile(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "console unavailable")
			return
		}
		h := w.Header()
		h.Set("Content-Type", contentType)
		h.Set("Content-Security-Policy", consoleCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-cache")
		_, _ = w.Write(body)
	}
}
