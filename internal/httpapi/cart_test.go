package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/atalaymudanyali/quickbytes/internal/carts"
	"github.com/atalaymudanyali/quickbytes/internal/menus"
)

// cartFixture is a restaurant (29,90 TL delivery, 300 TL minimum) selling Adana
// (450 TL; Boy: Normal, or Büyük for +80 TL; pick exactly 1) and Ayran (40 TL).
type cartFixture struct {
	menuFixture
	restaurantPath     string
	adana, ayran       menus.Item
	boy, normal, buyuk string // IDs
}

func newCartFixture(t *testing.T) cartFixture {
	t.Helper()
	return newCartFixtureWith(t, newAuthTestHandler(t))
}

func newCartFixtureWith(t *testing.T, h http.Handler) cartFixture {
	t.Helper()
	f := cartFixture{menuFixture: newMenuFixtureWith(t, h)}
	f.restaurantPath = strings.TrimSuffix(f.base, "/menu")
	if rec := send(t, f.h, http.MethodPatch, f.restaurantPath, f.owner,
		`{"delivery_fee_kurus": 2990, "min_basket_kurus": 30000}`); rec.Code != http.StatusOK {
		t.Fatalf("set delivery terms: %d %s", rec.Code, rec.Body)
	}
	cat := decode[menus.Category](t, send(t, f.h, http.MethodPost, f.base+"/categories", f.owner, `{"name": "Kebaplar"}`), http.StatusCreated)
	f.adana = decode[menus.Item](t, send(t, f.h, http.MethodPost, f.base+"/items", f.owner,
		`{"category_id": "`+cat.ID.String()+`", "name": "Adana", "price_kurus": 45000}`), http.StatusCreated)
	f.ayran = decode[menus.Item](t, send(t, f.h, http.MethodPost, f.base+"/items", f.owner,
		`{"category_id": "`+cat.ID.String()+`", "name": "Ayran", "price_kurus": 4000}`), http.StatusCreated)
	g := decode[menus.OptionGroup](t, send(t, f.h, http.MethodPost, f.base+"/items/"+f.adana.ID.String()+"/option-groups", f.owner,
		`{"name": "Boy", "min_picks": 1, "max_picks": 1}`), http.StatusCreated)
	choice := func(body string) string {
		return decode[menus.OptionChoice](t, send(t, f.h, http.MethodPost, f.base+"/option-groups/"+g.ID.String()+"/choices", f.owner, body), http.StatusCreated).ID.String()
	}
	f.boy, f.normal, f.buyuk = g.ID.String(), choice(`{"name": "Normal"}`), choice(`{"name": "Büyük", "extra_price_kurus": 8000}`)
	return f
}

