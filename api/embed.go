// Package api holds the OpenAPI description of the QuickBytes API. It's embedded
// into the binary, so the server can serve it at /openapi.yaml and tests can check
// the code against it.
package api

import _ "embed"

//go:embed openapi.yaml
var Spec []byte
