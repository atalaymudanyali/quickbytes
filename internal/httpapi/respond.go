package httpapi

import (
	"encoding/json"
	"net/http"
)

// errorBody is the one shape every error response has, so clients can handle all
// errors the same way:
//
//	{"error": {"code": "not_found", "message": "...", "request_id": "..."}}
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	// Code is stable and meant for programs (the app switches on it).
	Code string `json:"code"`
	// Message is meant for people and may change wording.
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{
		Code:      code,
		Message:   message,
		RequestID: requestIDFrom(r.Context()),
	}})
}
