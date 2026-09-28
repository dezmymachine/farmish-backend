package engagement

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/dezmymachine/farmish-backend/internal/audit"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/validation"
)

// Report reasons, statuses and resolution actions (reports CHECKs).
const (
	ReportReasonSpam           = "spam"
	ReportReasonFraud          = "fraud"
	ReportReasonProhibitedItem = "prohibited_item"
	ReportReasonOffensive      = "offensive"
	ReportReasonWrongCategory  = "wrong_category"
	ReportReasonOther          = "other"

	ReportStatusOpen      = "open"
	ReportStatusActioned  = "actioned"
	ReportStatusDismissed = "dismissed"

	ReportActionNone           = "none"
	ReportActionSuspendListing = "suspend_listing"
	ReportActionHideReview     = "hide_review"
)

var (
	// ErrAlreadyReported means the reporter already has an open report on
	// this target.
	ErrAlreadyReported = errors.New("this target was already reported")
	// ErrReportNotOpen means the report is already resolved.
	ErrReportNotOpen = errors.New("report is not open")
)

// validReportReasons mirrors the reports CHECK.
var validReportReasons = map[string]bool{
	ReportReasonSpam: true, ReportReasonFraud: true, ReportReasonProhibitedItem: true,
	ReportReasonOffensive: true, ReportReasonWrongCategory: true, ReportReasonOther: true,
}

// validReportStatuses mirrors the status CHECK.
var validReportStatuses = map[string]bool{
	ReportStatusOpen: true, ReportStatusActioned: true, ReportStatusDismissed: true,
}

// validReportActions mirrors the action CHECK.
var validReportActions = map[string]bool{
	ReportActionNone: true, ReportActionSuspendListing: true, ReportActionHideReview: true,
}

// ListingStore suspends listings inside the report's own transaction.
type ListingStore interface {
	SuspendInTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) error
}

// Report is one row of the reports table.
type Report struct {
	ID             uuid.UUID
	ReporterID     uuid.UUID
	ListingID      *uuid.UUID
	ReportedUserID *uuid.UUID
	Reason         string
	Description    *string
	Status         string
	Action         *string
	ResolutionNote *string
	ResolvedBy     *uuid.UUID
	ResolvedAt     *time.Time
	CreatedAt      time.Time
}

// ReportTarget is the admin list's target summary: either a listing or a
// user, never contact fields.
type ReportTarget struct {
	Listing *ReportListing
	User    *ReportUser
}

// ReportListing identifies a reported listing.
type ReportListing struct {
	ID    uuid.UUID
	Title string
	Slug  string
}

// ReportUser identifies a reported user.
type ReportUser struct {
	ID          uuid.UUID
	DisplayName *string
}

// ReportView is a report with its target summary.
type ReportView struct {
	Report Report
	Target ReportTarget
}

