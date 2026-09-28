// Package supply owns buyer supply requests (Phase 20b): priced quotes for
// bulk produce, with the admin-driven status machine the legacy app never
// had. Rules are in DOMAIN §11.
package supply

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/audit"
	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/geo"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// Statuses stored in supply_requests.status (DOMAIN §11).
const (
	StatusPending    = "pending"
	StatusConfirmed  = "confirmed"
	StatusProcessing = "processing"
	StatusDelivered  = "delivered"
	StatusCancelled  = "cancelled"
)

// Item bounds (DOMAIN §11).
const (
	MinItems = 1
	MaxItems = 20
	// numberAttempts bounds request-number generation on collisions.
	numberAttempts = 5
)

var (
	// ErrNotFound means no request matches, or the caller does not own it:
	// both read as 404 so ids cannot be probed.
	ErrNotFound = errors.New("supply request not found")
	// ErrInvalidTransition means the move is not in DOMAIN §11.
	ErrInvalidTransition = errors.New("invalid supply request transition")
)

// validUnits is every unit a request item may use: the union of the DOMAIN
// §8 listing sets (including the VOLUME set no listing group offers) plus
// heads.
var validUnits = func() map[string]bool {
	out := map[string]bool{}
	for _, group := range catalog.Groups {
		for _, unit := range group.Units {
			out[unit] = true
		}
	}
	for _, unit := range []string{"liters", "milliliters", "gallons"} {
		out[unit] = true
	}
	return out
}()

// legalMoves is DOMAIN §11's table: from → the statuses an admin may move
// to. Cancellation is the only owner move, from pending alone.
var legalMoves = map[string][]string{
	StatusPending:    {StatusConfirmed, StatusCancelled},
	StatusConfirmed:  {StatusProcessing, StatusCancelled},
	StatusProcessing: {StatusDelivered, StatusCancelled},
}

// UserStore reads the defaults for delivery contact fields.
type UserStore interface {
	Get(ctx context.Context, id uuid.UUID) (users.User, error)
}

// ItemInput is one requested product line.
type ItemInput struct {
	CategorySlug string
	ProductName  string
	Quantity     int32
	Unit         string
}

// Input is a new supply request.
type Input struct {
	Items           []ItemInput
	DeliveryName    string
	DeliveryPhone   string
	DeliveryAddress string
	ExpectedDate    string // YYYY-MM-DD, optional
	Notes           string
}

// Item is one stored request line.
type Item struct {
	ID           uuid.UUID
	CategoryID   uuid.UUID
	CategorySlug string
	ProductName  string
	Quantity     int32
	Unit         string
	SortOrder    int32
}

// Event is one status move.
type Event struct {
	ID        int64
	From      *string
	To        string
	ActorID   *uuid.UUID
	Note      *string
	CreatedAt time.Time
}

// Request is one supply request with its lines and trail.
type Request struct {
	ID              uuid.UUID
	RequestNumber   string
	UserID          uuid.UUID
	DeliveryName    *string
	DeliveryPhone   *string
	DeliveryAddress *string
	ExpectedDate    *time.Time
	Notes           *string
	Status          string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Items           []Item
	Events          []Event
}

// Service owns supply requests.
type Service struct {
	pool  *pgxpool.Pool
	users UserStore
	jobs  *jobs.Client
	log   *slog.Logger
	// Now is the clock, injectable so date and number tests never sleep.
	Now func() time.Time
	// number generates request numbers; newRequestNumber by default,
	// replaceable in tests to force collisions.
	number func(time.Time) (string, error)
}

// New returns the service.
func New(pool *pgxpool.Pool, users UserStore) *Service {
	return &Service{pool: pool, users: users, Now: time.Now, number: newRequestNumber}
}

// AttachJobClient gives the service the client it needs to enqueue status
// SMS inside the write transaction.
func (s *Service) AttachJobClient(client *jobs.Client) { s.jobs = client }

// AttachLogger gives the service its logger.
func (s *Service) AttachLogger(l *slog.Logger) { s.log = l }

