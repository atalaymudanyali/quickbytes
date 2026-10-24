package menus

import (
	"fmt"

	"github.com/google/uuid"
)

// Selection is a valid set of picks for one item.
type Selection struct {
	// Picks in menu display order (group by group), whatever order they were
	// sent in, so the same picks always come out the same.
	Picks []Pick
	// ExtraKurus is the sum of the picked choices' extra prices.
	ExtraKurus int
}

// Pick is one picked choice with the group it answers. The group comes along so
// an order can later copy both names ("Boy: Büyük").
type Pick struct {
	Group  OptionGroup
	Choice OptionChoice
}

// ChoiceErrorCode says what's wrong with a set of picks. The values are stable:
// clients may show their own message for each.
type ChoiceErrorCode string

const (
	UnknownChoice     ChoiceErrorCode = "unknown_choice"     // not a choice of this item
	DuplicateChoice   ChoiceErrorCode = "duplicate_choice"   // the same choice sent twice
	ChoiceUnavailable ChoiceErrorCode = "choice_unavailable" // switched off by the restaurant
	TooFewPicks       ChoiceErrorCode = "too_few_picks"      // fewer than the group's min_picks
	TooManyPicks      ChoiceErrorCode = "too_many_picks"     // more than the group's max_picks
)

// ChoiceError is the first problem found with a set of picks. GroupID is set for
// the count problems, ChoiceID for the others (and GroupID too, when the choice
// belongs to this item), so an app can scroll to the right place.
type ChoiceError struct {
	Code     ChoiceErrorCode
	GroupID  uuid.UUID
	ChoiceID uuid.UUID
	Message  string
}

func (e *ChoiceError) Error() string { return e.Message }

// ValidateChoices checks a customer's picks against an item's option rules and,
// if they're fine, returns them with their extra price. It's the one place these
// rules live: the cart and checkout both call it.
//
// It's pure: it looks only at its arguments, never at the database, so it can
// run against a cached menu, and the same inputs always give the same answer.
//
// Problems are checked in a fixed order and only the first is returned: each
// sent ID in turn (unknown, then sent twice, then unavailable), then each group's
// count in display order. The item's own availability is not checked here; the
// caller checks the item itself.
func ValidateChoices(item MenuItem, choiceIDs []uuid.UUID) (Selection, error) {
	// Where each of this item's choices lives: choice ID → (group, choice).
	type place struct{ g, c int }
	where := make(map[uuid.UUID]place)
	for gi, g := range item.OptionGroups {
		for ci, c := range g.Choices {
			where[c.ID] = place{gi, ci}
		}
	}

	picked := make(map[uuid.UUID]bool, len(choiceIDs))
	for _, id := range choiceIDs {
		p, ok := where[id]
		if !ok {
			return Selection{}, &ChoiceError{Code: UnknownChoice, ChoiceID: id,
				Message: fmt.Sprintf("choice %s is not an option of %s", id, item.Name)}
		}
		g := item.OptionGroups[p.g]
		c := g.Choices[p.c]
		if picked[id] {
			return Selection{}, &ChoiceError{Code: DuplicateChoice, GroupID: g.ID, ChoiceID: id,
				Message: fmt.Sprintf("%s: %s is picked more than once", g.Name, c.Name)}
		}
		if !c.Available {
			return Selection{}, &ChoiceError{Code: ChoiceUnavailable, GroupID: g.ID, ChoiceID: id,
				Message: fmt.Sprintf("%s: %s is not available right now", g.Name, c.Name)}
		}
		picked[id] = true
	}

	sel := Selection{Picks: []Pick{}}
	for _, g := range item.OptionGroups {
		n := 0
		for _, c := range g.Choices {
			if picked[c.ID] {
				n++
				sel.Picks = append(sel.Picks, Pick{Group: g.OptionGroup, Choice: c})
				sel.ExtraKurus += c.ExtraPriceKurus
			}
		}
		if n < g.MinPicks {
			return Selection{}, &ChoiceError{Code: TooFewPicks, GroupID: g.ID,
				Message: fmt.Sprintf("%s: %s", g.Name, picksRule(g.OptionGroup))}
		}
		if n > g.MaxPicks {
			return Selection{}, &ChoiceError{Code: TooManyPicks, GroupID: g.ID,
				Message: fmt.Sprintf("%s: %s", g.Name, picksRule(g.OptionGroup))}
		}
	}
	return sel, nil
}

// picksRule writes a group's rule the way a menu would: "pick exactly 1",
// "pick 1 to 3", "pick up to 2".
func picksRule(g OptionGroup) string {
	switch {
	case g.MinPicks == g.MaxPicks:
		return fmt.Sprintf("pick exactly %d", g.MinPicks)
	case g.MinPicks == 0:
		return fmt.Sprintf("pick up to %d", g.MaxPicks)
	default:
		return fmt.Sprintf("pick %d to %d", g.MinPicks, g.MaxPicks)
	}
}
