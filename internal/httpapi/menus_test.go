package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/atalaymudanyali/quickbytes/internal/menus"
)

// menuFixture is an owner with one restaurant, ready for menu calls.
type menuFixture struct {
	h     http.Handler
	owner string // access token
	base  string // "/restaurants/<id>/menu"
	mine  string // "/me/restaurants/<id>/menu"
}

func newMenuFixture(t *testing.T) menuFixture {
	t.Helper()
	h := newAuthTestHandler(t)
	owner := ownerToken(t, h)
	r := createRestaurant(t, h, owner, ciyaBody)
	return menuFixture{
		h: h, owner: owner,
		base: "/restaurants/" + r.ID.String() + "/menu",
		mine: "/me/restaurants/" + r.ID.String() + "/menu",
	}
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder, want int) T {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, want, rec.Body)
	}
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func TestOwnerManagesMenu(t *testing.T) {
	f := newMenuFixture(t)

	kebabs := decode[menus.Category](t, send(t, f.h, http.MethodPost, f.base+"/categories", f.owner,
		`{"name": "  Kebaplar  "}`), http.StatusCreated)
	if kebabs.Name != "Kebaplar" {
		t.Errorf("name = %q, want it trimmed", kebabs.Name)
	}

	adana := decode[menus.Item](t, send(t, f.h, http.MethodPost, f.base+"/items", f.owner,
		`{"category_id": "`+kebabs.ID.String()+`", "name": "Adana Kebap", "description": "Acılı, közlenmiş biberle", "price_kurus": 32000}`),
		http.StatusCreated)
	if adana.PriceKurus != 32000 || !adana.Available {
		t.Fatalf("created item = %+v", adana)
	}

	itemPath := f.base + "/items/" + adana.ID.String()
	soldOut := decode[menus.Item](t, send(t, f.h, http.MethodPatch, itemPath, f.owner, `{"available": false}`), http.StatusOK)
	if soldOut.Available || soldOut.PriceKurus != 32000 {
		t.Fatalf("after sold out = %+v", soldOut)
	}

	archived := decode[menus.Item](t, send(t, f.h, http.MethodPatch, itemPath, f.owner, `{"archived": true}`), http.StatusOK)
	if archived.ArchivedAt == nil {
		t.Fatal("archived item has no archived_at")
	}

	// The owner still sees it, marked, so it can be restored.
	m := decode[menus.Menu](t, send(t, f.h, http.MethodGet, f.mine, f.owner, ""), http.StatusOK)
	if len(m.Categories) != 1 || len(m.Categories[0].Items) != 1 || m.Categories[0].Items[0].ArchivedAt == nil {
		t.Fatalf("owner menu = %+v; want the archived item, marked", m)
	}

	restored := decode[menus.Item](t, send(t, f.h, http.MethodPatch, itemPath, f.owner, `{"archived": false}`), http.StatusOK)
	if restored.ArchivedAt != nil {
		t.Fatal("restore didn't clear archived_at")
	}

	renamed := decode[menus.Category](t, send(t, f.h, http.MethodPatch, f.base+"/categories/"+kebabs.ID.String(), f.owner,
		`{"name": "Kebap ve Izgara"}`), http.StatusOK)
	if renamed.Name != "Kebap ve Izgara" {
		t.Fatalf("renamed = %+v", renamed)
	}
}

