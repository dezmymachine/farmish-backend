// Command admin runs operator tasks against the configured database and
// Firebase project.
//
//	admin grant-admin  --email you@example.com | --uid <firebase-uid>
//	admin revoke-admin --email you@example.com | --uid <firebase-uid>
//
// The user must have signed in at least once (so their users row exists).
// users.role is updated first (it's authoritative), then the Firebase role
// claim is mirrored; re-running is safe.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/users"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "admin:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: admin grant-admin|revoke-admin --email E | --uid U")
	}
	var role string
	switch args[0] {
	case "grant-admin":
		role = users.RoleAdmin
	case "revoke-admin":
		role = users.RoleUser
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}

	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	email := fs.String("email", "", "email of the account")
	uid := fs.String("uid", "", "Firebase UID of the account")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if (*email == "") == (*uid == "") {
		return errors.New("pass exactly one of --email or --uid")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.Open(ctx, cfg.DB)
	if err != nil {
		return err
	}
	defer pool.Close()
	fb, err := auth.NewFirebase(ctx, cfg.Firebase)
	if err != nil {
		return err
	}
	svc := users.New(pool)

	u, err := find(ctx, svc, *email, *uid)
	if err != nil {
		return err
	}
	u, err = svc.SetRole(ctx, u.ID, role, fb)
	if err != nil {
		return err
	}
	fmt.Printf("ok: user %s (firebase uid %s) now has role %q\n", u.ID, u.FirebaseUID, u.Role)
	return nil
}

func find(ctx context.Context, svc *users.Service, email, uid string) (users.User, error) {
	if uid != "" {
		u, err := svc.GetByFirebaseUID(ctx, uid)
		if errors.Is(err, users.ErrNotFound) {
			return users.User{}, fmt.Errorf("no user with firebase uid %q (they must sign in once first)", uid)
		}
		return u, err
	}
	matches, err := svc.ListByEmail(ctx, email)
	if err != nil {
		return users.User{}, err
	}
	switch len(matches) {
	case 0:
		return users.User{}, fmt.Errorf("no user with email %q (they must sign in once first)", email)
	case 1:
		return matches[0], nil
	default:
		uids := make([]string, len(matches))
		for i, m := range matches {
			uids[i] = m.FirebaseUID + " (" + m.SignupMethod + ")"
		}
		return users.User{}, fmt.Errorf("%d accounts share email %q; pass --uid, one of: %s",
			len(matches), email, strings.Join(uids, ", "))
	}
}
