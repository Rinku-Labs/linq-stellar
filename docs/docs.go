// Package docs embeds the public API documentation, so the binary serves the
// same files the repository holds. It lives here because go:embed cannot reach
// out of a package's own directory.
package docs

import _ "embed"

// Guide is the integrator guide, in Markdown.
//
//go:embed api/README.md
var Guide []byte

// OpenAPI is the /v1 specification.
//
//go:embed api/openapi.yaml
var OpenAPI []byte
