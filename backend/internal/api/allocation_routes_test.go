package api

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
)

// Every allocator route is REGISTERED and ADULT-ONLY, asserted through the real
// router in one pass.
//
// Two failures this catches, and neither is hypothetical.
//
// GET /api/accounts/idle-cash and PUT /api/accounts/{id}/deposit-apy live in a
// group that also declares PUT and DELETE on /api/accounts/{accountID}. A static
// segment sitting beside a wildcard is exactly the arrangement that starts
// silently 404ing or 405ing when somebody reorders the group — and every other
// test in this package calls handlers directly, so not one of them would notice.
//
// And the allocator reads the household's whole financial position — balances,
// debts, salary-derived headroom, filing status — into one response. A route
// added to the group without the gate would hand that to a child account.
//
// A CHILD identity is what makes both assertions one assertion: RequireAdult
// answers 403 before the handler runs, so a 403 proves the route matched AND is
// gated, while a 404 proves it was never registered. Nothing touches a database,
// which is why this can cover routes whose handlers need one.
func TestAllocationRoutesAreRegisteredAndAdultOnly(t *testing.T) {
	s := NewServer(config.Config{
		SessionSecret: []byte("test-secret-for-allocation-routing-only"),
	}, nil, nil)
	h := s.routesWithAuth(stubAuth(auth.Identity{
		UserID:      uuid.New(),
		HouseholdID: uuid.New(),
		Email:       "child@example.com",
		DisplayName: "Child",
		Role:        auth.RoleChild,
		PersonID:    ptr(uuid.New()),
	}))

	planID := uuid.New().String()
	accountID := uuid.New().String()

	routes := []struct{ method, path string }{
		{"GET", "/api/allocation/buckets"},
		{"POST", "/api/allocation/plan"},
		{"GET", "/api/allocation/asset-location"},
		{"GET", "/api/allocation/plans"},
		{"POST", "/api/allocation/plans"},
		{"GET", "/api/allocation/plans/" + planID},
		{"DELETE", "/api/allocation/plans/" + planID},
		// The two that share a group with the account wildcards.
		{"GET", "/api/accounts/idle-cash"},
		{"PUT", "/api/accounts/" + accountID + "/deposit-apy"},
	}

	for _, r := range routes {
		rec := doRequest(h, r.method, r.path)
		switch rec.Code {
		case http.StatusNotFound:
			t.Errorf("%s %s → 404: the route is not registered", r.method, r.path)
		case http.StatusMethodNotAllowed:
			t.Errorf("%s %s → 405: the path matched but not for this method", r.method, r.path)
		case http.StatusForbidden:
			// Registered and adult-gated, which is the whole assertion.
		default:
			t.Errorf("%s %s → %d, want 403: a child reached past the adult gate",
				r.method, r.path, rec.Code)
		}
	}
}
