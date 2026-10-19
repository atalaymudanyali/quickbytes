package restaurants

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Nearby is a restaurant plus how far it is from the search point.
type Nearby struct {
	Restaurant
	DistanceM float64 `json:"distance_m"`
}

// Cursor marks where the previous page ended: the last restaurant's distance and ID.
// The next page starts right after it. Sorting by (distance, id) instead of distance
// alone gives every restaurant a unique position, even two at the same spot.
type Cursor struct {
	DistanceM float64   `json:"d"`
	ID        uuid.UUID `json:"id"`
}

var ErrBadCursor = errors.New("invalid cursor")

// Encode turns the cursor into an opaque string for the client to send back as is.
func (c Cursor) Encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func DecodeCursor(s string) (Cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, ErrBadCursor
	}
	var c Cursor
	if err := json.Unmarshal(b, &c); err != nil || c.ID == uuid.Nil {
		return Cursor{}, ErrBadCursor
	}
	return c, nil
}

// Near returns up to limit restaurants whose delivery zone contains the point,
// closest first, starting after the cursor (nil for the first page). The second
// return value is the cursor for the next page, or nil when there are no more.
func (s *Store) Near(ctx context.Context, lat, lng float64, limit int, after *Cursor) ([]Nearby, *Cursor, error) {
	var afterDist *float64
	var afterID *uuid.UUID
	if after != nil {
		afterDist, afterID = &after.DistanceM, &after.ID
	}

	// Two distance checks, on purpose:
	//  1. ST_DWithin(location, here, MaxDeliveryRadiusM) uses a fixed 10 km, so the
	//     GiST index can find the candidates without looking at every row.
	//  2. ST_DWithin(location, here, delivery_radius_m) then keeps only restaurants
	//     whose own zone reaches the customer. On its own it couldn't use the index,
	//     because the distance changes from row to row.
	// One extra row is fetched to find out whether another page exists.
	rows, err := s.pool.Query(ctx, `
		WITH here AS (
			SELECT `+point("$1::float8", "$2::float8")+` AS p
		)
		SELECT `+columns+`, ST_Distance(location, here.p) AS distance_m
		FROM restaurants, here
		WHERE ST_DWithin(location, here.p, $3)
		  AND ST_DWithin(location, here.p, delivery_radius_m)
		  AND ($4::float8 IS NULL OR (ST_Distance(location, here.p), id) > ($4::float8, $5::uuid))
		ORDER BY distance_m, id
		LIMIT $6`,
		lat, lng, MaxDeliveryRadiusM, afterDist, afterID, limit+1,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("search restaurants: %w", err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Nearby, error) {
		var n Nearby
		err := row.Scan(&n.ID, &n.OwnerProfileID, &n.Name, &n.Address, &n.Lat, &n.Lng,
			&n.DeliveryRadiusM, &n.AcceptingOrders, &n.CreatedAt, &n.UpdatedAt, &n.DistanceM)
		return n, err
	})
	if err != nil {
		return nil, nil, fmt.Errorf("read restaurants: %w", err)
	}

	if len(found) <= limit {
		return found, nil, nil
	}
	page := found[:limit]
	last := page[len(page)-1]
	return page, &Cursor{DistanceM: last.DistanceM, ID: last.ID}, nil
}
