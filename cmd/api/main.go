// Command api runs the Farmish HTTP API.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth"
	"github.com/dezmymachine/farmish-backend/internal/catalog"
	"github.com/dezmymachine/farmish-backend/internal/checkout"
	"github.com/dezmymachine/farmish-backend/internal/config"
	"github.com/dezmymachine/farmish-backend/internal/crypto"
	"github.com/dezmymachine/farmish-backend/internal/database"
	"github.com/dezmymachine/farmish-backend/internal/delivery"
	httpapi "github.com/dezmymachine/farmish-backend/internal/http"
	"github.com/dezmymachine/farmish-backend/internal/http/handlers"
	"github.com/dezmymachine/farmish-backend/internal/jobs"
	"github.com/dezmymachine/farmish-backend/internal/ledger"
	"github.com/dezmymachine/farmish-backend/internal/listings"
	"github.com/dezmymachine/farmish-backend/internal/media"
	"github.com/dezmymachine/farmish-backend/internal/notify"
	"github.com/dezmymachine/farmish-backend/internal/orders"
	"github.com/dezmymachine/farmish-backend/internal/payments"
	"github.com/dezmymachine/farmish-backend/internal/payouts"
	"github.com/dezmymachine/farmish-backend/internal/promotions"
	"github.com/dezmymachine/farmish-backend/internal/ratelimit"
	"github.com/dezmymachine/farmish-backend/internal/redisx"
	"github.com/dezmymachine/farmish-backend/internal/sellers"
	"github.com/dezmymachine/farmish-backend/internal/turnstile"
	"github.com/dezmymachine/farmish-backend/internal/users"
	"github.com/dezmymachine/farmish-backend/pkg/logger"
)

// shutdownBudget splits SHUTDOWN_TIMEOUT (the whole SIGTERM-to-exit budget,
// which must stay below the platform's kill grace period) into:
//   - drain: HTTP drain and job stop, which run concurrently
//   - jobsSoft/jobsHard: within drain, let running jobs finish, then cancel them
//   - dbClose: the final slice for closing the pool
func shutdownBudget(total time.Duration) (drain, jobsSoft, jobsHard, dbClose time.Duration) {
	dbClose = min(2*time.Second, total/4)
	drain = total - dbClose
	jobsSoft = drain * 2 / 3
	jobsHard = drain - jobsSoft
	return drain, jobsSoft, jobsHard, dbClose
}

