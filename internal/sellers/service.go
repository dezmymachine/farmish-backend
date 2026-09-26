package sellers

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/audit"
	"github.com/dezmymachine/farmish-backend/internal/crypto"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/geo"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

// idNumberPattern matches the API contract: 4-30 ASCII letters/digits/dashes.
var idNumberPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// ClaimSetter mirrors verification into Firebase custom claims. The database
// flag is authoritative; claim updates are best-effort, after the commit.
type ClaimSetter interface {
	SetClaim(ctx context.Context, uid, key string, value any) error
}

// Service reads and writes seller profiles.
type Service struct {
	pool   *pgxpool.Pool
	crypto *crypto.Crypter
	claims ClaimSetter
}

// New returns a Service. Claims may be nil only in tests that never verify.
func New(pool *pgxpool.Pool, c *crypto.Crypter, claims ClaimSetter) *Service {
	return &Service{pool: pool, crypto: c, claims: claims}
}

// GetMine returns the caller's own profile, or ErrNotFound when they never
// created one.
func (s *Service) GetMine(ctx context.Context, userID uuid.UUID) (Profile, error) {
	row, err := db.New(s.pool).GetSellerProfile(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, fmt.Errorf("%w: user %s", ErrNotFound, userID)
	}
	if err != nil {
		return Profile{}, fmt.Errorf("get seller profile: %w", err)
	}
	return fromRow(row), nil
}

// UpsertMine creates the caller's profile or edits it. Editing non-identity
// fields never changes the verification status; submitting (or changing)
// idType/idNumber encrypts the number, stores last4, moves the profile to
// pending (revoking a verification), and writes an audit event, all in one
// transaction.
func (s *Service) UpsertMine(ctx context.Context, userID uuid.UUID, in ProfileInput) (Profile, error) {
	whatsapp, err := s.validateInput(in)
	if err != nil {
		return Profile{}, err
	}

	var out Profile
	var revokedUID string
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		existing, err := q.GetSellerProfileForUpdate(ctx, userID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get seller profile: %w", err)
		}
		found := err == nil

		row, err := q.UpsertSellerProfile(ctx, db.UpsertSellerProfileParams{
			UserID: userID, BusinessName: in.BusinessName, Region: in.Region,
			District: in.District, Bio: in.Bio, ShowPhone: in.ShowPhone,
			ShowWhatsapp: in.ShowWhatsapp, WhatsappE164: whatsapp,
		})
		if err != nil {
			return fmt.Errorf("upsert seller profile: %w", err)
		}

		if in.IDType == nil {
			out = fromRow(row)
			return nil
		}
		if found && sameIdentity(s.crypto, existing, *in.IDType, *in.IDNumber) {
			out = fromRow(row)
			return nil
		}
		enc, err := s.crypto.Encrypt([]byte(*in.IDNumber))
		if err != nil {
			return fmt.Errorf("encrypt id number: %w", err)
		}
		last4 := (*in.IDNumber)[len(*in.IDNumber)-4:]
		row, err = q.SetSellerIdentity(ctx, db.SetSellerIdentityParams{
			UserID: userID, IDType: in.IDType, IDNumberEnc: &enc, IDNumberLast4: &last4,
		})
		if err != nil {
			return fmt.Errorf("set seller identity: %w", err)
		}
		previous := StatusUnverified
		if found {
			previous = existing.VerificationStatus
		}
		if err := audit.Record(ctx, tx, audit.Event{
			ActorID: &userID, Action: "seller.identity_submitted",
			TargetType: "seller", TargetID: userID.String(),
			Metadata: map[string]any{"previous_status": previous},
		}); err != nil {
			return err
		}
		if found && existing.VerificationStatus == StatusVerified {
			u, err := users.New(tx).SetSellerVerified(ctx, userID, false)
			if err != nil {
				return fmt.Errorf("revoke seller verification: %w", err)
			}
			revokedUID = u.FirebaseUID
		}
		out = fromRow(row)
		return nil
	})
	if err != nil {
		return Profile{}, err
	}
	if revokedUID != "" {
		s.setClaimBestEffort(ctx, revokedUID, false)
	}
	return out, nil
}

// GetPublic returns the safe projection of a seller, or ErrNotFound when the
// profile doesn't exist. It never touches id_number_enc.
func (s *Service) GetPublic(ctx context.Context, userID uuid.UUID) (PublicProfile, error) {
	row, err := db.New(s.pool).GetPublicSeller(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PublicProfile{}, fmt.Errorf("%w: user %s", ErrNotFound, userID)
	}
	if err != nil {
		return PublicProfile{}, fmt.Errorf("get public seller: %w", err)
	}
	return PublicProfile{
		UserID: userID, BusinessName: row.BusinessName,
		Region: row.Region, District: row.District, Bio: row.Bio,
		Verified: row.VerificationStatus == StatusVerified, MemberSince: row.CreatedAt,
	}, nil
}

