package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/crypto"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// The advisor surface, end to end against a real Postgres.
//
// The cases here are the ones where the BOUNDARY is the risk rather than the
// arithmetic — the arithmetic has its own tests next door in internal/advisor.
// What has to be proven at this layer: two surfaces quoting one figure agree to
// the cent, household and visibility scope hold on every read and write, and a
// transcript is genuinely unreadable in the database.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/

type advisorSurfaceFixture struct {
	t   *testing.T
	ctx context.Context
	srv *Server

	household, user uuid.UUID
	// spouse is a second adult in the SAME household, for the shared/private
	// visibility split.
	spouse uuid.UUID
	// otherHousehold/otherUser are a different household entirely.
	otherHousehold, otherUser uuid.UUID
}

func newAdvisorSurfaceFixture(t *testing.T) *advisorSurfaceFixture {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// A real cipher, not a stub. The transcript assertions below are about the
	// bytes actually landing sealed, which a pass-through would hide.
	cipher, err := crypto.New(bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}

	f := &advisorSurfaceFixture{
		t: t, ctx: ctx,
		srv:       &Server{Pool: pool, Queries: dbgen.New(pool), Cipher: cipher},
		household: uuid.New(), user: uuid.New(), spouse: uuid.New(),
		otherHousehold: uuid.New(), otherUser: uuid.New(),
	}

	f.seedHousehold(f.household, f.user, "Advisor Surface")
	f.exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
	        VALUES ($1, $2, $3, 'x', 'Spouse', 'member')`,
		f.spouse, f.household, f.spouse.String()+"@example.test")
	f.seedHousehold(f.otherHousehold, f.otherUser, "Somebody Else")

	return f
}

func (f *advisorSurfaceFixture) seedHousehold(hh, u uuid.UUID, name string) {
	f.t.Helper()
	f.exec(`INSERT INTO households (id, name) VALUES ($1, $2)`, hh, name)
	f.t.Cleanup(func() {
		_, _ = f.srv.Pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, hh)
	})
	f.exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
	        VALUES ($1, $2, $3, 'x', 'Tester', 'owner')`, u, hh, u.String()+"@example.test")
}

func (f *advisorSurfaceFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.srv.Pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("seed: %v\n%s", err, sql)
	}
}

// call routes one request through the real router, so the URL params and the
// scope arguments are the ones production uses rather than ones the test made
// up.
func (f *advisorSurfaceFixture) call(
	handler http.HandlerFunc, method, target string, body any,
	household, user uuid.UUID, urlParams map[string]string,
) *httptest.ResponseRecorder {
	f.t.Helper()

	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			f.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")

	rctx := chi.NewRouteContext()
	for k, v := range urlParams {
		rctx.URLParams.Add(k, v)
	}
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.ContextWithIdentity(ctx, auth.Identity{
		UserID: user, HouseholdID: household, Role: auth.RoleOwner,
	})

	rec := httptest.NewRecorder()
	handler(rec, req.WithContext(ctx))
	return rec
}

func decodeInto[T any](t *testing.T, rec *httptest.ResponseRecorder, out *T) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
}

