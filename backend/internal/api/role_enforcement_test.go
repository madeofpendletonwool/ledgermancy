package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
)

// Per-route role enforcement.
//
// Doc 21: "A `child` session gets 403/404 on every restricted endpoint, tested
// individually rather than by sampling. Include the pre-existing routes."
//
// The point of enumerating rather than sampling is that a role checked on some
// routes and not others is worse than no role at all — it implies protection
// that is not there. This test is also the tripwire for new routes: adding one
// to an adult-only group without listing it here is fine, but adding a NEW
// top-level group and forgetting auth.RequireAdult will not be caught by
// anything else.

// childRoutes is every endpoint a child login must be refused.
//
// Grouped by the route group they live in, in the order server.go declares
// them, so this list can be diffed against Routes() by eye.
var childRoutes = []struct{ method, path string }{
	// Household administration.
	{"GET", "/api/household/members"},
	{"GET", "/api/household/invites"},
	{"POST", "/api/household/invites"},
	{"DELETE", "/api/household/invites/" + uuid.Nil.String()},
	{"GET", "/api/household/people"},
	{"POST", "/api/household/people"},
	{"PUT", "/api/household/people/" + uuid.Nil.String()},
	{"DELETE", "/api/household/people/" + uuid.Nil.String()},
	{"GET", "/api/household/people/" + uuid.Nil.String() + "/allowance"},
	{"PUT", "/api/household/people/" + uuid.Nil.String() + "/allowance"},
	{"GET", "/api/household/people/" + uuid.Nil.String() + "/allowance/entries"},
	{"POST", "/api/household/people/" + uuid.Nil.String() + "/allowance/entries"},
	{"DELETE", "/api/household/allowance/entries/" + uuid.Nil.String()},
	{"PUT", "/api/household/members/" + uuid.Nil.String() + "/role"},

	// The operator surface. Continuity describes the instance's recovery
	// posture — host paths, backup state — and can trigger a database dump.
	{"GET", "/api/admin/continuity"},
	{"POST", "/api/admin/continuity/key-ack"},
	{"POST", "/api/admin/continuity/run"},
	{"GET", "/api/admin/continuity/export"},
	{"GET", "/api/admin/status"},

	// Delivery, and the stored digests it produces.
	{"POST", "/api/notifications/test"},
	{"POST", "/api/digest/test"},
	{"GET", "/api/digests/"},
	{"GET", "/api/digests/" + uuid.Nil.String()},
	{"POST", "/api/digests/" + uuid.Nil.String() + "/read"},

	// Institutions: linking and unlinking is not a child's to do.
	{"POST", "/api/plaid/link-token"},
	{"POST", "/api/plaid/exchange"},
	{"GET", "/api/plaid/items"},
	{"PATCH", "/api/plaid/items/" + uuid.Nil.String() + "/sharing"},
	{"DELETE", "/api/plaid/items/" + uuid.Nil.String()},

	// The household ledger and everything derived from it.
	{"GET", "/api/accounts/"},
	{"GET", "/api/transactions/"},
	{"POST", "/api/transactions/"},
	{"POST", "/api/transactions/import"},
	{"POST", "/api/transactions/bulk/tags"},
	{"POST", "/api/transactions/bulk/category"},
	{"POST", "/api/transactions/bulk/flags"},
	{"GET", "/api/merchants/"},
	{"GET", "/api/documents/"},
	{"POST", "/api/documents/"},
	{"GET", "/api/categories/"},
	{"POST", "/api/categories/"},
	{"GET", "/api/budgets/"},
	{"POST", "/api/budgets/"},
	{"GET", "/api/obligations/"},
	{"POST", "/api/obligations/"},

	// Payroll. A child has no salary in this app and no business reading a
	// parent's — and unlike the goals group below there is no read a child is
	// entitled to here, so the whole group is refused.
	//
	// Adult-only is not the whole access story for these routes: a paystub is
	// private to the person whose pay it is, and one adult reading another's is
	// refused per row in SQL rather than by this guard.
	{"GET", "/api/payroll/taxonomy"},
	{"GET", "/api/payroll/years"},
	{"GET", "/api/payroll/summary"},
	{"GET", "/api/payroll/savings-rate"},
	{"GET", "/api/payroll/tax-summary"},
	{"POST", "/api/payroll/parse"},
	{"POST", "/api/payroll/parse-document"},
	{"GET", "/api/payroll/employers"},
	{"POST", "/api/payroll/employers"},
	{"PUT", "/api/payroll/employers/" + uuid.Nil.String()},
	{"DELETE", "/api/payroll/employers/" + uuid.Nil.String()},
	{"GET", "/api/payroll/paystubs"},
	{"POST", "/api/payroll/paystubs"},
	{"GET", "/api/payroll/paystubs/" + uuid.Nil.String()},
	{"PUT", "/api/payroll/paystubs/" + uuid.Nil.String()},
	{"DELETE", "/api/payroll/paystubs/" + uuid.Nil.String()},
	{"POST", "/api/payroll/paystubs/" + uuid.Nil.String() + "/confirm"},
	{"PATCH", "/api/payroll/paystubs/" + uuid.Nil.String() + "/sharing"},
	{"GET", "/api/payroll/paystubs/" + uuid.Nil.String() + "/deposit-matches"},
	{"PUT", "/api/payroll/paystubs/" + uuid.Nil.String() + "/deposit"},

	// Goals: reads are visibility-scoped and allowed; writes are not.
	{"POST", "/api/goals/"},
	{"POST", "/api/goals/parse"},
	{"PUT", "/api/goals/" + uuid.Nil.String()},
	{"DELETE", "/api/goals/" + uuid.Nil.String()},
	{"POST", "/api/goals/" + uuid.Nil.String() + "/contributions"},
	{"DELETE", "/api/goals/contributions/" + uuid.Nil.String()},

	// Bill split: a split is a claim on another member's money.
	{"GET", "/api/splits/"},
	{"GET", "/api/splits/ledger"},
	{"GET", "/api/splits/transactions/" + uuid.Nil.String()},
	{"PUT", "/api/splits/transactions/" + uuid.Nil.String()},
	{"DELETE", "/api/splits/transactions/" + uuid.Nil.String()},
	{"POST", "/api/splits/" + uuid.Nil.String() + "/settle"},
	{"DELETE", "/api/splits/" + uuid.Nil.String() + "/settle"},

	// Household net worth: explicitly forbidden by the doc.
	{"GET", "/api/networth/"},
	{"GET", "/api/networth/history"},
	{"POST", "/api/networth/snapshot"},
	{"GET", "/api/networth/projection"},
	{"GET", "/api/networth/by-person"},

	// Projections over household assets.
	{"GET", "/api/projections/assumptions"},
	{"PUT", "/api/projections/assumptions"},
	{"GET", "/api/projections/contributions"},
	{"GET", "/api/projections/retirement"},

	// Investments and everything beside them.
	{"GET", "/api/holdings/"},
	{"GET", "/api/investments/"},
	{"GET", "/api/investments/performance"},
	{"GET", "/api/investments/allocation"},
	{"GET", "/api/investments/holdings"},
	{"PATCH", "/api/investments/accounts/" + uuid.Nil.String() + "/tax-treatment"},
	{"PATCH", "/api/investments/accounts/" + uuid.Nil.String() + "/beneficiary"},
	{"GET", "/api/liabilities/"},
	// A child must not set the rate and payment every payoff plan in the
	// household is computed from.
	{"PUT", "/api/accounts/" + uuid.Nil.String() + "/terms"},
	{"GET", "/api/manual-assets/"},
	{"POST", "/api/manual-assets/"},

	// Export is a bulk read of everything above.
	{"GET", "/api/export/transactions.csv"},
	{"GET", "/api/export/categories.csv"},
	{"GET", "/api/export/net-worth.csv"},
	{"GET", "/api/export/holdings.csv"},

	// Reporting.
	{"GET", "/api/reports/summary"},
	{"GET", "/api/reports/by-category"},
	{"GET", "/api/reports/cash-flow"},
	{"GET", "/api/reports/trend"},
	{"GET", "/api/reports/merchants"},
	{"GET", "/api/reports/merchant-explorer"},

	// The per-category breakdown. Nested under a category id, so it is worth
	// listing separately from /api/categories/ above: a route added inside an
	// existing group inherits its guard, but nothing proves that until it is here.
	{"GET", "/api/categories/" + uuid.Nil.String() + "/detail"},

	// The assistant reads household data by design.
	{"POST", "/api/chat"},

	// The advisor. Every route here reads the household's whole financial
	// position — debts, salary, retirement balances — into one response, and a
	// saved transcript is the household narrating its money in prose. The
	// ranker (doc 24) and the surface (doc 31) are listed together because they
	// share one route group and one reason to be adult-only.
	{"GET", "/api/advisor/"},
	{"GET", "/api/advisor/briefing"},
	{"GET", "/api/advisor/threads"},
	{"POST", "/api/advisor/threads"},
	{"GET", "/api/advisor/threads/" + uuid.Nil.String()},
	{"PATCH", "/api/advisor/threads/" + uuid.Nil.String()},
	{"DELETE", "/api/advisor/threads/" + uuid.Nil.String()},
	{"GET", "/api/advisor/action-items"},
	{"POST", "/api/advisor/action-items"},
	{"PATCH", "/api/advisor/action-items/" + uuid.Nil.String()},

	// Filing status is household tax information.
	{"GET", "/api/household/profile/"},
	{"PUT", "/api/household/profile/"},

	// The household insight feed and alerts.
	{"GET", "/api/insights/"},
	{"GET", "/api/insights/anomaly/suppressed"},
	{"POST", "/api/insights/anomaly/suppress"},
	{"DELETE", "/api/insights/anomaly/suppress"},
	{"GET", "/api/alerts/"},
	{"POST", "/api/alerts/"},
	{"GET", "/api/alerts/events"},

	// Outgoing webhooks: a standing instruction to send this household's events
	// to an address somebody chose. A child login has no business wiring the
	// family's finances to a destination of its own, and the delivery inspector
	// is a verbatim record of what was sent — so the whole group is refused,
	// reads included.
	//
	// A new TOP-LEVEL route group is exactly the case this test's header calls
	// out as caught by nothing else, so every route is listed rather than one
	// representative.
	{"GET", "/api/webhooks/"},
	{"POST", "/api/webhooks/"},
	{"GET", "/api/webhooks/triggers"},
	{"PUT", "/api/webhooks/" + uuid.Nil.String()},
	{"DELETE", "/api/webhooks/" + uuid.Nil.String()},
	{"POST", "/api/webhooks/" + uuid.Nil.String() + "/secret"},
	{"POST", "/api/webhooks/" + uuid.Nil.String() + "/test"},
	{"GET", "/api/webhooks/" + uuid.Nil.String() + "/messages"},
	{"GET", "/api/webhooks/" + uuid.Nil.String() + "/messages/" + uuid.Nil.String() + "/attempts"},
}