// Create records a supply request with its items, a creation event and a
// confirmation SMS, in one transaction. The number retries on collision.
func (s *Service) Create(ctx context.Context, userID uuid.UUID, in Input) (Request, error) {
	if s.jobs == nil {
		return Request{}, fmt.Errorf("supply: create: job client is not wired")
	}
	items, deliveryName, deliveryPhone, deliveryAddress, expectedDate, notes, err := s.validate(ctx, userID, in)
	if err != nil {
		return Request{}, err
	}
	var created Request
	for attempt := 0; attempt < numberAttempts; attempt++ {
		number, err := s.number(s.Now())
		if err != nil {
			return Request{}, err
		}
		err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			q := db.New(tx)
			row, err := q.InsertSupplyRequest(ctx, db.InsertSupplyRequestParams{
				RequestNumber: number, UserID: userID,
				DeliveryName: deliveryName, DeliveryPhone: deliveryPhone,
				DeliveryAddress: deliveryAddress, ExpectedDate: expectedDate, Notes: notes,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return errRetryNumber
			}
			if err != nil {
				return fmt.Errorf("insert supply request: %w", err)
			}
			for i, item := range items {
				if err := q.InsertSupplyRequestItem(ctx, db.InsertSupplyRequestItemParams{
					SupplyRequestID: row.ID, CategoryID: item.CategoryID,
					ProductName: item.ProductName, Quantity: item.Quantity,
					Unit: item.Unit, SortOrder: int32(i), //nolint:gosec // G115: at most 20 items
				}); err != nil {
					return fmt.Errorf("insert supply item: %w", err)
				}
			}
			if err := q.InsertSupplyRequestEvent(ctx, db.InsertSupplyRequestEventParams{
				SupplyRequestID: row.ID, ToStatus: StatusPending, ActorID: pgUUID(&userID),
			}); err != nil {
				return fmt.Errorf("insert creation event: %w", err)
			}
			if _, err := s.jobs.InsertTx(ctx, tx, notify.SMSArgs{
				UserID: userID, Template: notify.TemplateSupplyRequestPending,
				Params: map[string]string{"number": number},
			}, nil); err != nil {
				return fmt.Errorf("enqueue confirmation: %w", err)
			}
			created = fromRow(row)
			return nil
		})
		if errors.Is(err, errRetryNumber) {
			continue
		}
		if err != nil {
			return Request{}, err
		}
		return s.detail(ctx, created.ID, userID)
	}
	return Request{}, fmt.Errorf("supply: request number collided %d times", numberAttempts)
}

// errRetryNumber regenerates the request number inside Create's loop.
var errRetryNumber = errors.New("supply request number collision")

// validatedItems is validate's normalized output.
type validatedItem struct {
	CategoryID  uuid.UUID
	ProductName string
	Quantity    int32
	Unit        string
}