// ListByStatus returns the admin review queue for one status, newest
// submissions last (oldest first), with the total for pagination.
func (s *Service) ListByStatus(ctx context.Context, status string, limit, offset int) ([]AdminProfile, int64, error) {
	var verr validation.Error
	switch status {
	case StatusUnverified, StatusPending, StatusVerified, StatusRejected:
	default:
		verr.Add("status", "must be one of unverified, pending, verified, rejected")
	}
	if limit < 1 || limit > 50 {
		verr.Add("limit", "must be between 1 and 50")
	}
	if offset < 0 {
		verr.Add("page", "must be at least 1")
	}
	if err := verr.OrNil(); err != nil {
		return nil, 0, err
	}
	q := db.New(s.pool)
	total, err := q.CountSellerProfilesByStatus(ctx, status)
	if err != nil {
		return nil, 0, fmt.Errorf("count seller profiles: %w", err)
	}
	rows, err := q.ListSellerProfilesByStatus(ctx, db.ListSellerProfilesByStatusParams{
		VerificationStatus: status, Limit: int32(limit), Offset: int32(offset),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list seller profiles: %w", err)
	}
	out := make([]AdminProfile, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromListRow(r))
	}
	return out, total, nil
}

// Verify records an admin verification decision. Only a pending profile may
// be decided (else ErrInvalidTransition). The status change, the
// users.seller_verified flag and the audit event commit in one transaction;
// the Firebase claim follows best-effort after the commit.
func (s *Service) Verify(ctx context.Context, adminID, targetID uuid.UUID, decision, reason string) (AdminProfile, error) {
	var verr validation.Error
	switch decision {
	case DecisionApprove, DecisionReject:
	default:
		verr.Add("decision", "must be approve or reject")
	}
	reason = strings.TrimSpace(reason)
	if decision == DecisionReject && (utf8.RuneCountInString(reason) < 1 || utf8.RuneCountInString(reason) > 500) {
		verr.Add("reason", "is required for a rejection (1-500 characters)")
	}
	if err := verr.OrNil(); err != nil {
		return AdminProfile{}, err
	}

	var out AdminProfile
	var claimUID string
	var claimValue bool
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.GetSellerProfileForUpdate(ctx, targetID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: user %s", ErrNotFound, targetID)
		}
		if err != nil {
			return fmt.Errorf("get seller profile: %w", err)
		}
		if row.VerificationStatus != StatusPending {
			return fmt.Errorf("%w: profile is %s", ErrInvalidTransition, row.VerificationStatus)
		}
		status := StatusVerified
		claimValue = true
		var rejection *string
		action := "seller.verify"
		if decision == DecisionReject {
			status = StatusRejected
			claimValue = false
			rejection = &reason
			action = "seller.reject"
		}
		row, err = q.SetSellerVerification(ctx, db.SetSellerVerificationParams{
			UserID: targetID, VerificationStatus: status,
			ReviewedBy: pgUUID(adminID), RejectionReason: rejection,
		})
		if err != nil {
			return fmt.Errorf("set seller verification: %w", err)
		}
		u, err := users.New(tx).SetSellerVerified(ctx, targetID, claimValue)
		if err != nil {
			return fmt.Errorf("set seller verified flag: %w", err)
		}
		claimUID = u.FirebaseUID
		if err := audit.Record(ctx, tx, audit.Event{
			ActorID: &adminID, Action: action,
			TargetType: "seller", TargetID: targetID.String(),
			Metadata: map[string]any{"previous_status": StatusPending},
		}); err != nil {
			return err
		}
		out, err = s.adminView(ctx, tx, targetID)
		return err
	})
	if err != nil {
		return AdminProfile{}, err
	}
	s.setClaimBestEffort(ctx, claimUID, claimValue)
	return out, nil
}

// setClaimBestEffort mirrors verification into Firebase after the commit.
// The database is authoritative: a claim failure is logged and the 200 stands.
func (s *Service) setClaimBestEffort(ctx context.Context, uid string, verified bool) {
	if s.claims == nil {
		return
	}
	if err := s.claims.SetClaim(ctx, uid, "seller_verified", verified); err != nil {
		logger.FromContext(ctx).Warn("seller claim update failed; database remains authoritative",
			"error", err.Error(), "verified", verified)
	}
}