// sharedLimiter returns the limiter for per-user and per-operation limits:
// Upstash Redis with an in-process fallback when REDIS_URL is set, otherwise
// in-process only. An unreachable Redis at startup doesn't block boot.
func sharedLimiter(ctx context.Context, cfg config.Config, log *slog.Logger) (ratelimit.Limiter, func(), error) {
	local := ratelimit.NewMemory()
	go local.RunSweeper(ctx, time.Minute)
	if cfg.RedisURL == "" {
		log.Info("rate limiter backend", "shared", "memory")
		return local, func() {}, nil
	}

	client, err := redisx.New(cfg.RedisURL)
	if err != nil {
		return nil, nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if rtt, err := redisx.Ping(pingCtx, client); err != nil {
		log.Warn("redis unreachable at startup; shared limits use in-process fallback until it recovers", "error", err.Error())
	} else {
		log.Info("redis connected", "rtt_ms", rtt.Milliseconds())
	}
	log.Info("rate limiter backend", "shared", "redis", "timeout_ms", cfg.RedisTimeout.Milliseconds())

	return &ratelimit.Fallback{
		Primary:   ratelimit.NewRedis(client, "farmish:"+string(cfg.Env)+":rl:"),
		Secondary: local,
		Timeout:   cfg.RedisTimeout,
		Log:       log,
	}, func() { _ = client.Close() }, nil
}

// registry lists every job this service knows. Each phase registers its
// workers and periodic jobs here. The orphan sweep is registered only when
// media storage is configured (it is required when deployed).
func registry(log *slog.Logger, mediaSvc *media.Service, listingsSvc *listings.Service,
	paymentsSvc *payments.Service, checkoutSvc *checkout.Service, ordersSvc *orders.Service,
	sender notify.SMS, pool *pgxpool.Pool,
) *jobs.Registry {
	r := jobs.NewRegistry()
	jobs.Register(r, &jobs.NoopWorker{Log: log})
	if mediaSvc != nil {
		media.RegisterCleanupOrphans(r, mediaSvc, log)
	}
	listings.RegisterExpireDue(r, listingsSvc, log)
	listings.RegisterCountView(r, listingsSvc)
	payments.RegisterSucceeded(r, paymentsSvc, log)
	checkout.RegisterJobs(r, checkoutSvc, log)
	orders.RegisterJobs(r, ordersSvc, log)
	ledger.RegisterReconcile(r, pool, ledger.New(), log)
	notify.Register(r, notify.NewWorker(sender, log, pool))
	return r
}

// mediaService builds the media service when storage is configured. Locally
// R2_* is optional, so this may be nil (the API still serves everything
// else; the upload endpoint and the sweep need storage).
func mediaService(pool *pgxpool.Pool, cfg config.Config, log *slog.Logger) (*media.Service, error) {
	if !cfg.R2.Configured() {
		log.Warn("media storage is not configured: POST /v1/media/upload-url and the orphan sweep are disabled")
		return nil, nil
	}
	storage, err := media.NewR2(cfg.R2)
	if err != nil {
		return nil, err
	}
	return media.New(pool, storage), nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logger.New(os.Stdout, cfg.LogLevel).With("service", "farmish-api", "env", string(cfg.Env))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	drain, jobsSoft, jobsHard, dbCloseTimeout := shutdownBudget(cfg.ShutdownTimeout)

	pool, err := database.Open(ctx, cfg.DB)
	if err != nil {
		return err
	}
	defer func() {
		if !database.Close(pool, dbCloseTimeout) {
			log.Warn("database pool did not close in time; exiting anyway", "timeout", dbCloseTimeout)
		}
	}()
	log.Info("database connected", "max_conns", cfg.DB.MaxConns)

	// Firebase, media storage and the domain services are needed by both the
	// job workers (the orphan sweep, the expiry sweep) and the API, so they
	// are built once, before either.
	firebase, err := auth.NewFirebase(ctx, cfg.Firebase)
	if err != nil {
		return err
	}
	if cfg.Firebase.EmulatorHost != "" {
		log.Warn("FIREBASE AUTH EMULATOR IN USE: token signatures are NOT verified", "host", cfg.Firebase.EmulatorHost)
	}
	crypter, err := crypto.New(cfg.DataEncryptionKey)
	if err != nil {
		return err
	}
	mediaSvc, err := mediaService(pool, cfg, log)
	if err != nil {
		return err
	}
	usersSvc := users.New(pool)
	sellersSvc := sellers.New(pool, crypter, firebase)
	listingsSvc := listings.New(pool, catalog.New(pool), mediaSvc, sellersSvc)
	paymentsSvc := payments.New(pool, payments.NewPaystackClient(cfg.Paystack.SecretKey, cfg.Paystack.BaseURL),
		log, cfg.Paystack.FeeBps, cfg.Paystack.CallbackURL)
	promotionsSvc := promotions.New(pool, paymentsSvc, listingsSvc, ledger.New())
	// A settled promotion payment grants credits through the same purpose-handler
	// mechanism Phase 13a defined. Register it before any worker can run.
	paymentsSvc.RegisterPurpose(payments.PurposePromotion, promotionsSvc.HandlePromotionPaid)
	ordersSvc := orders.NewService(pool, cfg.Fulfilment.SellerAcceptTimeout, cfg.Fulfilment.EscrowAutoComplete)
	// Escrow release and refunds (Phase 17a) need the ledger and a Paystack
	// client of their own. Attached before any worker can run.
	ordersSvc.AttachLedger(ledger.New())
	ordersSvc.AttachLogger(log)
	ordersSvc.AttachPaystack(payments.NewPaystackClient(cfg.Paystack.SecretKey, cfg.Paystack.BaseURL))
	checkoutSvc := checkout.New(pool, paymentsSvc, payments.NewPaystackClient(cfg.Paystack.SecretKey, cfg.Paystack.BaseURL),
		delivery.Manual{}, ledger.New(), ordersSvc, log, cfg.Paystack.FeeBps,
		time.Duration(cfg.CheckoutExpiryMinutes)*time.Minute)
	// Both purposes exist now: promotion grants credits (Phase 14), checkout
	// holds escrow (Phase 15b). Register before any worker can run.
	paymentsSvc.RegisterPurpose(payments.PurposeCheckout, checkoutSvc.HandleCheckoutPaid)
	// The refund webhooks (Phase 17a) drive orders.Service's own state;
	// payments owns event dispatch but not the escrow domain, so the handlers
	// are registered here, once ordersSvc exists and before any worker runs.
	orders.RegisterRefundEvents(paymentsSvc, ordersSvc)
	// Seller payout accounts (Phase 18a) resolve through Paystack like the
	// refund flow does, with their own client.
	payoutsSvc := payouts.New(pool, crypter, payments.NewPaystackClient(cfg.Paystack.SecretKey, cfg.Paystack.BaseURL), sellersSvc)
	payoutsSvc.AttachLogger(log)
	notifySender := notify.SMS(notify.LogOnly{Log: log})
	if cfg.Notify.SMSEnabled {
		notifySender = notify.NewMNotify(cfg.Notify.APIKey, cfg.Notify.Sender, "")
		log.Info("transactional SMS enabled", "sender", cfg.Notify.Sender)
	}

	jobClient, err := jobs.NewClient(pool, registry(log, mediaSvc, listingsSvc, paymentsSvc, checkoutSvc, ordersSvc, notifySender, pool), log, jobs.Options{
		Work:       cfg.RunMode.WorksJobs(),
		MaxWorkers: cfg.JobsMaxWorkers,
	})
	if err != nil {
		return err
	}
	// The payments service enqueues payments.succeeded inside the transaction
	// that settles a payment, which needs the client that now exists. Done
	// before the server starts, so no request can arrive in between.
	paymentsSvc.AttachJobClient(jobClient)
	checkoutSvc.AttachJobClient(jobClient)
	ordersSvc.AttachJobClient(jobClient)
	payoutsSvc.AttachJobClient(jobClient)

	if cfg.RunMode.WorksJobs() {
		// Not the signal context: cancelling Start's context would abort running
		// jobs immediately. Shutdown goes through jobs.Stop (soft, then hard).
		if err := jobClient.Start(context.WithoutCancel(ctx)); err != nil {
			return fmt.Errorf("start job workers: %w", err)
		}
		// Stop jobs as soon as SIGTERM arrives, concurrently with the HTTP drain.
		jobsStopped := make(chan struct{})
		go func() {
			defer close(jobsStopped)
			<-ctx.Done()
			if err := jobs.Stop(jobClient, jobsSoft, jobsHard, log); err != nil {
				log.Error("job workers did not stop cleanly", "error", err.Error())
			}
		}()
		defer func() { <-jobsStopped }()
		log.Info("job workers started", "max_workers", cfg.JobsMaxWorkers)
	}

	var router http.Handler
	if cfg.RunMode.ServesAPI() {
		ipLimiter := ratelimit.NewMemory()
		go ipLimiter.RunSweeper(ctx, time.Minute)
		shared, closeShared, err := sharedLimiter(ctx, cfg, log)
		if err != nil {
			return err
		}
		defer closeShared()
		router, err = httpapi.NewRouter(cfg, log, httpapi.Deps{
			DB:             pool,
			Verifier:       firebase,
			Users:          usersSvc,
			Sellers:        sellersSvc,
			Catalog:        catalog.New(pool),
			Media:          mediaSvc,
			Listings:       listingsSvc,
			PublicListings: listingsSvc,
			// View counting is best effort: with a job queue but no worker
			// process (api-only run mode) the views simply queue up.
			Views:         listings.NewViewCounter(jobClient),
			ViewerHash:    handlers.NewViewerHasher(cfg.DataEncryptionKey),
			Payments:      paymentsSvc,
			Promotions:    promotionsSvc,
			Checkout:      checkoutSvc,
			Orders:        ordersSvc,
			OrderActions:  ordersSvc,
			Payouts:       payoutsSvc,
			Turnstile:     turnstile.New(cfg.TurnstileSecret),
			IPLimiter:     ipLimiter,
			SharedLimiter: shared,
		})
		if err != nil {
			return err
		}
	} else {
		router = httpapi.NewProbeRouter(cfg, log, pool)
	}
	log.Info("starting", "run_mode", string(cfg.RunMode))
	addr := net.JoinHostPort("", strconv.Itoa(cfg.Port))
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	return httpapi.Serve(ctx, httpapi.NewServer(addr, router), ln, drain, log)
}