// validate checks a new request and normalizes it: parent categories
// resolved to ids, phones to E.164, dates to days, contact defaults from
// the user.
func (s *Service) validate(ctx context.Context, userID uuid.UUID, in Input) ([]validatedItem, *string, *string, *string, pgtype.Date, *string, error) {
	var invalid validation.Error
	if len(in.Items) < MinItems || len(in.Items) > MaxItems {
		invalid.Add("items", "must hold between 1 and 20 items")
	}
	user, err := s.users.Get(ctx, userID)
	if err != nil {
		return nil, nil, nil, nil, pgtype.Date{}, nil, fmt.Errorf("get user: %w", err)
	}
	items := make([]validatedItem, 0, len(in.Items))
	for i, item := range in.Items {
		prefix := fmt.Sprintf("items[%d]", i)
		category, err := db.New(s.pool).GetSupplyCategoryBySlug(ctx, item.CategorySlug)
		if err != nil {
			invalid.Add(prefix+".categorySlug", "is not a known category")
			continue
		}
		if category.ParentID.Valid {
			invalid.Add(prefix+".categorySlug", "must be a parent category")
			continue
		}
		name := strings.TrimSpace(item.ProductName)
		if len(name) < 2 || len(name) > 100 {
			invalid.Add(prefix+".productName", "must be between 2 and 100 characters")
		}
		if item.Quantity < 1 {
			invalid.Add(prefix+".quantity", "must be at least 1")
		}
		if !validUnits[item.Unit] {
			invalid.Add(prefix+".unit", "is not a known unit")
		}
		items = append(items, validatedItem{
			CategoryID: category.ID, ProductName: name,
			Quantity: item.Quantity, Unit: item.Unit,
		})
	}
	name := strings.TrimSpace(in.DeliveryName)
	if name == "" && user.DisplayName != nil {
		name = strings.TrimSpace(*user.DisplayName)
	}
	if len(name) > 120 {
		invalid.Add("deliveryName", "must be at most 120 characters")
	}
	phone := strings.TrimSpace(in.DeliveryPhone)
	if phone == "" && user.Phone != nil {
		phone = *user.Phone
	}
	if phone != "" {
		normalized, err := geo.NormalizeGhanaPhone(phone)
		if err != nil {
			invalid.Add("deliveryPhone", "must be a Ghanaian phone number")
		} else {
			phone = normalized
		}
	}
	address := strings.TrimSpace(in.DeliveryAddress)
	if len(address) > 300 {
		invalid.Add("deliveryAddress", "must be at most 300 characters")
	}
	var expectedDate pgtype.Date
	switch day, err := time.Parse("2006-01-02", strings.TrimSpace(in.ExpectedDate)); {
	case strings.TrimSpace(in.ExpectedDate) == "":
	case err != nil:
		invalid.Add("expectedDate", "must be YYYY-MM-DD")
	case day.Before(today(s.Now())):
		invalid.Add("expectedDate", "must be today or later")
	default:
		expectedDate = pgtype.Date{Time: day, Valid: true}
	}
	notes := strings.TrimSpace(in.Notes)
	if len(notes) > 1000 {
		invalid.Add("notes", "must be at most 1000 characters")
	}
	if err := invalid.OrNil(); err != nil {
		return nil, nil, nil, nil, pgtype.Date{}, nil, err
	}
	return items, orNil(name), orNil(phone), orNil(address), expectedDate, orNil(notes), nil
}

// today returns the current date in Africa/Accra (UTC+0, no DST) at
// midnight, for expected-date comparisons.
func today(now time.Time) time.Time {
	year, month, day := now.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// newRequestNumber returns SUP-YYYYMMDD-XXXXXX with 6 random uppercase
// base36 characters, dated in Africa/Accra.
func newRequestNumber(now time.Time) (string, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate request number: %w", err)
	}
	value := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	out := make([]byte, 6)
	for i := 5; i >= 0; i-- {
		out[i] = alphabet[value%36]
		value /= 36
	}
	return fmt.Sprintf("SUP-%s-%s", now.UTC().Format("20060102"), string(out)), nil
}

// Get returns one of the caller's requests with its items and events.
// Anyone else's id reads as not found.
func (s *Service) Get(ctx context.Context, callerID, requestID uuid.UUID) (Request, error) {
	row, err := db.New(s.pool).GetSupplyRequestByID(ctx, requestID)
	if errors.Is(err, pgx.ErrNoRows) || row.UserID != callerID {
		return Request{}, fmt.Errorf("%w: %s", ErrNotFound, requestID)
	}
	if err != nil {
		return Request{}, fmt.Errorf("get supply request: %w", err)
	}
	return s.detail(ctx, row.ID, callerID)
}

// detail loads items and events for a request the caller owns.
func (s *Service) detail(ctx context.Context, requestID, callerID uuid.UUID) (Request, error) {
	q := db.New(s.pool)
	row, err := q.GetSupplyRequestByID(ctx, requestID)
	if err != nil {
		return Request{}, fmt.Errorf("get supply request: %w", err)
	}
	if row.UserID != callerID {
		return Request{}, fmt.Errorf("%w: %s", ErrNotFound, requestID)
	}
	items, err := q.ListSupplyRequestItems(ctx, row.ID)
	if err != nil {
		return Request{}, fmt.Errorf("list supply items: %w", err)
	}
	events, err := q.ListSupplyRequestEvents(ctx, row.ID)
	if err != nil {
		return Request{}, fmt.Errorf("list supply events: %w", err)
	}
	out := fromRow(row)
	for _, item := range items {
		var slug string
		if err := s.pool.QueryRow(ctx, `SELECT slug FROM categories WHERE id = $1`, item.CategoryID).Scan(&slug); err != nil {
			return Request{}, fmt.Errorf("get item category: %w", err)
		}
		out.Items = append(out.Items, Item{
			ID: item.ID, CategoryID: item.CategoryID, CategorySlug: slug,
			ProductName: item.ProductName, Quantity: item.Quantity, Unit: item.Unit,
			SortOrder: item.SortOrder,
		})
	}
	for _, event := range events {
		out.Events = append(out.Events, Event{
			ID: event.ID, From: event.FromStatus, To: event.ToStatus,
			ActorID: pgOrNil(event.ActorID), Note: event.Note, CreatedAt: event.CreatedAt,
		})
	}
	return out, nil
}

