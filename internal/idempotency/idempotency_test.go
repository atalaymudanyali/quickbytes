package idempotency

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atalaymudanyali/quickbytes/internal/testutil"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

type fixture struct {
	pool  *pgxpool.Pool
	store *Store
	users *users.Store
}

func setup(t *testing.T) fixture {
	t.Helper()
	pool := testutil.DB(t)
	return fixture{pool: pool, store: NewStore(pool), users: users.NewStore(pool)}
}

func (f fixture) user(t *testing.T) uuid.UUID {
	t.Helper()
	u, _, err := f.users.Create(context.Background(), testutil.UniqueEmail(), "hash", "Test")
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// age moves a key's times back, as if it was created (and claimed) that long ago.
func (f fixture) age(t *testing.T, user uuid.UUID, key, column string, by time.Duration) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE idempotency_keys SET `+column+` = `+column+` - make_interval(secs => $3)
		WHERE user_id = $1 AND key = $2`, user, key, by.Seconds()); err != nil {
		t.Fatal(err)
	}
}

// mustClaim calls Begin and fails the test unless it claimed the key.
func (f fixture) mustClaim(t *testing.T, user uuid.UUID, key string, fp Fingerprint) Claim {
	t.Helper()
	c, resp, err := f.store.Begin(context.Background(), user, key, fp)
	if err != nil || resp != nil {
		t.Fatalf("Begin: want a claim, got response %v, err %v", resp, err)
	}
	return c
}

var (
	checkout      = FingerprintOf("POST", "/orders", []byte(`{"address_id":"a"}`))
	otherCheckout = FingerprintOf("POST", "/orders", []byte(`{"address_id":"b"}`))
)

func TestFingerprintCoversMethodPathAndBody(t *testing.T) {
	same := FingerprintOf("POST", "/orders", []byte(`{"address_id":"a"}`))
	if same != checkout {
		t.Error("the same request gave a different fingerprint")
	}
	for name, fp := range map[string]Fingerprint{
		"method": FingerprintOf("PUT", "/orders", []byte(`{"address_id":"a"}`)),
		"path":   FingerprintOf("POST", "/cart", []byte(`{"address_id":"a"}`)),
		"body":   otherCheckout,
	} {
		if fp == checkout {
			t.Errorf("another %s gave the same fingerprint", name)
		}
	}
	// Without the separator, these two would hash the same bytes.
	if FingerprintOf("POST", "/orders", []byte("/x{}")) == FingerprintOf("POST", "/orders/x", []byte("{}")) {
		t.Error("path and body ran into each other")
	}
}

func TestFinishedKeyReplaysItsAnswer(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	user := f.user(t)

	c := f.mustClaim(t, user, "k1", checkout)
	want := Response{Status: 201, Body: []byte(`{"id":"order-1"}`)}
	if err := f.store.Save(ctx, c, want); err != nil {
		t.Fatal(err)
	}

	for i := range 3 {
		_, got, err := f.store.Begin(ctx, user, "k1", checkout)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.Status != want.Status || !bytes.Equal(got.Body, want.Body) {
			t.Fatalf("attempt %d: got %+v, want the stored %+v", i+2, got, want)
		}
	}
}

func TestKeyInProgressIsRefused(t *testing.T) {
	f := setup(t)
	user := f.user(t)
	f.mustClaim(t, user, "k1", checkout)

	_, _, err := f.store.Begin(context.Background(), user, "k1", checkout)
	if !errors.Is(err, ErrInProgress) {
		t.Fatalf("got %v, want ErrInProgress", err)
	}
}

func TestKeyReusedForAnotherRequest(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	user := f.user(t)

	// While the first request runs...
	c := f.mustClaim(t, user, "k1", checkout)
	if _, _, err := f.store.Begin(ctx, user, "k1", otherCheckout); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("in progress: got %v, want ErrKeyReused", err)
	}
	// ...and after it finished.
	if err := f.store.Save(ctx, c, Response{Status: 201, Body: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.Begin(ctx, user, "k1", otherCheckout); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("done: got %v, want ErrKeyReused", err)
	}
}

func TestKeysBelongToOneUser(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice, bob := f.user(t), f.user(t)

	c := f.mustClaim(t, alice, "same-key", checkout)
	if err := f.store.Save(ctx, c, Response{Status: 201, Body: []byte(`{"owner":"alice"}`)}); err != nil {
		t.Fatal(err)
	}
	// Bob picked the same key: he gets his own claim, not Alice's answer.
	f.mustClaim(t, bob, "same-key", checkout)
}

func TestSimultaneousAttemptsOnlyOneClaims(t *testing.T) {
	f := setup(t)
	user := f.user(t)

	const attempts = 10
	var (
		wg               sync.WaitGroup
		start            = make(chan struct{})
		mu               sync.Mutex
		claims, inFlight int
	)
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // all go at once
			_, resp, err := f.store.Begin(context.Background(), user, "k1", checkout)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && resp == nil:
				claims++
			case errors.Is(err, ErrInProgress):
				inFlight++
			default:
				t.Errorf("unexpected: response %v, err %v", resp, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if claims != 1 || inFlight != attempts-1 {
		t.Fatalf("%d claimed, %d in progress; want 1 and %d", claims, inFlight, attempts-1)
	}
}

func TestAbandonedKeyIsTakenOver(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	user := f.user(t)

	dead := f.mustClaim(t, user, "k1", checkout)
	f.age(t, user, "k1", "claimed_at", AbandonedAfter+time.Second)

	// Only the same request may take it over.
	if _, _, err := f.store.Begin(ctx, user, "k1", otherCheckout); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("other request: got %v, want ErrKeyReused", err)
	}
	retry := f.mustClaim(t, user, "k1", checkout)

	// The first attempt wakes up after all: its answer must not win.
	if err := f.store.Save(ctx, dead, Response{Status: 409, Body: []byte(`{"old":true}`)}); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("old attempt's Save: got %v, want ErrClaimLost", err)
	}
	if err := f.store.Release(ctx, dead); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Save(ctx, retry, Response{Status: 201, Body: []byte(`{"new":true}`)}); err != nil {
		t.Fatalf("retry's Save: %v", err)
	}
	_, got, err := f.store.Begin(ctx, user, "k1", checkout)
	if err != nil || got == nil || string(got.Body) != `{"new":true}` {
		t.Fatalf("got %+v, %v; want the retry's answer", got, err)
	}
}

func TestRecentKeyInProgressIsNotTakenOver(t *testing.T) {
	f := setup(t)
	user := f.user(t)
	f.mustClaim(t, user, "k1", checkout)
	f.age(t, user, "k1", "claimed_at", AbandonedAfter-5*time.Second)

	if _, _, err := f.store.Begin(context.Background(), user, "k1", checkout); !errors.Is(err, ErrInProgress) {
		t.Fatalf("got %v, want ErrInProgress", err)
	}
}

func TestExpiredKeyStartsOver(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	user := f.user(t)

	c := f.mustClaim(t, user, "k1", checkout)
	if err := f.store.Save(ctx, c, Response{Status: 201, Body: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	f.age(t, user, "k1", "created_at", Lifetime+time.Minute)

	// A day later the key means a new action, even for another request.
	f.mustClaim(t, user, "k1", otherCheckout)
	var fresh bool
	if err := f.pool.QueryRow(ctx, `SELECT created_at > now() - interval '1 minute'
		FROM idempotency_keys WHERE user_id = $1 AND key = 'k1'`, user).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Error("the restarted key kept its old created_at, so it would expire right away")
	}
}

func TestReleasedKeyCanBeClaimedAgain(t *testing.T) {
	f := setup(t)
	user := f.user(t)
	c := f.mustClaim(t, user, "k1", checkout)
	if err := f.store.Release(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	f.mustClaim(t, user, "k1", checkout)
}

func TestDeleteExpiredKeepsFreshKeys(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	user := f.user(t)

	f.mustClaim(t, user, "old", checkout)
	f.age(t, user, "old", "created_at", Lifetime+time.Minute)
	f.mustClaim(t, user, "fresh", checkout)
	f.age(t, user, "fresh", "created_at", Lifetime-time.Minute)

	// Other packages' tests may have left old keys too: loop until a short batch.
	for {
		n, err := f.store.DeleteExpired(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n < cleanupBatch {
			break
		}
	}
	var keys []string
	rows, err := f.pool.Query(ctx, `SELECT key FROM idempotency_keys WHERE user_id = $1 ORDER BY key`, user)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	if len(keys) != 1 || keys[0] != "fresh" {
		t.Fatalf("left %v, want [fresh]", keys)
	}
}
