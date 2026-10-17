package users_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/atalaymudanyali/quickbytes/internal/testutil"
	"github.com/atalaymudanyali/quickbytes/internal/users"
)

func TestCreateGivesCustomerProfile(t *testing.T) {
	ctx := context.Background()
	store := users.NewStore(testutil.DB(t))

	u, p, err := store.Create(ctx, testutil.UniqueEmail(), "hash", "Ayşe Yılmaz")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.Role != users.RoleCustomer || p.UserID != u.ID {
		t.Fatalf("profile = %+v, want a customer profile for the new user", p)
	}

	profiles, err := store.ListProfiles(ctx, u.ID)
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profiles = %v, err = %v; want exactly one", profiles, err)
	}
}

func TestEmailIsUniqueIgnoringCase(t *testing.T) {
	ctx := context.Background()
	store := users.NewStore(testutil.DB(t))
	email := testutil.UniqueEmail()

	if _, _, err := store.Create(ctx, email, "hash", "First"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, _, err := store.Create(ctx, strings.ToUpper(email), "hash", "Second")
	if !errors.Is(err, users.ErrEmailTaken) {
		t.Fatalf("err = %v, want ErrEmailTaken", err)
	}

	got, err := store.GetByEmail(ctx, strings.ToUpper(email))
	if err != nil || got.FullName != "First" {
		t.Fatalf("lookup by upper-case email = %+v, %v; want the first user", got, err)
	}
}

func TestAddProfileOncePerRole(t *testing.T) {
	ctx := context.Background()
	store := users.NewStore(testutil.DB(t))
	u, _, err := store.Create(ctx, testutil.UniqueEmail(), "hash", "Mehmet")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := store.AddProfile(ctx, u.ID, users.RoleDriver); err != nil {
		t.Fatalf("add driver profile: %v", err)
	}
	if _, err := store.AddProfile(ctx, u.ID, users.RoleDriver); !errors.Is(err, users.ErrProfileExists) {
		t.Fatalf("second driver profile err = %v, want ErrProfileExists", err)
	}
	if _, err := store.AddProfile(ctx, u.ID, users.RoleCustomer); !errors.Is(err, users.ErrProfileExists) {
		t.Fatalf("second customer profile err = %v, want ErrProfileExists", err)
	}
}

func TestGetMissingUser(t *testing.T) {
	store := users.NewStore(testutil.DB(t))
	if _, err := store.GetByEmail(context.Background(), testutil.UniqueEmail()); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