// SAFE-TO-SPEND AGREEMENT. The advisor's chat tool and the Budgets endpoint
// must return the identical figure, to the cent.
//
// This is the doc-24 agreement rule inherited one surface further. Two places
// disagreeing about how much slack a household has is worse than neither
// existing: the Budgets page prints this number, the advisor allocates it, and
// a user who notices they differ will trust neither again.
func TestSafeToSpendToolAgreesWithTheEndpoint(t *testing.T) {
	f := newAdvisorSurfaceFixture(t)

	rec := f.call(f.srv.handleSafeToSpend, "GET", "/api/budgets/safe-to-spend", nil,
		f.household, f.user, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("endpoint: %d %s", rec.Code, rec.Body.String())
	}
	var endpoint struct {
		SafeToSpend           string `json:"safe_to_spend"`
		ExpectedIncome        string `json:"expected_income"`
		FixedCosts            string `json:"fixed_costs"`
		BudgetedDiscretionary string `json:"budgeted_discretionary"`
		GoalContributions     string `json:"goal_contributions"`
		SafeToSpendAfterBills string `json:"safe_to_spend_after_bills"`
	}
	decodeInto(t, rec, &endpoint)

	raw, err := f.srv.executeChatTool(f.ctx,
		auth.Identity{UserID: f.user, HouseholdID: f.household, Role: auth.RoleOwner},
		"safe_to_spend", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	var tool map[string]any
	if err := json.Unmarshal([]byte(raw), &tool); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}

	// Compared as DECIMALS, not as strings, and the difference is not pedantry.
	// The Budgets endpoint has always serialised a raw decimal ("0", "412.5");
	// the chat tool returns StringFixed(2) ("0.00", "412.50") because the model
	// must never be handed a figure it might reformat. Both are right for their
	// reader, and the claim under test is the one that matters — the two
	// surfaces quote the same MONEY. A string comparison would fail here on a
	// formatting difference that is deliberate on both sides.
	for _, c := range []struct{ field, want string }{
		{"safe_to_spend", endpoint.SafeToSpend},
		{"expected_income", endpoint.ExpectedIncome},
		{"fixed_costs", endpoint.FixedCosts},
		{"budgeted_discretionary", endpoint.BudgetedDiscretionary},
		{"goal_contributions", endpoint.GoalContributions},
		{"safe_to_spend_after_bills", endpoint.SafeToSpendAfterBills},
	} {
		got, _ := tool[c.field].(string)
		gotDec, err := decimal.NewFromString(got)
		if err != nil {
			t.Errorf("%s: tool returned %q, which is not a decimal", c.field, got)
			continue
		}
		wantDec, err := decimal.NewFromString(c.want)
		if err != nil {
			t.Errorf("%s: endpoint returned %q, which is not a decimal", c.field, c.want)
			continue
		}
		if !gotDec.Equal(wantDec) {
			t.Errorf("%s: tool says %s, the Budgets endpoint says %s — these must agree to the cent",
				c.field, gotDec, wantDec)
		}
		// And the tool's own contract, separately: two places, always, so the
		// model is never handed a figure it has to finish.
		if got != gotDec.StringFixed(2) {
			t.Errorf("%s: tool returned %q; every money field it returns must be finished to the cent",
				c.field, got)
		}
	}
}

