package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/atalaymudanyali/quickbytes/api"
)

func TestDocs(t *testing.T) {
	h := newTestHandler(t, fakePinger{})
	get := func(path string) *httptest.ResponseRecorder {
		return do(t, h, httptest.NewRequest(http.MethodGet, path, nil))
	}

	t.Run("spec", func(t *testing.T) {
		rec := get("/openapi.yaml")
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), api.Spec) {
			t.Fatalf("status = %d; want 200 with the embedded spec", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/yaml" {
			t.Errorf("Content-Type = %q, want application/yaml", ct)
		}
	})

	t.Run("/docs redirects to /docs/", func(t *testing.T) {
		rec := get("/docs")
		if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/docs/" {
			t.Fatalf("status = %d, Location = %q; want 301 to /docs/", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("page loads the embedded Swagger UI and our spec", func(t *testing.T) {
		rec := get("/docs/")
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.Contains(body, "swagger-ui-bundle.js") || !strings.Contains(body, `url: "/openapi.yaml"`) {
			t.Fatalf("status = %d; body: %.300s", rec.Code, body)
		}
	})

	for _, file := range []string{"swagger-ui-bundle.js", "swagger-ui.css", "favicon-32x32.png"} {
		t.Run(file, func(t *testing.T) {
			if rec := get("/docs/" + file); rec.Code != http.StatusOK || rec.Body.Len() == 0 {
				t.Fatalf("status = %d, %d bytes", rec.Code, rec.Body.Len())
			}
		})
	}

	t.Run("unknown file", func(t *testing.T) {
		if rec := get("/docs/nope.js"); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}
