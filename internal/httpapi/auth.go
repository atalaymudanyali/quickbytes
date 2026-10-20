package httpapi

import (
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"unicode/utf8"

	"github.com/atalaymudanyali/quickbytes/internal/users"
)

type signupRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	FullName string `json:"full_name"`
}

type accountResponse struct {
	User     users.User      `json:"user"`
	Profiles []users.Profile `json:"profiles"`
	// Tokens is set by sign-up and login, and left out by GET /me.
	Tokens *tokenPair `json:"tokens,omitempty"`
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	// Every attempt counts, before the body is even read, so being refused costs
	// almost nothing: no hashing, and no "email_taken" answer to learn from.
	if res := s.limiter.Hit(r.Context(), s.limits.signupIP, clientIP(r)); !res.Allowed {
		writeTooManyRequests(w, r, res.RetryAfter)
		return
	}

	var req signupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	req.FullName = strings.TrimSpace(req.FullName)
	if fields := validateSignup(req); len(fields) > 0 {
		writeValidationError(w, r, fields)
		return
	}

	hash, err := s.hasher.Hash(req.Password)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	user, profile, err := s.users.Create(r.Context(), req.Email, hash, req.FullName)
	if errors.Is(err, users.ErrEmailTaken) {
		writeError(w, r, http.StatusConflict, "email_taken", "an account with this email already exists")
		return
	}
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	profiles := []users.Profile{profile}
	tokens, err := s.startSession(r.Context(), user.ID, profiles)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, accountResponse{User: user, Profiles: profiles, Tokens: tokens})
}

func validateSignup(req signupRequest) map[string]string {
	fields := map[string]string{}
	if !validEmail(req.Email) {
		fields["email"] = "must be a valid email address"
	}
	// Count characters, not bytes: "ş" is one character but two bytes.
	if n := utf8.RuneCountInString(req.Password); n < 8 || n > 128 {
		fields["password"] = "must be 8 to 128 characters"
	}
	if n := utf8.RuneCountInString(req.FullName); n < 1 || n > 100 {
		fields["full_name"] = "must be 1 to 100 characters"
	}
	return fields
}

// validEmail accepts a bare address like ali@example.com. It rejects forms mail
// software allows but a sign-up form shouldn't, such as "Ali <ali@example.com>".
func validEmail(s string) bool {
	if len(s) > 254 {
		return false
	}
	addr, err := mail.ParseAddress(s)
	return err == nil && addr.Address == s && strings.Contains(s[strings.LastIndex(s, "@"):], ".")
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ip := clientIP(r)
	// Every attempt from this IP counts, even one with a broken body.
	if res := s.limiter.Hit(ctx, s.limits.loginIP, ip); !res.Allowed {
		writeTooManyRequests(w, r, res.RetryAfter)
		return
	}

	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	email := emailKey(req.Email)
	pair := email + ":" + ip

	// The two email limits count only failures, so look before trying, and count
	// after a failure. This happens before any password hashing, so a blocked
	// attempt costs almost nothing. The counters are the same whether or not the
	// email has an account, so a 429 reveals nothing about which emails exist.
	if res := s.limiter.Check(ctx, s.limits.loginPair, pair); !res.Allowed {
		writeTooManyRequests(w, r, res.RetryAfter)
		return
	}
	if res := s.limiter.Check(ctx, s.limits.loginEmail, email); !res.Allowed {
		writeTooManyRequests(w, r, res.RetryAfter)
		return
	}
	failed := func() {
		s.limiter.Hit(ctx, s.limits.loginPair, pair)
		s.limiter.Hit(ctx, s.limits.loginEmail, email)
		writeInvalidCredentials(w, r)
	}

	user, err := s.users.GetByEmail(ctx, strings.TrimSpace(req.Email))
	if errors.Is(err, users.ErrNotFound) {
		// Hash anyway, so "no such email" takes as long as "wrong password". If it
		// answered faster, an attacker could time responses to learn which emails
		// have accounts.
		_, _ = s.hasher.Verify(req.Password, s.dummyHash())
		failed()
		return
	}
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}

	ok, err := s.hasher.Verify(req.Password, user.PasswordHash)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	if !ok {
		failed()
		return
	}
	// The right password: forget this device's earlier typos. The any-IP counter
	// is kept, so the owner logging in doesn't hand an attacker elsewhere a fresh
	// set of guesses.
	s.limiter.Reset(ctx, s.limits.loginPair, pair)

	profiles, err := s.users.ListProfiles(ctx, user.ID)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	tokens, err := s.startSession(ctx, user.ID, profiles)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, accountResponse{User: user, Profiles: profiles, Tokens: tokens})
}

// writeInvalidCredentials gives the same answer for an unknown email and a wrong
// password, so the response doesn't reveal which emails are registered.
func writeInvalidCredentials(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusUnauthorized, "invalid_credentials", "email or password is incorrect")
}

// dummyHash is a real hash of a random password, made once with the server's own
// settings, so verifying against it costs the same as a real check.
func (s *Server) dummyHash() string {
	s.dummyOnce.Do(func() {
		s.dummy, _ = s.hasher.Hash(newRequestID())
	})
	return s.dummy
}
