package auth_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/atalaymudanyali/quickbytes/internal/auth"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

func setup(t *testing.T) (*auth.RefreshStore, users.User) {
	t.Helper()
	pool := testutil.DB(t)
	u, _, err := users.NewStore(pool).Create(context.Background(), testutil.UniqueEmail(), "hash", "Test")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return auth.NewRefreshStore(pool, time.Hour), u
}

func TestRotate(t *testing.T) {
	ctx := context.Background()
	store, u := setup(t)

	first, err := store.Start(ctx, u.ID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	userID, second, err := store.Rotate(ctx, first)
	if err != nil || userID != u.ID || second == first {
		t.Fatalf("rotate = (%v, %q, %v), want the user and a new token", userID, second, err)
	}
	if _, _, err := store.Rotate(ctx, second); err != nil {
		t.Fatalf("rotating the new token: %v", err)
	}
}

func TestReuseRevokesWholeFamily(t *testing.T) {
	ctx := context.Background()
	store, u := setup(t)

	stolen, _ := store.Start(ctx, u.ID)
	_, current, _ := store.Rotate(ctx, stolen) // the real app moves on

	// The attacker replays the old token: detected and refused...
	if _, _, err := store.Rotate(ctx, stolen); !errors.Is(err, auth.ErrRefreshReused) {
		t.Fatalf("replay err = %v, want ErrRefreshReused", err)
	}
	// ...and the real app's token is dead too, so whoever holds it must log in again.
	if _, _, err := store.Rotate(ctx, current); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("current token after reuse: err = %v, want ErrInvalidRefresh", err)
	}
}

func TestConcurrentRotateOnlyOneWins(t *testing.T) {
	ctx := context.Background()
	store, u := setup(t)
	token, _ := store.Start(ctx, u.ID)

	const n = 5
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := store.Rotate(ctx, token); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d of %d simultaneous refreshes succeeded, want exactly 1", wins, n)
	}
}

func TestRevokeAndUnknown(t *testing.T) {
	ctx := context.Background()
	store, u := setup(t)
	token, _ := store.Start(ctx, u.ID)

	if err := store.Revoke(ctx, token); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, _, err := store.Rotate(ctx, token); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("rotate after logout: err = %v, want ErrInvalidRefresh", err)
	}
	if err := store.Revoke(ctx, "never-issued"); err != nil {
		t.Fatalf("revoking an unknown token should be harmless: %v", err)
	}
	if _, _, err := store.Rotate(ctx, "never-issued"); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("unknown token: err = %v, want ErrInvalidRefresh", err)
	}
}
