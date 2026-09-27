package payouts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/audit"
	"github.com/dezmymachine/farmish-backend/internal/crypto"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// banksTTL is how long a bank list is reused without asking Paystack again.
const banksTTL = time.Hour

// SellerStore is the part of sellers.Service this package uses: the profile
// requirement and the business name for the name check.
type SellerStore interface {
	GetMine(ctx context.Context, userID uuid.UUID) (sellers.Profile, error)
}

// Service owns seller payout accounts.
type Service struct {
	pool     *pgxpool.Pool
	crypto   *crypto.Crypter
	paystack payments.Provider
	sellers  SellerStore
	jobs     *jobs.Client
	log      *slog.Logger
	// Now is the clock, injectable so cooldown and cache tests never sleep.
	Now func() time.Time

	mu    sync.Mutex
	banks map[string]cachedBanks
}

// cachedBanks is one ListBanks answer with its fetch time.
type cachedBanks struct {
	at    time.Time
	banks []payments.Bank
}

// New returns the service. crypto and paystack are required: account numbers
// must never be stored or resolved without them.
func New(pool *pgxpool.Pool, c *crypto.Crypter, paystack payments.Provider, sellers SellerStore) *Service {
	return &Service{
		pool: pool, crypto: c, paystack: paystack, sellers: sellers,
		Now: time.Now, banks: map[string]cachedBanks{},
	}
}

// AttachJobClient gives the service the client it needs to enqueue the
// security-alert SMS inside the upsert transaction. cmd/api calls it once,
// after the registry exists.
func (s *Service) AttachJobClient(client *jobs.Client) { s.jobs = client }

// AttachLogger gives the service the logger its Paystack warnings go to.
func (s *Service) AttachLogger(l *slog.Logger) { s.log = l }

func (s *Service) logger() *slog.Logger {
	if s.log == nil {
		return slog.Default()
	}
	return s.log
}

// ListBanks returns Paystack's banks for one type, cached in memory for an
// hour. An empty type returns both lists concatenated, mobile money first.
func (s *Service) ListBanks(ctx context.Context, accountType string) ([]payments.Bank, error) {
	if accountType != "" && !validTypes[accountType] {
		var invalid validation.Error
		invalid.Add("type", "must be mobile_money or ghipss")
		return nil, invalid.OrNil()
	}
	types := []string{TypeMobileMoney, TypeGhipss}
	if accountType != "" {
		types = []string{accountType}
	}
	var out []payments.Bank
	for _, t := range types {
		banks, err := s.listBanks(ctx, t)
		if err != nil {
			return nil, err
		}
		out = append(out, banks...)
	}
	return out, nil
}

// listBanks returns one cached type list, refreshing it past its TTL.
func (s *Service) listBanks(ctx context.Context, accountType string) ([]payments.Bank, error) {
	s.mu.Lock()
	cached, ok := s.banks[accountType]
	s.mu.Unlock()
	if ok && s.Now().Sub(cached.at) < banksTTL {
		return cached.banks, nil
	}
	banks, err := s.paystack.ListBanks(ctx, payments.ListBanksInput{Currency: "GHS", Type: accountType})
	if err != nil {
		return nil, fmt.Errorf("list banks: %w", err)
	}
	s.mu.Lock()
	s.banks[accountType] = cachedBanks{at: s.Now(), banks: banks}
	s.mu.Unlock()
	return banks, nil
}

// Get returns the seller's payout account. Sellers without a profile get
// ErrSellerProfileRequired; sellers without an account get ErrNotFound.
func (s *Service) Get(ctx context.Context, sellerID uuid.UUID) (Account, error) {
	if _, err := s.sellers.GetMine(ctx, sellerID); err != nil {
		if errors.Is(err, sellers.ErrNotFound) {
			return Account{}, ErrSellerProfileRequired
		}
		return Account{}, fmt.Errorf("get seller profile: %w", err)
	}
	row, err := db.New(s.pool).GetPayoutAccountBySeller(ctx, sellerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, fmt.Errorf("%w: seller %s", ErrNotFound, sellerID)
	}
	if err != nil {
		return Account{}, fmt.Errorf("get payout account: %w", err)
	}
	return fromRow(row), nil
}