func TestMenuAccessRules(t *testing.T) {
	f := newMenuFixture(t)
	kebabs := decode[menus.Category](t, send(t, f.h, http.MethodPost, f.base+"/categories", f.owner, `{"name": "Kebaplar"}`), http.StatusCreated)
	stranger := ownerToken(t, f.h)

	t.Run("a customer can't manage menus", func(t *testing.T) {
		customer := signUp(t, f.h).Tokens.AccessToken
		if rec := send(t, f.h, http.MethodPost, f.base+"/categories", customer, `{"name": "x"}`); rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("another owner gets 404", func(t *testing.T) {
		for _, call := range []struct{ method, path, body string }{
			{http.MethodPost, f.base + "/categories", `{"name": "x"}`},
			{http.MethodPatch, f.base + "/categories/" + kebabs.ID.String(), `{"archived": true}`},
			{http.MethodGet, f.mine, ""},
		} {
			rec := send(t, f.h, call.method, call.path, stranger, call.body)
			if rec.Code != http.StatusNotFound || decodeError(t, rec).Code != "restaurant_not_found" {
				t.Errorf("%s %s: status = %d, want 404 restaurant_not_found", call.method, call.path, rec.Code)
			}
		}
	})

	t.Run("an item can't use another restaurant's category", func(t *testing.T) {
		other := newMenuFixture(t)
		theirs := decode[menus.Category](t, send(t, other.h, http.MethodPost, other.base+"/categories", other.owner, `{"name": "Pideler"}`), http.StatusCreated)
		rec := send(t, f.h, http.MethodPost, f.base+"/items", f.owner,
			`{"category_id": "`+theirs.ID.String()+`", "name": "Pide", "price_kurus": 15000}`)
		if rec.Code != http.StatusUnprocessableEntity || decodeError(t, rec).Fields["category_id"] == "" {
			t.Fatalf("status = %d; want 422 about category_id", rec.Code)
		}
	})
}

func TestMenuValidation(t *testing.T) {
	f := newMenuFixture(t)
	kebabs := decode[menus.Category](t, send(t, f.h, http.MethodPost, f.base+"/categories", f.owner, `{"name": "Kebaplar"}`), http.StatusCreated)
	cat := `"category_id": "` + kebabs.ID.String() + `"`

	for name, c := range map[string]struct{ method, path, body string }{
		"price zero":            {http.MethodPost, f.base + "/items", `{` + cat + `, "name": "x", "price_kurus": 0}`},
		"price over 100,000 TL": {http.MethodPost, f.base + "/items", `{` + cat + `, "name": "x", "price_kurus": 10000001}`},
		"price as decimal":      {http.MethodPost, f.base + "/items", `{` + cat + `, "name": "x", "price_kurus": 320.5}`},
		"missing fields":        {http.MethodPost, f.base + "/items", `{}`},
		"empty category name":   {http.MethodPost, f.base + "/categories", `{"name": "   "}`},
		"empty patch":           {http.MethodPatch, f.base + "/categories/" + kebabs.ID.String(), `{}`},
	} {
		t.Run(name, func(t *testing.T) {
			rec := send(t, f.h, c.method, c.path, f.owner, c.body)
			if rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 or 422; body: %s", rec.Code, rec.Body)
			}
		})
	}

	t.Run("unknown item", func(t *testing.T) {
		rec := send(t, f.h, http.MethodPatch, f.base+"/items/01a14bab-224e-7468-8920-42d59bbb55ee", f.owner, `{"available": false}`)
		if rec.Code != http.StatusNotFound || decodeError(t, rec).Code != "item_not_found" {
			t.Fatalf("status = %d, want 404 item_not_found", rec.Code)
		}
	})
}

func TestReorderEndpoints(t *testing.T) {
	f := newMenuFixture(t)
	soups := decode[menus.Category](t, send(t, f.h, http.MethodPost, f.base+"/categories", f.owner, `{"name": "Çorbalar"}`), http.StatusCreated)
	kebabs := decode[menus.Category](t, send(t, f.h, http.MethodPost, f.base+"/categories", f.owner, `{"name": "Kebaplar"}`), http.StatusCreated)
	addItem := func(name string) menus.Item {
		return decode[menus.Item](t, send(t, f.h, http.MethodPost, f.base+"/items", f.owner,
			`{"category_id": "`+kebabs.ID.String()+`", "name": "`+name+`", "price_kurus": 30000}`), http.StatusCreated)
	}
	adana, urfa := addItem("Adana"), addItem("Urfa")

	m := decode[menus.Menu](t, send(t, f.h, http.MethodPut, f.base+"/categories/order", f.owner,
		`{"category_ids": ["`+kebabs.ID.String()+`", "`+soups.ID.String()+`"]}`), http.StatusOK)
	if m.Categories[0].Name != "Kebaplar" {
		t.Fatalf("categories not reordered: first is %q", m.Categories[0].Name)
	}

	m = decode[menus.Menu](t, send(t, f.h, http.MethodPut, f.base+"/categories/"+kebabs.ID.String()+"/order", f.owner,
		`{"item_ids": ["`+urfa.ID.String()+`", "`+adana.ID.String()+`"]}`), http.StatusOK)
	if m.Categories[0].Items[0].Name != "Urfa" {
		t.Fatalf("items not reordered: first is %q", m.Categories[0].Items[0].Name)
	}

	t.Run("incomplete list", func(t *testing.T) {
		rec := send(t, f.h, http.MethodPut, f.base+"/categories/order", f.owner, `{"category_ids": ["`+kebabs.ID.String()+`"]}`)
		if rec.Code != http.StatusUnprocessableEntity || decodeError(t, rec).Fields["category_ids"] == "" {
			t.Fatalf("status = %d, want 422 about category_ids", rec.Code)
		}
	})

	t.Run("another owner", func(t *testing.T) {
		rec := send(t, f.h, http.MethodPut, f.base+"/categories/order", ownerToken(t, f.h),
			`{"category_ids": ["`+kebabs.ID.String()+`", "`+soups.ID.String()+`"]}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

func TestPublicMenu(t *testing.T) {
	f := newMenuFixture(t)
	public := f.base // "/restaurants/<id>/menu": the same address the owner writes under

	soups := decode[menus.Category](t, send(t, f.h, http.MethodPost, f.base+"/categories", f.owner, `{"name": "Çorbalar"}`), http.StatusCreated)
	winter := decode[menus.Category](t, send(t, f.h, http.MethodPost, f.base+"/categories", f.owner, `{"name": "Kış Menüsü"}`), http.StatusCreated)
	addItem := func(categoryID, name string) menus.Item {
		return decode[menus.Item](t, send(t, f.h, http.MethodPost, f.base+"/items", f.owner,
			`{"category_id": "`+categoryID+`", "name": "`+name+`", "price_kurus": 9000}`), http.StatusCreated)
	}
	lentil := addItem(soups.ID.String(), "Mercimek")
	ezogelin := addItem(soups.ID.String(), "Ezogelin")
	tarhana := addItem(soups.ID.String(), "Tarhana")
	addItem(winter.ID.String(), "Kelle Paça")

	send(t, f.h, http.MethodPatch, f.base+"/items/"+ezogelin.ID.String(), f.owner, `{"available": false}`)
	send(t, f.h, http.MethodPatch, f.base+"/items/"+tarhana.ID.String(), f.owner, `{"archived": true}`)
	send(t, f.h, http.MethodPatch, f.base+"/categories/"+winter.ID.String(), f.owner, `{"archived": true}`)

	// No token: browsing is public.
	m := decode[menus.Menu](t, send(t, f.h, http.MethodGet, public, "", ""), http.StatusOK)

	if len(m.Categories) != 1 || m.Categories[0].Name != "Çorbalar" {
		t.Fatalf("categories = %+v; want only Çorbalar (Kış Menüsü is archived)", m.Categories)
	}
	items := m.Categories[0].Items
	if len(items) != 2 || items[0].ID != lentil.ID || items[1].ID != ezogelin.ID {
		t.Fatalf("items = %+v; want Mercimek and Ezogelin (Tarhana is archived)", items)
	}
	if items[1].Available {
		t.Error("sold-out Ezogelin should be shown with available: false")
	}
	if body := send(t, f.h, http.MethodGet, public, "", "").Body.String(); strings.Contains(body, "archived_at") {
		t.Errorf("public menu mentions archived_at: %s", body)
	}

	for _, path := range []string{"/restaurants/01a14bab-224e-7468-8920-42d59bbb55ee/menu", "/restaurants/nope/menu"} {
		if rec := send(t, f.h, http.MethodGet, path, "", ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", path, rec.Code)
		}
	}
}

func TestPublicMenuIsCached(t *testing.T) {
	if os.Getenv("TEST_REDIS_URL") == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	f := newMenuFixture(t)
	soups := decode[menus.Category](t, send(t, f.h, http.MethodPost, f.base+"/categories", f.owner, `{"name": "Çorbalar"}`), http.StatusCreated)

	get := func() *httptest.ResponseRecorder { return send(t, f.h, http.MethodGet, f.base, "", "") }
	if got := get().Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("first read: X-Cache = %q, want MISS", got)
	}
	if got := get().Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("second read: X-Cache = %q, want HIT", got)
	}

	// A change through the API drops the copy, and the next read shows it.
	send(t, f.h, http.MethodPost, f.base+"/items", f.owner,
		`{"category_id": "`+soups.ID.String()+`", "name": "Mercimek", "price_kurus": 9000}`)
	rec := get()
	m := decode[menus.Menu](t, rec, http.StatusOK)
	if rec.Header().Get("X-Cache") != "MISS" || len(m.Categories[0].Items) != 1 {
		t.Fatalf("after adding a dish: X-Cache = %q, %d dishes; want MISS and the new dish",
			rec.Header().Get("X-Cache"), len(m.Categories[0].Items))
	}
}
