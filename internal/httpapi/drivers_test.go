package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atalaymudanyali/quickbytes/internal/cache"
	"github.com/atalaymudanyali/quickbytes/internal/drivers"
	"github.com/atalaymudanyali/quickbytes/internal/locations"
	"github.com/atalaymudanyali/quickbytes/internal/shiftkey"
	"github.com/atalaymudanyali/quickbytes/internal/testutil"
)

const testShiftKeySecret = "test-only-shift-secret-0123456789abcdef"

// signUpAs signs up a user with an extra profile and returns an access token that
// lists it.
func signUpAs(t *testing.T, h http.Handler, role string) string {
	t.Helper()
	acc := signUp(t, h)
	if rec := addProfile(t, h, acc.Tokens.AccessToken, role); rec.Code != http.StatusCreated {
		t.Fatalf("add %s profile: %d %s", role, rec.Code, rec.Body)
	}
	return refreshTokens(t, h, acc.Tokens.RefreshToken).AccessToken
}

func startShift(t *testing.T, h http.Handler, token string) startShiftResponse {
	t.Helper()
	rec := send(t, h, http.MethodPost, "/driver/shift/start", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("start shift: %d %s", rec.Code, rec.Body)
	}
	var got startShiftResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func saveDriver(t *testing.T, h http.Handler, token, vehicle string) drivers.Driver {
	t.Helper()
	rec := send(t, h, http.MethodPut, "/driver", token, `{"vehicle": "`+vehicle+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("save driver: %d %s", rec.Code, rec.Body)
	}
	var d drivers.Driver
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDriverRoutesNeedADriverProfile(t *testing.T) {
	h := newAuthTestHandler(t)
	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/driver", ""},
		{http.MethodPut, "/driver", `{"vehicle": "car"}`},
		{http.MethodPost, "/driver/shift/start", ""},
		{http.MethodPost, "/driver/shift/end", ""},
	}
	callers := map[string]string{
		"customer":   signUp(t, h).Tokens.AccessToken,
		"restaurant": signUpAs(t, h, "restaurant"),
	}
	for _, rt := range routes {
		if rec := send(t, h, rt.method, rt.path, "", rt.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no login: status = %d, want 401", rt.method, rt.path, rec.Code)
		}
		for who, token := range callers {
			rec := send(t, h, rt.method, rt.path, token, rt.body)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s as %s: status = %d, want 403", rt.method, rt.path, who, rec.Code)
			}
		}
	}
}

func TestDriverDetails(t *testing.T) {
	h := newAuthTestHandler(t)
	token := signUpAs(t, h, "driver")

	if rec := send(t, h, http.MethodGet, "/driver", token, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("before saving: status = %d, want 404", rec.Code)
	}
	for _, body := range []string{`{"vehicle": "skateboard"}`, `{"vehicle": ""}`, `{}`} {
		if rec := send(t, h, http.MethodPut, "/driver", token, body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422", body, rec.Code)
		}
	}

	if d := saveDriver(t, h, token, "bicycle"); d.Vehicle != drivers.VehicleBicycle || d.Status != drivers.StatusOffline {
		t.Fatalf("created %+v, want an offline driver on a bicycle", d)
	}
	saveDriver(t, h, token, "motorcycle")

	rec := send(t, h, http.MethodGet, "/driver", token, "")
	var got drivers.Driver
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	if got.Vehicle != drivers.VehicleMotorcycle {
		t.Fatalf("vehicle = %q, want the edit to stick", got.Vehicle)
	}
}

func TestShiftNeedsDriverDetails(t *testing.T) {
	h := newAuthTestHandler(t)
	token := signUpAs(t, h, "driver")
	for _, path := range []string{"/driver/shift/start", "/driver/shift/end"} {
		rec := send(t, h, http.MethodPost, path, token, "")
		if code := decodeError(t, rec).Code; rec.Code != http.StatusConflict || code != "driver_details_missing" {
			t.Errorf("%s: %d %q, want 409 driver_details_missing", path, rec.Code, code)
		}
	}
}

func TestStartAndEndShift(t *testing.T) {
	h := newAuthTestHandler(t)
	token := signUpAs(t, h, "driver")
	id := saveDriver(t, h, token, "motorcycle").ID

	first := startShift(t, h, token)
	if first.Driver.Status != drivers.StatusAvailable {
		t.Fatalf("status = %q, want available", first.Driver.Status)
	}
	if again := startShift(t, h, token); again.Driver.Status != drivers.StatusAvailable {
		t.Fatalf("second start: status = %q, want available", again.Driver.Status)
	}

	t.Run("the key signs messages the location service can check", func(t *testing.T) {
		msg := []byte("41.0370,28.9850,counter=1")
		sig := shiftkey.Sign(first.ShiftKey.Key, msg)
		now := time.Now()
		if err := shiftkey.Verify(testShiftKeySecret, id, first.ShiftKey.ExpiresAt, msg, sig, now); err != nil {
			t.Fatalf("verify: %v", err)
		}
		if err := shiftkey.Verify(testShiftKeySecret, id, first.ShiftKey.ExpiresAt, msg, sig, first.ShiftKey.ExpiresAt); !errors.Is(err, shiftkey.ErrExpired) {
			t.Fatalf("at expiry: err = %v, want ErrExpired", err)
		}
		if left := time.Until(first.ShiftKey.ExpiresAt); left < 11*time.Hour || left > 12*time.Hour {
			t.Fatalf("key lasts %v, want about the 12 hours the server was given", left)
		}
	})

	for range 2 { // ending twice is harmless
		rec := send(t, h, http.MethodPost, "/driver/shift/end", token, "")
		var d drivers.Driver
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("end: %d %s", rec.Code, rec.Body)
		}
		if d.Status != drivers.StatusOffline {
			t.Fatalf("status = %q, want offline", d.Status)
		}
	}
}