// CreateReport records a report on exactly one target. A second open report
// on the same target is a 409; a dismissed one may be reported again.
func (s *Service) CreateReport(ctx context.Context, reporterID uuid.UUID, listingID, reportedUserID *uuid.UUID, reason, description string) (Report, error) {
	if (listingID == nil) == (reportedUserID == nil) {
		var invalid validation.Error
		invalid.Add("target", "exactly one of listingId or userId is required")
		return Report{}, invalid.OrNil()
	}
	var invalid validation.Error
	if !validReportReasons[reason] {
		invalid.Add("reason", "must be spam, fraud, prohibited_item, offensive, wrong_category or other")
	}
	trimmed := strings.TrimSpace(description)
	if reason == ReportReasonOther && trimmed == "" {
		invalid.Add("description", "is required when the reason is other")
	}
	if len(trimmed) > 1000 {
		invalid.Add("description", "must be at most 1000 characters")
	}
	if err := invalid.OrNil(); err != nil {
		return Report{}, err
	}
	if listingID != nil {
		if _, err := db.New(s.pool).GetListingForFavorite(ctx, *listingID); errors.Is(err, pgx.ErrNoRows) {
			return Report{}, fmt.Errorf("%w: %s", ErrNotFound, *listingID)
		} else if err != nil {
			return Report{}, fmt.Errorf("get listing: %w", err)
		}
	} else if _, err := s.users.Get(ctx, *reportedUserID); err != nil {
		return Report{}, fmt.Errorf("%w: %s", ErrNotFound, *reportedUserID)
	}
	row, err := db.New(s.pool).InsertReport(ctx, db.InsertReportParams{
		ReporterID: reporterID, ListingID: pgUUID(listingID), ReportedUserID: pgUUID(reportedUserID),
		Reason: reason, Description: orNil(trimmed),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Report{}, ErrAlreadyReported
	}
	if err != nil {
		return Report{}, fmt.Errorf("insert report: %w", err)
	}
	return fromReportRow(row), nil
}

// ListReports returns the moderation queue, oldest first.
func (s *Service) ListReports(ctx context.Context, status string, limit, offset int32) ([]ReportView, int64, error) {
	var filter *string
	if status != "" {
		if !validReportStatuses[status] {
			var invalid validation.Error
			invalid.Add("status", "must be open, actioned or dismissed")
			return nil, 0, invalid.OrNil()
		}
		filter = &status
	}
	rows, err := db.New(s.pool).ListReports(ctx, db.ListReportsParams{
		Status: filter, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list reports: %w", err)
	}
	total, err := db.New(s.pool).CountReports(ctx, filter)
	if err != nil {
		return nil, 0, fmt.Errorf("count reports: %w", err)
	}
	views := make([]ReportView, 0, len(rows))
	for _, row := range rows {
		view, err := s.reportView(ctx, fromReportRow(row))
		if err != nil {
			return nil, 0, err
		}
		views = append(views, view)
	}
	return views, total, nil
}

// reportView attaches the target summary to a report.
func (s *Service) reportView(ctx context.Context, report Report) (ReportView, error) {
	view := ReportView{Report: report}
	if report.ListingID != nil {
		var title, slug string
		if err := s.pool.QueryRow(ctx,
			`SELECT title, slug FROM listings WHERE id = $1`, *report.ListingID).Scan(&title, &slug); err != nil {
			return ReportView{}, fmt.Errorf("get reported listing: %w", err)
		}
		view.Target.Listing = &ReportListing{ID: *report.ListingID, Title: title, Slug: slug}
		return view, nil
	}
	user, err := s.users.Get(ctx, *report.ReportedUserID)
	if err != nil {
		return ReportView{}, fmt.Errorf("get reported user: %w", err)
	}
	view.Target.User = &ReportUser{ID: *report.ReportedUserID, DisplayName: user.DisplayName}
	return view, nil
}

// ResolveReport decides an open report: dismiss, act without a listing
// change, suspend the listing, or hide a named review — each with an audit
// event, in one transaction. hide_review names its review explicitly
// (owner decision).
func (s *Service) ResolveReport(ctx context.Context, adminID, reportID uuid.UUID, status, action, note string, reviewID *uuid.UUID, listings ListingStore) (Report, error) {
	var invalid validation.Error
	if status != ReportStatusActioned && status != ReportStatusDismissed {
		invalid.Add("status", "must be actioned or dismissed")
	}
	if !validReportActions[action] {
		invalid.Add("action", "must be none, suspend_listing or hide_review")
	}
	trimmed := strings.TrimSpace(note)
	if len(trimmed) < 1 || len(trimmed) > 1000 {
		invalid.Add("note", "must be between 1 and 1000 characters")
	}
	if action == ReportActionHideReview && reviewID == nil {
		invalid.Add("reviewId", "is required to hide a review")
	}
	if action != ReportActionHideReview && reviewID != nil {
		invalid.Add("reviewId", "is only allowed to hide a review")
	}
	if err := invalid.OrNil(); err != nil {
		return Report{}, err
	}
	var resolved db.Report
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		report, err := q.GetReportForUpdate(ctx, reportID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNotFound, reportID)
		}
		if err != nil {
			return fmt.Errorf("lock report: %w", err)
		}
		if report.Status != ReportStatusOpen {
			return ErrReportNotOpen
		}
		if action == ReportActionSuspendListing {
			if !report.ListingID.Valid {
				var bad validation.Error
				bad.Add("action", "suspend_listing needs a listing report")
				return bad.OrNil()
			}
			if err := listings.SuspendInTx(ctx, tx, uuid.UUID(report.ListingID.Bytes)); err != nil {
				return err
			}
		}
		if action == ReportActionHideReview {
			if err := s.hideReviewTx(ctx, tx, adminID, *reviewID, trimmed); err != nil {
				return err
			}
		}
		now := s.Now()
		row, err := q.ResolveReport(ctx, db.ResolveReportParams{
			ID: reportID, Status: status, Action: &action,
			ResolutionNote: &trimmed, ResolvedBy: pgtype.UUID{Bytes: adminID, Valid: true}, ResolvedAt: &now,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrReportNotOpen
		}
		if err != nil {
			return fmt.Errorf("resolve report: %w", err)
		}
		resolved = row
		return audit.Record(ctx, tx, audit.Event{
			ActorID: &adminID, Action: "report.resolve", TargetType: "report", TargetID: reportID.String(),
			Metadata: map[string]any{"status": status, "action": action, "note": trimmed},
		})
	})
	if err != nil {
		return Report{}, err
	}
	return fromReportRow(resolved), nil
}

