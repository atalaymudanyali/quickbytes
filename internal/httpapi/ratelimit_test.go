package httpapi

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/atalaymudanyali/quickbytes/internal/ratelimit"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
)

// newLimitedHandler is a test server with rate limits on, using small numbers so
// the tests stay short. Other tests run without limits: they all come from the
// same fake IP and would block each other.
func newLimitedHandler(t *testing.T, change func(*limits)) http.Handler {
	t.Helper()
	rdb := testutil.Redis(t)
	srv, h := newTestServerWithRedis(t, rdb)
	srv.limiter = ratelimit.New(rdb, discardLogger())
	srv.limits = defaultLimits
	change(&srv.limits)
	return h
}

// uniqueIP gives each test its own IP, so tests sharing one Redis don't share
// counters.
func uniqueIP() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("10.%d.%d.%d", b[0], b[1], b[2])
}

func loginFrom(t *testing.T, h http.Handler, ip, email, password string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/auth/login",
		strings.NewReader(`{"email": "`+email+`", "password": "`+password+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":51234"
	return do(t, h, req)
}

// newAccount signs up a user and returns their email. The password is "correct-password".
func newAccount(t *testing.T, h http.Handler) string {
	t.Helper()
	email := testutil.UniqueEmail()
	if rec := postJSON(t, h, "/auth/signup", signupBody(email, "correct-password")); rec.Code != http.StatusCreated {
		t.Fatalf("signup failed: %d %s", rec.Code, rec.Body)
	}
	return email
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int, what string) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("%s: status = %d, want %d; body: %s", what, rec.Code, want, rec.Body)
	}
	if want == http.StatusTooManyRequests {
		if secs, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || secs < 1 {
			t.Fatalf("%s: Retry-After = %q, want a whole number of seconds ≥ 1", what, rec.Header().Get("Retry-After"))
		}
	}
}

func TestLoginLimitPerIP(t *testing.T) {
	h := newLimitedHandler(t, func(l *limits) { l.loginIP.Limit = 3 })
	ip := uniqueIP()

	// One machine trying many different emails.
	for i := 1; i <= 3; i++ {
		wantStatus(t, loginFrom(t, h, ip, testutil.UniqueEmail(), "guess"), http.StatusUnauthorized, fmt.Sprintf("attempt %d", i))
	}
	wantStatus(t, loginFrom(t, h, ip, testutil.UniqueEmail(), "guess"), http.StatusTooManyRequests, "attempt 4")
	wantStatus(t, loginFrom(t, h, uniqueIP(), testutil.UniqueEmail(), "guess"), http.StatusUnauthorized, "another IP")
}

func TestLoginLimitPerEmailAndIP(t *testing.T) {
	h := newLimitedHandler(t, func(l *limits) { l.loginPair.Limit = 2 })
	email, attacker := newAccount(t, h), uniqueIP()

	wantStatus(t, loginFrom(t, h, attacker, email, "guess-1"), http.StatusUnauthorized, "failure 1")
	wantStatus(t, loginFrom(t, h, attacker, email, "guess-2"), http.StatusUnauthorized, "failure 2")
	// Blocked even with the right password: otherwise the attacker could keep
	// guessing and just ignore the 429s until one guess comes back 200.
	wantStatus(t, loginFrom(t, h, attacker, email, "correct-password"), http.StatusTooManyRequests, "right password after the limit")
	// The real owner, on their own phone, isn't affected.
	wantStatus(t, loginFrom(t, h, uniqueIP(), email, "correct-password"), http.StatusOK, "owner from another IP")
}

func TestSuccessfulLoginResetsTheEmailAndIPCounter(t *testing.T) {
	h := newLimitedHandler(t, func(l *limits) { l.loginPair.Limit = 2 })
	email, ip := newAccount(t, h), uniqueIP()

	wantStatus(t, loginFrom(t, h, ip, email, "typo"), http.StatusUnauthorized, "typo 1")
	wantStatus(t, loginFrom(t, h, ip, email, "correct-password"), http.StatusOK, "right password")
	wantStatus(t, loginFrom(t, h, ip, email, "typo"), http.StatusUnauthorized, "typo after login")
	// Without the reset, this would be failure 3 of 2.
	wantStatus(t, loginFrom(t, h, ip, email, "typo"), http.StatusUnauthorized, "second typo after login")
}

func TestLoginLimitPerEmailFromAnyIP(t *testing.T) {
	h := newLimitedHandler(t, func(l *limits) { l.loginEmail.Limit = 2 })
	email := newAccount(t, h)

	// Many machines, one guess each, at one account.
	wantStatus(t, loginFrom(t, h, uniqueIP(), email, "guess-1"), http.StatusUnauthorized, "machine 1")
	wantStatus(t, loginFrom(t, h, uniqueIP(), email, "guess-2"), http.StatusUnauthorized, "machine 2")
	wantStatus(t, loginFrom(t, h, uniqueIP(), email, "guess-3"), http.StatusTooManyRequests, "machine 3")
}

// The counters don't depend on whether an account exists, so being blocked
// mustn't reveal it either.
func TestRateLimitLooksTheSameForUnknownEmails(t *testing.T) {
	h := newLimitedHandler(t, func(l *limits) { l.loginPair.Limit = 1 })
	ip := uniqueIP()

	blocked := map[string]*httptest.ResponseRecorder{}
	for name, email := range map[string]string{"registered": newAccount(t, h), "unknown": testutil.UniqueEmail()} {
		wantStatus(t, loginFrom(t, h, ip, email, "guess"), http.StatusUnauthorized, name+" email, failure 1")
		blocked[name] = loginFrom(t, h, ip, email, "guess")
		wantStatus(t, blocked[name], http.StatusTooManyRequests, name+" email, attempt 2")
	}
	a, b := decodeError(t, blocked["registered"]), decodeError(t, blocked["unknown"])
	if a.Code != b.Code || a.Message != b.Message {
		t.Fatalf("429s differ: %+v vs %+v", a, b)
	}
}

func TestLoginCaseAndSpacesShareOneCounter(t *testing.T) {
	h := newLimitedHandler(t, func(l *limits) { l.loginPair.Limit = 1 })
	email, ip := newAccount(t, h), uniqueIP()

	wantStatus(t, loginFrom(t, h, ip, email, "guess"), http.StatusUnauthorized, "failure 1")
	// Same account to the database, so it must be the same counter.
	wantStatus(t, loginFrom(t, h, ip, " "+strings.ToUpper(email)+" ", "guess"), http.StatusTooManyRequests, "uppercased email")
}