// SetAccount resolves the seller's details with Paystack, name-checks them,
// creates the transfer recipient, and stores the encrypted account. The two
// Paystack calls run outside any transaction; the upsert, audit event and
// security-alert enqueue commit in one.
//
// displayName is the seller's users.display_name ("" when unset), for the
// name check alongside the business name.
func (s *Service) SetAccount(ctx context.Context, sellerID uuid.UUID, displayName string, in Input) (Account, error) {
	if s.crypto == nil {
		return Account{}, fmt.Errorf("payouts: set account: encrypter is not wired")
	}
	if s.paystack == nil {
		return Account{}, fmt.Errorf("payouts: set account: paystack is not wired")
	}
	if s.jobs == nil {
		return Account{}, fmt.Errorf("payouts: set account: job client is not wired")
	}
	profile, err := s.sellers.GetMine(ctx, sellerID)
	if errors.Is(err, sellers.ErrNotFound) {
		return Account{}, ErrSellerProfileRequired
	}
	if err != nil {
		return Account{}, fmt.Errorf("get seller profile: %w", err)
	}
	var invalid validation.Error
	if !validTypes[in.Type] {
		invalid.Add("type", "must be mobile_money or ghipss")
	}
	number, ok := NormalizeNumber(in.Type, in.AccountNumber)
	if !ok {
		invalid.Add("accountNumber", "must be 10-20 digits")
	}
	if err := invalid.OrNil(); err != nil {
		return Account{}, err
	}
	banks, err := s.listBanks(ctx, in.Type)
	if err != nil {
		return Account{}, err
	}
	var bankName string
	for _, bank := range banks {
		if bank.Code == in.BankCode {
			bankName = bank.Name
			break
		}
	}
	if bankName == "" {
		var unknown validation.Error
		unknown.Add("bankCode", "is not a known bank for this account type")
		return Account{}, unknown.OrNil()
	}

	resolved, err := s.paystack.ResolveAccount(ctx, payments.ResolveInput{
		AccountNumber: number, BankCode: in.BankCode,
	})
	if err != nil {
		s.logger().Warn("payout account unresolvable",
			slog.String("seller_id", sellerID.String()), slog.String("error", err.Error()))
		return Account{}, fmt.Errorf("%w: %s", ErrUnresolvable, err.Error())
	}
	status := StatusNeedsReview
	var verifiedAt *time.Time
	if NameMatches(resolved.AccountName, profile.BusinessName, displayName) {
		status = StatusVerified
		now := s.Now()
		verifiedAt = &now
	}
	recipient, err := s.paystack.CreateTransferRecipient(ctx, payments.TransferRecipientInput{
		Type: recipientType(in.Type), Name: resolved.AccountName,
		AccountNumber: number, BankCode: in.BankCode, Currency: "GHS",
	})
	if err != nil {
		return Account{}, fmt.Errorf("create transfer recipient: %w", err)
	}
	sealed, err := s.crypto.Encrypt([]byte(number))
	if err != nil {
		return Account{}, fmt.Errorf("encrypt account number: %w", err)
	}

	var stored db.SellerPayoutAccount
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		previous, err := q.GetPayoutAccountBySeller(ctx, sellerID)
		previousExists := true
		if errors.Is(err, pgx.ErrNoRows) {
			previousExists = false
		} else if err != nil {
			return fmt.Errorf("lock payout account: %w", err)
		}
		var cooldown *time.Time
		if previousExists {
			until := s.Now().Add(CooldownAfterChange)
			cooldown = &until
		}
		row, err := q.UpsertPayoutAccount(ctx, db.UpsertPayoutAccountParams{
			SellerID: sellerID, Type: in.Type, BankCode: in.BankCode, BankName: bankName,
			AccountNumberEnc: sealed, AccountNumberMask: Mask(number),
			AccountName: resolved.AccountName, RecipientCode: recipient.RecipientCode,
			Status: status, VerifiedAt: verifiedAt, CooldownUntil: cooldown,
		})
		if err != nil {
			return fmt.Errorf("upsert payout account: %w", err)
		}
		stored = row
		meta := map[string]any{"type": in.Type, "bank_code": in.BankCode, "status": status, "first_setup": !previousExists}
		if previousExists {
			meta["old_mask"] = previous.AccountNumberMask
			meta["new_mask"] = Mask(number)
		}
		if err := audit.Record(ctx, tx, audit.Event{
			ActorID: &sellerID, Action: "payout_account.set", TargetType: "seller", TargetID: sellerID.String(),
			Metadata: meta,
		}); err != nil {
			return err
		}
		if _, err := s.jobs.InsertTx(ctx, tx, notify.SMSArgs{
			UserID: sellerID, Template: notify.TemplatePayoutAccountChanged,
			Params: map[string]string{},
		}, nil); err != nil {
			return fmt.Errorf("enqueue security alert: %w", err)
		}
		return nil
	})
	if err != nil {
		return Account{}, err
	}
	return fromRow(stored), nil
}

// Approve marks a needs_review account verified. A seller who never set one
// gets ErrNotFound; an account that is already verified gets
// ErrAlreadyVerified.
func (s *Service) Approve(ctx context.Context, adminID, sellerID uuid.UUID) (Account, error) {
	now := s.Now()
	var approved db.SellerPayoutAccount
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := q.GetPayoutAccountBySeller(ctx, sellerID); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: seller %s", ErrNotFound, sellerID)
		} else if err != nil {
			return fmt.Errorf("get payout account: %w", err)
		}
		row, err := db.New(tx).SetPayoutAccountVerified(ctx, db.SetPayoutAccountVerifiedParams{
			SellerID: sellerID, VerifiedAt: &now,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAlreadyVerified
		}
		if err != nil {
			return fmt.Errorf("approve payout account: %w", err)
		}
		approved = row
		return audit.Record(ctx, tx, audit.Event{
			ActorID: &adminID, Action: "payout_account.approve", TargetType: "seller", TargetID: sellerID.String(),
			Metadata: map[string]any{"mask": row.AccountNumberMask},
		})
	})
	if err != nil {
		return Account{}, err
	}
	return fromRow(approved), nil
}

// fromRow maps the generated row onto the domain type. The ciphertext never
// leaves: only the mask does.
func fromRow(r db.SellerPayoutAccount) Account {
	return Account{
		SellerID: r.SellerID, Type: r.Type, BankCode: r.BankCode, BankName: r.BankName,
		NumberMask: r.AccountNumberMask, AccountName: r.AccountName, RecipientCode: r.RecipientCode,
		Status: r.Status, VerifiedAt: r.VerifiedAt, CooldownUntil: r.CooldownUntil,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}
