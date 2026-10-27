package httpapi

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/atalaymudanyali/quickbytes/internal/payments"
	"github.com/atalaymudanyali/quickbytes/internal/webhook"
)

// maxWebhookBytes caps a webhook's body. A real one is under 1 KiB; the cap
// stops anyone from making us read a huge body before its signature has even
// been checked.
const maxWebhookBytes = 64 << 10

// webhookAnswer tells the sender what was done. The provider only looks at
// the 2xx; the outcome is for people reading logs or trying it with curl.
type webhookAnswer struct {
	Outcome payments.Outcome `json:"outcome"`
}

// handlePayProviderWebhook receives the payment provider's webhooks.
//
// There's no login: the provider has no account with us. Its signature is its
// ID card, checked with the shared secret before anything else is read, so
// without the secret every request is a 400.
//
// What each answer means to the provider:
//   - 200: handled (or handled before: a repeat is a 200 too, never an
//     error, or the provider would keep sending it). Stop sending.
//   - 400: not a genuine webhook. Nothing was changed.
//   - 500: our problem (the database). Nothing was changed, the webhook
//     wasn't marked as seen, and the provider's next try is handled for real.
func (s *Server) handlePayProviderWebhook(w http.ResponseWriter, r *http.Request) {
	// The exact bytes received: the signature is over these, and parsing the
	// JSON first could change them.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "webhook body is too large")
			return
		}
		writeError(w, r, http.StatusBadRequest, "invalid_webhook", "could not read the webhook body")
		return
	}

	ev, err := payments.ParsePayProviderWebhook(s.webhookSecret, r.Header.Get(webhook.Header), body, time.Now())
	switch {
	case errors.Is(err, payments.ErrUnsupportedWebhook):
		// Genuine, but about something we don't handle: say so, and don't
		// make the provider try again for hours.
		s.logger.Info("webhook of a type we don't handle; ignored", "event_id", ev.ID, "type", ev.Type)
		writeJSON(w, http.StatusOK, webhookAnswer{Outcome: "ignored"})
		return
	case err != nil:
		s.logger.Warn("webhook refused", "err", err)
		writeError(w, r, http.StatusBadRequest, "invalid_webhook", "the webhook's signature or body is not valid")
		return
	}

	outcome, err := s.payments.ApplyWebhook(r.Context(), ev)
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, webhookAnswer{Outcome: outcome})
}