// List returns the caller's requests, newest first, with an optional status
// filter.
func (s *Service) List(ctx context.Context, callerID uuid.UUID, status string, limit, offset int32) ([]Request, int64, error) {
	if status != "" && !validStatus(status) {
		var invalid validation.Error
		invalid.Add("status", "must be pending, confirmed, processing, delivered or cancelled")
		return nil, 0, invalid.OrNil()
	}
	q := db.New(s.pool)
	rows, err := q.ListSupplyRequestsByUser(ctx, db.ListSupplyRequestsByUserParams{
		UserID: callerID, Status: orNil(status), Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list supply requests: %w", err)
	}
	total, err := q.CountSupplyRequestsByUser(ctx, db.CountSupplyRequestsByUserParams{
		UserID: callerID, Status: orNil(status),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("count supply requests: %w", err)
	}
	out := make([]Request, 0, len(rows))
	for _, row := range rows {
		detail, err := s.detail(ctx, row.ID, callerID)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, detail)
	}
	return out, total, nil
}

// AdminList returns every request, newest first, with optional filters.
func (s *Service) AdminList(ctx context.Context, status string, limit, offset int32) ([]Request, int64, error) {
	if status != "" && !validStatus(status) {
		var invalid validation.Error
		invalid.Add("status", "must be pending, confirmed, processing, delivered or cancelled")
		return nil, 0, invalid.OrNil()
	}
	q := db.New(s.pool)
	rows, err := q.ListAllSupplyRequests(ctx, db.ListAllSupplyRequestsParams{
		Status: orNil(status), Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list supply requests: %w", err)
	}
	total, err := q.CountAllSupplyRequests(ctx, orNil(status))
	if err != nil {
		return nil, 0, fmt.Errorf("count supply requests: %w", err)
	}
	out := make([]Request, 0, len(rows))
	for _, row := range rows {
		detail, err := s.adminDetail(ctx, row.ID)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, detail)
	}
	return out, total, nil
}

// adminDetail loads items and events without the ownership check.
func (s *Service) adminDetail(ctx context.Context, requestID uuid.UUID) (Request, error) {
	q := db.New(s.pool)
	row, err := q.GetSupplyRequestByID(ctx, requestID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, fmt.Errorf("%w: %s", ErrNotFound, requestID)
	}
	if err != nil {
		return Request{}, fmt.Errorf("get supply request: %w", err)
	}
	out := fromRow(row)
	items, err := q.ListSupplyRequestItems(ctx, row.ID)
	if err != nil {
		return Request{}, fmt.Errorf("list supply items: %w", err)
	}
	for _, item := range items {
		var slug string
		if err := s.pool.QueryRow(ctx, `SELECT slug FROM categories WHERE id = $1`, item.CategoryID).Scan(&slug); err != nil {
			return Request{}, fmt.Errorf("get item category: %w", err)
		}
		out.Items = append(out.Items, Item{
			ID: item.ID, CategoryID: item.CategoryID, CategorySlug: slug,
			ProductName: item.ProductName, Quantity: item.Quantity, Unit: item.Unit,
			SortOrder: item.SortOrder,
		})
	}
	events, err := q.ListSupplyRequestEvents(ctx, row.ID)
	if err != nil {
		return Request{}, fmt.Errorf("list supply events: %w", err)
	}
	for _, event := range events {
		out.Events = append(out.Events, Event{
			ID: event.ID, From: event.FromStatus, To: event.ToStatus,
			ActorID: pgOrNil(event.ActorID), Note: event.Note, CreatedAt: event.CreatedAt,
		})
	}
	return out, nil
}

// Transition moves a request in one transaction: the guarded status write,
// the event row, the audit event and the requester's SMS commit together.
// Admins may make any legal move; owners only pending → cancelled, and only
// on their own requests (anyone else reads as not found).
func (s *Service) Transition(ctx context.Context, actorID uuid.UUID, admin bool, requestID uuid.UUID, to, note string) (Request, error) {
	if s.jobs == nil {
		return Request{}, fmt.Errorf("supply: transition: job client is not wired")
	}
	if !validStatus(to) {
		var invalid validation.Error
		invalid.Add("status", "must be pending, confirmed, processing, delivered or cancelled")
		return Request{}, invalid.OrNil()
	}
	if len(note) > 500 {
		var invalid validation.Error
		invalid.Add("note", "must be at most 500 characters")
		return Request{}, invalid.OrNil()
	}
	var moved db.SupplyRequest
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.GetSupplyRequestForUpdate(ctx, requestID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNotFound, requestID)
		}
		if err != nil {
			return fmt.Errorf("lock supply request: %w", err)
		}
		if row.UserID != actorID && !admin {
			return fmt.Errorf("%w: %s", ErrNotFound, requestID)
		}
		if !admin && (row.Status != StatusPending || to != StatusCancelled) {
			return ErrInvalidTransition
		}
		if admin && !allowed(row.Status, to) {
			return ErrInvalidTransition
		}
		updated, err := q.SetSupplyRequestStatus(ctx, db.SetSupplyRequestStatusParams{
			ID: requestID, Status: to, Status_2: row.Status,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidTransition
		}
		if err != nil {
			return fmt.Errorf("set supply status: %w", err)
		}
		moved = updated
		if err := q.InsertSupplyRequestEvent(ctx, db.InsertSupplyRequestEventParams{
			SupplyRequestID: requestID, FromStatus: &row.Status, ToStatus: to,
			ActorID: pgUUID(&actorID), Note: orNil(strings.TrimSpace(note)),
		}); err != nil {
			return fmt.Errorf("insert supply event: %w", err)
		}
		if err := audit.Record(ctx, tx, audit.Event{
			ActorID: &actorID, Action: "supply_request.transition", TargetType: "supply_request", TargetID: requestID.String(),
			Metadata: map[string]any{"from": row.Status, "to": to},
		}); err != nil {
			return err
		}
		_, err = s.jobs.InsertTx(ctx, tx, notify.SMSArgs{
			UserID: row.UserID, Template: "supply_request_" + to,
			Params: map[string]string{"number": row.RequestNumber},
		}, nil)
		return err
	})
	if err != nil {
		return Request{}, err
	}
	return s.detail(ctx, moved.ID, moved.UserID)
}

