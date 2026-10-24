package menus_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/menus"
)

// A pide built in memory: no database needed, since the validator is pure.
//
//	Boy        pick exactly 1   Küçük +0, Büyük +40 TL
//	Ekstralar  pick up to 2     Kaşar +15 TL, Yumurta +10 TL, Sucuk +25 TL, Pastırma +40 TL (off)
//	İçecek     pick 1 to 2      Ayran +20 TL, Şalgam +25 TL
var (
	size, extras, drink          = uuid.New(), uuid.New(), uuid.New()
	small, large                 = uuid.New(), uuid.New()
	cheese, egg, sucuk, pastirma = uuid.New(), uuid.New(), uuid.New(), uuid.New()
	ayran, salgam                = uuid.New(), uuid.New()
	elsewhere                    = uuid.New() // a choice of some other dish
)

func pide() menus.MenuItem {
	group := func(id uuid.UUID, name string, min, max int, choices ...menus.OptionChoice) menus.MenuOptionGroup {
		return menus.MenuOptionGroup{
			OptionGroup: menus.OptionGroup{ID: id, Name: name, MinPicks: min, MaxPicks: max},
			Choices:     choices,
		}
	}
	choice := func(id uuid.UUID, name string, extra int, available bool) menus.OptionChoice {
		return menus.OptionChoice{ID: id, Name: name, ExtraPriceKurus: extra, Available: available}
	}
	return menus.MenuItem{
		Item: menus.Item{ID: uuid.New(), Name: "Kıymalı Pide", PriceKurus: 22000, Available: true},
		OptionGroups: []menus.MenuOptionGroup{
			group(size, "Boy", 1, 1, choice(small, "Küçük", 0, true), choice(large, "Büyük", 4000, true)),
			group(extras, "Ekstralar", 0, 2, choice(cheese, "Kaşar", 1500, true), choice(egg, "Yumurta", 1000, true), choice(sucuk, "Sucuk", 2500, true),
				choice(pastirma, "Pastırma", 4000, false)),
			group(drink, "İçecek", 1, 2, choice(ayran, "Ayran", 2000, true), choice(salgam, "Şalgam", 2500, true)),
		},
	}
}

func TestValidateChoices(t *testing.T) {
	tests := []struct {
		name   string
		picks  []uuid.UUID
		extra  int                   // for valid picks
		order  []uuid.UUID           // for valid picks: choice IDs in the order returned
		code   menus.ChoiceErrorCode // for invalid picks
		group  uuid.UUID             // for invalid picks: the group the error points at
		choice uuid.UUID             // for invalid picks: the choice it points at
	}{
		// Valid
		{name: "only the required groups", picks: ids(small, ayran), extra: 2000, order: ids(small, ayran)},
		{name: "everything allowed", picks: ids(large, cheese, sucuk, ayran, salgam), extra: 4000 + 1500 + 2500 + 2000 + 2500,
			order: ids(large, cheese, sucuk, ayran, salgam)},
		{name: "sent order doesn't matter", picks: ids(salgam, sucuk, large), extra: 2500 + 2500 + 4000, order: ids(large, sucuk, salgam)},

		// Counts
		{name: "required group left empty", picks: ids(ayran), code: menus.TooFewPicks, group: size},
		{name: "nothing picked at all", picks: nil, code: menus.TooFewPicks, group: size},
		{name: "two sizes", picks: ids(small, large, ayran), code: menus.TooManyPicks, group: size},
		{name: "three extras, max 2", picks: ids(small, cheese, sucuk, egg, ayran), code: menus.TooManyPicks, group: extras},
		{name: "first failing group wins", picks: ids(small, large), code: menus.TooManyPicks, group: size},
		{name: "later group too few", picks: ids(small, cheese), code: menus.TooFewPicks, group: drink},

		// Choices
		{name: "another dish's choice", picks: ids(small, ayran, elsewhere), code: menus.UnknownChoice, choice: elsewhere},
		{name: "same choice twice", picks: ids(small, small, ayran), code: menus.DuplicateChoice, group: size, choice: small},
		{name: "switched off", picks: ids(small, pastirma, ayran), code: menus.ChoiceUnavailable, group: extras, choice: pastirma},
		{name: "unavailable beats counts", picks: ids(pastirma), code: menus.ChoiceUnavailable, group: extras, choice: pastirma},
		{name: "unknown beats counts", picks: ids(elsewhere), code: menus.UnknownChoice, choice: elsewhere},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sel, err := menus.ValidateChoices(pide(), tt.picks)

			if tt.code == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				if sel.ExtraKurus != tt.extra {
					t.Errorf("extra = %d kuruş, want %d", sel.ExtraKurus, tt.extra)
				}
				var got []uuid.UUID
				for _, p := range sel.Picks {
					got = append(got, p.Choice.ID)
				}
				if !slices.Equal(got, tt.order) {
					t.Errorf("picks came back in the wrong order")
				}
				return
			}

			var ce *menus.ChoiceError
			if !errors.As(err, &ce) {
				t.Fatalf("err = %v, want a *ChoiceError with code %s", err, tt.code)
			}
			if ce.Code != tt.code || ce.GroupID != tt.group || ce.ChoiceID != tt.choice {
				t.Errorf("got code %s group %v choice %v; want %s %v %v (%s)",
					ce.Code, ce.GroupID, ce.ChoiceID, tt.code, tt.group, tt.choice, ce.Message)
			}
		})
	}
}

func TestValidateChoicesMessages(t *testing.T) {
	tests := []struct {
		picks []uuid.UUID
		want  string
	}{
		{ids(ayran), "Boy: pick exactly 1"},
		{ids(small, cheese), "İçecek: pick 1 to 2"},
		{ids(small, ayran, cheese, sucuk, uuid.Nil), "choice 00000000-0000-0000-0000-000000000000 is not an option of Kıymalı Pide"},
		{ids(small, pastirma, ayran), "Ekstralar: Pastırma is not available right now"},
		{ids(small, small, ayran), "Boy: Küçük is picked more than once"},
		{ids(small, cheese, sucuk, egg, ayran), "Ekstralar: pick up to 2"},
	}
	for _, tt := range tests {
		_, err := menus.ValidateChoices(pide(), tt.picks)
		if err == nil || err.Error() != tt.want {
			t.Errorf("message = %v, want %q", err, tt.want)
		}
	}
}

func TestValidateChoicesWithoutOptions(t *testing.T) {
	plain := menus.MenuItem{Item: menus.Item{Name: "Ayran"}, OptionGroups: []menus.MenuOptionGroup{}}
	sel, err := menus.ValidateChoices(plain, nil)
	if err != nil || sel.ExtraKurus != 0 || sel.Picks == nil || len(sel.Picks) != 0 {
		t.Fatalf("plain item, no picks: sel = %+v, err = %v; want empty and valid", sel, err)
	}
	var ce *menus.ChoiceError
	if _, err := menus.ValidateChoices(plain, ids(cheese)); !errors.As(err, &ce) || ce.Code != menus.UnknownChoice {
		t.Fatalf("plain item with a pick: err = %v, want unknown_choice", err)
	}
}

func ids(v ...uuid.UUID) []uuid.UUID { return v }
