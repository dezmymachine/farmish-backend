package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/audit"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// ProviderName identifies the provider in webhook_events.
const ProviderName = "paystack"

// EventHandler settles one provider event inside the caller's transaction.
//
// It returns the outcome to record and an error. A non-nil error rolls the
// transaction back — including the webhook_events row — and makes the endpoint
// answer 500 so Paystack retries. A business rejection is *not* an error: it
// returns an "rejected:<reason>" outcome and nil, which commits and answers
// 200, because retrying a mismatch will not fix it.
type EventHandler func(ctx context.Context, tx pgx.Tx, data json.RawMessage) (outcome string, err error)

// PurposeHandler applies a successful payment to whatever it was for. It runs
// inside the payments.succeeded job's own transaction and must be idempotent:
// a retry re-runs it, and from Phase 13b the ledger's UNIQUE(kind, reference)
// is what makes that safe.
type PurposeHandler func(ctx context.Context, tx pgx.Tx, payment Payment) error

// Service owns payments: it creates charges with the processing fee grossed up
// onto the buyer, settles them from signed webhooks, and verifies with the
// provider when a webhook never arrives.
type Service struct {
	pool *pgxpool.Pool
	ps   Provider
	log  *slog.Logger
	// feeBps is Paystack's fee, grossed onto the buyer (DOMAIN §2.2).
	feeBps int
	// callbackURL is where Paystack returns the buyer.
	callbackURL string
	// jobs enqueues payments.succeeded in the same transaction that settles the
	// payment. Nil disables the enqueue, which only the job-less run modes want.
	jobs *jobs.Client
	now  func() time.Time

	events   map[string]EventHandler
	purposes map[string]PurposeHandler
}

// Option customises a Service. Purpose handlers arrive through one: Phase 14
// registers 'promotion', Phase 15b 'checkout', and tests register their own on
// the instance they build.
type Option func(*Service)

// WithPurposeHandler registers the handler for a payment purpose.
func WithPurposeHandler(purpose string, h PurposeHandler) Option {
	return func(s *Service) { s.purposes[purpose] = h }
}

// WithJobClient makes the service enqueue payments.succeeded when a payment
// settles. Without it, a successful payment has no follow-up work queued.
func WithJobClient(client *jobs.Client) Option {
	return func(s *Service) { s.jobs = client }
}

// WithEventHandler registers or replaces an event handler. The built-in
// charge.success and charge.failed handlers are registered by New; this exists
// for tests and for an event a later phase adds.
func WithEventHandler(event string, h EventHandler) Option {
	return func(s *Service) { s.events[event] = h }
}

