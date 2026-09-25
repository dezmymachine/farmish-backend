package config

import (
	"strings"
	"testing"
	"time"
)

func lookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestFromLookup_Defaults(t *testing.T) {
	cfg, err := FromLookup(lookup(map[string]string{
		"APP_ENV":      "development",
		"CORS_ORIGINS": "http://localhost:3000, https://farmish.gh/",
		"DATABASE_URL": "postgres://u:p@localhost:5432/farmish",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != 8080 || cfg.LogLevel != "info" || cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	want := []string{"http://localhost:3000", "https://farmish.gh"}
	if strings.Join(cfg.CORSOrigins, ",") != strings.Join(want, ",") {
		t.Errorf("CORSOrigins = %v, want %v", cfg.CORSOrigins, want)
	}
	if cfg.IsProduction() {
		t.Error("development config reported as production")
	}
	if cfg.DB.MaxConns != 10 || cfg.DB.StatementTimeout != 30*time.Second {
		t.Errorf("unexpected DB defaults: %+v", cfg.DB)
	}
}

func TestFromLookup_Overrides(t *testing.T) {
	cfg, err := FromLookup(lookup(map[string]string{
		"APP_ENV":              "production",
		"PORT":                 "9000",
		"LOG_LEVEL":            "DEBUG",
		"SHUTDOWN_TIMEOUT":     "5s",
		"CORS_ORIGINS":         "https://farmish.gh",
		"DATABASE_URL":         "postgresql://u:p@db:5432/farmish",
		"DB_MAX_CONNS":         "25",
		"DB_STATEMENT_TIMEOUT": "2s",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != 9000 || cfg.LogLevel != "debug" || cfg.ShutdownTimeout != 5*time.Second || !cfg.IsProduction() {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	if cfg.DB.MaxConns != 25 || cfg.DB.StatementTimeout != 2*time.Second {
		t.Errorf("DB overrides not applied: %+v", cfg.DB)
	}
}

func TestFromLookup_Invalid(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"missing required", map[string]string{}, []string{"APP_ENV is required", "CORS_ORIGINS is required", "DATABASE_URL is required"}},
		{"bad db url", map[string]string{"APP_ENV": "test", "CORS_ORIGINS": "https://a.gh", "DATABASE_URL": "mysql://x"}, []string{"DATABASE_URL must start with"}},
		{"bad max conns", map[string]string{"APP_ENV": "test", "CORS_ORIGINS": "https://a.gh", "DATABASE_URL": "postgres://x", "DB_MAX_CONNS": "0"}, []string{"DB_MAX_CONNS must be"}},
		{"bad statement timeout", map[string]string{"APP_ENV": "test", "CORS_ORIGINS": "https://a.gh", "DATABASE_URL": "postgres://x", "DB_STATEMENT_TIMEOUT": "soon"}, []string{"DB_STATEMENT_TIMEOUT must be"}},
		{"bad env", map[string]string{"APP_ENV": "prod", "CORS_ORIGINS": "https://a.gh"}, []string{"APP_ENV must be one of"}},
		{"bad port", map[string]string{"APP_ENV": "test", "PORT": "99999", "CORS_ORIGINS": "https://a.gh"}, []string{"PORT must be"}},
		{"bad log level", map[string]string{"APP_ENV": "test", "LOG_LEVEL": "loud", "CORS_ORIGINS": "https://a.gh"}, []string{"LOG_LEVEL must be"}},
		{"bad timeout", map[string]string{"APP_ENV": "test", "SHUTDOWN_TIMEOUT": "-1s", "CORS_ORIGINS": "https://a.gh"}, []string{"SHUTDOWN_TIMEOUT must be"}},
		{"wildcard cors", map[string]string{"APP_ENV": "test", "CORS_ORIGINS": "*"}, []string{"not *"}},
		{"cors no scheme", map[string]string{"APP_ENV": "test", "CORS_ORIGINS": "farmish.gh"}, []string{"must start with http"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := FromLookup(lookup(tt.env))
			if err == nil {
				t.Fatal("expected error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}
