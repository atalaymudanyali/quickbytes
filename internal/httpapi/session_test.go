package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/atalaymudanyali/quickbytes/internal/testutil"
)

func signUp(t *testing.T, h http.Handler) accountResponse {
	t.Helper()
	rec := postJSON(t, h, "/auth/signup", signupBody(testutil.UniqueEmail(), "correct-password"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("signup: %d %s", rec.Code, rec.Body)
	}
	var acc accountResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &acc); err != nil || acc.Tokens == nil {
		t.Fatalf("signup response has no tokens: %s", rec.Body)
	}
	return acc
}

func getMe(t *testing.T, h http.Handler, accessToken string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	return do(t, h, req)
}

func TestMeNeedsAValidToken(t *testing.T) {
	h := newAuthTestHandler(t)
	acc := signUp(t, h)

	rec := getMe(t, h, acc.Tokens.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("with token: status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var me accountResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me.User.ID != acc.User.ID || me.Tokens != nil {
		t.Fatalf("/me = %+v, want the signed-up user and no tokens", me)
	}

	for name, token := range map[string]string{"missing": "", "invalid": "abc.def.ghi"} {
		t.Run(name, func(t *testing.T) {
			rec := getMe(t, h, token)
			if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatalf("status = %d, WWW-Authenticate = %q; want 401 with the header",
					rec.Code, rec.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

func TestRefreshAndLogout(t *testing.T) {
	h := newAuthTestHandler(t)
	acc := signUp(t, h)

	rec := postJSON(t, h, "/auth/refresh", `{"refresh_token": "`+acc.Tokens.RefreshToken+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: status = %d; body: %s", rec.Code, rec.Body)
	}
	var pair tokenPair
	_ = json.Unmarshal(rec.Body.Bytes(), &pair)
	if pair.RefreshToken == "" || pair.RefreshToken == acc.Tokens.RefreshToken {
		t.Fatal("refresh didn't return a new refresh token")
	}
	if rec := getMe(t, h, pair.AccessToken); rec.Code != http.StatusOK {
		t.Fatalf("new access token rejected: %d", rec.Code)
	}

	if rec := postJSON(t, h, "/auth/logout", `{"refresh_token": "`+pair.RefreshToken+`"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("logout: status = %d, want 204", rec.Code)
	}
	rec = postJSON(t, h, "/auth/refresh", `{"refresh_token": "`+pair.RefreshToken+`"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("refresh after logout: status = %d, want 401", rec.Code)
	}
	if got := decodeError(t, rec); got.Code != "invalid_refresh_token" {
		t.Fatalf("code = %q, want invalid_refresh_token", got.Code)
	}
}