// New returns a Service over a pool and a provider.
func New(pool *pgxpool.Pool, ps Provider, log *slog.Logger, feeBps int, callbackURL string, opts ...Option) *Service {
	s := &Service{
		pool: pool, ps: ps, log: log,
		feeBps: feeBps, callbackURL: callbackURL,
		now:      time.Now,
		events:   map[string]EventHandler{},
		purposes: map[string]PurposeHandler{},
	}
	s.events[EventChargeSuccess] = s.onChargeSuccess
	s.events[EventChargeFailed] = s.onChargeFailed
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// CreateInput starts a payment. BasePesewas is what the platform must net; the
// processing fee is added on top by the gross-up. InitializeInput, by
// contrast, is what the service sends to Paystack.
type CreateInput struct {
	UserID uuid.UUID
	// Email may be empty for a phone sign-in; a deterministic placeholder is
	// used then, because Paystack requires an address.
	Email       string
	Purpose     string
	PurposeRef  string
	BasePesewas int64
	// Metadata is stored with the payment row and forwarded to Paystack with
	// the payment's own identifiers. It holds purchase-time facts, such as a
	// promotion tier's credit count, that must not change if reference data is
	// edited later.
	Metadata map[string]any
}

// Initialize creates a pending payment and asks Paystack where to send the
// buyer.
//
// The row is committed before the provider is called: the network call must
// never hold a database transaction open, and a payment that Paystack never
// heard of is a row that expires as abandoned rather than a stuck transaction.
func (s *Service) Initialize(ctx context.Context, in CreateInput) (Payment, error) {
	var verr validation.Error
	if in.UserID == uuid.Nil {
		verr.Add("userId", "is required")
	}
	if !validPurpose(in.Purpose) {
		verr.Add("purpose", "must be promotion or checkout")
	}
	if in.PurposeRef == "" {
		verr.Add("purposeRef", "is required")
	}
	if in.BasePesewas <= 0 {
		verr.Add("baseAmount", "must be more than 0")
	}
	if err := verr.OrNil(); err != nil {
		return Payment{}, err
	}
	charge, fee, err := grossUp(in.BasePesewas, s.feeBps)
	if err != nil {
		return Payment{}, err
	}
	reference, err := newReference()
	if err != nil {
		return Payment{}, err
	}

	// Step 1: the pending row, committed on its own.
	var payment Payment
	metadata, err := marshalMetadata(in.Metadata)
	if err != nil {
		return Payment{}, err
	}
	if err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		row, err := db.New(tx).InsertPayment(ctx, db.InsertPaymentParams{
			Reference: reference, UserID: in.UserID, Purpose: in.Purpose,
			PurposeRef: in.PurposeRef, BasePesewas: in.BasePesewas,
			ProcessingFeePesewas: fee, ChargePesewas: charge, Metadata: metadata,
		})
		if err != nil {
			return fmt.Errorf("insert payment: %w", err)
		}
		payment = fromRow(row)
		return nil
	}); err != nil {
		return Payment{}, err
	}

	// Step 2: the provider call, outside any transaction.
	email := in.Email
	if email == "" {
		email = placeholderEmail(in.UserID)
	}
	providerMetadata := make(map[string]any, len(in.Metadata)+3)
	for key, value := range in.Metadata {
		providerMetadata[key] = value
	}
	providerMetadata["payment_id"] = payment.ID.String()
	providerMetadata["purpose"] = in.Purpose
	providerMetadata["purpose_ref"] = in.PurposeRef
	result, err := s.ps.InitializeTransaction(ctx, InitializeInput{
		Email: email, AmountPesewas: charge, Reference: reference,
		CallbackURL: s.callbackURL,
		Metadata:    providerMetadata,
	})
	if err != nil {
		// The buyer never got a chance to pay, so the payment is dead rather
		// than pending. The row stays as the audit trail of the attempt.
		if failErr := s.markFailed(ctx, payment.ID, providerFailureReason(err)); failErr != nil {
			s.log.Error("mark payment failed after a provider error",
				slog.String("payment_id", payment.ID.String()), slog.String("error", failErr.Error()))
		}
		return Payment{}, fmt.Errorf("initialize payment with paystack: %w", err)
	}

	// Step 3: where to send the buyer.
	if err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		row, err := db.New(tx).SetPaymentAuthorizationURL(ctx, db.SetPaymentAuthorizationURLParams{
			ID: payment.ID, AuthorizationUrl: optionalString(result.AuthorizationURL),
		})
		if err != nil {
			return fmt.Errorf("store authorization url: %w", err)
		}
		payment = fromRow(row)
		return nil
	}); err != nil {
		return Payment{}, err
	}
	return payment, nil
}

// providerFailureReason keeps the provider's message for the audit trail
// without pretending it is our own text.
func providerFailureReason(err error) string {
	if errors.Is(err, ErrRejected) {
		return "paystack rejected the charge: " + err.Error()
	}
	return "paystack was unavailable"
}

// markFailed moves a pending payment to failed, ignoring the case where it is
// no longer pending (a webhook may have won the race).
func (s *Service) markFailed(ctx context.Context, id uuid.UUID, reason string) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := db.New(tx).MarkPaymentFailed(ctx, db.MarkPaymentFailedParams{
			ID: id, FailureReason: optionalString(reason),
		}); err != nil {
			return fmt.Errorf("mark payment failed: %w", err)
		}
		return nil
	})
}

