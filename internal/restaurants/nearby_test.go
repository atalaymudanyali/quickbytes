package restaurants_test

import (
	"context"
	"math/rand/v2"
	"testing"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/restaurants"
)

// The test database is shared with other test packages and keeps rows from earlier
// runs, so these tests only look at the restaurants they created themselves.
func onlyOurs(found []restaurants.Nearby, ours map[uuid.UUID]bool) []restaurants.Nearby {
	var out []restaurants.Nearby
	for _, n := range found {
		if ours[n.ID] {
			out = append(out, n)
		}
	}
	return out
}

func near(t *testing.T, f fixture, at [2]float64, limit int, after *restaurants.Cursor) ([]restaurants.Nearby, *restaurants.Cursor) {
	t.Helper()
	page, next, err := f.store.Near(context.Background(), at[0], at[1], limit, after)
	if err != nil {
		t.Fatalf("near: %v", err)
	}
	return page, next
}

// nearAll walks through every page. Earlier test runs leave restaurants at the same
// spots, so ours aren't guaranteed to be on the first page.
func nearAll(t *testing.T, f fixture, at [2]float64) []restaurants.Nearby {
	t.Helper()
	return nearAllPaged(t, f, at, 50)
}

func nearAllPaged(t *testing.T, f fixture, at [2]float64, pageSize int) []restaurants.Nearby {
	t.Helper()
	var all []restaurants.Nearby
	var cursor *restaurants.Cursor
	for pages := 0; ; pages++ {
		if pages > 1000 {
			t.Fatal("pagination never ended")
		}
		page, next := near(t, f, at, pageSize, cursor)
		all = append(all, page...)
		if next == nil {
			return all
		}
		cursor = next
	}
}

func TestNearDistanceAndDeliveryZones(t *testing.T) {
	f := setup(t)
	owner := f.newOwner(t)
	moda := f.newRestaurant(t, owner, modaParki, 3000)     // ~1.2 km from the pier
	taksimSmall := f.newRestaurant(t, owner, taksim, 3000) // ~6 km away, delivers 3 km
	taksimBig := f.newRestaurant(t, owner, taksim, 10000)  // ~6 km away, delivers 10 km
	ours := map[uuid.UUID]bool{moda.ID: true, taksimSmall.ID: true, taksimBig.ID: true}

	got := onlyOurs(nearAll(t, f, kadikoyIskele), ours)

	if len(got) != 2 || got[0].ID != moda.ID || got[1].ID != taksimBig.ID {
		t.Fatalf("got %d of ours: %v; want Moda then the 10 km Taksim branch", len(got), names(got))
	}
	if d := got[0].DistanceM; d < 1150 || d > 1300 {
		t.Errorf("pier → Moda = %.0f m, want about 1.2 km", d)
	}
	if d := got[1].DistanceM; d < 5000 || d > 7000 {
		t.Errorf("pier → Taksim = %.0f m, want about 6 km", d)
	}
}

func TestNearPagesReturnEachRestaurantOnce(t *testing.T) {
	f := setup(t)
	owner := f.newOwner(t)

	// A random spot in Istanbul, so other runs' restaurants are unlikely to be here.
	center := [2]float64{40.85 + rand.Float64()*0.6, 28.10 + rand.Float64()*1.6}
	ours := map[uuid.UUID]bool{}
	for i := range 7 {
		at := [2]float64{center[0] + float64(i)*0.001, center[1]} // ~110 m apart
		ours[f.newRestaurant(t, owner, at, 2000).ID] = true
	}
	// Two at exactly the same spot: same distance, so the ID decides their order.
	ours[f.newRestaurant(t, owner, center, 2000).ID] = true

	got := onlyOurs(nearAllPaged(t, f, center, 3), ours)
	seen := map[uuid.UUID]bool{}
	for i, n := range got {
		if seen[n.ID] {
			t.Fatalf("%s appeared on two pages", n.ID)
		}
		seen[n.ID] = true
		if i > 0 && n.DistanceM < got[i-1].DistanceM {
			t.Fatalf("results out of order at position %d", i)
		}
	}
	if len(seen) != len(ours) {
		t.Fatalf("found %d of our %d restaurants across all pages", len(seen), len(ours))
	}
}

func TestCursorRoundTrip(t *testing.T) {
	c := restaurants.Cursor{DistanceM: 1234.5678901234, ID: uuid.Must(uuid.NewV7())}
	got, err := restaurants.DecodeCursor(c.Encode())
	if err != nil || got != c {
		t.Fatalf("round trip = %+v, %v; want %+v", got, err, c)
	}
	for _, bad := range []string{"", "not base64!", "e30"} { // e30 is "{}"
		if _, err := restaurants.DecodeCursor(bad); err == nil {
			t.Errorf("DecodeCursor(%q) accepted", bad)
		}
	}
}

func names(ns []restaurants.Nearby) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.ID.String()
	}
	return out
}
