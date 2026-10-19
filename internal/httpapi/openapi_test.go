package httpapi

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

// The spec is loaded once for all tests in this package.
var spec = sync.OnceValues(func() (*openapi3.T, error) {
	doc, err := openapi3.NewLoader().LoadFromData(api.Spec)
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

// checked records every spec operation whose response was checked during this
// test run, so TestMain can make sure none was left out.
var checked sync.Map

// TestMain runs the package's tests, then fails the run if some endpoint in the
// spec never had a response checked against it. Without this, the checker could
// quietly skip everything and every test would still pass.
func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 && os.Getenv("TEST_DATABASE_URL") != "" && !isFilteredRun() {
		doc, err := spec()
		if err != nil {
			fmt.Println("load spec:", err)
			os.Exit(1)
		}
		var missing []string
		total := 0
		for path, item := range doc.Paths.Map() {
			for method := range item.Operations() {
				total++
				if _, ok := checked.Load(method + " " + path); !ok {
					missing = append(missing, method+" "+path)
				}
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			fmt.Printf("FAIL: no test checked a response of these %d endpoints against the spec:\n  %s\n",
				len(missing), strings.Join(missing, "\n  "))
			code = 1
		} else if testing.Verbose() {
			fmt.Printf("spec coverage: responses of all %d endpoints were checked against api/openapi.yaml\n", total)
		}
	}
	os.Exit(code)
}

// isFilteredRun reports whether only some tests were selected with -run, in which
// case not every endpoint is expected to be exercised.
func isFilteredRun() bool {
	f := flag.Lookup("test.run")
	return f != nil && f.Value.String() != ""
}

// TestEveryRouteIsInTheSpec compares the endpoints the server registers with the
// operations in api/openapi.yaml, in both directions.
func TestEveryRouteIsInTheSpec(t *testing.T) {
	doc, err := spec()
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}

	var inSpec []string
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			inSpec = append(inSpec, method+" "+path)
		}
	}
	var inCode []string
	for _, rt := range NewServer(Deps{}).routes() {
		inCode = append(inCode, rt.pattern)
	}
	sort.Strings(inSpec)
	sort.Strings(inCode)

	for _, r := range inCode {
		if !slices.Contains(inSpec, r) {
			t.Errorf("%s is served but missing from api/openapi.yaml", r)
		}
	}
	for _, r := range inSpec {
		if !slices.Contains(inCode, r) {
			t.Errorf("%s is in api/openapi.yaml but not served", r)
		}
	}
	if !t.Failed() {
		t.Logf("%d endpoints, identical in code and spec", len(inCode))
	}
}

// checkAgainstSpec returns an error if a response doesn't match what the spec says
// that endpoint returns: an undocumented status code, a missing or extra field, a
// wrong type. Responses to paths the spec doesn't know (like the router's own 404
// for /nope) are skipped; TestEveryRouteIsInTheSpec covers missing endpoints.
func checkAgainstSpec(req *http.Request, reqBody []byte, status int, header http.Header, body []byte) error {
	router, err := specRouter()
	if err != nil {
		return err
	}
	// The spec's server is http://localhost:8080, and test requests go to
	// example.com, so point a copy of the request at the spec's server.
	match := req.Clone(req.Context())
	match.URL.Scheme, match.URL.Host, match.Host = "http", "localhost:8080", "localhost:8080"

	route, pathParams, err := router.FindRoute(match)
	if err != nil {
		return nil
	}
	checked.Store(route.Method+" "+route.Path, true)

	reqInput := &openapi3filter.RequestValidationInput{
		Request: match, PathParams: pathParams, Route: route,
		Options: &openapi3filter.Options{
			MultiError: true,
			// Login is the server's job, tested elsewhere; here we only compare shapes.
			AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
		},
	}
	// If the server accepted a request (2xx), the spec must accept it too.
	// Otherwise the docs, or a client generated from them, would refuse something
	// the server allows. Rejected requests are skipped: tests send invalid ones on
	// purpose.
	if status >= 200 && status < 300 {
		match.Body = io.NopCloser(bytes.NewReader(reqBody))
		if err := openapi3filter.ValidateRequest(req.Context(), reqInput); err != nil {
			return fmt.Errorf("the server accepted this request, but the spec says it's invalid: %w", err)
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

// validatedBySpec wraps a handler so every response it gives in a test is checked
// against the spec. All the existing HTTP tests go through it, so they double as
// spec tests without being changed.
func validatedBySpec(t *testing.T, h http.Handler) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The handler reads the body, so keep a copy to check afterwards.
		var reqBody []byte
		if r.Body != nil {
			reqBody, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(reqBody))
		}

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)

		if err := checkAgainstSpec(r, reqBody, rec.Code, rec.Header(), rec.Body.Bytes()); err != nil {
			t.Errorf("%s %s → %d doesn't match api/openapi.yaml:\n%v\nbody: %s",
				r.Method, r.URL.Path, rec.Code, err, strings.TrimSpace(rec.Body.String()))
		}

		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
}

// TestSpecCheckCatchesMismatches proves the checker isn't silently passing
// everything: a response that breaks the spec in three different ways must fail.
func TestSpecCheckCatchesMismatches(t *testing.T) {
	cases := map[string]struct {
		path   string
		status int
		body   string
	}{
		"wrong value":            {"/healthz", 200, `{"status": "maybe"}`},
		"undocumented status":    {"/healthz", 418, `{"status": "ok"}`},
		"missing required field": {"/readyz", 503, `{"error": {"message": "no code"}}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, c.path, nil)
			header := http.Header{"Content-Type": {"application/json"}}
			if err := checkAgainstSpec(req, nil, c.status, header, []byte(c.body)); err == nil {
				t.Fatal("a response that breaks the spec was accepted")
			}
		})
	}
}