// WebhookEvent is the shape of the body Paystack posts.
type WebhookEvent struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// HandleWebhook processes one signed event body.
//
// It is idempotent: the event is keyed '<event>:<data.id>' and the unique
// (provider, event_key) constraint means a replay loses the insert race and
// returns nil without a second effect. A duplicate therefore commits nothing
// and the endpoint answers 200.
func (s *Service) HandleWebhook(ctx context.Context, raw []byte) error {
	var event WebhookEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return fmt.Errorf("%w: parse body: %w", ErrMalformedEvent, err)
	}
	if event.Event == "" {
		return fmt.Errorf("%w: no event field", ErrMalformedEvent)
	}
	var identity struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(event.Data, &identity); err != nil {
		return fmt.Errorf("%w: parse data: %w", ErrMalformedEvent, err)
	}
	if len(identity.ID) == 0 || string(identity.ID) == "null" {
		return fmt.Errorf("%w: data has no id", ErrMalformedEvent)
	}
	// The key is the provider's own id, so the same event delivered twice (or
	// replayed by Paystack, or synthesised by the verify fallback) collides.
	eventKey := event.Event + ":" + string(identity.ID)

	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		row, err := db.New(tx).InsertWebhookEvent(ctx, db.InsertWebhookEventParams{
			Provider: ProviderName, EventKey: eventKey, EventType: event.Event, Payload: raw,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// A replay. Nothing to do, and nothing recorded twice.
			return nil
		}
		if err != nil {
			return fmt.Errorf("record webhook event: %w", err)
		}

		handler, known := s.events[event.Event]
		if !known {
			// An event we do not act on yet: recorded, acknowledged, ignored.
			if err := s.completeEvent(ctx, tx, row.ID, OutcomeIgnored); err != nil {
				return err
			}
			return nil
		}
		outcome, err := handler(ctx, tx, event.Data)
		if err != nil {
			// Rolls back, including this row, so a retry is possible.
			return fmt.Errorf("handle %s: %w", event.Event, err)
		}
		return s.completeEvent(ctx, tx, row.ID, outcome)
	})
}

// completeEvent stamps the outcome. The row is committed with the state change
// the handler made, or not at all.
func (s *Service) completeEvent(ctx context.Context, tx pgx.Tx, id int64, outcome string) error {
	if _, err := tx.Exec(ctx, `UPDATE webhook_events SET processed_at = now(), outcome = $2 WHERE id = $1`,
		id, outcome); err != nil {
		return fmt.Errorf("complete webhook event: %w", err)
	}
	return nil
}

// chargeData is the part of a charge event this phase acts on. The payload
// carries much more (and personal data), so only these fields are read.
type chargeData struct {
	ID        int64  `json:"id"`
	Reference string `json:"reference"`
	Amount    int64  `json:"amount"`
	Currency  string `json:"currency"`
	Fees      int64  `json:"fees"`
	Channel   string `json:"channel"`
	Status    string `json:"status"`
	Reason    string `json:"reason"`
	PaidAt    string `json:"paid_at"`
}

