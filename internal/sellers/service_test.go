package sellers_test

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/crypto"
	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

func strptr(s string) *string { return &s }

func testCrypter(t *testing.T) *crypto.Crypter {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	c, err := crypto.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// mustUser creates a users row from a synthetic identity, without touching
// the emulator (for paths that never update Firebase claims).
func mustUser(t *testing.T, pool *pgxpool.Pool, uid string) users.User {
	t.Helper()
	u, err := users.New(pool).Resolve(context.Background(), auth.Identity{
		UID: uid, Email: uid + "@farmish.test", Provider: "password",
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// emulatorUser creates a real emulator account plus its users row, for paths
// that mirror verification into Firebase claims.
func emulatorUser(t *testing.T, pool *pgxpool.Pool, fb *auth.Firebase) (users.User, string) {
	t.Helper()
	ctx := context.Background()
	eu := authtest.EmailUser(t)
	id, err := fb.Verify(ctx, eu.Token)
	if err != nil {
		t.Fatal(err)
	}
	u, err := users.New(pool).Resolve(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return u, eu.Token
}

func validInput() sellers.ProfileInput {
	return sellers.ProfileInput{
		BusinessName: "Akosua Farms",
		Region:       "Ashanti",
		District:     "Kumasi Metro",
		Bio:          strptr("Maize and cassava."),
		ShowPhone:    true,
		Whatsapp:     strptr("0241234567"),
	}
}

func withID(in sellers.ProfileInput, idType, idNumber string) sellers.ProfileInput {
	in.IDType = strptr(idType)
	in.IDNumber = strptr(idNumber)
	return in
}

func TestSellerProfile_UpsertAndGet(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	s := sellers.New(pool, testCrypter(t), nil)
	u := mustUser(t, pool, "upsert-1")

	if _, err := s.GetMine(ctx, u.ID); !errors.Is(err, sellers.ErrNotFound) {
		t.Fatalf("GetMine before create = %v", err)
	}
	p, err := s.UpsertMine(ctx, u.ID, validInput())
	if err != nil {
		t.Fatal(err)
	}
	if p.VerificationStatus != sellers.StatusUnverified || p.IDType != nil ||
		p.SubmittedAt != nil || p.Whatsapp == nil || *p.Whatsapp != "+233241234567" {
		t.Errorf("created profile = %+v", p)
	}
	if got, err := s.GetMine(ctx, u.ID); err != nil || got.BusinessName != "Akosua Farms" {
		t.Errorf("GetMine = %+v, %v", got, err)
	}

	// Editing non-identity fields never changes the verification status.
	edit := validInput()
	edit.BusinessName = "Akosua Farms Ltd"
	edit.District = "Asokwa"
	edit.ShowPhone = false
	edit.Whatsapp = nil
	p, err = s.UpsertMine(ctx, u.ID, edit)
	if err != nil {
		t.Fatal(err)
	}
	if p.VerificationStatus != sellers.StatusUnverified || p.BusinessName != "Akosua Farms Ltd" ||
		p.District != "Asokwa" || p.ShowPhone || p.Whatsapp != nil {
		t.Errorf("edited profile = %+v", p)
	}
}

func TestSellerProfile_IdentitySubmissionSetsPending(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	c := testCrypter(t)
	s := sellers.New(pool, c, nil)
	u := mustUser(t, pool, "identity-1")

	p, err := s.UpsertMine(ctx, u.ID, withID(validInput(), "ghana_card", "GHA-123456789-0"))
	if err != nil {
		t.Fatal(err)
	}
	if p.VerificationStatus != sellers.StatusPending || p.SubmittedAt == nil ||
		p.IDType == nil || *p.IDType != "ghana_card" ||
		p.IDNumberLast4 == nil || *p.IDNumberLast4 != "89-0" {
		t.Errorf("submitted profile = %+v", p)
	}

	// Encrypted at rest: the raw column holds a v1 ciphertext, not plaintext.
	var enc *string
	if err := pool.QueryRow(ctx, `SELECT id_number_enc FROM seller_profiles WHERE user_id = $1`, u.ID).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if enc == nil || !strings.HasPrefix(*enc, "v1:") || strings.Contains(*enc, "GHA-123456789-0") {
		t.Errorf("id_number_enc = %v", enc)
	}
	if plain, err := c.Decrypt(*enc); err != nil || string(plain) != "GHA-123456789-0" {
		t.Errorf("decrypt = %q, %v", plain, err)
	}

	if got, err := users.New(pool).Get(ctx, u.ID); err != nil || got.SellerVerified {
		t.Errorf("users.seller_verified = %+v, %v", got, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'seller.identity_submitted' AND target_id = $1`,
		u.ID.String()).Scan(&n); err != nil || n != 1 {
		t.Errorf("identity_submitted events = %d, err %v", n, err)
	}
	if pub, err := s.GetPublic(ctx, u.ID); err != nil || pub.Verified || pub.MemberSince.IsZero() {
		t.Errorf("public = %+v, %v", pub, err)
	}
}

func TestSellerProfile_IdentityChangeRevokesVerification(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	fb := authtest.Firebase(t)
	s := sellers.New(pool, testCrypter(t), fb)
	seller, _ := emulatorUser(t, pool, fb)
	admin := mustUser(t, pool, "identity-admin")

	if _, err := s.UpsertMine(ctx, seller.ID, withID(validInput(), "ghana_card", "GHA-111111111-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(ctx, admin.ID, seller.ID, sellers.DecisionApprove, ""); err != nil {
		t.Fatal(err)
	}
	claims, _ := fb.CustomClaims(ctx, seller.FirebaseUID)
	if claims["seller_verified"] != true {
		t.Fatalf("claim after approve = %v", claims)
	}

	// A different ID re-submits: pending again, flag and claim revoked.
	p, err := s.UpsertMine(ctx, seller.ID, withID(validInput(), "passport", "P-222222222-2"))
	if err != nil {
		t.Fatal(err)
	}
	if p.VerificationStatus != sellers.StatusPending || p.IDNumberLast4 == nil || *p.IDNumberLast4 != "22-2" {
		t.Errorf("resubmitted profile = %+v", p)
	}
	if got, err := users.New(pool).Get(ctx, seller.ID); err != nil || got.SellerVerified {
		t.Errorf("users.seller_verified = %+v, %v", got, err)
	}
	claims, _ = fb.CustomClaims(ctx, seller.FirebaseUID)
	if claims["seller_verified"] != false {
		t.Errorf("claim after ID change = %v", claims)
	}

	// Re-submitting the same ID is not a change: no new audit event.
	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'seller.identity_submitted'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertMine(ctx, seller.ID, withID(validInput(), "passport", "P-222222222-2")); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'seller.identity_submitted'`).Scan(&after); err != nil || after != before {
		t.Errorf("identity_submitted events %d -> %d (want no new event)", before, after)
	}
}

func TestUpsertMine_Validation(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	s := sellers.New(pool, testCrypter(t), nil)
	u := mustUser(t, pool, "validation-1")

	bad := validInput()
	bad.Region = "Accra"
	onlyType := validInput()
	onlyType.IDType = strptr("ghana_card")
	onlyNumber := validInput()
	onlyNumber.IDNumber = strptr("GHA-1")
	badPhone := validInput()
	badPhone.Whatsapp = strptr("+2348012345678")
	badIDType := withID(validInput(), "nin", "GHA-1234")
	shortID := withID(validInput(), "ghana_card", "ab")

	for name, in := range map[string]sellers.ProfileInput{
		"bad region":        bad,
		"idType without idNumber": onlyType,
		"idNumber without idType": onlyNumber,
		"bad whatsapp":      badPhone,
		"bad idType":        badIDType,
		"short idNumber":    shortID,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.UpsertMine(ctx, u.ID, in)
			var verr *validation.Error
			if !errors.As(err, &verr) || len(verr.Fields) == 0 {
				t.Fatalf("err = %v, want *validation.Error with fields", err)
			}
		})
	}
}

func TestAdminVerify_ApproveAndClaim(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	fb := authtest.Firebase(t)
	s := sellers.New(pool, testCrypter(t), fb)
	seller, _ := emulatorUser(t, pool, fb)
	admin := mustUser(t, pool, "verify-admin")

	if _, err := s.UpsertMine(ctx, seller.ID, withID(validInput(), "voters_id", "V-999999999-9")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Verify(ctx, admin.ID, seller.ID, sellers.DecisionApprove, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.VerificationStatus != sellers.StatusVerified || got.ReviewedAt == nil ||
		got.ReviewedBy == nil || *got.ReviewedBy != admin.ID || got.RejectionReason != nil ||
		got.Email == nil {
		t.Errorf("approved profile = %+v", got)
	}
	if u, err := users.New(pool).Get(ctx, seller.ID); err != nil || !u.SellerVerified {
		t.Errorf("users.seller_verified = %+v, %v", u, err)
	}
	claims, err := fb.CustomClaims(ctx, seller.FirebaseUID)
	if err != nil || claims["seller_verified"] != true {
		t.Errorf("claim = %v, err %v", claims, err)
	}
	var meta string
	if err := pool.QueryRow(ctx, `SELECT metadata::text FROM audit_events WHERE action = 'seller.verify' AND target_id = $1`,
		seller.ID.String()).Scan(&meta); err != nil || !strings.Contains(meta, "pending") {
		t.Errorf("seller.verify audit = %q, err %v", meta, err)
	}

	// Reject path on a fresh submission.
	seller2, _ := emulatorUser(t, pool, fb)
	if _, err := s.UpsertMine(ctx, seller2.ID, withID(validInput(), "passport", "P-1")); err == nil {
		t.Fatal("short ID accepted")
	}
	if _, err := s.UpsertMine(ctx, seller2.ID, withID(validInput(), "passport", "P-12345")); err != nil {
		t.Fatal(err)
	}
	got, err = s.Verify(ctx, admin.ID, seller2.ID, sellers.DecisionReject, "Photo does not match")
	if err != nil {
		t.Fatal(err)
	}
	if got.VerificationStatus != sellers.StatusRejected || got.RejectionReason == nil ||
		*got.RejectionReason != "Photo does not match" {
		t.Errorf("rejected profile = %+v", got)
	}
	if u, err := users.New(pool).Get(ctx, seller2.ID); err != nil || u.SellerVerified {
		t.Errorf("users.seller_verified after reject = %+v, %v", u, err)
	}
	claims, _ = fb.CustomClaims(ctx, seller2.FirebaseUID)
	if claims["seller_verified"] != false {
		t.Errorf("claim after reject = %v", claims)
	}
}

func TestVerify_Guards(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	s := sellers.New(pool, testCrypter(t), nil)
	admin := mustUser(t, pool, "guards-admin")
	seller := mustUser(t, pool, "guards-seller")

	if _, err := s.Verify(ctx, admin.ID, seller.ID, sellers.DecisionApprove, ""); !errors.Is(err, sellers.ErrNotFound) {
		t.Errorf("missing profile: err = %v", err)
	}
	if _, err := s.UpsertMine(ctx, seller.ID, validInput()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(ctx, admin.ID, seller.ID, sellers.DecisionApprove, ""); !errors.Is(err, sellers.ErrInvalidTransition) {
		t.Errorf("unverified profile: err = %v", err)
	}
	if _, err := s.Verify(ctx, admin.ID, seller.ID, sellers.DecisionReject, "  "); err == nil {
		t.Error("reject without reason accepted")
	} else {
		var verr *validation.Error
		if !errors.As(err, &verr) {
			t.Errorf("reject without reason: err = %T %v", err, err)
		}
	}
	if _, err := s.Verify(ctx, admin.ID, seller.ID, sellers.DecisionApprove, ""); !errors.Is(err, sellers.ErrInvalidTransition) {
		t.Errorf("double decision: err = %v", err)
	}
}

func TestListByStatus(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	s := sellers.New(pool, testCrypter(t), nil)
	admin := mustUser(t, pool, "list-admin")

	for i, uid := range []string{"list-1", "list-2", "list-3"} {
		u := mustUser(t, pool, uid)
		in := withID(validInput(), "ghana_card", "GHA-00000000-"+string(rune('0'+i)))
		if _, err := s.UpsertMine(ctx, u.ID, in); err != nil {
			t.Fatal(err)
		}
	}
	u1, err := users.New(pool).GetByFirebaseUID(ctx, "list-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(ctx, admin.ID, u1.ID, sellers.DecisionApprove, ""); err != nil {
		t.Fatal(err)
	}

	items, total, err := s.ListByStatus(ctx, sellers.StatusPending, 20, 0)
	if err != nil || total != 2 || len(items) != 2 {
		t.Fatalf("pending: %d items, total %d, err %v", len(items), total, err)
	}
	if items[0].Email == nil {
		t.Error("admin view lacks owner email")
	}
	items, total, err = s.ListByStatus(ctx, sellers.StatusVerified, 20, 0)
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("verified: %d items, total %d, err %v", len(items), total, err)
	}
	if items[0].IDNumberLast4 == nil {
		t.Error("admin view lacks last4")
	}
	if _, _, err := s.ListByStatus(ctx, "bogus", 20, 0); err == nil {
		t.Error("bogus status accepted")
	}
	if _, _, err := s.ListByStatus(ctx, sellers.StatusPending, 51, 0); err == nil {
		t.Error("limit 51 accepted")
	}
}
