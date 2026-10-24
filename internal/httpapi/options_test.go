package httpapi

import (
	"net/http"
	"testing"

	"github.com/atalaymudanyali/quickbytes/internal/menus"
)

// optionFixture is a menu with one dish, ready for option calls.
type optionFixture struct {
	menuFixture
	item menus.Item
}

func newOptionFixture(t *testing.T) optionFixture {
	t.Helper()
	f := newMenuFixture(t)
	pides := decode[menus.Category](t, send(t, f.h, http.MethodPost, f.base+"/categories", f.owner, `{"name": "Pideler"}`), http.StatusCreated)
	pide := decode[menus.Item](t, send(t, f.h, http.MethodPost, f.base+"/items", f.owner,
		`{"category_id": "`+pides.ID.String()+`", "name": "Kıymalı Pide", "price_kurus": 22000}`), http.StatusCreated)
	return optionFixture{menuFixture: f, item: pide}
}

func (f optionFixture) groupsPath() string {
	return f.base + "/items/" + f.item.ID.String() + "/option-groups"
}

func TestOwnerManagesOptions(t *testing.T) {
	f := newOptionFixture(t)
	etagBefore := send(t, f.h, http.MethodGet, f.base, "", "").Header().Get("ETag")

	// "Size: pick exactly 1", with a free and a priced choice.
	size := decode[menus.OptionGroup](t, send(t, f.h, http.MethodPost, f.groupsPath(), f.owner,
		`{"name": "  Boy  ", "min_picks": 1, "max_picks": 1}`), http.StatusCreated)
	if size.Name != "Boy" || size.ItemID != f.item.ID || size.Position != 1 {
		t.Fatalf("created group = %+v", size)
	}
	choicesPath := f.base + "/option-groups/" + size.ID.String() + "/choices"
	small := decode[menus.OptionChoice](t, send(t, f.h, http.MethodPost, choicesPath, f.owner, `{"name": "Küçük"}`), http.StatusCreated)
	large := decode[menus.OptionChoice](t, send(t, f.h, http.MethodPost, choicesPath, f.owner,
		`{"name": "Büyük", "extra_price_kurus": 4000}`), http.StatusCreated)
	if small.ExtraPriceKurus != 0 || !small.Available || large.ExtraPriceKurus != 4000 || large.Position != 2 {
		t.Fatalf("created choices = %+v, %+v", small, large)
	}

	// The public menu shows it, under a new ETag.
	rec := send(t, f.h, http.MethodGet, f.base, "", "")
	m := decode[menus.Menu](t, rec, http.StatusOK)
	if rec.Header().Get("ETag") == etagBefore {
		t.Error("adding options didn't change the menu's ETag")
	}
	groups := m.Categories[0].Items[0].OptionGroups
	if len(groups) != 1 || groups[0].ID != size.ID || len(groups[0].Choices) != 2 || groups[0].Choices[1].ID != large.ID {
		t.Fatalf("public menu options = %+v", groups)
	}

	// Edits.
	groupPath := f.base + "/option-groups/" + size.ID.String()
	renamed := decode[menus.OptionGroup](t, send(t, f.h, http.MethodPatch, groupPath, f.owner, `{"name": "Porsiyon"}`), http.StatusOK)
	if renamed.Name != "Porsiyon" || renamed.MinPicks != 1 || renamed.MaxPicks != 1 {
		t.Fatalf("renamed group = %+v", renamed)
	}
	largePath := f.base + "/option-choices/" + large.ID.String()
	off := decode[menus.OptionChoice](t, send(t, f.h, http.MethodPatch, largePath, f.owner, `{"available": false, "extra_price_kurus": 4500}`), http.StatusOK)
	if off.Available || off.ExtraPriceKurus != 4500 || off.Name != "Büyük" {
		t.Fatalf("updated choice = %+v", off)
	}

	// Deletes: 204 with no body, then 404 the second time.
	if rec := send(t, f.h, http.MethodDelete, largePath, f.owner, ""); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("delete choice: %d %q, want 204 and no body", rec.Code, rec.Body)
	}
	if rec := send(t, f.h, http.MethodDelete, largePath, f.owner, ""); rec.Code != http.StatusNotFound || decodeError(t, rec).Code != "option_choice_not_found" {
		t.Fatalf("delete choice again: %d %s, want 404 option_choice_not_found", rec.Code, rec.Body)
	}
	etagBeforeDelete := send(t, f.h, http.MethodGet, f.base, "", "").Header().Get("ETag")
	if rec := send(t, f.h, http.MethodDelete, groupPath, f.owner, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete group: %d %s, want 204", rec.Code, rec.Body)
	}
	rec = send(t, f.h, http.MethodGet, f.base, "", "")
	m = decode[menus.Menu](t, rec, http.StatusOK)
	if len(m.Categories[0].Items[0].OptionGroups) != 0 || rec.Header().Get("ETag") == etagBeforeDelete {
		t.Fatalf("after deleting the group: options %+v, ETag %s (was %s)",
			m.Categories[0].Items[0].OptionGroups, rec.Header().Get("ETag"), etagBeforeDelete)
	}
	if rec := send(t, f.h, http.MethodDelete, groupPath, f.owner, ""); rec.Code != http.StatusNotFound || decodeError(t, rec).Code != "option_group_not_found" {
		t.Fatalf("delete group again: %d %s, want 404 option_group_not_found", rec.Code, rec.Body)
	}
}