// Threads: household and visibility scope on every path, and a transcript that
// is genuinely sealed in the database.
func TestAdvisorThreads(t *testing.T) {
	f := newAdvisorSurfaceFixture(t)
	me := func() (uuid.UUID, uuid.UUID) { return f.household, f.user }

	// --- create ------------------------------------------------------------
	hh, u := me()
	rec := f.call(f.srv.handleCreateThread, "POST", "/api/advisor/threads",
		map[string]any{"title": "Can we afford the house?"}, hh, u, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created threadResponse
	decodeInto(t, rec, &created)
	if created.Title != "Can we afford the house?" || !created.IsShared {
		t.Fatalf("created = %+v; is_shared must default to true", created)
	}

	// --- a turn is persisted sealed ---------------------------------------
	const secret = "our combined gross is $184,000"
	f.srv.persistTurn(f.ctx, created.ID, f.household, secret, "Here is what that supports.",
		[]chatToolTrace{{Tool: "safe_to_spend", Result: json.RawMessage(`{"safe_to_spend":"412.00"}`)}})

	// THE COLUMN MUST BE UNREADABLE. A raw SELECT is what an operator with
	// database access sees, and a transcript is the most sensitive text this app
	// holds — a household narrating its salary and its debts in prose.
	var stored []byte
	if err := f.srv.Pool.QueryRow(f.ctx,
		`SELECT content FROM advisor_messages m
		 JOIN advisor_threads t ON t.id = m.thread_id
		 WHERE t.id = $1 AND m.role = 'user'`, created.ID,
	).Scan(&stored); err != nil {
		t.Fatalf("read stored message: %v", err)
	}
	if bytes.Contains(stored, []byte(secret)) {
		t.Error("the transcript is readable in a raw SELECT; it must be sealed")
	}

	// --- fetch: it round-trips, with the tool trace intact -----------------
	rec = f.call(f.srv.handleGetThread, "GET", "/api/advisor/threads/"+created.ID.String(), nil,
		hh, u, map[string]string{"threadID": created.ID.String()})
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	var detail threadDetailResponse
	decodeInto(t, rec, &detail)
	if len(detail.Messages) != 2 {
		t.Fatalf("got %d messages, want the user turn and the assistant turn", len(detail.Messages))
	}
	if detail.Messages[0].Content != secret {
		t.Errorf("user turn = %q, want it back verbatim through the cipher", detail.Messages[0].Content)
	}

	// A TRANSCRIPT WITHOUT TOOL RESULTS BREAKS THE ONE RULE THE CHAT HAS. Every
	// number the model states has to come from a tool result; a reloaded history
	// of figures with no provenance is one the model will re-read as current.
	assistant := detail.Messages[1]
	if len(assistant.ToolTrace) != 1 || assistant.ToolTrace[0].Tool != "safe_to_spend" {
		t.Errorf("assistant turn carries tool_trace %+v; every persisted assistant turn needs its provenance",
			assistant.ToolTrace)
	}
	// And every reloaded figure is marked as history, not as this month's.
	for _, m := range detail.Messages {
		if !m.Stale {
			t.Error("a reloaded turn must be marked stale — its figures are context, never current")
		}
	}

	// --- another household cannot see it ----------------------------------
	rec = f.call(f.srv.handleGetThread, "GET", "/api/advisor/threads/"+created.ID.String(), nil,
		f.otherHousehold, f.otherUser, map[string]string{"threadID": created.ID.String()})
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-household get = %d, want 404 (not 403 — which ids exist is itself a leak)", rec.Code)
	}
	rec = f.call(f.srv.handleDeleteThread, "DELETE", "/api/advisor/threads/"+created.ID.String(), nil,
		f.otherHousehold, f.otherUser, map[string]string{"threadID": created.ID.String()})
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-household delete = %d, want 404", rec.Code)
	}

	// --- a private thread is invisible to the spouse -----------------------
	rec = f.call(f.srv.handleCreateThread, "POST", "/api/advisor/threads",
		map[string]any{"title": "Private", "is_shared": false}, hh, u, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create private: %d %s", rec.Code, rec.Body.String())
	}
	var private threadResponse
	decodeInto(t, rec, &private)

	rec = f.call(f.srv.handleGetThread, "GET", "/api/advisor/threads/"+private.ID.String(), nil,
		f.household, f.spouse, map[string]string{"threadID": private.ID.String()})
	if rec.Code != http.StatusNotFound {
		t.Errorf("spouse reading a private thread = %d, want 404", rec.Code)
	}

	// The spouse still sees the shared one, and only that one.
	rec = f.call(f.srv.handleListThreads, "GET", "/api/advisor/threads", nil, f.household, f.spouse, nil)
	var spouseList []threadResponse
	decodeInto(t, rec, &spouseList)
	if len(spouseList) != 1 || spouseList[0].ID != created.ID {
		t.Errorf("spouse sees %d thread(s); expected only the shared one", len(spouseList))
	}

	// --- delete ------------------------------------------------------------
	rec = f.call(f.srv.handleDeleteThread, "DELETE", "/api/advisor/threads/"+created.ID.String(), nil,
		hh, u, map[string]string{"threadID": created.ID.String()})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	// The messages go with it, or a deleted conversation leaves its transcript
	// behind — which is the opposite of what "delete" means here.
	var remaining int
	if err := f.srv.Pool.QueryRow(f.ctx,
		`SELECT count(*) FROM advisor_messages WHERE thread_id = $1`, created.ID).Scan(&remaining); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d messages survived the thread's deletion", remaining)
	}
}

