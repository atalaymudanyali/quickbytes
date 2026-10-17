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
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusCreated, accountResponse{User: user, Profiles: []users.Profile{profile}})
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
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	user, err := s.users.GetByEmail(r.Context(), strings.TrimSpace(req.Email))
	if errors.Is(err, users.ErrNotFound) {
		// Hash anyway, so "no such email" takes as long as "wrong password". If it
		// answered faster, an attacker could time responses to learn which emails
		// have accounts.
		_, _ = s.hasher.Verify(req.Password, s.dummyHash())
		writeInvalidCredentials(w, r)
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
		writeInvalidCredentials(w, r)
		return
	}

	profiles, err := s.users.ListProfiles(r.Context(), user.ID)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, accountResponse{User: user, Profiles: profiles})
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
