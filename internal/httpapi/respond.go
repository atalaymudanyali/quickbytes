package httpapi

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"

	"github.com/google/uuid"
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
	Message string `json:"message"`
	// Fields explains which input fields were wrong, for validation errors.
	Fields map[string]string `json:"fields,omitempty"`
	// GroupID and ChoiceID point at the option group or choice an
	// option-picking error is about (uuid.Nil: left out).
	GroupID   uuid.UUID `json:"group_id,omitzero"`
	ChoiceID  uuid.UUID `json:"choice_id,omitzero"`
	RequestID string    `json:"request_id,omitempty"`
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

// writeValidationError reports every invalid field at once, so a form can highlight
// all its mistakes in one round trip.
func writeValidationError(w http.ResponseWriter, r *http.Request, fields map[string]string) {
	writeJSON(w, http.StatusUnprocessableEntity, errorBody{Error: errorDetail{
		Code:      "validation_failed",
		Message:   "some fields are invalid",
		Fields:    fields,
		RequestID: requestIDFrom(r.Context()),
	}})
}

// writeInternalError logs the real cause and tells the client only that something
// failed, since error details can reveal how the system works inside.
func (s *Server) writeInternalError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Error("internal error", "request_id", requestIDFrom(r.Context()), "err", err)
	writeError(w, r, http.StatusInternalServerError, "internal", "something went wrong")
}

const maxBodyBytes = 1 << 20 // 1 MiB

// decodeJSON reads a JSON request body into dst. It requires the
// "Content-Type: application/json" header, and rejects bodies over 1 MiB,
// unknown fields (usually a typo in the client) and anything after the object.
// It writes the error response itself and returns false on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	// "application/json; charset=utf-8" is fine too, so parse instead of comparing.
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		writeError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type",
			`the body must be JSON, sent with "Content-Type: application/json"`)
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large")
			return false
		}
		writeError(w, r, http.StatusBadRequest, "invalid_json", "request body is not valid JSON for this endpoint: "+err.Error())
		return false
	}
	if dec.More() {
		writeError(w, r, http.StatusBadRequest, "invalid_json", "request body must contain a single JSON object")
		return false
	}
	return true
}
