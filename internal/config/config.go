// Package config loads service configuration from environment variables and
// fails fast when required values are missing or invalid.
package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dezmymachine/farmish-backend/internal/turnstile"
)

// Env is the deployment environment.
type Env string

const (
	EnvDevelopment Env = "development"
	EnvTest        Env = "test"
	EnvStaging     Env = "staging"
	EnvProduction  Env = "production"
)

// RunMode selects what a process runs (RUN_MODE).
type RunMode string

const (
	RunAll    RunMode = "all"    // HTTP API + job workers (default, single process in v1)
	RunAPI    RunMode = "api"    // HTTP API only; jobs are enqueued, never worked
	RunWorker RunMode = "worker" // job workers only; HTTP serves just /healthz and /readyz
)

// ServesAPI reports whether the process serves the full HTTP API.
func (m RunMode) ServesAPI() bool { return m == RunAll || m == RunAPI }

// WorksJobs reports whether the process runs job workers.
func (m RunMode) WorksJobs() bool { return m == RunAll || m == RunWorker }

// Config is the fully validated service configuration.
type Config struct {
	Env             Env
	Port            int
	LogLevel        string
	CORSOrigins     []string
	ShutdownTimeout time.Duration
	DB              DB
	Firebase        Firebase
	RunMode         RunMode
	JobsMaxWorkers  int
	ClientIP        ClientIP
	TurnstileSecret string
	// RedisURL enables the shared (Upstash) rate-limit backend. Empty means
	// in-process limits only.
	RedisURL string
	// RedisTimeout bounds each shared rate-limit call before falling back to
	// in-process limits (REDIS_TIMEOUT). Keep Redis in the API's region so
	// the default holds.
	RedisTimeout time.Duration
	// DataEncryptionKey is the decoded DATA_ENCRYPTION_KEY (32 bytes) for
	// AES-256-GCM encryption at rest (ID and account numbers).
	DataEncryptionKey []byte
	// R2 configures media storage. Endpoint is empty for real R2 (built
	// from AccountID) and points at the local S3 stand-in in dev/test.
	R2 R2
	// Paystack configures the payment provider (Phase 13a).
	Paystack Paystack
}

// Paystack configures the Paystack API.
type Paystack struct {
	// SecretKey authenticates every call and signs webhook verification. It
	// must never be logged or returned in an error.
	SecretKey string
	// PublicKey is for clients only; the backend never calls anything with it.
	PublicKey string
	// BaseURL is the API root, overridable so tests point at an httptest
	// server.
	BaseURL string
	// CallbackURL is where Paystack returns the buyer after a payment: the
	// frontend's payment-status page.
	CallbackURL string
	// FeeBps is Paystack's Ghana processing fee in basis points, grossed up
	// onto the buyer (DOMAIN §2.2). 195 = 1.95%.
	FeeBps int
}

// R2 configures Cloudflare R2 (or any S3-compatible endpoint).
type R2 struct {
	AccountID     string
	AccessKeyID   string
	SecretKey     string
	Bucket        string
	PublicBaseURL string
	// Endpoint overrides the derived R2 endpoint (local S3 stand-in).
	Endpoint string
}

// Configured reports whether media storage is configured.
func (r R2) Configured() bool { return r.Bucket != "" && r.AccessKeyID != "" }

// DevDataEncryptionKey is the clearly labelled dev-only DATA_ENCRYPTION_KEY
// shipped in .env.example. Config refuses it in staging and production, the
// same way it refuses Cloudflare's Turnstile test secrets.
const DevDataEncryptionKey = "9MkjfhxyTr/ApaqBuztKBSrbWYo6kr/wIcg5/RfbmUs="

// ClientIP configures how the real client address is derived (rate limits,
// logs, Turnstile). See middleware.ClientIPResolver.
type ClientIP struct {
	// TrustedProxies are proxies in front of the API (the hosting platform's
	// edge) whose X-Forwarded-For entries are believed.
	TrustedProxies []netip.Prefix
	// TrustCloudflare believes CF-Connecting-IP when the request reached us
	// from a Cloudflare edge address.
	TrustCloudflare bool
}

