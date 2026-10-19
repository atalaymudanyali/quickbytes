package httpapi

import (
	"io/fs"
	"net/http"

	"github.com/atalaymudanyali/quickbytes/api"
)

// docsHandler serves the API's own documentation:
//
//	GET /openapi.yaml  the spec itself, for tools and code generators
//	GET /docs/         Swagger UI: browse the API and try requests in the browser
//
// These aren't part of the API, so they're not in the route table or the spec.
func docsHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(api.Spec)
	})

	// The page loads its files with relative links ("swagger-ui.css"), which only
	// work under /docs/ with the trailing slash, so /docs redirects there.
	mux.Handle("GET /docs", http.RedirectHandler("/docs/", http.StatusMovedPermanently))

	ui, err := fs.Sub(api.SwaggerUI, "swaggerui")
	if err != nil {
		panic(err) // the directory is embedded at build time, so this can't happen
	}
	mux.Handle("GET /docs/", http.StripPrefix("/docs/", http.FileServerFS(ui)))
	return mux
}
