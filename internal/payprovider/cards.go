package payprovider

// Outcome is what a test card does when a payment is authorized with it.
type Outcome int

const (
	// Approve: the amount is held straight away (authorized).
	Approve Outcome = iota
	// Decline: refused by the card's bank (failed, card_declined).
	Decline
	// InsufficientFunds: not enough money on the card (failed,
	// insufficient_funds).
	InsufficientFunds
	// Slow: the bank hasn't answered yet. The payment stays processing and
	// gets its final answer (authorized) later.
	Slow
	// Hang: authorized straight away, but the answer is sent only after 30
	// seconds, longer than a caller should wait. The caller gives up without
	// knowing the money was held.
	Hang
)

// Test cards, like real providers' (Stripe's approve, decline and
// insufficient-funds numbers are the same). Any other number is refused: this
// provider never sees a real card.
var testCards = map[string]Outcome{
	"4242424242424242": Approve,
	"4000000000000002": Decline,
	"4000000000009995": InsufficientFunds,
	"4000000000007777": Slow,
	"4000000000003333": Hang,
}

// TestCard says what a card number does, and whether it's a test card at all.
func TestCard(number string) (Outcome, bool) {
	o, ok := testCards[number]
	return o, ok
}
