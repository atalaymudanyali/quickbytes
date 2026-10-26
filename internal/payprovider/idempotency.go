package payprovider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/atalaymudanyali/quickbytes/internal/idempotency"
)

const (
	idempotencyKeyHeader = "Idempotency-Key"
	// replayedHeader marks an answer that was stored earlier and sent again.
	replayedHeader = "Idempotent-Replayed"
	// requestDeadline bounds how long an idempotent request may run. It stays
	// below idempotency.AbandonedAfter, after which a key in progress is
	// taken over, so the request holding it must have stopped by then.
	requestDeadline = idempotency.AbandonedAfter / 2
	maxBodyBytes    = 1 << 16 // 64 KiB: payment requests are tiny
)

// idempotent makes a write safe to retry, like the API's checkout (see
// internal/httpapi/idempotency.go, which this follows), and like Stripe: the
// merchant sends an Idempotency-Key header, the same for every attempt of one
// action.
//
//   - the first attempt runs the handler, and its answer is stored with the key;
//   - a later attempt with the same key and request gets that answer back,
//     marked Idempotent-Replayed: true, without moving any money;
//   - while the first attempt still runs: 409 idempotency_in_progress;
//   - the same key for a different request: 422 idempotency_key_reused.
//
// Keys belong to the merchant (they're stored in the provider's own
// idempotency_keys table, owned by merchants), so this sits behind
// requireMerchant.
//
// Answers below 500 are final and stored: a declined card stays declined for
// that key. A 500 means something broke and nothing was committed, so the key
// is released for the next attempt. Handlers that move money store their
// answer inside the payment's transaction (saveAnswerIn), so the money and the
// answer commit together.
func (s *Server) idempotent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(idempotencyKeyHeader)
		switch {
		case key == "":
			writeError(w, r, http.StatusBadRequest, "idempotency_key_missing",
				"send an Idempotency-Key header (a new random value, such as a UUID, for each action; the same one when retrying it)")
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

		claim, stored, err := s.idempotency.Begin(r.Context(), merchantFrom(r.Context()).ID, key, fp)
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
			writeJSONBody(w, stored.Status, stored.Body)
			return
		}

		// From here on this attempt holds the key. Storing or releasing it must
		// happen even if the caller hangs up, so that doesn't use the request's
		// context.
		after := context.WithoutCancel(r.Context())
		defer func() {
			if v := recover(); v != nil {
				s.releaseKey(after, r, claim)
				panic(v) // recoverPanics turns it into the 500
			}
		}()

		held := &heldKey{claim: claim}
		ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), heldKeyKey, held), requestDeadline)
		defer cancel()
		rec := &responseCopy{ResponseWriter: w, status: http.StatusOK}
		next(rec, r.WithContext(ctx))

		if rec.status >= 500 {
			s.releaseKey(after, r, claim)
			return
		}
		if held.saved {
			return
		}
		err = s.idempotency.Save(after, claim, idempotency.Response{Status: rec.status, Body: rec.body.Bytes()})
		if err != nil {
			// The caller already has its answer. The key stays in progress and
			// is taken over after a minute, so retries are only delayed.
			s.logger.Error("save idempotent response", "request_id", requestIDFrom(r.Context()), "err", err)
		}
	}
}

// heldKey is the key the current request holds, for saveAnswerIn.
type heldKey struct {
	claim idempotency.Claim
	saved bool // the handler stored the answer itself
}

// saveAnswerIn stores the answer to the request's Idempotency-Key inside tx, so
// it commits or rolls back with the payment change. The handler must then send
// exactly status and body. Outside idempotent it does nothing.
func saveAnswerIn(ctx context.Context, tx pgx.Tx, status int, body []byte) error {
	held, ok := ctx.Value(heldKeyKey).(*heldKey)
	if !ok {
		return nil
	}
	if err := idempotency.SaveTx(ctx, tx, held.claim, idempotency.Response{Status: status, Body: body}); err != nil {
		return err
	}
	held.saved = true
	return nil
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
