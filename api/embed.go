// Package api holds the OpenAPI description of the QuickBytes API, and the Swagger
// UI page that displays it. Both are embedded into the binary, so the server can
// serve them at /openapi.yaml and /docs, and tests can check the code against the
// spec.
package api

import "embed"

//go:embed openapi.yaml
var Spec []byte

// SwaggerUI is a pinned Swagger UI release (see scripts/update-swagger-ui.sh) plus
// our own index.html.
//
//go:embed swaggerui
var SwaggerUI embed.FS
