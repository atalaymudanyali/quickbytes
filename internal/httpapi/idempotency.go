package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/atalaymudanyali/quickbytes/internal/idempotency"
)

const (
	idempotencyKeyHeader = "Idempotency-Key"
	// replayedHeader marks an answer that was stored earlier and sent again,
	// so a client (or a person debugging) can tell a replay from fresh work.
	replayedHeader = "Idempotent-Replayed"
	// requestDeadline bounds how long an idempotent request may run. It must
	// stay below idempotency.AbandonedAfter: a key in progress that long is
	// taken over, so the request holding it must have stopped by then.
	requestDeadline = idempotency.AbandonedAfter / 2
)

// idempotent makes a POST safe to retry. The client sends an Idempotency-Key
// header, the same for every attempt of one action:
//
//   - the first attempt runs the handler, and its answer (status and body) is
//     stored with the key;
//   - a later attempt with the same key and body gets that stored answer back,
//     marked with Idempotent-Replayed: true, without running the handler;
//   - while the first attempt still runs: 409 idempotency_in_progress;
//   - the same key with a different body: 422 idempotency_key_reused.
//
// Keys belong to the logged-in user, so this must sit behind requireAuth.
//
// Answers below 500 are final ("your cart is empty" stays the answer to that
// attempt) and are stored. A 500 (or a panic) means something broke midway,
// so the key is released and the next attempt runs the handler again.
func (s *Server) idempotent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(idempotencyKeyHeader)
		switch {
		case key == "":
			writeError(w, r, http.StatusBadRequest, "idempotency_key_missing",
				"send an Idempotency-Key header (a new random value, such as a UUID, for each checkout; the same one when retrying it)")
			return
		case !idempotency.ValidKey(key):
			writeError(w, r, http.StatusBadRequest, "idempotency_key_invalid",
				"the Idempotency-Key must be 1 to 255 printable characters, without spaces")
			return
		}

		// The fingerprint needs the whole body, so read it here and hand the
		// handler a copy.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large")
				return
			}
			writeError(w, r, http.StatusBadRequest, "invalid_json", "could not read the request body")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		fp := idempotency.FingerprintOf(r.Method, r.URL.Path, body)

		claim, stored, err := s.idempotency.Begin(r.Context(), mustUserID(r.Context()), key, fp)
		switch {
		case errors.Is(err, idempotency.ErrInProgress):
			w.Header().Set("Retry-After", "1")
			writeError(w, r, http.StatusConflict, "idempotency_in_progress",
				"a request with this Idempotency-Key is still being handled; retry shortly to get its answer")
			return
		case errors.Is(err, idempotency.ErrKeyReused):
			writeError(w, r, http.StatusUnprocessableEntity, "idempotency_key_reused",
				"this Idempotency-Key was already used for a different request; use a new key for a new action")
			return
		case err != nil:
			s.writeInternalError(w, r, err)
			return
		case stored != nil:
			w.Header().Set(replayedHeader, "true")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(stored.Status)
			_, _ = w.Write(stored.Body)
			return
		}

		// From here on this attempt holds the key. Storing the answer must
		// happen even if the client hangs up, so it doesn't use the request's
		// context.
		after := context.WithoutCancel(r.Context())
		defer func() {
			if v := recover(); v != nil {
				s.releaseKey(after, r, claim)
				panic(v) // recoverPanics turns it into the 500
			}
		}()

		ctx, cancel := context.WithTimeout(r.Context(), requestDeadline)
		defer cancel()
		rec := &responseCopy{ResponseWriter: w, status: http.StatusOK}
		next(rec, r.WithContext(ctx))

		if rec.status >= 500 {
			s.releaseKey(after, r, claim)
			return
		}
		err = s.idempotency.Save(after, claim, idempotency.Response{Status: rec.status, Body: rec.body.Bytes()})
		if err != nil {
			// The client already has its answer. The key stays in progress and
			// is taken over after a minute, so retries are only delayed.
			s.logger.Error("save idempotent response", "request_id", requestIDFrom(r.Context()), "err", err)
		}
	}
}

func (s *Server) releaseKey(ctx context.Context, r *http.Request, c idempotency.Claim) {
	if err := s.idempotency.Release(ctx, c); err != nil {
		s.logger.Error("release idempotency key", "request_id", requestIDFrom(r.Context()), "err", err)
	}
}

// responseCopy sends a response on as usual and keeps a copy of its status and
// body, to be stored for replays.
type responseCopy struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (c *responseCopy) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *responseCopy) Write(b []byte) (int, error) {
	c.body.Write(b)
	return c.ResponseWriter.Write(b)
}