func TestOptionValidation(t *testing.T) {
	f := newOptionFixture(t)
	extras := decode[menus.OptionGroup](t, send(t, f.h, http.MethodPost, f.groupsPath(), f.owner,
		`{"name": "Ekstralar", "min_picks": 0, "max_picks": 3}`), http.StatusCreated)
	groupPath := f.base + "/option-groups/" + extras.ID.String()
	choicesPath := groupPath + "/choices"
	cheese := decode[menus.OptionChoice](t, send(t, f.h, http.MethodPost, choicesPath, f.owner,
		`{"name": "Kaşar", "extra_price_kurus": 1500}`), http.StatusCreated)
	choicePath := f.base + "/option-choices/" + cheese.ID.String()

	cases := []struct {
		name, method, path, body, field string
	}{
		{"group without picks", http.MethodPost, f.groupsPath(), `{"name": "Boy"}`, "min_picks"},
		{"group without name", http.MethodPost, f.groupsPath(), `{"min_picks": 0, "max_picks": 1}`, "name"},
		{"max_picks too high", http.MethodPost, f.groupsPath(), `{"name": "Boy", "min_picks": 0, "max_picks": 25}`, "max_picks"},
		{"max_picks zero", http.MethodPost, f.groupsPath(), `{"name": "Boy", "min_picks": 0, "max_picks": 0}`, "max_picks"},
		{"min above max, both sent", http.MethodPost, f.groupsPath(), `{"name": "Boy", "min_picks": 2, "max_picks": 1}`, "min_picks"},
		{"min above the stored max", http.MethodPatch, groupPath, `{"min_picks": 5}`, "min_picks"},
		{"empty group patch", http.MethodPatch, groupPath, `{}`, "body"},
		{"blank group name", http.MethodPatch, groupPath, `{"name": "   "}`, "name"},
		{"choice without name", http.MethodPost, choicesPath, `{"extra_price_kurus": 100}`, "name"},
		{"negative extra price", http.MethodPost, choicesPath, `{"name": "Sucuk", "extra_price_kurus": -100}`, "extra_price_kurus"},
		{"available on create", http.MethodPost, choicesPath, `{"name": "Sucuk", "available": false}`, "available"},
		{"empty choice patch", http.MethodPatch, choicePath, `{}`, "body"},
		{"extra price too high", http.MethodPatch, choicePath, `{"extra_price_kurus": 10000001}`, "extra_price_kurus"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := send(t, f.h, c.method, c.path, f.owner, c.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body: %s", rec.Code, rec.Body)
			}
			if _, ok := decodeError(t, rec).Fields[c.field]; !ok {
				t.Errorf("no error on %q: %s", c.field, rec.Body)
			}
		})
	}
}

func TestOptionsOfOtherRestaurantsAreNotFound(t *testing.T) {
	f := newOptionFixture(t)
	size := decode[menus.OptionGroup](t, send(t, f.h, http.MethodPost, f.groupsPath(), f.owner,
		`{"name": "Boy", "min_picks": 1, "max_picks": 1}`), http.StatusCreated)
	small := decode[menus.OptionChoice](t, send(t, f.h, http.MethodPost, f.base+"/option-groups/"+size.ID.String()+"/choices", f.owner,
		`{"name": "Küçük"}`), http.StatusCreated)

	// Another owner, using their own restaurant in the URL with f's IDs.
	other := newMenuFixtureWith(t, f.h)
	cases := []struct {
		name, method, path, body, code string
	}{
		{"group on their item", http.MethodPost, other.base + "/items/" + f.item.ID.String() + "/option-groups",
			`{"name": "x", "min_picks": 0, "max_picks": 1}`, "item_not_found"},
		{"edit their group", http.MethodPatch, other.base + "/option-groups/" + size.ID.String(), `{"name": "x"}`, "option_group_not_found"},
		{"delete their group", http.MethodDelete, other.base + "/option-groups/" + size.ID.String(), "", "option_group_not_found"},
		{"choice in their group", http.MethodPost, other.base + "/option-groups/" + size.ID.String() + "/choices", `{"name": "x"}`, "option_group_not_found"},
		{"edit their choice", http.MethodPatch, other.base + "/option-choices/" + small.ID.String(), `{"name": "x"}`, "option_choice_not_found"},
		{"delete their choice", http.MethodDelete, other.base + "/option-choices/" + small.ID.String(), "", "option_choice_not_found"},
		// f's restaurant in the URL, but the other owner's token.
		{"their restaurant", http.MethodDelete, f.base + "/option-groups/" + size.ID.String(), "", "restaurant_not_found"},
		{"not a UUID", http.MethodPatch, f.base + "/option-choices/nope", `{"name": "x"}`, "option_choice_not_found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			token := other.owner
			if c.name == "not a UUID" {
				token = f.owner
			}
			rec := send(t, f.h, c.method, c.path, token, c.body)
			if rec.Code != http.StatusNotFound || decodeError(t, rec).Code != c.code {
				t.Fatalf("status = %d, body %s; want 404 %s", rec.Code, rec.Body, c.code)
			}
		})
	}

	// And nothing changed.
	m := decode[menus.Menu](t, send(t, f.h, http.MethodGet, f.base, "", ""), http.StatusOK)
	groups := m.Categories[0].Items[0].OptionGroups
	if len(groups) != 1 || groups[0].Name != "Boy" || len(groups[0].Choices) != 1 || groups[0].Choices[0].Name != "Küçük" {
		t.Fatalf("options after the attempts = %+v", groups)
	}
}
