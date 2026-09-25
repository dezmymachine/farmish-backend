// Package config loads service configuration from environment variables and
// fails fast when required values are missing or invalid.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Env is the deployment environment.
type Env string

const (
	EnvDevelopment Env = "development"
	EnvTest        Env = "test"
	EnvStaging     Env = "staging"
	EnvProduction  Env = "production"
)

// Config is the fully validated service configuration.
type Config struct {
	Env             Env
	Port            int
	LogLevel        string
	CORSOrigins     []string
	ShutdownTimeout time.Duration
	DB              DB
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
		Env:             Env(required("APP_ENV")),
		Port:            8080,
		LogLevel:        "info",
		ShutdownTimeout: 15 * time.Second,
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
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT must be a positive duration, got %q", v))
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

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}