// onChargeSuccess settles a paid charge. It is the only writer of success.
func (s *Service) onChargeSuccess(ctx context.Context, tx pgx.Tx, data json.RawMessage) (string, error) {
	var charge chargeData
	if err := json.Unmarshal(data, &charge); err != nil {
		// A malformed charge cannot be retried into shape.
		return OutcomeIgnored, nil
	}
	q := db.New(tx)
	payment, err := q.GetPaymentByReferenceForUpdate(ctx, charge.Reference)
	if errors.Is(err, pgx.ErrNoRows) {
		// We were never sent this reference: a charge on someone else's
		// integration, or a stale event.
		s.log.Warn("paystack charge for an unknown reference",
			slog.String("reference", charge.Reference), slog.Int64("provider_id", charge.ID))
		return OutcomeUnknownReference, nil
	}
	if err != nil {
		return "", fmt.Errorf("lock payment: %w", err)
	}
	current := fromRow(payment)
	if current.Status != StatusPending {
		// Already settled, most likely by the verify fallback. The webhook is a
		// duplicate of an effect that happened.
		return OutcomeIgnored, nil
	}

	// The money has to be what we asked for. A mismatch is never settled: it is
	// logged loudly, audited, and left pending for a human (DOMAIN §2.2 lets
	// the actual fee differ, but not the charge).
	if reason, ok := mismatchOutcome(current, charge); !ok {
		s.log.Error("payment amount mismatch",
			slog.String("reference", current.Reference),
			slog.Int64("expected_pesewas", current.Charge),
			slog.Int64("received_pesewas", charge.Amount),
			slog.String("currency", charge.Currency))
		if err := audit.Record(ctx, tx, audit.Event{
			Action: "payment.amount_mismatch", TargetType: "payment", TargetID: current.ID.String(),
			Metadata: map[string]any{
				"reference": current.Reference,
				"expected":  current.Charge,
				"received":  charge.Amount,
				"currency":  charge.Currency,
			},
		}); err != nil {
			return "", err
		}
		return reason, nil
	}

	paidAt := s.now()
	if charge.PaidAt != "" {
		if parsed, err := time.Parse(time.RFC3339, charge.PaidAt); err == nil {
			paidAt = parsed
		}
	}
	settled, err := q.SettlePaymentSuccess(ctx, db.SettlePaymentSuccessParams{
		ID: current.ID, PaidAt: &paidAt, Channel: optionalString(charge.Channel),
		PaystackFeePesewas: optionalInt(charge.Fees),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Lost a race with another delivery: it settled, we did not.
		return OutcomeIgnored, nil
	}
	if err != nil {
		return "", fmt.Errorf("settle payment: %w", err)
	}

	// The follow-up work is enqueued in the same transaction as the state
	// change, so it can never exist without the payment being settled.
	if s.jobs != nil {
		if _, err := s.jobs.InsertTx(ctx, tx, SucceededArgs{PaymentID: current.ID}, jobs.Unique()); err != nil {
			return "", fmt.Errorf("enqueue payments.succeeded: %w", err)
		}
	}
	_ = settled
	s.log.Info("payment settled",
		slog.String("reference", current.Reference), slog.String("purpose", current.Purpose),
		slog.Int64("charge_pesewas", current.Charge), slog.Int64("paystack_fee_pesewas", charge.Fees))
	return OutcomeProcessed, nil
}

// mismatchOutcome reports why a charge does not match the payment, or ok=false
// when it does. The charge total is the check: a currency or amount difference
// both mean the money is not ours to keep.
func mismatchOutcome(payment Payment, charge chargeData) (reason string, ok bool) {
	if charge.Currency != "" && charge.Currency != payment.Currency {
		return OutcomeCurrencyMismatch, false
	}
	if charge.Amount != payment.Charge {
		return OutcomeAmountMismatch, false
	}
	return "", true
}

// onChargeFailed records a charge Paystack could not collect.
func (s *Service) onChargeFailed(ctx context.Context, tx pgx.Tx, data json.RawMessage) (string, error) {
	var charge chargeData
	if err := json.Unmarshal(data, &charge); err != nil {
		return OutcomeIgnored, nil
	}
	q := db.New(tx)
	payment, err := q.GetPaymentByReferenceForUpdate(ctx, charge.Reference)
	if errors.Is(err, pgx.ErrNoRows) {
		s.log.Warn("paystack failure for an unknown reference", slog.String("reference", charge.Reference))
		return OutcomeUnknownReference, nil
	}
	if err != nil {
		return "", fmt.Errorf("lock payment: %w", err)
	}
	if fromRow(payment).Status != StatusPending {
		return OutcomeIgnored, nil
	}
	reason := charge.Reason
	if reason == "" {
		reason = "the charge was declined"
	}
	if _, err := q.MarkPaymentFailed(ctx, db.MarkPaymentFailedParams{
		ID: payment.ID, FailureReason: optionalString(reason),
	}); err != nil {
		return "", fmt.Errorf("mark payment failed: %w", err)
	}
	return OutcomeProcessed, nil
}

// Verify returns a payment to its owner, asking Paystack when the local row is
// still pending and old enough that the buyer should have finished by now.
//
// A reference that does not exist and one that belongs to somebody else both
// return ErrNotFound, so the endpoint's 404 leaks nothing.
func (s *Service) Verify(ctx context.Context, userID uuid.UUID, reference string) (Payment, error) {
	q := db.New(s.pool)
	row, err := q.GetPaymentByReference(ctx, reference)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, fmt.Errorf("%w: %s", ErrNotFound, reference)
	}
	if err != nil {
		return Payment{}, fmt.Errorf("get payment: %w", err)
	}
	payment := fromRow(row)
	if payment.UserID != userID {
		return Payment{}, fmt.Errorf("%w: %s", ErrNotFound, reference)
	}
	if payment.Status != StatusPending || s.now().Sub(payment.CreatedAt) < verifyFallbackDelay {
		return payment, nil
	}

	transaction, err := s.ps.VerifyTransaction(ctx, reference)
	if err != nil {
		if errors.Is(err, ErrProviderUnavailable) {
			// Leave the payment pending: the next call, or a webhook, will try
			// again. The buyer still sees pending, which is the truth.
			return payment, fmt.Errorf("verify with paystack: %w", err)
		}
		return payment, fmt.Errorf("verify payment: %w", err)
	}

	switch transaction.Status {
	case StatusSuccess:
		// The same code path as the webhook, keyed on the provider's own
		// transaction id, so a webhook that arrives afterwards is a duplicate
		// and does nothing.
		if err := s.HandleWebhook(ctx, s.syntheticChargeSuccess(transaction)); err != nil {
			return payment, fmt.Errorf("settle from verify: %w", err)
		}
	case StatusFailed, StatusAbandoned:
		reason := "the charge was not completed"
		if transaction.Status == StatusFailed {
			reason = "the charge was declined"
		}
		if err := s.markAbandoned(ctx, payment, transaction.Status, reason); err != nil {
			return payment, err
		}
	}

	settled, err := q.GetPaymentByReference(ctx, reference)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, fmt.Errorf("%w: %s", ErrNotFound, reference)
	}
	if err != nil {
		return Payment{}, fmt.Errorf("reload payment: %w", err)
	}
	return fromRow(settled), nil
}