// Action items: created from an accepted option, moved between statuses, and
// scoped to the household on every mutation.
func TestAdvisorActionItems(t *testing.T) {
	f := newAdvisorSurfaceFixture(t)

	rec := f.call(f.srv.handleCreateActionItem, "POST", "/api/advisor/action-items",
		map[string]any{
			"title":  "Put $400/mo at the 22.99% card",
			"detail": "Avoids $1,204.11 of interest",
			"source": "option",
		}, f.household, f.user, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var item actionItemResponse
	decodeInto(t, rec, &item)
	if item.Status != "open" || item.Source != "option" {
		t.Fatalf("created = %+v; want an open item sourced from an option", item)
	}
	if item.CompletedAt != nil {
		t.Error("a new item must not carry a completion time")
	}

	// An unknown source is rejected rather than stored — `source` is a CHECK
	// column and a 400 naming the allowed set is a better answer than a 500 from
	// a constraint violation nobody can read.
	rec = f.call(f.srv.handleCreateActionItem, "POST", "/api/advisor/action-items",
		map[string]any{"title": "x", "source": "executed"}, f.household, f.user, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad source = %d, want 400", rec.Code)
	}

	// --- done sets completed_at; reopening clears it ----------------------
	rec = f.call(f.srv.handleUpdateActionItem, "PATCH", "/api/advisor/action-items/"+item.ID.String(),
		map[string]any{"status": "done"}, f.household, f.user, map[string]string{"itemID": item.ID.String()})
	if rec.Code != http.StatusOK {
		t.Fatalf("mark done: %d %s", rec.Code, rec.Body.String())
	}
	var done actionItemResponse
	decodeInto(t, rec, &done)
	if done.Status != "done" || done.CompletedAt == nil {
		t.Errorf("done = %+v; completed_at must be set by the same statement that sets the status", done)
	}

	rec = f.call(f.srv.handleUpdateActionItem, "PATCH", "/api/advisor/action-items/"+item.ID.String(),
		map[string]any{"status": "open"}, f.household, f.user, map[string]string{"itemID": item.ID.String()})
	var reopened actionItemResponse
	decodeInto(t, rec, &reopened)
	if reopened.Status != "open" || reopened.CompletedAt != nil {
		t.Errorf("reopened = %+v; completed_at must be cleared on the way back out of done", reopened)
	}

	// --- an unknown status is refused --------------------------------------
	rec = f.call(f.srv.handleUpdateActionItem, "PATCH", "/api/advisor/action-items/"+item.ID.String(),
		map[string]any{"status": "paid"}, f.household, f.user, map[string]string{"itemID": item.ID.String()})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad status = %d, want 400", rec.Code)
	}

	// --- household scope on the mutation ------------------------------------
	rec = f.call(f.srv.handleUpdateActionItem, "PATCH", "/api/advisor/action-items/"+item.ID.String(),
		map[string]any{"status": "dismissed"}, f.otherHousehold, f.otherUser,
		map[string]string{"itemID": item.ID.String()})
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-household update = %d, want 404", rec.Code)
	}

	// --- and it is invisible to another household's list -------------------
	rec = f.call(f.srv.handleListActionItems, "GET", "/api/advisor/action-items", nil,
		f.otherHousehold, f.otherUser, nil)
	var theirs []actionItemResponse
	decodeInto(t, rec, &theirs)
	if len(theirs) != 0 {
		t.Errorf("another household sees %d of our action items", len(theirs))
	}
}

