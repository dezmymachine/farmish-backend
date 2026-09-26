package users_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

func countUsers(t *testing.T, pool *pgxpool.Pool, uid string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE firebase_uid = $1`, uid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestResolve_CreatesThenReuses(t *testing.T) {
	pool := dbtest.Pool(t)
	svc := users.New(pool)
	ctx := context.Background()

	id := auth.Identity{UID: "uid-email", Email: "Ama@Farmish.test", Provider: "password", Name: "  Ama Mensah  "}
	u, err := svc.Resolve(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if u.SignupMethod != auth.SignupEmail || u.Role != users.RoleUser || u.IsAdmin() || u.SellerVerified ||
		*u.Email != "Ama@Farmish.test" || u.Phone != nil || *u.DisplayName != "Ama Mensah" {
		t.Errorf("created user = %+v", u)
	}

	again, err := svc.Resolve(ctx, id)
	if err != nil || again.ID != u.ID || !again.UpdatedAt.Equal(u.UpdatedAt) {
		t.Errorf("second resolve: %+v, err %v (want same row, no write)", again, err)
	}
	if n := countUsers(t, pool, "uid-email"); n != 1 {
		t.Errorf("%d rows", n)
	}
}

func TestResolve_Phone(t *testing.T) {
	svc := users.New(dbtest.Pool(t))
	u, err := svc.Resolve(context.Background(), auth.Identity{UID: "uid-phone", Phone: "+233241234567", Provider: "phone"})
	if err != nil {
		t.Fatal(err)
	}
	if u.SignupMethod != auth.SignupPhone || *u.Phone != "+233241234567" || u.Email != nil || u.DisplayName != nil {
		t.Errorf("phone user = %+v", u)
	}
}

func TestResolve_SyncsFirebaseOwnedFields(t *testing.T) {
	svc := users.New(dbtest.Pool(t))
	ctx := context.Background()
	u, err := svc.Resolve(ctx, auth.Identity{UID: "uid-sync", Email: "old@farmish.test", Provider: "google.com"})
	if err != nil {
		t.Fatal(err)
	}
	synced, err := svc.Resolve(ctx, auth.Identity{UID: "uid-sync", Email: "new@farmish.test", EmailVerified: true, Provider: "google.com"})
	if err != nil {
		t.Fatal(err)
	}
	if synced.ID != u.ID || *synced.Email != "new@farmish.test" || !synced.EmailVerified || synced.SignupMethod != auth.SignupSocial {
		t.Errorf("synced = %+v", synced)
	}
}

func TestResolve_ConcurrentFirstSignIn(t *testing.T) {
	pool := dbtest.Pool(t)
	svc := users.New(pool)
	id := auth.Identity{UID: "uid-race", Email: "race@farmish.test", Provider: "password"}

	const n = 10
	ids := make([]string, n)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, err := svc.Resolve(context.Background(), id)
			if err != nil {
				errs <- err
				return
			}
			ids[i] = u.ID.String()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for _, got := range ids {
		if got != ids[0] {
			t.Fatalf("resolved different users: %v", ids)
		}
	}
	if c := countUsers(t, pool, "uid-race"); c != 1 {
		t.Errorf("%d rows for one account", c)
	}
}

func TestResolve_RejectsUnsupportedProviders(t *testing.T) {
	pool := dbtest.Pool(t)
	svc := users.New(pool)
	for _, p := range []string{"custom", "anonymous", ""} {
		_, err := svc.Resolve(context.Background(), auth.Identity{UID: "uid-" + p, Provider: p})
		if !errors.Is(err, users.ErrUnsupportedProvider) {
			t.Errorf("provider %q: err = %v", p, err)
		}
		if n := countUsers(t, pool, "uid-"+p); n != 0 {
			t.Errorf("provider %q created a row", p)
		}
	}
}

func TestResolve_LongIdPNameIsClamped(t *testing.T) {
	svc := users.New(dbtest.Pool(t))
	long := strings.Repeat("é", 100)
	u, err := svc.Resolve(context.Background(), auth.Identity{UID: "uid-long", Provider: "google.com", Name: long})
	if err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(*u.DisplayName)); got != users.MaxDisplayNameLen {
		t.Errorf("display name has %d runes", got)
	}
}

func TestUpdateDisplayName(t *testing.T) {
	svc := users.New(dbtest.Pool(t))
	ctx := context.Background()
	u, err := svc.Resolve(ctx, auth.Identity{UID: "uid-name", Provider: "phone", Phone: "+233200000000"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.UpdateDisplayName(ctx, u.ID, "  Kofi  ")
	if err != nil || *got.DisplayName != "Kofi" || !got.UpdatedAt.After(u.UpdatedAt) {
		t.Errorf("update: %+v, err %v", got, err)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("a", 81)} {
		if _, err := svc.UpdateDisplayName(ctx, u.ID, bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

type fakeClaims struct {
	calls []string
	err   error
}

func (f *fakeClaims) SetRoleClaim(_ context.Context, uid, role string) error {
	f.calls = append(f.calls, uid+"="+role)
	return f.err
}

func TestSetRole(t *testing.T) {
	svc := users.New(dbtest.Pool(t))
	ctx := context.Background()
	u, err := svc.Resolve(ctx, auth.Identity{UID: "uid-role", Provider: "password", Email: "r@farmish.test"})
	if err != nil {
		t.Fatal(err)
	}

	claims := &fakeClaims{}
	got, err := svc.SetRole(ctx, u.ID, users.RoleAdmin, claims)
	if err != nil || !got.IsAdmin() || len(claims.calls) != 1 || claims.calls[0] != "uid-role=admin" {
		t.Fatalf("grant: %+v, calls %v, err %v", got, claims.calls, err)
	}

	// The DB role is authoritative: it's saved even if the claim sync fails.
	failing := &fakeClaims{err: errors.New("firebase down")}
	if _, err := svc.SetRole(ctx, u.ID, users.RoleUser, failing); err == nil {
		t.Fatal("expected claim sync error")
	}
	if cur, _ := svc.Get(ctx, u.ID); cur.Role != users.RoleUser {
		t.Errorf("role = %q after failed claim sync, want user", cur.Role)
	}

	if _, err := svc.SetRole(ctx, u.ID, "superuser", claims); err == nil {
		t.Error("accepted unknown role")
	}
}

func TestListByEmail(t *testing.T) {
	svc := users.New(dbtest.Pool(t))
	ctx := context.Background()
	for _, id := range []auth.Identity{
		{UID: "a", Email: "same@farmish.test", Provider: "password"},
		{UID: "b", Email: "SAME@farmish.test", Provider: "google.com"},
		{UID: "c", Email: "other@farmish.test", Provider: "password"},
	} {
		if _, err := svc.Resolve(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	got, err := svc.ListByEmail(ctx, "same@farmish.test")
	if err != nil || len(got) != 2 {
		t.Errorf("got %d users (citext match), err %v", len(got), err)
	}
}