func (f cartFixture) add(t *testing.T, token, itemID string, qty int, choices ...string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"menu_item_id": "` + itemID + `", "quantity": ` + strconv.Itoa(qty) + `, "choice_ids": [` + quoteAll(choices) + `]}`
	return send(t, f.h, http.MethodPost, "/cart/items", token, body)
}

func quoteAll(ids []string) string {
	q := make([]string, len(ids))
	for i, id := range ids {
		q[i] = `"` + id + `"`
	}
	return strings.Join(q, ", ")
}

func TestCartNeedsALogin(t *testing.T) {
	h := newAuthTestHandler(t)
	if rec := send(t, h, http.MethodGet, "/cart", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestEmptyCart(t *testing.T) {
	h := newAuthTestHandler(t)
	c := decode[carts.Cart](t, send(t, h, http.MethodGet, "/cart", signUp(t, h).Tokens.AccessToken, ""), http.StatusOK)
	if c.Restaurant != nil || len(c.Items) != 0 || c.TotalKurus != 0 {
		t.Fatalf("got %+v, want an empty cart", c)
	}
}

func TestAddChangeRemove(t *testing.T) {
	f := newCartFixture(t)
	me := signUp(t, f.h).Tokens.AccessToken

	decode[carts.Cart](t, f.add(t, me, f.adana.ID.String(), 1, f.buyuk), http.StatusOK)
	decode[carts.Cart](t, f.add(t, me, f.adana.ID.String(), 1, f.buyuk), http.StatusOK) // merges
	c := decode[carts.Cart](t, f.add(t, me, f.ayran.ID.String(), 3), http.StatusOK)

	// 2 × (450 + 80 TL) + 3 × 40 TL = 1180 TL, plus 29,90 TL delivery.
	if len(c.Items) != 2 || c.Items[0].Quantity != 2 {
		t.Fatalf("lines = %+v, want Adana Büyük ×2 and Ayran ×3", c.Items)
	}
	if c.SubtotalKurus != 118000 || c.DeliveryFeeKurus != 2990 || c.TotalKurus != 120990 || c.BelowMinimum {
		t.Fatalf("subtotal %d, fee %d, total %d, below %v", c.SubtotalKurus, c.DeliveryFeeKurus, c.TotalKurus, c.BelowMinimum)
	}
	adanaLine, ayranLine := "/cart/items/"+c.Items[0].ID.String(), "/cart/items/"+c.Items[1].ID.String()

	t.Run("change quantity and choices", func(t *testing.T) {
		c := decode[carts.Cart](t, send(t, f.h, http.MethodPatch, adanaLine, me,
			`{"quantity": 1, "choice_ids": ["`+f.normal+`"]}`), http.StatusOK)
		if *c.Items[0].UnitPriceKurus != 45000 || c.Items[0].Options[0].ChoiceName != "Normal" {
			t.Fatalf("line = %+v, want Adana Normal at 450 TL", c.Items[0])
		}
	})

	t.Run("bad changes", func(t *testing.T) {
		for _, body := range []string{`{"quantity": 0}`, `{"quantity": 21}`, `{}`} {
			if rec := send(t, f.h, http.MethodPatch, ayranLine, me, body); rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s: status = %d, want 422", body, rec.Code)
			}
		}
		rec := send(t, f.h, http.MethodPatch, adanaLine, me, `{"choice_ids": []}`)
		if e := decodeError(t, rec); rec.Code != http.StatusUnprocessableEntity || e.Code != "too_few_picks" || e.GroupID.String() != f.boy {
			t.Fatalf("no size: status %d, error %+v; want 422 too_few_picks for the Boy group", rec.Code, e)
		}
	})

	t.Run("removing the last line empties the cart", func(t *testing.T) {
		decode[carts.Cart](t, send(t, f.h, http.MethodDelete, ayranLine, me, ""), http.StatusOK)
		c := decode[carts.Cart](t, send(t, f.h, http.MethodDelete, adanaLine, me, ""), http.StatusOK)
		if c.Restaurant != nil || len(c.Items) != 0 {
			t.Fatalf("got %+v, want an empty cart", c)
		}
		if rec := send(t, f.h, http.MethodDelete, adanaLine, me, ""); rec.Code != http.StatusNotFound {
			t.Fatalf("removing again: status = %d, want 404", rec.Code)
		}
	})
}

func TestAddRejects(t *testing.T) {
	f := newCartFixture(t)
	me := signUp(t, f.h).Tokens.AccessToken

	rec := f.add(t, me, f.adana.ID.String(), 1) // no size picked
	if e := decodeError(t, rec); rec.Code != http.StatusUnprocessableEntity || e.Code != "too_few_picks" || e.GroupID.String() != f.boy {
		t.Fatalf("status %d, error %+v; want 422 too_few_picks for the Boy group", rec.Code, e)
	}
	rec = f.add(t, me, "01a14bab-224e-7468-8920-42d59bbb55ee", 1)
	if rec.Code != http.StatusNotFound || decodeError(t, rec).Code != "item_not_found" {
		t.Fatalf("unknown dish: status = %d, want 404 item_not_found", rec.Code)
	}
	for _, body := range []string{`{"quantity": 1}`, `{"menu_item_id": "` + f.ayran.ID.String() + `", "quantity": 0}`} {
		if rec := send(t, f.h, http.MethodPost, "/cart/items", me, body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422", body, rec.Code)
		}
	}
	f.add(t, me, f.ayran.ID.String(), 15)
	if rec := f.add(t, me, f.ayran.ID.String(), 6); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("15 + 6 ayran: status = %d, want 422", rec.Code)
	}
}

func TestSecondRestaurantGets409(t *testing.T) {
	first, second := newCartFixture(t), newCartFixture(t)
	h := first.h
	second.h = h // same server, different restaurants
	me := signUp(t, h).Tokens.AccessToken
	first.add(t, me, first.ayran.ID.String(), 1)

	rec := second.add(t, me, second.ayran.ID.String(), 1)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if e := decodeError(t, rec); e.Code != "cart_restaurant_mismatch" || !strings.Contains(e.Message, "Çiya") {
		t.Fatalf("error = %+v, want cart_restaurant_mismatch naming the first restaurant", e)
	}

	if rec := send(t, h, http.MethodDelete, "/cart", me, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("clear: status = %d, want 204", rec.Code)
	}
	decode[carts.Cart](t, second.add(t, me, second.ayran.ID.String(), 1), http.StatusOK)
}

func TestMenuChangesShowInTheCartAtOnce(t *testing.T) {
	f := newCartFixture(t)
	me := signUp(t, f.h).Tokens.AccessToken
	f.add(t, me, f.ayran.ID.String(), 2)
	f.add(t, me, f.adana.ID.String(), 1, f.normal)
	decode[carts.Cart](t, send(t, f.h, http.MethodGet, "/cart", me, ""), http.StatusOK) // warm the menu cache

	if rec := send(t, f.h, http.MethodPatch, f.base+"/items/"+f.ayran.ID.String(), f.owner, `{"price_kurus": 5000}`); rec.Code != http.StatusOK {
		t.Fatalf("price change: %d %s", rec.Code, rec.Body)
	}
	if rec := send(t, f.h, http.MethodPatch, f.base+"/items/"+f.adana.ID.String(), f.owner, `{"available": false}`); rec.Code != http.StatusOK {
		t.Fatalf("switch off: %d %s", rec.Code, rec.Body)
	}

	c := decode[carts.Cart](t, send(t, f.h, http.MethodGet, "/cart", me, ""), http.StatusOK)
	if *c.Items[0].UnitPriceKurus != 5000 {
		t.Errorf("ayran = %d, want the new 5000", *c.Items[0].UnitPriceKurus)
	}
	if p := c.Items[1].Problem; len(c.Items) != 2 || p == nil || p.Code != carts.ItemUnavailable || c.Items[1].UnitPriceKurus != nil {
		t.Errorf("adana line = %+v, want kept, flagged item_unavailable, no price", c.Items[1])
	}
	if c.SubtotalKurus != 10000 || !c.BelowMinimum || c.KurusToMinimum != 20000 {
		t.Errorf("subtotal %d, below %v, missing %d; want 10000, true, 20000", c.SubtotalKurus, c.BelowMinimum, c.KurusToMinimum)
	}
}

func TestCustomersOnlySeeTheirOwnCart(t *testing.T) {
	f := newCartFixture(t)
	me, other := signUp(t, f.h).Tokens.AccessToken, signUp(t, f.h).Tokens.AccessToken
	c := decode[carts.Cart](t, f.add(t, me, f.ayran.ID.String(), 1), http.StatusOK)
	mine := "/cart/items/" + c.Items[0].ID.String()

	if theirs := decode[carts.Cart](t, send(t, f.h, http.MethodGet, "/cart", other, ""), http.StatusOK); len(theirs.Items) != 0 {
		t.Fatalf("other customer sees %d lines, want none", len(theirs.Items))
	}
	for _, rec := range []*httptest.ResponseRecorder{
		send(t, f.h, http.MethodPatch, mine, other, `{"quantity": 5}`),
		send(t, f.h, http.MethodDelete, mine, other, ""),
	} {
		if rec.Code != http.StatusNotFound || decodeError(t, rec).Code != "cart_item_not_found" {
			t.Fatalf("status = %d, want 404 cart_item_not_found", rec.Code)
		}
	}
	if c := decode[carts.Cart](t, send(t, f.h, http.MethodGet, "/cart", me, ""), http.StatusOK); c.Items[0].Quantity != 1 {
		t.Fatalf("my line changed to %d", c.Items[0].Quantity)
	}
}
