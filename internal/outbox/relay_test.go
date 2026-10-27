package outbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/testutil"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakePublisher remembers what it was given. It can fail every call (the
// broker is down), and before, if set, runs at the start of each call.
type fakePublisher struct {
	mu        sync.Mutex
	published []Event
	fail      error
	before    func(events []Event)
}

func (p *fakePublisher) Publish(_ context.Context, events []Event) error {
	if p.before != nil {
		p.before(events)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return p.fail
	}
	p.published = append(p.published, events...)
	return nil
}

func (p *fakePublisher) setFail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fail = err
}

func (p *fakePublisher) got() []Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Event(nil), p.published...)
}

// add writes events for the aggregates, in that order, each in its own
// transaction (as moves do), and returns their IDs.
func add(t *testing.T, pool *pgxpool.Pool, aggregates ...uuid.UUID) []uuid.UUID {
	t.Helper()
	ids := make([]uuid.UUID, len(aggregates))
	for i, a := range aggregates {
		ids[i] = uuid.Must(uuid.NewV7())
		err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
			return AddIn(context.Background(), tx, Event{
				ID: ids[i], Topic: "orders", AggregateID: a, Type: "order.test",
				Payload: []byte(`{"id":"` + ids[i].String() + `"}`),
			})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

// relayFor is a relay limited to these aggregates' events.
func relayFor(pool *pgxpool.Pool, pub Publisher, aggregates ...uuid.UUID) *Relay {
	r := NewRelay(pool, pub, quiet)
	r.only = aggregates
	return r
}

// drain runs rounds until one publishes nothing.
func drain(t *testing.T, r *Relay) {
	t.Helper()
	for {
		n, err := r.PublishWaiting(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

// waiting counts the events not yet published, among these.
func waiting(t *testing.T, pool *pgxpool.Pool, ids []uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox WHERE id = ANY($1) AND published_at IS NULL`, ids).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func idsOf(events []Event) []uuid.UUID {
	out := make([]uuid.UUID, len(events))
	for i, e := range events {
		out[i] = e.ID
	}
	return out
}

func equalIDs(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// checkOncePerEventInOrder checks that every event in want was published
// exactly once, and each aggregate's events in the order they were added.
func checkOncePerEventInOrder(t *testing.T, got []Event, want map[uuid.UUID][]uuid.UUID) {
	t.Helper()
	seen := map[uuid.UUID]int{}
	byAggregate := map[uuid.UUID][]uuid.UUID{}
	for _, e := range got {
		seen[e.ID]++
		byAggregate[e.AggregateID] = append(byAggregate[e.AggregateID], e.ID)
	}
	total := 0
	for a, ids := range want {
		total += len(ids)
		for _, id := range ids {
			if seen[id] != 1 {
				t.Fatalf("event %s published %d times, want 1", id, seen[id])
			}
		}
		if !equalIDs(byAggregate[a], ids) {
			t.Fatalf("aggregate %s published as %v, want %v", a, byAggregate[a], ids)
		}
	}
	if len(got) != total {
		t.Fatalf("%d published, want %d", len(got), total)
	}
}

func TestRelayPublishesAndStampsEvents(t *testing.T) {
	pool := testutil.DB(t)
	a, b := uuid.New(), uuid.New()
	ids := add(t, pool, a, b, a)
	pub := &fakePublisher{}
	r := relayFor(pool, pub, a, b)

	n, err := r.PublishWaiting(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// One event per aggregate per round: a's second event waits for the next.
	if n != 2 || !equalIDs(idsOf(pub.got()), ids[:2]) {
		t.Fatalf("first round published %v, want %v", idsOf(pub.got()), ids[:2])
	}
	drain(t, r)
	checkOncePerEventInOrder(t, pub.got(), map[uuid.UUID][]uuid.UUID{a: {ids[0], ids[2]}, b: {ids[1]}})
	if w := waiting(t, pool, ids); w != 0 {
		t.Fatalf("%d events still waiting, want 0", w)
	}
	got := pub.got()
	if got[0].Topic != "orders" || got[0].AggregateID != a || got[0].Type != "order.test" ||
		string(got[0].Payload) != `{"id": "`+ids[0].String()+`"}` {
		t.Fatalf("published %+v (payload %s), want the row as written", got[0], got[0].Payload)
	}
}

// The broker is down: rounds fail, nothing is stamped, nothing is lost. Once
// it's back, every event goes out, in order.
func TestRelayWhileTheBrokerIsDown(t *testing.T) {
	pool := testutil.DB(t)
	a := uuid.New()
	ids := add(t, pool, a, a)
	pub := &fakePublisher{}
	pub.setFail(errors.New("broker down"))
	r := relayFor(pool, pub, a)

	for range 3 {
		if _, err := r.PublishWaiting(context.Background()); err == nil {
			t.Fatal("want the publisher's error")
		}
	}
	ids = append(ids, add(t, pool, a)...) // orders keep moving meanwhile
	if w := waiting(t, pool, ids); w != 3 {
		t.Fatalf("%d events waiting while the broker is down, want all 3", w)
	}

	pub.setFail(nil)
	drain(t, r)
	checkOncePerEventInOrder(t, pub.got(), map[uuid.UUID][]uuid.UUID{a: ids})
}

// The broker stored the batch, but the relay never heard back (its answer
// was lost, or the API was killed right then): the rows aren't stamped, so
// the next round publishes them again. A repeat, never a loss.
func TestLostAnswerMeansARepeatNotALoss(t *testing.T) {
	pool := testutil.DB(t)
	a := uuid.New()
	ids := add(t, pool, a)
	stored := 0
	pub := &fakePublisher{}
	pub.before = func([]Event) {
		stored++
		if stored == 1 {
			pub.setFail(errors.New("connection reset after the broker stored it"))
		} else {
			pub.setFail(nil)
		}
	}
	r := relayFor(pool, pub, a)

	if _, err := r.PublishWaiting(context.Background()); err == nil {
		t.Fatal("want the lost answer's error")
	}
	if w := waiting(t, pool, ids); w != 1 {
		t.Fatalf("%d waiting after a lost answer, want the event still waiting", w)
	}
	drain(t, r)
	if stored != 2 || len(pub.got()) != 1 || pub.got()[0].ID != ids[0] {
		t.Fatalf("broker got the batch %d times, publisher kept %d; want the event sent again", stored, len(pub.got()))
	}
}

// Deterministic: relay A is stuck mid-publish, holding its rows. Relay B
// skips them (it doesn't wait), takes the other aggregate's event, and leaves
// the later event of A's aggregate alone, because A's earlier one hasn't gone
// out yet.
func TestASecondRelaySkipsWhatTheFirstHolds(t *testing.T) {
	pool := testutil.DB(t)
	x, y := uuid.New(), uuid.New()
	ids := add(t, pool, x) // x1
	inside, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	letAGo := func() { releaseOnce.Do(func() { close(release) }) }
	// Even when the test fails early: a relay stuck holding rows would keep
	// its connection, and closing the pool would wait for it forever.
	t.Cleanup(letAGo)
	pubA := &fakePublisher{before: func([]Event) {
		close(inside)
		<-release
	}}
	done := make(chan error, 1)
	go func() {
		_, err := relayFor(pool, pubA, x, y).PublishWaiting(context.Background())
		done <- err
	}()
	<-inside // A holds x1, locked, mid-publish

	ids = append(ids, add(t, pool, x, y)...) // x2, y1
	pubB := &fakePublisher{}
	type result struct {
		n   int
		err error
	}
	doneB := make(chan result, 1)
	go func() {
		n, err := relayFor(pool, pubB, x, y).PublishWaiting(context.Background())
		doneB <- result{n, err}
	}()
	var b result
	select {
	case b = <-doneB:
	case <-time.After(2 * time.Second):
		letAGo()
		<-doneB
		t.Fatal("relay B waited for relay A's rows instead of skipping them")
	}
	if b.err != nil {
		t.Fatal(b.err)
	}
	if b.n != 1 || pubB.got()[0].ID != ids[2] {
		t.Fatalf("relay B published %v, want only y1 (%s): x1 is A's, x2 must wait for x1", idsOf(pubB.got()), ids[2])
	}

	letAGo()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := idsOf(pubA.got()); !equalIDs(got, ids[:1]) {
		t.Fatalf("relay A published %v, want x1", got)
	}
	drain(t, relayFor(pool, pubB, x, y))
	all := append(pubA.got(), pubB.got()...)
	checkOncePerEventInOrder(t, all, map[uuid.UUID][]uuid.UUID{x: {ids[0], ids[1]}, y: {ids[2]}})
}

// Two relays race over many aggregates' events: every event goes out exactly
// once (the publisher never fails here), and each aggregate's in order.
func TestTwoRelaysRacing(t *testing.T) {
	pool := testutil.DB(t)
	var aggregates []uuid.UUID
	for range 30 {
		aggregates = append(aggregates, uuid.New())
	}
	want := map[uuid.UUID][]uuid.UUID{}
	for range 3 {
		for _, a := range aggregates {
			want[a] = append(want[a], add(t, pool, a)...)
		}
	}

	// One shared publisher: its list is the order the broker received them.
	pub := &fakePublisher{before: func([]Event) { time.Sleep(time.Millisecond) }}
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			r := relayFor(pool, pub, aggregates...)
			for idle := 0; idle < 3; {
				n, err := r.PublishWaiting(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					idle++
				}
			}
		})
	}
	wg.Wait()
	drain(t, relayFor(pool, pub, aggregates...)) // anything both left behind
	checkOncePerEventInOrder(t, pub.got(), want)
}

// Run keeps going through failures (backing off) and publishes everything
// once the broker is back; it stops with its context.
func TestRunRecoversAndStops(t *testing.T) {
	pool := testutil.DB(t)
	a := uuid.New()
	ids := add(t, pool, a, a)
	pub := &fakePublisher{}
	pub.setFail(errors.New("broker down"))
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		relayFor(pool, pub, a).Run(ctx, 10*time.Millisecond)
		close(stopped)
	}()

	time.Sleep(100 * time.Millisecond)
	pub.setFail(nil)
	deadline := time.Now().Add(5 * time.Second) // the first backoff is 1–1.2 s
	for waiting(t, pool, ids) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("events still waiting 5 s after the broker came back")
		}
		time.Sleep(20 * time.Millisecond)
	}
	checkOncePerEventInOrder(t, pub.got(), map[uuid.UUID][]uuid.UUID{a: ids})

	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't stop with its context")
	}
}

func TestRelayBackoff(t *testing.T) {
	for _, c := range []struct {
		failures int
		base     time.Duration
	}{{1, time.Second}, {2, 2 * time.Second}, {3, 4 * time.Second}, {5, 16 * time.Second}, {6, 30 * time.Second}, {100, 30 * time.Second}} {
		for range 20 {
			d := relayBackoff(c.failures)
			if d < c.base || d > c.base+c.base/5 {
				t.Fatalf("after %d failures: %v, want %v plus at most 20%%", c.failures, d, c.base)
			}
		}
	}
}