// markAbandoned moves a pending payment to failed or abandoned, whichever the
// provider reported.
func (s *Service) markAbandoned(ctx context.Context, payment Payment, status, reason string) error {
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		params := db.MarkPaymentAbandonedParams{ID: payment.ID, FailureReason: optionalString(reason)}
		var err error
		if status == StatusFailed {
			_, err = q.MarkPaymentFailed(ctx,
				db.MarkPaymentFailedParams{ID: payment.ID, FailureReason: optionalString(reason)})
		} else {
			_, err = q.MarkPaymentAbandoned(ctx, params)
		}
		if err != nil {
			return fmt.Errorf("mark payment %s: %w", status, err)
		}
		return nil
	})
}

// syntheticChargeSuccess builds the event body the webhook would have carried,
// so both paths converge on one implementation and one event key.
func (s *Service) syntheticChargeSuccess(t Transaction) []byte {
	paidAt := ""
	if t.PaidAt != nil {
		paidAt = t.PaidAt.Format(time.RFC3339)
	}
	body := map[string]any{
		"event": EventChargeSuccess,
		"data": map[string]any{
			"id": t.ID, "reference": t.Reference, "amount": t.AmountPesewas,
			"currency": t.Currency, "fees": t.FeesPesewas, "channel": t.Channel,
			"status": t.Status, "paid_at": paidAt,
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		// A map of scalars always marshals; treat it as a programming error.
		panic("payments: marshal synthetic event: " + err.Error())
	}
	return raw
}

// marshalMetadata encodes a payment snapshot as JSON. An empty snapshot is
// stored as an object, never NULL, so readers always get a map.
func marshalMetadata(metadata map[string]any) ([]byte, error) {
	if len(metadata) == 0 {
		return []byte("{}"), nil
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("encode payment metadata: %w", err)
	}
	return raw, nil
}

// optionalString returns nil for an empty string, so a column Paystack left
// blank stays NULL rather than becoming "".
func optionalString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// optionalInt returns nil for zero.
func optionalInt(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

// PaymentForLookup returns the reference for a payment id, for handlers that
// were handed an id.
func (s *Service) PaymentForLookup(ctx context.Context, id uuid.UUID) (Payment, error) {
	row, err := db.New(s.pool).GetPaymentByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Payment{}, fmt.Errorf("get payment: %w", err)
	}
	return fromRow(row), nil
}

// runPurposeHandler applies a settled payment, inside the caller's transaction.
func (s *Service) runPurposeHandler(ctx context.Context, tx pgx.Tx, payment Payment) error {
	handler, ok := s.purposes[payment.Purpose]
	if !ok {
		// Nothing is registered for this purpose yet. The payment is settled and
		// that is not an error: the purpose's own phase adds the handler, and a
		// job that failed here would only be retried to the same result.
		s.log.Warn("no purpose handler registered",
			slog.String("purpose", payment.Purpose), slog.String("reference", payment.Reference))
		return nil
	}
	if err := handler(ctx, tx, payment); err != nil {
		return fmt.Errorf("apply %s payment %s: %w", payment.Purpose, payment.Reference, err)
	}
	return nil
}

// AttachJobClient gives the service the River client it needs to enqueue
// payments.succeeded. cmd/api calls it once, while building the job registry,
// because the client does not exist until the registry does.
func (s *Service) AttachJobClient(client *jobs.Client) { s.jobs = client }
