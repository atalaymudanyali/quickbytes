package addresses_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/addresses"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

var home = addresses.New{
	Label: "Ev", AddressLine: "Caferağa Mh., Moda Cd. No:40, Kadıköy",
	Details: "Kat 3, daire 7", Lat: 40.9840, Lng: 29.0270,
}

// newCustomer signs up a user and returns their customer profile's ID.
func newCustomer(t *testing.T, u *users.Store) uuid.UUID {
	t.Helper()
	_, p, err := u.Create(context.Background(), testutil.UniqueEmail(), "hash", "Customer")
	if err != nil {
		t.Fatal(err)
	}
	return p.ID
}

func TestCreateAndList(t *testing.T) {
	pool := testutil.DB(t)
	store, ctx := addresses.NewStore(pool), context.Background()
	me, other := newCustomer(t, users.NewStore(pool)), newCustomer(t, users.NewStore(pool))

	first, err := store.Create(ctx, me, home)
	if err != nil {
		t.Fatal(err)
	}
	if first.Lat != home.Lat || first.Lng != home.Lng || first.Details != home.Details {
		t.Fatalf("got %+v, want the saved fields back", first)
	}
	if _, err := store.Create(ctx, other, home); err != nil {
		t.Fatal(err)
	}

	mine, err := store.List(ctx, me)
	if err != nil || len(mine) != 1 || mine[0].ID != first.ID {
		t.Fatalf("list = %v, %v; want only my one address", mine, err)
	}
}

func TestLimitHoldsUnderConcurrentCreates(t *testing.T) {
	pool := testutil.DB(t)
	store, ctx := addresses.NewStore(pool), context.Background()
	me := newCustomer(t, users.NewStore(pool))

	// Twice the limit, all at once: without the profile lock, several would count
	// the same number and slip past the cap.
	var wg sync.WaitGroup
	errs := make(chan error, 2*addresses.MaxPerCustomer)
	for range 2 * addresses.MaxPerCustomer {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Create(ctx, me, home)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	created, refused := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			created++
		case errors.Is(err, addresses.ErrLimitReached):
			refused++
		default:
			t.Fatal(err)
		}
	}
	if created != addresses.MaxPerCustomer || refused != addresses.MaxPerCustomer {
		t.Fatalf("created %d, refused %d; want %d each", created, refused, addresses.MaxPerCustomer)
	}
}

func TestUpdateAndDeleteOnlyYourOwn(t *testing.T) {
	pool := testutil.DB(t)
	store, ctx := addresses.NewStore(pool), context.Background()
	me, other := newCustomer(t, users.NewStore(pool)), newCustomer(t, users.NewStore(pool))
	a, err := store.Create(ctx, me, home)
	if err != nil {
		t.Fatal(err)
	}

	work := "İş"
	if _, err := store.Update(ctx, a.ID, other, addresses.Patch{Label: &work}); !errors.Is(err, addresses.ErrNotFound) {
		t.Fatalf("someone else's update: err = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, a.ID, other); !errors.Is(err, addresses.ErrNotFound) {
		t.Fatalf("someone else's delete: err = %v, want ErrNotFound", err)
	}

	got, err := store.Update(ctx, a.ID, me, addresses.Patch{Label: &work})
	if err != nil || got.Label != work || got.AddressLine != home.AddressLine || got.Lat != home.Lat {
		t.Fatalf("update = %+v, %v; want only the label changed", got, err)
	}
	if err := store.Delete(ctx, a.ID, me); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, a.ID, me); !errors.Is(err, addresses.ErrNotFound) {
		t.Fatalf("second delete: err = %v, want ErrNotFound", err)
	}
}