// childAllowedRoutes is the complement: the surface a child login MUST reach.
// A guard that refuses these has locked the child out of their own app.
var childAllowedRoutes = []struct{ method, path string }{
	{"GET", "/api/me/person"},
	{"PUT", "/api/me/person"},
	{"GET", "/api/me/allowance"},
	{"GET", "/api/me/allowance/entries"},
	{"POST", "/api/me/allowance/entries"},
	{"GET", "/api/me/accounts"},
	{"GET", "/api/me/goals"},
	{"GET", "/api/goals/"},
	{"GET", "/api/household/"},
	{"GET", "/api/capabilities"},
	{"GET", "/api/preferences/"},
}

// ownerOnlyRoutes is the subset of childRoutes that a plain member is also
// refused. Kept as its own list, rather than recognised by a path suffix,
// because the suffix trick only worked while every owner-only route ended in
// "/role" — the operator surface does not, and a test that silently stops
// checking is worse than one that fails.
var ownerOnlyRoutes = []struct{ method, path string }{
	{"PUT", "/api/household/members/" + uuid.Nil.String() + "/role"},
	{"GET", "/api/admin/continuity"},
	{"POST", "/api/admin/continuity/key-ack"},
	{"POST", "/api/admin/continuity/run"},
	{"GET", "/api/admin/continuity/export"},
	{"GET", "/api/admin/status"},
}

