package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout is how long a call to the provider may take before we stop
// waiting and treat it as unanswered. Well below the hang card's 30 seconds,
// and well below checkout's own 30-second deadline.
const DefaultTimeout = 5 * time.Second

// maxAnswerBytes caps how much of an answer is read. The provider's answers
// are small; anything bigger is not one of them.
const maxAnswerBytes = 1 << 20

// PayProvider is the plug for our own mock payment service (cmd/payprovider):
// it speaks its HTTP API and translates its answers into ours.
type PayProvider struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

// The compiler checks the plug fits the socket.
var _ Provider = (*PayProvider)(nil)

// NewPayProvider talks to the provider at baseURL (http://payprovider:8090 in
// Compose) as the merchant with apiKey. Every call gives up after timeout.
func NewPayProvider(baseURL, apiKey string, timeout time.Duration) *PayProvider {
	return &PayProvider{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client:  &http.Client{Timeout: timeout},
	}
}

func (p *PayProvider) Authorize(ctx context.Context, req AuthorizeRequest) (ProviderPayment, error) {
	body, err := json.Marshal(map[string]any{
		"amount_kurus": req.AmountKurus,
		"card_number":  req.CardNumber,
		"reference":    req.Reference,
	})
	if err != nil {
		return ProviderPayment{}, err
	}
	return p.payment(ctx, http.MethodPost, "/v1/payments", req.Key, body)
}

func (p *PayProvider) Capture(ctx context.Context, key, providerID string) (ProviderPayment, error) {
	return p.payment(ctx, http.MethodPost, "/v1/payments/"+url.PathEscape(providerID)+"/capture", key, nil)
}

func (p *PayProvider) Void(ctx context.Context, key, providerID string) (ProviderPayment, error) {
	return p.payment(ctx, http.MethodPost, "/v1/payments/"+url.PathEscape(providerID)+"/void", key, nil)
}

func (p *PayProvider) Refund(ctx context.Context, key, providerID string) (ProviderPayment, error) {
	return p.payment(ctx, http.MethodPost, "/v1/payments/"+url.PathEscape(providerID)+"/refund", key, nil)
}

func (p *PayProvider) Get(ctx context.Context, providerID string) (ProviderPayment, error) {
	return p.payment(ctx, http.MethodGet, "/v1/payments/"+url.PathEscape(providerID), "", nil)
}

func (p *PayProvider) FindByReference(ctx context.Context, reference string) ([]ProviderPayment, error) {
	status, raw, err := p.call(ctx, http.MethodGet, "/v1/payments?reference="+url.QueryEscape(reference), "", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, refusalOrNoAnswer(status, raw)
	}
	var list struct {
		Payments []wirePayment `json:"payments"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("%w: unreadable payment list: %v", ErrNoAnswer, err)
	}
	out := make([]ProviderPayment, 0, len(list.Payments))
	for _, w := range list.Payments {
		pp, err := w.ours()
		if err != nil {
			return nil, err
		}
		out = append(out, pp)
	}
	return out, nil
}

// wirePayment is a payment as the provider sends it.
type wirePayment struct {
	ID          string  `json:"id"`
	AmountKurus int     `json:"amount_kurus"`
	Status      string  `json:"status"`
	DeclineCode *string `json:"decline_code"`
}

// providerStatuses translates the provider's statuses into ours. Here the words happen
// to match; a Stripe plug would map "requires_capture" to Authorized,
// "succeeded" to Captured, and so on.
var providerStatuses = map[string]Status{
	"processing": Processing,
	"authorized": Authorized,
	"captured":   Captured,
	"voided":     Voided,
	"refunded":   Refunded,
	"failed":     Failed,
}

var declineCodes = map[string]string{
	"card_declined":      CardDeclined,
	"insufficient_funds": InsufficientFunds,
}

func (w wirePayment) ours() (ProviderPayment, error) {
	s, ok := providerStatuses[w.Status]
	if !ok || w.ID == "" {
		// Not an answer we understand, so not one we can act on.
		return ProviderPayment{}, fmt.Errorf("%w: payment %q with unexpected status %q", ErrNoAnswer, w.ID, w.Status)
	}
	pp := ProviderPayment{ID: w.ID, Status: s, AmountKurus: w.AmountKurus}
	if s == Failed {
		code := CardDeclined // a refusal we have no word for is still a decline
		if w.DeclineCode != nil {
			if c, ok := declineCodes[*w.DeclineCode]; ok {
				code = c
			}
		}
		pp.DeclineCode = &code
	}
	return pp, nil
}

// payment makes a call whose answer is one payment.
//
// A declined card (402) and a move the payment's status didn't allow (409)
// still carry the payment as it is now: that's news about the payment, so
// it's returned like a success. The caller compares the status with what it
// wanted, as it always does.
func (p *PayProvider) payment(ctx context.Context, method, path, key string, body []byte) (ProviderPayment, error) {
	status, raw, err := p.call(ctx, method, path, key, body)
	if err != nil {
		return ProviderPayment{}, err
	}
	switch status {
	case http.StatusOK, http.StatusCreated:
		var w wirePayment
		if err := json.Unmarshal(raw, &w); err != nil {
			return ProviderPayment{}, fmt.Errorf("%w: unreadable payment: %v", ErrNoAnswer, err)
		}
		return w.ours()
	case http.StatusPaymentRequired, http.StatusConflict:
		var r struct {
			Payment *wirePayment `json:"payment"`
		}
		if json.Unmarshal(raw, &r) == nil && r.Payment != nil {
			return r.Payment.ours()
		}
	}
	return ProviderPayment{}, refusalOrNoAnswer(status, raw)
}

// call sends one request and returns the status and body of the answer. Not
// getting an answer at all (the timeout, a refused connection, a body cut
// off halfway) is ErrNoAnswer.
func (p *PayProvider) call(ctx context.Context, method, path, key string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %s %s: %v", ErrNoAnswer, method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %s %s: reading the answer: %v", ErrNoAnswer, method, path, err)
	}
	return resp.StatusCode, raw, nil
}

// refusalOrNoAnswer sorts an answer that isn't a payment.
//
// Only a 4xx where the provider judged the request itself (400, 404, 422...)
// is a final no. These are "no answer" instead, as nothing was decided about
// the payment: 5xx (the provider broke), 408 and 429 (try later), 401 (our
// key is wrong: a setup mistake, not a reason to fail a customer's payment)
// and 409 idempotency_in_progress (another call with this key is still
// running; its answer is the one that counts).
func refusalOrNoAnswer(status int, raw []byte) error {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	code := e.Error.Code

	switch {
	case status >= 500, status == http.StatusRequestTimeout, status == http.StatusTooManyRequests,
		status == http.StatusUnauthorized, status == http.StatusConflict && code == "idempotency_in_progress":
		return fmt.Errorf("%w: provider answered %d %s", ErrNoAnswer, status, code)
	case status >= 400:
		return &RefusedError{HTTPStatus: status, Code: code, Message: e.Error.Message}
	default:
		return fmt.Errorf("%w: unexpected answer %d", ErrNoAnswer, status)
	}
}

// IsRefused says whether err is the provider's final no to a request.
func IsRefused(err error) bool {
	var r *RefusedError
	return errors.As(err, &r)
}