// allowed reports whether an admin may move from → to (DOMAIN §11).
func allowed(from, to string) bool {
	for _, next := range legalMoves[from] {
		if next == to {
			return true
		}
	}
	return false
}

// validStatus mirrors the supply_requests CHECK.
func validStatus(status string) bool {
	switch status {
	case StatusPending, StatusConfirmed, StatusProcessing, StatusDelivered, StatusCancelled:
		return true
	}
	return false
}

// fromRow maps the generated row onto the domain type.
func fromRow(r db.SupplyRequest) Request {
	out := Request{
		ID: r.ID, RequestNumber: r.RequestNumber, UserID: r.UserID,
		DeliveryName: r.DeliveryName, DeliveryPhone: r.DeliveryPhone,
		DeliveryAddress: r.DeliveryAddress,
		Notes:           r.Notes, Status: r.Status, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.ExpectedDate.Valid {
		day := r.ExpectedDate.Time
		out.ExpectedDate = &day
	}
	return out
}

// orNil blanks empty strings to NULL.
func orNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// pgUUID widens an optional id for the generated queries.
func pgUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}

// pgOrNil narrows a nullable actor id for the domain type.
func pgOrNil(id pgtype.UUID) *uuid.UUID {
	if !id.Valid {
		return nil
	}
	value := uuid.UUID(id.Bytes)
	return &value
}
