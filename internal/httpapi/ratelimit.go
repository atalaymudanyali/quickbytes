package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/atalaymudanyali/quickbytes/internal/ratelimit"
)

// limits are the rate limits of the auth endpoints. They live on the Server (not
// as constants) so tests can use smaller numbers.
type limits struct {
	// Every login attempt from one IP: stops one machine trying many emails.
	loginIP ratelimit.Rule
	// Failed logins for one email from one IP: stops one attacker guessing one
	// account, without blocking the real owner on their own phone.
	loginPair ratelimit.Rule
	// Failed logins for one email from any IP: a backstop against many machines
	// (a botnet) guessing one account. High, so a stranger can't easily use it to
	// block someone else's login.
	loginEmail ratelimit.Rule
}

var defaultLimits = limits{
	loginIP:    ratelimit.Rule{Name: "login:ip", Limit: 20, Window: time.Minute},
	loginPair:  ratelimit.Rule{Name: "login:email-ip", Limit: 5, Window: 15 * time.Minute},
	loginEmail: ratelimit.Rule{Name: "login:email", Limit: 50, Window: time.Hour},
}

// clientIP is the address the request came from. X-Forwarded-For is ignored on
// purpose: anyone can send it, so trusting it would let an attacker claim a new IP
// on every request. Once a proxy (nginx) sits in front, it should be trusted only
// when the request comes from that proxy.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// emailKey turns an email into a counter name. It's hashed, so whoever looks
// inside Redis doesn't see our users' addresses, and lowercased, because the
// database treats Ali@x.com and ali@x.com as the same account.
func emailKey(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(sum[:])
}

// writeTooManyRequests answers 429 with Retry-After: how many seconds until the
// client may try again (rounded up, so it never says 0 while still blocked).
func writeTooManyRequests(w http.ResponseWriter, r *http.Request, retryAfter time.Duration) {
	secs := int((retryAfter + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
	writeError(w, r, http.StatusTooManyRequests, "rate_limited", "too many attempts, try again later")
}