// onLiveMap reports whether a driver comes back from the helper dispatch will use. A
// driver stays there for 30 seconds of silence, so if one is gone at once, something
// took them off.
func onLiveMap(t *testing.T, store *locations.Store, id uuid.UUID) bool {
	t.Helper()
	found, err := store.Near(context.Background(), 41.0370, 28.9850, 1000, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range found {
		if f.DriverID == id {
			return true
		}
	}
	return false
}

func putOnLiveMap(t *testing.T, store *locations.Store, id uuid.UUID) {
	t.Helper()
	ping := locations.Ping{DriverID: id, Counter: 1, Lat: 41.0370, Lng: 28.9850}
	if _, err := store.Save(context.Background(), ping, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !onLiveMap(t, store, id) {
		t.Fatal("the driver isn't on the map to begin with")
	}
}

func TestEndShiftLeavesTheLiveMapAtOnce(t *testing.T) {
	srv, h := newAuthTestServer(t)
	store := locations.NewStore(testutil.Redis(t))
	srv.positions = store

	token := signUpAs(t, h, "driver")
	id := saveDriver(t, h, token, "motorcycle").ID
	startShift(t, h, token)
	putOnLiveMap(t, store, id)

	if rec := send(t, h, http.MethodPost, "/driver/shift/end", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("end: %d %s", rec.Code, rec.Body)
	}
	if onLiveMap(t, store, id) {
		t.Fatal("still on the map after ending the shift")
	}
}

// Refused because they're carrying food: the customer still needs to see them.
func TestEndShiftDuringADeliveryStaysOnTheLiveMap(t *testing.T) {
	srv, h := newAuthTestServer(t)
	store := locations.NewStore(testutil.Redis(t))
	srv.positions = store

	token := signUpAs(t, h, "driver")
	id := saveDriver(t, h, token, "motorcycle").ID
	if _, err := testutil.DB(t).Exec(context.Background(),
		`UPDATE drivers SET status = 'on_delivery' WHERE profile_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	putOnLiveMap(t, store, id)

	if rec := send(t, h, http.MethodPost, "/driver/shift/end", token, ""); rec.Code != http.StatusConflict {
		t.Fatalf("end: %d %s, want 409", rec.Code, rec.Body)
	}
	if !onLiveMap(t, store, id) {
		t.Fatal("left the map during a delivery")
	}
}

// Postgres decides who is free, so a shift ends even when the map can't hear about it.
func TestEndShiftWorksWithRedisDown(t *testing.T) {
	srv, h := newAuthTestServer(t)
	down, err := cache.Open("redis://127.0.0.1:1/0") // port 1: nothing listens there
	if err != nil {
		t.Fatal(err)
	}
	defer down.Close()
	srv.positions = locations.NewStore(down)

	token := signUpAs(t, h, "driver")
	saveDriver(t, h, token, "motorcycle")
	startShift(t, h, token)

	rec := send(t, h, http.MethodPost, "/driver/shift/end", token, "")
	var d drivers.Driver
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("end: %d %s", rec.Code, rec.Body)
	}
	if d.Status != drivers.StatusOffline {
		t.Fatalf("status = %q, want offline", d.Status)
	}
}

// The two services agree: a key from the API signs a packet the location service
// accepts, for the driver the API said it was for.
func TestShiftKeyWorksAtTheLocationService(t *testing.T) {
	h := newAuthTestHandler(t)
	token := signUpAs(t, h, "driver")
	id := saveDriver(t, h, token, "motorcycle").ID
	shift := startShift(t, h, token)

	packet := locations.Seal(locations.Ping{
		DriverID:     shift.Driver.ID,
		KeyExpiresAt: shift.ShiftKey.ExpiresAt,
		Counter:      1,
		Lat:          41.0370,
		Lng:          28.9850,
	}, shift.ShiftKey.Key)
	got, err := locations.Open(testShiftKeySecret, packet, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.DriverID != id {
		t.Fatalf("driver = %s, want %s", got.DriverID, id)
	}
}

func TestOnDelivery(t *testing.T) {
	h := newAuthTestHandler(t)
	token := signUpAs(t, h, "driver")
	id := saveDriver(t, h, token, "motorcycle").ID
	// Nothing sets on_delivery until drivers can accept orders (#38), so set it here.
	if _, err := testutil.DB(t).Exec(context.Background(),
		`UPDATE drivers SET status = 'on_delivery' WHERE profile_id = $1`, id); err != nil {
		t.Fatal(err)
	}

	rec := send(t, h, http.MethodPost, "/driver/shift/end", token, "")
	if code := decodeError(t, rec).Code; rec.Code != http.StatusConflict || code != "on_delivery" {
		t.Fatalf("end: %d %q, want 409 on_delivery", rec.Code, code)
	}
	// Starting still gives a fresh key, but must not make them look free.
	if got := startShift(t, h, token); got.Driver.Status != drivers.StatusOnDelivery || len(got.ShiftKey.Key) == 0 {
		t.Fatalf("start: %+v, want on_delivery with a key", got)
	}
}
