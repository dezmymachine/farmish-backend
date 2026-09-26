// Package users maps verified Firebase identities to rows in the users table.
package users

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/db"
)

// Roles stored in users.role. users.role is authoritative for authorization.
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// MaxDisplayNameLen matches the users.display_name CHECK and the API spec.
const MaxDisplayNameLen = 80

var (
	// ErrUnsupportedProvider means the token came from a sign-in method we
	// don't accept (custom tokens, anonymous).
	ErrUnsupportedProvider = errors.New("unsupported sign-in provider")
	ErrNotFound            = errors.New("user not found")
)

// User is a row of the users table.
type User struct {
	ID             uuid.UUID
	FirebaseUID    string
	SignupMethod   string
	Email          *string
	EmailVerified  bool
	Phone          *string
	DisplayName    *string
	Role           string
	SellerVerified bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// IsAdmin reports whether the user has the admin role.
func (u User) IsAdmin() bool { return u.Role == RoleAdmin }

func fromRow(r db.User) User {
	return User{
		ID: r.ID, FirebaseUID: r.FirebaseUid, SignupMethod: r.SignupMethod,
		Email: r.Email, EmailVerified: r.EmailVerified, Phone: r.PhoneE164,
		DisplayName: r.DisplayName, Role: r.Role, SellerVerified: r.SellerVerified,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// Service reads and writes users.
type Service struct {
	q db.Querier
}

// New returns a Service over a pool or transaction.
func New(conn db.DBTX) *Service {
	return &Service{q: db.New(conn)}
}

// Resolve returns the user for a verified identity, creating the row on first
// sign-in and mirroring Firebase-owned fields (email, phone) when they change.
// Concurrent first requests for the same account create exactly one row.
func (s *Service) Resolve(ctx context.Context, id auth.Identity) (User, error) {
	method, ok := auth.SignupMethod(id.Provider)
	if !ok {
		return User{}, fmt.Errorf("%w: %q", ErrUnsupportedProvider, id.Provider)
	}

	row, err := s.q.GetUserByFirebaseUID(ctx, id.UID)
	if errors.Is(err, pgx.ErrNoRows) {
		row, err = s.q.InsertUser(ctx, db.InsertUserParams{
			FirebaseUid:   id.UID,
			SignupMethod:  method,
			Email:         optional(id.Email),
			EmailVerified: id.EmailVerified,
			PhoneE164:     optional(id.Phone),
			DisplayName:   optional(clampName(id.Name)),
		})
		if errors.Is(err, pgx.ErrNoRows) { // lost the insert race: the row exists now
			row, err = s.q.GetUserByFirebaseUID(ctx, id.UID)
		}
		if err != nil {
			return User{}, fmt.Errorf("create user: %w", err)
		}
		return fromRow(row), nil
	}
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}

	if !sameStr(row.Email, id.Email) || row.EmailVerified != id.EmailVerified || !sameStr(row.PhoneE164, id.Phone) {
		synced, err := s.q.SyncUserIdentity(ctx, db.SyncUserIdentityParams{
			ID: row.ID, Email: optional(id.Email), EmailVerified: id.EmailVerified, PhoneE164: optional(id.Phone),
		})
		switch {
		case err == nil:
			row = synced
		case !errors.Is(err, pgx.ErrNoRows): // no rows: a concurrent request already synced
			return User{}, fmt.Errorf("sync user identity: %w", err)
		}
	}
	return fromRow(row), nil
}

// Get returns a user by ID.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (User, error) {
	return s.one(s.q.GetUserByID(ctx, id))
}

// GetByFirebaseUID returns a user by Firebase UID.
func (s *Service) GetByFirebaseUID(ctx context.Context, uid string) (User, error) {
	return s.one(s.q.GetUserByFirebaseUID(ctx, uid))
}

// ListByEmail returns every account with this email (no account linking).
func (s *Service) ListByEmail(ctx context.Context, email string) ([]User, error) {
	rows, err := s.q.ListUsersByEmail(ctx, &email)
	if err != nil {
		return nil, fmt.Errorf("list users by email: %w", err)
	}
	out := make([]User, len(rows))
	for i, r := range rows {
		out[i] = fromRow(r)
	}
	return out, nil
}

// UpdateDisplayName trims and stores the display name.
func (s *Service) UpdateDisplayName(ctx context.Context, id uuid.UUID, name string) (User, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > MaxDisplayNameLen {
		return User{}, fmt.Errorf("display name must be 1-%d characters", MaxDisplayNameLen)
	}
	return s.one(s.q.UpdateUserDisplayName(ctx, db.UpdateUserDisplayNameParams{ID: id, DisplayName: &name}))
}

// SetSellerVerified flips the users.seller_verified flag. Sellers call it
// inside their verification transaction (see internal/sellers).
func (s *Service) SetSellerVerified(ctx context.Context, id uuid.UUID, verified bool) (User, error) {
	return s.one(s.q.SetUserSellerVerified(ctx, db.SetUserSellerVerifiedParams{ID: id, SellerVerified: verified}))
}

// SetRole updates users.role in the database, then mirrors it into the
// Firebase custom claim. The database write is authoritative; if the claim
// update fails the error is returned and re-running is safe.
func (s *Service) SetRole(ctx context.Context, id uuid.UUID, role string, claims auth.ClaimsSetter) (User, error) {
	if role != RoleUser && role != RoleAdmin {
		return User{}, fmt.Errorf("unknown role %q", role)
	}
	u, err := s.one(s.q.SetUserRole(ctx, db.SetUserRoleParams{ID: id, Role: role}))
	if err != nil {
		return User{}, err
	}
	if err := claims.SetRoleClaim(ctx, u.FirebaseUID, role); err != nil {
		return u, fmt.Errorf("role saved, but syncing the Firebase claim failed (re-run to retry): %w", err)
	}
	return u, nil
}

func (s *Service) one(row db.User, err error) (User, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	return fromRow(row), nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func sameStr(p *string, s string) bool {
	if p == nil {
		return s == ""
	}
	return *p == s
}

// clampName trims an IdP-provided name to what users.display_name accepts.
func clampName(name string) string {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) <= MaxDisplayNameLen {
		return name
	}
	return strings.TrimSpace(string([]rune(name)[:MaxDisplayNameLen]))
}

type ctxKey struct{}

// WithContext returns ctx carrying the authenticated user.
func WithContext(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// FromContext returns the authenticated user set by the auth middleware.
func FromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}