// adminView loads the review-queue view of one profile.
func (s *Service) adminView(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (AdminProfile, error) {
	row, err := db.New(tx).GetSellerProfile(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminProfile{}, fmt.Errorf("%w: user %s", ErrNotFound, userID)
	}
	if err != nil {
		return AdminProfile{}, fmt.Errorf("get seller profile: %w", err)
	}
	u, err := users.New(tx).Get(ctx, userID)
	if err != nil {
		return AdminProfile{}, fmt.Errorf("get seller user: %w", err)
	}
	out := fromRow(row)
	return AdminProfile{Profile: out, DisplayName: u.DisplayName, Email: u.Email, Phone: u.Phone}, nil
}

// pgUUID lifts a UUID into a valid pgtype value for nullable uuid columns.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// validateInput checks the write model and returns the normalised WhatsApp
// number (nil when absent). Failures come back as *validation.Error with
// camelCase API field names.
func (s *Service) validateInput(in ProfileInput) (*string, error) {
	var verr validation.Error
	if n := utf8.RuneCountInString(in.BusinessName); n < 2 || n > 120 {
		verr.Add("businessName", "must be 2-120 characters")
	}
	if !geo.IsRegion(in.Region) {
		verr.Add("region", "must be one of the 16 regions")
	}
	if n := utf8.RuneCountInString(in.District); n < 2 || n > 80 {
		verr.Add("district", "must be 2-80 characters")
	}
	if in.Bio != nil && utf8.RuneCountInString(*in.Bio) > 1000 {
		verr.Add("bio", "must be at most 1000 characters")
	}
	var whatsapp *string
	if in.Whatsapp != nil && strings.TrimSpace(*in.Whatsapp) != "" {
		norm, err := geo.NormalizeGhanaPhone(*in.Whatsapp)
		if err != nil {
			verr.Add("whatsapp", "must be a Ghana phone number (+233 followed by 9 digits)")
		} else {
			whatsapp = &norm
		}
	}
	if (in.IDType == nil) != (in.IDNumber == nil) {
		verr.Add("idNumber", "idType and idNumber must be given together")
	}
	if in.IDType != nil {
		switch *in.IDType {
		case IDGhanaCard, IDPassport, IDVotersID, IDDriversLicense:
		default:
			verr.Add("idType", "must be one of ghana_card, passport, voters_id, drivers_license")
		}
	}
	if in.IDNumber != nil {
		n := len(*in.IDNumber)
		if n < 4 || n > 30 || !idNumberPattern.MatchString(*in.IDNumber) {
			verr.Add("idNumber", "must be 4-30 letters, digits or dashes")
		}
	}
	if err := verr.OrNil(); err != nil {
		return nil, err
	}
	return whatsapp, nil
}

// sameIdentity reports whether the submitted ID matches the stored one, by
// decrypting and comparing in constant time. An undecryptable stored value
// counts as different, forcing a re-submission.
func sameIdentity(c *crypto.Crypter, stored db.SellerProfile, idType, idNumber string) bool {
	if stored.IDType == nil || stored.IDNumberEnc == nil || *stored.IDType != idType {
		return false
	}
	plain, err := c.Decrypt(*stored.IDNumberEnc)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(plain, []byte(idNumber)) == 1
}

func fromRow(r db.SellerProfile) Profile {
	var reviewedBy *uuid.UUID
	if r.ReviewedBy.Valid {
		id := uuid.UUID(r.ReviewedBy.Bytes)
		reviewedBy = &id
	}
	return Profile{
		UserID: r.UserID, BusinessName: r.BusinessName, Region: r.Region,
		District: r.District, Bio: r.Bio, ShowPhone: r.ShowPhone,
		ShowWhatsapp: r.ShowWhatsapp, Whatsapp: r.WhatsappE164,
		VerificationStatus: r.VerificationStatus, IDType: r.IDType,
		IDNumberLast4: r.IDNumberLast4, SubmittedAt: r.SubmittedAt,
		ReviewedAt: r.ReviewedAt, ReviewedBy: reviewedBy,
		RejectionReason: r.RejectionReason, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func fromListRow(r db.ListSellerProfilesByStatusRow) AdminProfile {
	var reviewedBy *uuid.UUID
	if r.ReviewedBy.Valid {
		id := uuid.UUID(r.ReviewedBy.Bytes)
		reviewedBy = &id
	}
	return AdminProfile{
		Profile: Profile{
			UserID: r.UserID, BusinessName: r.BusinessName, Region: r.Region,
			District: r.District, Bio: r.Bio, ShowPhone: r.ShowPhone,
			ShowWhatsapp: r.ShowWhatsapp, Whatsapp: r.WhatsappE164,
			VerificationStatus: r.VerificationStatus, IDType: r.IDType,
			IDNumberLast4: r.IDNumberLast4, SubmittedAt: r.SubmittedAt,
			ReviewedAt: r.ReviewedAt, ReviewedBy: reviewedBy,
			RejectionReason: r.RejectionReason, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		},
		DisplayName: r.DisplayName, Email: r.Email, Phone: r.PhoneE164,
	}
}