// HideReview hides a review for moderation, with an audit event. An already
// hidden review cannot be hidden again.
func (s *Service) HideReview(ctx context.Context, adminID, reviewID uuid.UUID, reason string) (Review, error) {
	trimmed := strings.TrimSpace(reason)
	if len(trimmed) < 1 || len(trimmed) > 500 {
		var invalid validation.Error
		invalid.Add("reason", "must be between 1 and 500 characters")
		return Review{}, invalid.OrNil()
	}
	var hidden db.Review
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.hideReviewTx(ctx, tx, adminID, reviewID, trimmed); err != nil {
			return err
		}
		row, err := db.New(tx).GetReviewByID(ctx, reviewID)
		if err != nil {
			return fmt.Errorf("get hidden review: %w", err)
		}
		hidden = row
		return nil
	})
	if err != nil {
		return Review{}, err
	}
	return fromReviewRow(hidden), nil
}

// hideReviewTx hides a review inside the caller's transaction, shared by
// HideReview and report resolution.
func (s *Service) hideReviewTx(ctx context.Context, tx pgx.Tx, adminID, reviewID uuid.UUID, reason string) error {
	existing, err := db.New(tx).GetReviewByID(ctx, reviewID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, reviewID)
	}
	if err != nil {
		return fmt.Errorf("get review: %w", err)
	}
	if existing.HiddenAt != nil {
		return ErrAlreadyHidden
	}
	now := s.Now()
	if _, err := db.New(tx).SetReviewHidden(ctx, db.SetReviewHiddenParams{
		ID: reviewID, HiddenAt: &now,
	}); err != nil {
		return fmt.Errorf("hide review: %w", err)
	}
	return audit.Record(ctx, tx, audit.Event{
		ActorID: &adminID, Action: "review.hide", TargetType: "review", TargetID: reviewID.String(),
		Metadata: map[string]any{"reason": reason},
	})
}

// fromReportRow maps the generated row onto the domain type.
func fromReportRow(r db.Report) Report {
	return Report{
		ID: r.ID, ReporterID: r.ReporterID,
		ListingID:      pgOrNil(r.ListingID),
		ReportedUserID: pgOrNil(r.ReportedUserID),
		Reason:         r.Reason, Description: r.Description, Status: r.Status,
		Action: r.Action, ResolutionNote: r.ResolutionNote,
		ResolvedBy:     pgUserOrNil(r.ResolvedBy),
		ResolvedAt:     r.ResolvedAt, CreatedAt: r.CreatedAt,
	}
}

// pgUUID widens an optional id for the generated queries.
func pgUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}

// pgOrNil narrows a nullable id for the domain type.
func pgOrNil(id pgtype.UUID) *uuid.UUID {
	if !id.Valid {
		return nil
	}
	value := uuid.UUID(id.Bytes)
	return &value
}

// pgUserOrNil narrows a nullable actor id for the domain type.
func pgUserOrNil(id pgtype.UUID) *uuid.UUID {
	return pgOrNil(id)
}
