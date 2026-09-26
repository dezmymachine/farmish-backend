package payments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/db"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
)

// SucceededArgs applies a settled payment to its purpose. The webhook (or the
// verify fallback) enqueues it in the same transaction that settles the payment.
type SucceededArgs struct {
	PaymentID uuid.UUID `json:"paymentId"`
}

// Kind implements river.JobArgs.
func (SucceededArgs) Kind() string { return "payments.succeeded" }

// SucceededWorker applies a successful payment. It runs the purpose handler in
// its own transaction: the payment was settled in an earlier one, and a retry
// has to be safe on its own.
type SucceededWorker struct {
	river.WorkerDefaults[SucceededArgs]
	Service *Service
	Log     *slog.Logger
}

// Work implements river.Worker.
func (w *SucceededWorker) Work(ctx context.Context, job *river.Job[SucceededArgs]) error {
	return database.InTx(ctx, w.Service.pool, func(tx pgx.Tx) error {
		row, err := db.New(tx).GetPaymentByID(ctx, job.Args.PaymentID)
		if errors.Is(err, pgx.ErrNoRows) {
			// The payment row is never deleted, so this means the database was
			// reset. Retrying cannot help.
			w.Log.Warn("payments.succeeded for an unknown payment",
				slog.String("payment_id", job.Args.PaymentID.String()))
			return nil
		}
		if err != nil {
			return fmt.Errorf("load payment: %w", err)
		}
		payment := fromRow(row)
		if payment.Status != StatusSuccess {
			// Only a settled payment is applied. A refunded or reversed one
			// later (Phase 17) is a different job.
			w.Log.Info("payments.succeeded skipped: payment is not settled",
				slog.String("reference", payment.Reference), slog.String("status", payment.Status))
			return nil
		}
		return w.Service.runPurposeHandler(ctx, tx, payment)
	})
}

// RegisterSucceeded adds the worker.
func RegisterSucceeded(r *jobs.Registry, svc *Service, log *slog.Logger) {
	jobs.Register(r, &SucceededWorker{Service: svc, Log: log})
}