func isOwnerOnly(method, path string) bool {
	for _, r := range ownerOnlyRoutes {
		if r.method == method && r.path == path {
			return true
		}
	}
	return false
}

// newRoleTestServer builds a router whose auth middleware is replaced by a stub
// that injects a fixed identity.
//
// Built through NewServer rather than a Server literal so the rate limiters
// exist — a nil limiter panics in middleware, which surfaces as a 500 before
// any role check runs and would make this test assert nothing.
//
// The database is deliberately nil. This test is about the ROUTING guard, and a
// handler that gets past the guard and then fails on a nil pool has already
// proved the point.
func newRoleTestServer(t *testing.T, role string) http.Handler {
	t.Helper()
	s := NewServer(config.Config{
		SessionSecret: []byte("test-secret-for-role-enforcement-only"),
	}, nil, nil)
	return s.routesWithAuth(stubAuth(auth.Identity{
		UserID:      uuid.New(),
		HouseholdID: uuid.New(),
		Email:       "test@example.com",
		DisplayName: "Test",
		Role:        role,
		PersonID:    ptr(uuid.New()),
	}))
}

func ptr[T any](v T) *T { return &v }

func stubAuth(identity auth.Identity) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(
				auth.ContextWithIdentity(r.Context(), identity)))
		})
	}
}

