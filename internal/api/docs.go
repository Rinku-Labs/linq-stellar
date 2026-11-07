package api

import (
	"bytes"
	"net/http"
	"sync"

	"github.com/Rinku-Labs/linq-stellar/docs"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

const docsPageTop = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Linq Settlement API</title>
<style>
body { max-width: 46rem; margin: 2rem auto; padding: 0 1rem; font: 16px/1.6 system-ui, sans-serif; color: #1a1a1a; }
nav { margin-bottom: 1.5rem; }
pre { background: #f4f4f5; padding: 1rem; overflow-x: auto; border-radius: 6px; }
code { font: 0.9em ui-monospace, monospace; }
table { border-collapse: collapse; width: 100%; }
th, td { border: 1px solid #d4d4d8; padding: 0.4rem 0.6rem; text-align: left; vertical-align: top; }
</style>
</head>
<body>
<nav><a href="/docs/openapi.yaml">OpenAPI spec</a> · <a href="/docs/README.md">Markdown</a></nav>
`

const docsPageBottom = "</body>\n</html>\n"

// docsPage renders the guide once. Tests build many routers and the content
// cannot change while the process runs.
var docsPage = sync.OnceValues(func() ([]byte, error) {
	var page bytes.Buffer
	page.WriteString(docsPageTop)
	markdown := goldmark.New(goldmark.WithExtensions(extension.Table))
	if err := markdown.Convert(docs.Guide, &page); err != nil {
		return nil, err
	}
	page.WriteString(docsPageBottom)
	return page.Bytes(), nil
})

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	page, err := docsPage()
	if err != nil {
		s.Log.Error("could not render the api docs", "error", err)
		writeError(w, http.StatusInternalServerError, "could not load the docs")
		return
	}
	// The page has no scripts and loads nothing from anywhere else.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

func serveDocFile(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}
}