// The household profile: validated here rather than only by the schema, and
// clearable — because "I have not told you my filing status" is not "single".
func TestHouseholdProfile(t *testing.T) {
	f := newAdvisorSurfaceFixture(t)

	rec := f.call(f.srv.handleGetProfile, "GET", "/api/household/profile/", nil, f.household, f.user, nil)
	var initial householdProfileResponse
	decodeInto(t, rec, &initial)
	if initial.FilingStatus != nil || initial.RiskDrawdownFloor != nil {
		t.Errorf("a fresh household starts with an empty profile, got %+v", initial)
	}

	rec = f.call(f.srv.handleUpdateProfile, "PUT", "/api/household/profile/",
		map[string]any{"filing_status": "married_joint", "risk_drawdown_floor": "20.00"},
		f.household, f.user, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	var saved householdProfileResponse
	decodeInto(t, rec, &saved)
	if saved.FilingStatus == nil || *saved.FilingStatus != "married_joint" {
		t.Errorf("filing_status = %v", saved.FilingStatus)
	}
	if saved.RiskDrawdownFloor == nil || *saved.RiskDrawdownFloor != "20.00" {
		t.Errorf("risk_drawdown_floor = %v", saved.RiskDrawdownFloor)
	}

	for _, bad := range []map[string]any{
		{"filing_status": "married"},      // not one of the four
		{"risk_drawdown_floor": "twenty"}, // not a decimal
		{"risk_drawdown_floor": "140"},    // a percent, so out of range
		{"risk_drawdown_floor": "-5"},     // likewise
	} {
		rec = f.call(f.srv.handleUpdateProfile, "PUT", "/api/household/profile/", bad,
			f.household, f.user, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("update with %v = %d, want 400", bad, rec.Code)
		}
	}

	// Clearing is a real operation, not a no-op.
	rec = f.call(f.srv.handleUpdateProfile, "PUT", "/api/household/profile/",
		map[string]any{"filing_status": nil, "risk_drawdown_floor": nil}, f.household, f.user, nil)
	var cleared householdProfileResponse
	decodeInto(t, rec, &cleared)
	if cleared.FilingStatus != nil || cleared.RiskDrawdownFloor != nil {
		t.Errorf("cleared profile = %+v; null must clear the column", cleared)
	}
}

// CONTRIBUTION ROOM MUST NOT CONFLATE A CAP WITH PERMISSION.
//
// AnnualLimitFor answers "what is the cap for this account type at this age" and
// nothing else. A Roth IRA has a MAGI phase-out, and above it the correct
// headroom is $0 rather than $7,500. The tool therefore ships `eligibility` as
// its own field from day one — "unknown" in this doc's cycle, and doc 32's
// phase-out table makes it more. A client that renders "you have $7,500 of room"
// has nowhere to put "…but you may not be allowed to use it" if the field
// arrives later.
func TestContributionRoomSeparatesCapFromPermission(t *testing.T) {
	f := newAdvisorSurfaceFixture(t)

	raw, err := f.srv.executeChatTool(f.ctx,
		auth.Identity{UserID: f.user, HouseholdID: f.household, Role: auth.RoleOwner},
		"contribution_room", json.RawMessage(`{"year":2026}`))
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	var out struct {
		TaxYear          int  `json:"tax_year"`
		LimitsConfigured bool `json:"limits_configured"`
		PaystubCount     int  `json:"paystub_count"`
		Groups           []struct {
			Group           string `json:"group"`
			AnnualLimit     string `json:"annual_limit"`
			UsedYTD         string `json:"used_ytd"`
			Eligibility     string `json:"eligibility"`
			UsedYTDVerified bool   `json:"used_ytd_verified"`
		} `json:"groups"`
		Uncapped []string `json:"uncapped_account_types"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}

	if !out.LimitsConfigured || len(out.Groups) == 0 {
		t.Fatalf("2026 limits should be configured with groups, got %+v", out)
	}
	for _, g := range out.Groups {
		if g.Eligibility != "unknown" {
			t.Errorf("%s eligibility = %q; this doc ships the field as unknown, and it must be PRESENT",
				g.Group, g.Eligibility)
		}
		if g.AnnualLimit == "" {
			t.Errorf("%s has no annual_limit", g.Group)
		}
		// With no confirmed stub on file the year's deferrals are UNMEASURED,
		// not zero. Reporting them as verified would imply full headroom that
		// may not be there.
		if out.PaystubCount == 0 && g.UsedYTDVerified {
			t.Errorf("%s reports used_ytd as verified with no paystubs on file", g.Group)
		}
	}

	// Taxable, 529, trust and UTMA have no federal annual deferral cap.
	// AnnualLimitFor declines to invent one, and the tool says so rather than
	// omitting them and letting a reader conclude they were forgotten.
	if len(out.Uncapped) == 0 {
		t.Error("the uncapped account types must be named, not silently absent")
	}
}

// A thread id the caller cannot see is a 404 from /api/chat too, and the turn is
// never persisted to it — the scope check runs before the stream opens.
func TestChatRejectsAnotherHouseholdsThread(t *testing.T) {
	f := newAdvisorSurfaceFixture(t)

	mine, err := f.srv.Queries.CreateAdvisorThread(f.ctx, dbgen.CreateAdvisorThreadParams{
		HouseholdID: f.household, UserID: &f.user, Title: "Mine", IsShared: true,
	})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	_, err = f.srv.Queries.GetAdvisorThread(f.ctx, dbgen.GetAdvisorThreadParams{
		ID: mine.ID, HouseholdID: f.otherHousehold, UserID: &f.otherUser,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("another household resolved the thread (err = %v); the scope check is the only thing "+
			"standing between a guessed id and somebody else's transcript", err)
	}
}