// Firebase configures token verification and the Admin SDK.
type Firebase struct {
	ProjectID string
	// Credentials is the service-account JSON (read from a file if
	// FIREBASE_CREDENTIALS_JSON is a path). Empty means no credentials:
	// ID tokens can still be verified, but custom claims can't be set.
	Credentials []byte
	// EmulatorHost is FIREBASE_AUTH_EMULATOR_HOST. The SDK reads the env var
	// itself; it's recorded here so startup can warn and config can refuse it.
	EmulatorHost string
}

// DB configures the Postgres connection pool.
type DB struct {
	URL              string
	MaxConns         int32
	StatementTimeout time.Duration
}

// IsProduction reports whether the service runs in production.
func (c Config) IsProduction() bool { return c.Env == EnvProduction }

// Load reads configuration from the process environment.
func Load() (Config, error) {
	return FromLookup(os.LookupEnv)
}

// FromLookup reads configuration using lookup (os.LookupEnv in production,
// a map in tests). All problems are reported together.
func FromLookup(lookup func(string) (string, bool)) (Config, error) {
	var errs []error
	get := func(key string) string {
		v, _ := lookup(key)
		return strings.TrimSpace(v)
	}
	required := func(key string) string {
		v := get(key)
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", key))
		}
		return v
	}

	cfg := Config{
		Env:      Env(required("APP_ENV")),
		Port:     8080,
		LogLevel: "info",
		// Total SIGTERM-to-exit budget; keep it below the platform's kill
		// grace period (Docker's default is 10s).
		ShutdownTimeout: 9 * time.Second,
		RunMode:         RunAll,
		RedisTimeout:    200 * time.Millisecond,
		JobsMaxWorkers:  10,
		DB: DB{
			URL:              required("DATABASE_URL"),
			MaxConns:         10,
			StatementTimeout: 30 * time.Second,
		},
	}

	switch cfg.Env {
	case EnvDevelopment, EnvTest, EnvStaging, EnvProduction, "":
	default:
		errs = append(errs, fmt.Errorf("APP_ENV must be one of development|test|staging|production, got %q", cfg.Env))
	}

	if v := get("PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			errs = append(errs, fmt.Errorf("PORT must be an integer in 1..65535, got %q", v))
		} else {
			cfg.Port = p
		}
	}

	if v := get("LOG_LEVEL"); v != "" {
		switch strings.ToLower(v) {
		case "debug", "info", "warn", "error":
			cfg.LogLevel = strings.ToLower(v)
		default:
			errs = append(errs, fmt.Errorf("LOG_LEVEL must be one of debug|info|warn|error, got %q", v))
		}
	}

	if v := get("SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 3*time.Second {
			errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT must be a duration of at least 3s, got %q", v))
		} else {
			cfg.ShutdownTimeout = d
		}
	}

	if u := cfg.DB.URL; u != "" && !strings.HasPrefix(u, "postgres://") && !strings.HasPrefix(u, "postgresql://") {
		errs = append(errs, errors.New("DATABASE_URL must start with postgres:// or postgresql://"))
	}

	if v := get("DB_MAX_CONNS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 || n > 100 {
			errs = append(errs, fmt.Errorf("DB_MAX_CONNS must be an integer in 1..100, got %q", v))
		} else {
			cfg.DB.MaxConns = int32(n)
		}
	}

	if v := get("DB_STATEMENT_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("DB_STATEMENT_TIMEOUT must be a positive duration, got %q", v))
		} else {
			cfg.DB.StatementTimeout = d
		}
	}

	for _, o := range strings.Split(required("CORS_ORIGINS"), ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if o == "*" {
			errs = append(errs, errors.New("CORS_ORIGINS must be an explicit allowlist, not *"))
			continue
		}
		if !strings.HasPrefix(o, "http://") && !strings.HasPrefix(o, "https://") {
			errs = append(errs, fmt.Errorf("CORS_ORIGINS entry %q must start with http:// or https://", o))
			continue
		}
		cfg.CORSOrigins = append(cfg.CORSOrigins, strings.TrimRight(o, "/"))
	}

	if v := get("RUN_MODE"); v != "" {
		switch m := RunMode(strings.ToLower(v)); m {
		case RunAll, RunAPI, RunWorker:
			cfg.RunMode = m
		default:
			errs = append(errs, fmt.Errorf("RUN_MODE must be one of all|api|worker, got %q", v))
		}
	}

	if v := get("JOBS_MAX_WORKERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			errs = append(errs, fmt.Errorf("JOBS_MAX_WORKERS must be an integer in 1..200, got %q", v))
		} else {
			cfg.JobsMaxWorkers = n
		}
	}

	for _, c := range strings.Split(get("TRUSTED_PROXIES"), ",") {
		if c = strings.TrimSpace(c); c == "" {
			continue
		}
		p, err := netip.ParsePrefix(c)
		if err != nil {
			errs = append(errs, fmt.Errorf("TRUSTED_PROXIES entry %q is not a CIDR (e.g. 10.0.0.0/8)", c))
			continue
		}
		cfg.ClientIP.TrustedProxies = append(cfg.ClientIP.TrustedProxies, p.Masked())
	}
	if v := get("TRUST_CLOUDFLARE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("TRUST_CLOUDFLARE must be true or false, got %q", v))
		}
		cfg.ClientIP.TrustCloudflare = b
	}

	if v := get("REDIS_URL"); v != "" {
		switch {
		case strings.HasPrefix(v, "rediss://"):
		case strings.HasPrefix(v, "redis://"):
			if cfg.Env == EnvStaging || cfg.Env == EnvProduction {
				errs = append(errs, fmt.Errorf("REDIS_URL must use rediss:// (TLS) when APP_ENV=%s", cfg.Env))
			}
		default:
			// Never echo the value: it embeds the password.
			errs = append(errs, errors.New("REDIS_URL must start with redis:// or rediss://"))
		}
		cfg.RedisURL = v
	}

	if v := get("REDIS_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 10*time.Millisecond || d > 5*time.Second {
			errs = append(errs, fmt.Errorf("REDIS_TIMEOUT must be a duration between 10ms and 5s, got %q", v))
		} else {
			cfg.RedisTimeout = d
		}
	}

	cfg.TurnstileSecret = required("TURNSTILE_SECRET")
	if cfg.Env == EnvStaging || cfg.Env == EnvProduction {
		if turnstile.IsTestSecret(cfg.TurnstileSecret) {
			errs = append(errs, fmt.Errorf("TURNSTILE_SECRET is a Cloudflare test secret; not allowed when APP_ENV=%s", cfg.Env))
		}
	}

	keyB64 := required("DATA_ENCRYPTION_KEY")
	if keyB64 != "" {
		// Never echo the value: it encrypts ID and account numbers.
		key, err := base64.StdEncoding.DecodeString(keyB64)
		if err != nil || len(key) != 32 {
			errs = append(errs, errors.New("DATA_ENCRYPTION_KEY must be base64 of 32 bytes"))
		} else {
			cfg.DataEncryptionKey = key
		}
	}

	cfg.Firebase = Firebase{
		ProjectID:    required("FIREBASE_PROJECT_ID"),
		EmulatorHost: get("FIREBASE_AUTH_EMULATOR_HOST"),
	}
	deployed := cfg.Env == EnvStaging || cfg.Env == EnvProduction
	if deployed && keyB64 == DevDataEncryptionKey {
		errs = append(errs, fmt.Errorf("DATA_ENCRYPTION_KEY is the dev key; not allowed when APP_ENV=%s", cfg.Env))
	}

	// Media storage: required when deployed (Phase 10), optional locally so
	// the API still boots without a local S3. Never echo the secret.
	cfg.R2 = R2{
		AccountID:     get("R2_ACCOUNT_ID"),
		AccessKeyID:   get("R2_ACCESS_KEY_ID"),
		SecretKey:     get("R2_SECRET_ACCESS_KEY"),
		Bucket:        get("R2_BUCKET"),
		PublicBaseURL: strings.TrimRight(get("R2_PUBLIC_BASE_URL"), "/"),
		Endpoint:      strings.TrimRight(get("R2_ENDPOINT"), "/"),
	}
	if deployed {
		for name, value := range map[string]string{
			"R2_ACCOUNT_ID": cfg.R2.AccountID, "R2_ACCESS_KEY_ID": cfg.R2.AccessKeyID,
			"R2_SECRET_ACCESS_KEY": cfg.R2.SecretKey, "R2_BUCKET": cfg.R2.Bucket,
			"R2_PUBLIC_BASE_URL": cfg.R2.PublicBaseURL,
		} {
			if value == "" {
				errs = append(errs, fmt.Errorf("%s is required when APP_ENV=%s", name, cfg.Env))
			}
		}
		if cfg.R2.Endpoint != "" {
			errs = append(errs, fmt.Errorf("R2_ENDPOINT must not be set when APP_ENV=%s (it is for local S3 only)", cfg.Env))
		}
	}
	// Paystack: the key prefix has to match the environment, so a test key can
	// never reach production and a live key never reaches a dev database.
	cfg.Paystack = Paystack{
		SecretKey:   required("PAYSTACK_SECRET_KEY"),
		PublicKey:   required("PAYSTACK_PUBLIC_KEY"),
		BaseURL:     "https://api.paystack.co",
		CallbackURL: required("PAYSTACK_CALLBACK_URL"),
		FeeBps:      195,
	}
	if v := get("PAYSTACK_BASE_URL"); v != "" {
		cfg.Paystack.BaseURL = strings.TrimRight(v, "/")
	}
	wantSecret, wantPublic := "sk_test_", "pk_test_"
	if deployed {
		wantSecret, wantPublic = "sk_live_", "pk_live_"
	}
	for _, key := range []struct{ name, prefix, value string }{
		{"PAYSTACK_SECRET_KEY", wantSecret, cfg.Paystack.SecretKey},
		{"PAYSTACK_PUBLIC_KEY", wantPublic, cfg.Paystack.PublicKey},
	} {
		if key.value != "" && !strings.HasPrefix(key.value, key.prefix) {
			errs = append(errs, fmt.Errorf("%s must start with %s when APP_ENV=%s", key.name, key.prefix, cfg.Env))
		}
	}
	if v := get("PAYSTACK_FEE_BPS"); v != "" {
		bps, err := strconv.Atoi(v)
		switch {
		case err != nil || bps < 0 || bps > 1000:
			errs = append(errs, fmt.Errorf("PAYSTACK_FEE_BPS must be a basis-point rate between 0 and 1000, got %q", v))
		default:
			cfg.Paystack.FeeBps = bps
		}
	}

	if cfg.Firebase.EmulatorHost != "" && deployed {
		// Emulator mode skips token signature checks: never allow it when deployed.
		errs = append(errs, fmt.Errorf("FIREBASE_AUTH_EMULATOR_HOST must not be set when APP_ENV=%s", cfg.Env))
	}
	if v := get("FIREBASE_CREDENTIALS_JSON"); v != "" {
		creds, projectID, err := loadCredentials(v)
		switch {
		case err != nil:
			errs = append(errs, err)
		case cfg.Firebase.ProjectID != "" && projectID != cfg.Firebase.ProjectID:
			errs = append(errs, fmt.Errorf("FIREBASE_CREDENTIALS_JSON is for project %q, but FIREBASE_PROJECT_ID is %q",
				projectID, cfg.Firebase.ProjectID))
		}
		cfg.Firebase.Credentials = creds
	} else if deployed {
		errs = append(errs, fmt.Errorf("FIREBASE_CREDENTIALS_JSON is required when APP_ENV=%s", cfg.Env))
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// loadCredentials accepts inline service-account JSON or a path to the file.
func loadCredentials(v string) (data []byte, projectID string, err error) {
	data = []byte(v)
	if !strings.HasPrefix(v, "{") {
		b, err := os.ReadFile(v) //nolint:gosec // G304: operator-supplied path from the process environment, not request input
		if err != nil {
			return nil, "", fmt.Errorf("FIREBASE_CREDENTIALS_JSON: cannot read file %q: %w", v, err)
		}
		data = b
	}
	var sa struct {
		Type      string `json:"type"`
		ProjectID string `json:"project_id"`
	}
	// Never echo the content: it contains a private key.
	if err := json.Unmarshal(data, &sa); err != nil || sa.Type != "service_account" {
		return nil, "", errors.New("FIREBASE_CREDENTIALS_JSON is not a service-account JSON key")
	}
	return data, sa.ProjectID, nil
}
