package payprovider

// Spec tests: every HTTP answer in this package's tests is checked against
// api/payprovider.yaml, the same way internal/httpapi checks the API against
// api/openapi.yaml (see its openapi_test.go, which this follows).

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"

	"github.com/atalaymudanyali/quickbytes/api"
)

var spec = sync.OnceValues(func() (*openapi3.T, error) {
	doc, err := openapi3.NewLoader().LoadFromData(api.PayProviderSpec)
	if err != nil {
		return nil, err
	}
	if err := doc.Validate(context.Background()); err != nil {
		return nil, err
	}
	return doc, nil
})

var specRouter = sync.OnceValues(func() (routers.Router, error) {
	doc, err := spec()
	if err != nil {
		return nil, err
	}
	return gorillamux.NewRouter(doc)
})

// checked records every spec operation whose answer was checked in this run.
var checked sync.Map

// TestMain fails the run if some endpoint in the spec never had an answer
// checked against it, so the checker can't quietly skip everything.
func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 && os.Getenv("TEST_PAYPROVIDER_DATABASE_URL") != "" && !isFilteredRun() {
		doc, err := spec()
		if err != nil {
			fmt.Println("load spec:", err)
			os.Exit(1)
		}
		var missing []string
		for path, item := range doc.Paths.Map() {
			for method := range item.Operations() {
				if _, ok := checked.Load(method + " " + path); !ok {
					missing = append(missing, method+" "+path)
				}
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			fmt.Printf("FAIL: no test checked an answer of these endpoints against api/payprovider.yaml:\n  %s\n",
				strings.Join(missing, "\n  "))
			code = 1
		}
	}
	os.Exit(code)
}

func isFilteredRun() bool {
	f := flag.Lookup("test.run")
	return f != nil && f.Value.String() != ""
}

// TestEveryRouteIsInTheSpec compares the served endpoints with the spec, both
// ways.
func TestEveryRouteIsInTheSpec(t *testing.T) {
	doc, err := spec()
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	var inSpec, inCode []string
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			inSpec = append(inSpec, method+" "+path)
		}
	}
	for _, rt := range (&Server{}).routes() {
		inCode = append(inCode, rt.pattern)
	}
	for _, r := range inCode {
		if !slices.Contains(inSpec, r) {
			t.Errorf("%s is served but missing from api/payprovider.yaml", r)
		}
	}
	for _, r := range inSpec {
		if !slices.Contains(inCode, r) {
			t.Errorf("%s is in api/payprovider.yaml but not served", r)
		}
	}
}

// checkAgainstSpec returns an error if an answer doesn't match the spec.
// Paths the spec doesn't know are skipped (TestEveryRouteIsInTheSpec covers
// missing endpoints).
func checkAgainstSpec(req *http.Request, reqBody []byte, status int, header http.Header, body []byte) error {
	router, err := specRouter()
	if err != nil {
		return err
	}
	match := req.Clone(req.Context())
	match.URL.Scheme, match.URL.Host, match.Host = "http", "localhost:8090", "localhost:8090"

	route, pathParams, err := router.FindRoute(match)
	if err != nil {
		return nil
	}
	checked.Store(route.Method+" "+route.Path, true)

	reqInput := &openapi3filter.RequestValidationInput{
		Request: match, PathParams: pathParams, Route: route,
		Options: &openapi3filter.Options{
			MultiError:         true,
			AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
		},
	}
	// A request the provider accepted must be one the spec accepts too.
	if status >= 200 && status < 300 {
		match.Body = io.NopCloser(bytes.NewReader(reqBody))
		if err := openapi3filter.ValidateRequest(req.Context(), reqInput); err != nil {
			return fmt.Errorf("the provider accepted this request, but the spec says it's invalid: %w", err)
		}
	}
	return openapi3filter.ValidateResponse(req.Context(), &openapi3filter.ResponseValidationInput{
		RequestValidationInput: reqInput,
		Status:                 status,
		Header:                 header,
		Body:                   io.NopCloser(bytes.NewReader(body)),
		Options:                &openapi3filter.Options{IncludeResponseStatus: true, MultiError: true},
	})
}

// validatedBySpec wraps a handler so every answer it gives in a test is checked
// against the spec.
func validatedBySpec(t *testing.T, h http.Handler) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody []byte
		if r.Body != nil {
			reqBody, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(reqBody))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if err := checkAgainstSpec(r, reqBody, rec.Code, rec.Header(), rec.Body.Bytes()); err != nil {
			t.Errorf("%s %s → %d doesn't match api/payprovider.yaml:\n%v\nbody: %s",
				r.Method, r.URL.Path, rec.Code, err, strings.TrimSpace(rec.Body.String()))
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
}

// The checker must fail answers that break the spec.
func TestSpecCheckCatchesMismatches(t *testing.T) {
	cases := map[string]struct {
		method, path string
		status       int
		body         string
	}{
		"wrong value":         {"GET", "/healthz", 200, `{"status": "maybe"}`},
		"undocumented status": {"GET", "/healthz", 418, `{"status": "ok"}`},
		"unknown status name": {"GET", "/v1/payments/01a17880-7c89-7afd-ac33-f3935dc16620", 200,
			`{"id": "01a17880-7c89-7afd-ac33-f3935dc16620", "amount_kurus": 1, "currency": "TRY", "status": "paid",
			  "card_last4": "4242", "created_at": "2026-10-26T10:00:00Z", "updated_at": "2026-10-26T10:00:00Z"}`},
		"402 without the payment": {"POST", "/v1/payments", 402,
			`{"error": {"code": "card_declined", "message": "declined"}}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, nil)
			header := http.Header{"Content-Type": {"application/json"}}
			if err := checkAgainstSpec(req, nil, c.status, header, []byte(c.body)); err == nil {
				t.Fatal("an answer that breaks the spec was accepted")
			}
		})
	}
}
