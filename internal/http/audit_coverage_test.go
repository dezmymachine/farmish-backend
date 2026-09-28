package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dezmymachine/farmish-backend/internal/auth/authtest"
	"github.com/dezmymachine/farmish-backend/internal/http/api"
)

// requiredAuditActions is the Phase 21 audit list, one entry per action
// audit.Record must leave behind. user.role_change is written by the operator
// CLI, so cmd/admin's own test proves it.
var requiredAuditActions = []string{
	"payment.succeeded",
	"payment.amount_mismatch",
	"refund.queued", "refund.processed", "refund.failed",
	"payout.created", "payout.succeeded", "payout.failed",
	"payout_account.set", "payout_account.approve",
	"dispute.opened", "dispute.resolve",
	"seller.verify", "seller.reject",
	"report.resolve",
	"supply_request.transition",
}

// TestAudit_Coverage drives every audited flow through the real endpoints and
// workers and asserts each action left an audit row that names its target. A
// group per fixture; the table at the end fails if the groups stop covering
// the required list.
func TestAudit_Coverage(t *testing.T) {
	groups := []struct {
		name    string
		actions []string
		drive   func(t *testing.T) *pgxpool.Pool
	}{
		{
			name: "orders, payments, refunds and disputes",
			actions: []string{
				"payment.succeeded", "payment.amount_mismatch",
				"refund.queued", "refund.processed", "refund.failed",
				"dispute.opened", "dispute.resolve",
			},
			drive: func(t *testing.T) *pgxpool.Pool {
				f := newAdminFixture(t)
				driveOrderMoney(t, f)
				return f.pool
			},
		},
		{
			name: "payouts and payout accounts",
			actions: []string{
				"payout.created", "payout.succeeded", "payout.failed",
				"payout_account.set", "payout_account.approve",
			},
			drive: func(t *testing.T) *pgxpool.Pool {
				f := newExecPayoutFixture(t)
				drivePayouts(t, f)
				return f.pool
			},
		},
		{
			name:    "seller verification",
			actions: []string{"seller.verify", "seller.reject"},
			drive: func(t *testing.T) *pgxpool.Pool {
				r, pool, fb := sellersRouter(t)
				_, adminTok := authAdmin(t, pool, fb)
				for decision, body := range map[string]string{
					"approve": `{"decision":"approve"}`,
					"reject":  `{"decision":"reject","reason":"Blurry photo"}`,
				} {
					u, tok := authUser(t, pool, fb)
					put := jsonRequest(http.MethodPut, "/v1/me/seller-profile", tok, validProfileBody)
					if w := serve(t, r, put); w.Code != http.StatusOK {
						t.Fatalf("profile: %d %s", w.Code, w.Body.String())
					}
					req := jsonRequest(http.MethodPost, "/v1/admin/sellers/"+u.ID.String()+"/verification", adminTok, body)
					if w := serve(t, r, req); w.Code != http.StatusOK {
						t.Fatalf("%s: %d %s", decision, w.Code, w.Body.String())
					}
				}
				return pool
			},
		},
		{
			name:    "moderation and supply",
			actions: []string{"report.resolve", "supply_request.transition"},
			drive: func(t *testing.T) *pgxpool.Pool {
				f := newReportSupplyFixture(t)
				_, adminTok := authAdmin(t, f.pool, authtest.Firebase(t))

				_, _, listingID := f.activeListing(t, "audit-cov-seller@example.com", "Audit maize")
				reporterTok, _ := f.plainToken(t)
				req := jsonRequest(http.MethodPost, "/v1/reports", reporterTok,
					`{"listingId": "`+listingID.String()+`", "reason": "prohibited_item", "description": "Banned goods."}`)
				w := serve(t, f.router, req)
				if w.Code != http.StatusCreated {
					t.Fatalf("report: %d %s", w.Code, w.Body.String())
				}
				var report api.Report
				if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
					t.Fatal(err)
				}
				req = jsonRequest(http.MethodPost, "/v1/admin/reports/"+report.Id.String()+"/resolve", adminTok,
					`{"status": "dismissed", "action": "none", "note": "Checked, fine."}`)
				if w := serve(t, f.router, req); w.Code != http.StatusOK {
					t.Fatalf("resolve report: %d %s", w.Code, w.Body.String())
				}

				userTok, _ := f.plainToken(t)
				req = jsonRequest(http.MethodPost, "/v1/supply-requests", userTok,
					`{"items": [{"categorySlug": "fresh-produce", "productName": "Tomatoes", "quantity": 10, "unit": "kg"}],
					  "deliveryAddress": "Plot 12, Kumasi", "expectedDate": "2099-01-01"}`)
				w = serve(t, f.router, req)
				if w.Code != http.StatusCreated {
					t.Fatalf("supply: %d %s", w.Code, w.Body.String())
				}
				var created api.SupplyRequest
				if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
					t.Fatal(err)
				}
				req = jsonRequest(http.MethodPost, "/v1/admin/supply-requests/"+created.Id.String()+"/status", adminTok,
					`{"status": "confirmed", "note": "On it."}`)
				if w := serve(t, f.router, req); w.Code != http.StatusOK {
					t.Fatalf("transition: %d %s", w.Code, w.Body.String())
				}
				return f.pool
			},
		},
	}

	var covered []string
	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) {
			pool := g.drive(t)
			for _, action := range g.actions {
				var rows, blank int
				if err := pool.QueryRow(context.Background(),
					`SELECT count(*), count(*) FILTER (WHERE target_type = '' OR target_id = '')
					 FROM audit_events WHERE action = $1`, action).Scan(&rows, &blank); err != nil {
					t.Fatal(err)
				}
				if rows == 0 {
					t.Errorf("%s: no audit row", action)
				}
				if blank != 0 {
					t.Errorf("%s: %d rows without a target", action, blank)
				}
			}
		})
		covered = append(covered, g.actions...)
	}

	slices.Sort(covered)
	want := slices.Clone(requiredAuditActions)
	slices.Sort(want)
	if !slices.Equal(covered, want) {
		t.Errorf("the groups cover %v, the Phase 21 list is %v", covered, want)
	}
}
