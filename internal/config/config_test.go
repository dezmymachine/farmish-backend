package config

import (
	"os"
	"path/filepath"
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
		"APP_ENV":             "development",
		"CORS_ORIGINS":        "http://localhost:3000, https://farmish.gh/",
		"DATABASE_URL":        "postgres://u:p@localhost:5432/farmish",
		"FIREBASE_PROJECT_ID": "farmish-dev",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != 8080 || cfg.LogLevel != "info" || cfg.ShutdownTimeout != 9*time.Second {
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
		"APP_ENV":                   "production",
		"PORT":                      "9000",
		"LOG_LEVEL":                 "DEBUG",
		"SHUTDOWN_TIMEOUT":          "5s",
		"CORS_ORIGINS":              "https://farmish.gh",
		"DATABASE_URL":              "postgresql://u:p@db:5432/farmish",
		"DB_MAX_CONNS":              "25",
		"DB_STATEMENT_TIMEOUT":      "2s",
		"FIREBASE_PROJECT_ID":       "farmish-prod",
		"FIREBASE_CREDENTIALS_JSON": `{"type":"service_account","project_id":"farmish-prod"}`,
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
		{"missing required", map[string]string{}, []string{"APP_ENV is required", "CORS_ORIGINS is required", "DATABASE_URL is required", "FIREBASE_PROJECT_ID is required"}},
		{"bad db url", map[string]string{"APP_ENV": "test", "CORS_ORIGINS": "https://a.gh", "DATABASE_URL": "mysql://x", "FIREBASE_PROJECT_ID": "p"}, []string{"DATABASE_URL must start with"}},
		{"bad max conns", map[string]string{"APP_ENV": "test", "CORS_ORIGINS": "https://a.gh", "DATABASE_URL": "postgres://x", "DB_MAX_CONNS": "0"}, []string{"DB_MAX_CONNS must be"}},
		{"bad statement timeout", map[string]string{"APP_ENV": "test", "CORS_ORIGINS": "https://a.gh", "DATABASE_URL": "postgres://x", "DB_STATEMENT_TIMEOUT": "soon"}, []string{"DB_STATEMENT_TIMEOUT must be"}},
		{"bad env", map[string]string{"APP_ENV": "prod", "CORS_ORIGINS": "https://a.gh"}, []string{"APP_ENV must be one of"}},
		{"bad port", map[string]string{"APP_ENV": "test", "PORT": "99999", "CORS_ORIGINS": "https://a.gh"}, []string{"PORT must be"}},
		{"bad log level", map[string]string{"APP_ENV": "test", "LOG_LEVEL": "loud", "CORS_ORIGINS": "https://a.gh"}, []string{"LOG_LEVEL must be"}},
		{"bad timeout", map[string]string{"APP_ENV": "test", "SHUTDOWN_TIMEOUT": "-1s", "CORS_ORIGINS": "https://a.gh"}, []string{"SHUTDOWN_TIMEOUT must be"}},
		{"timeout too short", map[string]string{"APP_ENV": "test", "SHUTDOWN_TIMEOUT": "2s", "CORS_ORIGINS": "https://a.gh"}, []string{"at least 3s"}},
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

func base(extra map[string]string) map[string]string {
	m := map[string]string{
		"APP_ENV":             "development",
		"CORS_ORIGINS":        "https://farmish.gh",
		"DATABASE_URL":        "postgres://x",
		"FIREBASE_PROJECT_ID": "farmish-dev",
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

const saJSON = `{"type":"service_account","project_id":"farmish-dev","private_key":"-----BEGIN PRIVATE KEY-----\nsecret\n"}`

func TestFirebase_CredentialsInlineAndFile(t *testing.T) {
	cfg, err := FromLookup(lookup(base(map[string]string{"FIREBASE_CREDENTIALS_JSON": saJSON})))
	if err != nil {
		t.Fatal(err)
	}
	if string(cfg.Firebase.Credentials) != saJSON {
		t.Error("inline credentials not loaded")
	}

	path := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(path, []byte(saJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = FromLookup(lookup(base(map[string]string{"FIREBASE_CREDENTIALS_JSON": path})))
	if err != nil {
		t.Fatal(err)
	}
	if string(cfg.Firebase.Credentials) != saJSON {
		t.Error("credentials file not loaded")
	}

	// Credentials and the emulator are optional in development.
	cfg, err = FromLookup(lookup(base(map[string]string{"FIREBASE_AUTH_EMULATOR_HOST": "127.0.0.1:9099"})))
	if err != nil || cfg.Firebase.Credentials != nil || cfg.Firebase.EmulatorHost != "127.0.0.1:9099" {
		t.Errorf("dev without credentials: cfg %+v, err %v", cfg.Firebase, err)
	}
}

func TestFirebase_Invalid(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"emulator in production", map[string]string{"APP_ENV": "production", "FIREBASE_AUTH_EMULATOR_HOST": "x:9099", "FIREBASE_CREDENTIALS_JSON": saJSON}, "FIREBASE_AUTH_EMULATOR_HOST must not be set"},
		{"emulator in staging", map[string]string{"APP_ENV": "staging", "FIREBASE_AUTH_EMULATOR_HOST": "x:9099", "FIREBASE_CREDENTIALS_JSON": saJSON}, "FIREBASE_AUTH_EMULATOR_HOST must not be set"},
		{"no credentials in production", map[string]string{"APP_ENV": "production"}, "FIREBASE_CREDENTIALS_JSON is required"},
		{"missing file", map[string]string{"FIREBASE_CREDENTIALS_JSON": "/nonexistent/sa.json"}, "cannot read file"},
		{"not a service account", map[string]string{"FIREBASE_CREDENTIALS_JSON": `{"type":"authorized_user"}`}, "not a service-account"},
		{"wrong project", map[string]string{"FIREBASE_PROJECT_ID": "other", "FIREBASE_CREDENTIALS_JSON": saJSON}, `is for project "farmish-dev"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := FromLookup(lookup(base(tt.env)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Error("error leaks credential content")
			}
		})
	}
}

func TestRunMode(t *testing.T) {
	cfg, err := FromLookup(lookup(base(nil)))
	if err != nil || cfg.RunMode != RunAll || cfg.JobsMaxWorkers != 10 {
		t.Fatalf("defaults: %v %d %v", cfg.RunMode, cfg.JobsMaxWorkers, err)
	}
	for mode, want := range map[string][2]bool{"all": {true, true}, "API": {true, false}, "worker": {false, true}} {
		cfg, err := FromLookup(lookup(base(map[string]string{"RUN_MODE": mode, "JOBS_MAX_WORKERS": "4"})))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.RunMode.ServesAPI() != want[0] || cfg.RunMode.WorksJobs() != want[1] || cfg.JobsMaxWorkers != 4 {
			t.Errorf("%s: serves=%t works=%t workers=%d", mode, cfg.RunMode.ServesAPI(), cfg.RunMode.WorksJobs(), cfg.JobsMaxWorkers)
		}
	}
	for env, want := range map[string]string{"RUN_MODE": "RUN_MODE must be", "JOBS_MAX_WORKERS": "JOBS_MAX_WORKERS must be"} {
		if _, err := FromLookup(lookup(base(map[string]string{env: "0"}))); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s=0: err %v", env, err)
		}
	}
}