// doRequest issues a request with a matching CSRF cookie and header, so the
// CSRF layer never masks the role result.
func doRequest(h http.Handler, method, path string) *httptest.ResponseRecorder {
	body := strings.NewReader("{}")
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.CSRFHeaderName, "test-csrf")
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "test-csrf"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestChildIsRefusedOnEveryRestrictedRoute(t *testing.T) {
	h := newRoleTestServer(t, auth.RoleChild)

	for _, route := range childRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec := doRequest(h, route.method, route.path)

			// 403 is what RequireAdult returns. 404 is acceptable only if the
			// route genuinely does not exist — which would be a bug in this
			// list, so it is reported rather than passed.
			if rec.Code == http.StatusNotFound {
				t.Fatalf("route not found; the test list is out of step with Routes()")
			}
			if rec.Code != http.StatusForbidden {
				t.Errorf("child got %d, want 403 — this endpoint is not guarded", rec.Code)
			}
		})
	}
}

// TestAdultReachesEveryRestrictedRoute is the other half. Without it, a guard
// that refused everybody would pass the test above.
func TestAdultReachesEveryRestrictedRoute(t *testing.T) {
	h := newRoleTestServer(t, auth.RoleMember)

	for _, route := range childRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec := doRequest(h, route.method, route.path)

			// The handler runs and fails on the nil database, which is fine —
			// what matters is that the ROLE guard let it through. Owner-only
			// routes legitimately refuse a plain member.
			if isOwnerOnly(route.method, route.path) {
				if rec.Code != http.StatusForbidden {
					t.Errorf("member got %d on an owner-only route, want 403", rec.Code)
				}
				return
			}
			if rec.Code == http.StatusForbidden {
				t.Errorf("adult got 403 — this endpoint is over-guarded")
			}
			if rec.Code == http.StatusNotFound {
				t.Errorf("route not found; the test list is out of step with Routes()")
			}
		})
	}
}

func TestChildReachesTheirOwnSurface(t *testing.T) {
	h := newRoleTestServer(t, auth.RoleChild)

	for _, route := range childAllowedRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec := doRequest(h, route.method, route.path)

			if rec.Code == http.StatusForbidden {
				t.Errorf("child got 403 on their own surface — the child view is broken")
			}
			if rec.Code == http.StatusNotFound {
				t.Errorf("route not found; the test list is out of step with Routes()")
			}
		})
	}
}

// TestOwnerOnlyRoutes: role management belongs to the owner alone.
func TestOwnerOnlyRoutes(t *testing.T) {
	owner := newRoleTestServer(t, auth.RoleOwner)
	member := newRoleTestServer(t, auth.RoleMember)

	for _, route := range ownerOnlyRoutes {
		if rec := doRequest(member, route.method, route.path); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: member got %d, want 403", route.method, route.path, rec.Code)
		}
		if rec := doRequest(owner, route.method, route.path); rec.Code == http.StatusForbidden {
			t.Errorf("%s %s: owner was refused their own route", route.method, route.path)
		}
	}
}
