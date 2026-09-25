package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/database/dbtest"
)

// /readyz against real pgx pools: a live database and an unreachable one.
func TestReadyz_RealPostgres(t *testing.T) {
	live := dbtest.Pool(t)

	dead, err := pgxpool.New(context.Background(), "postgres://farmish:farmish@127.0.0.1:1/farmish?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dead.Close)

	for name, tc := range map[string]struct {
		pool *pgxpool.Pool
		want int
	}{
		"db up":   {live, http.StatusOK},
		"db down": {dead, http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			testRouterWith(t, tc.pool).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}
