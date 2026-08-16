package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/advisor"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/crypto"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// The financial plan (MAD-258), end to end against a real Postgres.
//
// The boundary risks this layer owns: the append-only rule on decisions holds
// against every write path, household scope holds on every read and write,
// bodies are genuinely sealed in the database, and the briefing carries the
// plan digest opened — the agreement that makes the chat quote the same plan
// the Plan page shows.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/ -run TestPlan

type planFixture struct {
	t   *testing.T
	ctx context.Context
	srv *Server

	household, user uuid.UUID
	spouse          uuid.UUID
	personID        uuid.UUID
	otherHousehold  uuid.UUID
	otherUser       uuid.UUID
}

func newPlanFixture(t *testing.T) *planFixture {
	t.Helper()

	url := testdb.URL(t)

	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// A real cipher: the sealing assertions below are about the bytes that
	// actually land in the database, which a pass-through would hide.
	cipher, err := crypto.New(bytes.Repeat([]byte("p"), 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}

	f := &planFixture{
		t: t, ctx: ctx,
		srv:       &Server{Pool: pool, Queries: dbgen.New(pool), Cipher: cipher},
		household: uuid.New(), user: uuid.New(), spouse: uuid.New(),
		personID:       uuid.New(),
		otherHousehold: uuid.New(), otherUser: uuid.New(),
	}

	f.seedHousehold(f.household, f.user, "Plan Household")
	f.exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
	        VALUES ($1, $2, $3, 'x', 'Spouse', 'member')`,
		f.spouse, f.household, f.spouse.String()+"@example.test")
	f.exec(`INSERT INTO household_people (id, household_id, display_name)
	        VALUES ($1, $2, 'Kid')`, f.personID, f.household)
	f.seedHousehold(f.otherHousehold, f.otherUser, "Other Household")

	return f
}

func (f *planFixture) seedHousehold(hh, u uuid.UUID, name string) {
	f.t.Helper()
	f.exec(`INSERT INTO households (id, name) VALUES ($1, $2)`, hh, name)
	f.t.Cleanup(func() {
		_, _ = f.srv.Pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, hh)
	})
	f.exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
	        VALUES ($1, $2, $3, 'x', 'Tester', 'owner')`, u, hh, u.String()+"@example.test")
}

func (f *planFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.srv.Pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("seed: %v\n%s", err, sql)
	}
}

func (f *planFixture) call(
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

func planBody(t *testing.T, rec *httptest.ResponseRecorder) financialPlanResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out financialPlanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// A section round-trips, lands SEALED in the database, and the upsert keeps
// one row per slot.
func TestPlanSectionRoundTripAndSealing(t *testing.T) {
	f := newPlanFixture(t)

	rec := f.call(f.srv.handleSavePlanSection, "PUT", "/api/plan/sections",
		map[string]any{"kind": "strategy", "body": "Three months of fixed costs; teaching income is stable."},
		f.household, f.user, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}

	// Sealed at rest: the plaintext must not appear in the column.
	var raw []byte
	err := f.srv.Pool.QueryRow(f.ctx,
		`SELECT body FROM plan_sections WHERE household_id = $1 AND kind = 'strategy'`,
		f.household).Scan(&raw)
	if err != nil {
		t.Fatalf("read raw section: %v", err)
	}
	if bytes.Contains(raw, []byte("teaching income")) {
		t.Fatalf("plan body landed in plaintext: %q", raw)
	}

	// Update through the same upsert: still one row, new body.
	rec = f.call(f.srv.handleSavePlanSection, "PUT", "/api/plan/sections",
		map[string]any{"kind": "strategy", "body": "Updated strategy."},
		f.household, f.user, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("resave: %d %s", rec.Code, rec.Body.String())
	}
	var n int
	if err := f.srv.Pool.QueryRow(f.ctx,
		`SELECT count(*) FROM plan_sections WHERE household_id = $1`, f.household).Scan(&n); err != nil || n != 1 {
		t.Fatalf("upsert should keep one strategy row, have %d (%v)", n, err)
	}

	got := planBody(t, f.call(f.srv.handleGetFinancialPlan, "GET", "/api/plan", nil, f.household, f.user, nil))
	if len(got.Sections) != 1 || got.Sections[0].Kind != "strategy" || got.Sections[0].Body != "Updated strategy." {
		t.Fatalf("round trip: %+v", got.Sections)
	}

	// The spouse sees the same plan: the plan is a household surface.
	spouseView := planBody(t, f.call(f.srv.handleGetFinancialPlan, "GET", "/api/plan", nil, f.household, f.spouse, nil))
	if len(spouseView.Sections) != 1 || spouseView.Sections[0].Body != "Updated strategy." {
		t.Fatalf("spouse cannot read the household plan: %+v", spouseView.Sections)
	}

	// Another household reads nothing and cannot write over the slot.
	other := f.call(f.srv.handleGetFinancialPlan, "GET", "/api/plan", nil, f.otherHousehold, f.otherUser, nil)
	if other.Code != http.StatusOK {
		t.Fatalf("other household read: %d", other.Code)
	}
	var empty financialPlanResponse
	if err := json.Unmarshal(other.Body.Bytes(), &empty); err != nil || len(empty.Sections) != 0 {
		t.Fatalf("other household should see no sections: %s", other.Body.String())
	}
}

// The person rule: a person section needs a household person; no other kind
// takes one; a person from another household does not exist.
func TestPlanSectionPersonRule(t *testing.T) {
	f := newPlanFixture(t)

	bad := f.call(f.srv.handleSavePlanSection, "PUT", "/api/plan/sections",
		map[string]any{"kind": "person", "body": "whose note?"},
		f.household, f.user, nil)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("person without person_id should 400, got %d", bad.Code)
	}

	bad = f.call(f.srv.handleSavePlanSection, "PUT", "/api/plan/sections",
		map[string]any{"kind": "strategy", "person_id": f.personID.String(), "body": "x"},
		f.household, f.user, nil)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("strategy with person_id should 400, got %d", bad.Code)
	}

	// A person id from another household: 404, the scoped-read answer.
	otherPerson := uuid.New()
	f.exec(`INSERT INTO household_people (id, household_id, display_name)
	        VALUES ($1, $2, 'Their Kid')`, otherPerson, f.otherHousehold)
	bad = f.call(f.srv.handleSavePlanSection, "PUT", "/api/plan/sections",
		map[string]any{"kind": "person", "person_id": otherPerson.String(), "body": "x"},
		f.household, f.user, nil)
	if bad.Code != http.StatusNotFound {
		t.Fatalf("foreign person should 404, got %d", bad.Code)
	}

	ok := f.call(f.srv.handleSavePlanSection, "PUT", "/api/plan/sections",
		map[string]any{"kind": "person", "person_id": f.personID.String(), "body": "529 reasoning"},
		f.household, f.user, nil)
	if ok.Code != http.StatusOK {
		t.Fatalf("own person should save, got %d %s", ok.Code, ok.Body.String())
	}
	got := planBody(t, f.call(f.srv.handleGetFinancialPlan, "GET", "/api/plan", nil, f.household, f.user, nil))
	if len(got.Sections) != 1 || got.Sections[0].PersonName == nil || *got.Sections[0].PersonName != "Kid" {
		t.Fatalf("person section should carry the name: %+v", got.Sections)
	}
}

// The decisions log is append-only: confirmed rows refuse edit and delete, a
// superseding decision is a NEW row pointing back, and the proposal path
// (edit → confirm, or discard) is the only mutable corner.
func TestPlanDecisionsAppendOnly(t *testing.T) {
	f := newPlanFixture(t)

	rec := f.call(f.srv.handleCreatePlanDecision, "POST", "/api/plan/decisions",
		map[string]any{"topic": "Hold EF at 3 months", "body": "Teaching income is stable."},
		f.household, f.user, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var first planDecisionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if first.Status != "confirmed" || first.Source != "manual" {
		t.Fatalf("defaults: %+v", first)
	}

	// Editing a confirmed decision is refused, naming the alternative.
	rec = f.call(f.srv.handleUpdatePlanDecision, "PATCH", "/api/plan/decisions/"+first.ID.String(),
		map[string]any{"topic": "rewritten"}, f.household, f.user,
		map[string]string{"decisionID": first.ID.String()})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("edit confirmed should 400, got %d", rec.Code)
	}
	rec = f.call(f.srv.handleDeletePlanDecision, "DELETE", "/api/plan/decisions/"+first.ID.String(),
		nil, f.household, f.user, map[string]string{"decisionID": first.ID.String()})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("delete confirmed should 400, got %d", rec.Code)
	}

	// Supersede: a new decision that names the old one. The old row stays,
	// flagged superseded by the derived flag.
	rec = f.call(f.srv.handleCreatePlanDecision, "POST", "/api/plan/decisions",
		map[string]any{"topic": "Raise EF to 4 months", "body": "Daycare costs landed.",
			"supersedes": first.ID.String()},
		f.household, f.user, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("supersede create: %d %s", rec.Code, rec.Body.String())
	}
	var second planDecisionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if second.Supersedes == nil || *second.Supersedes != first.ID {
		t.Fatalf("supersedes pointer: %+v", second)
	}

	got := planBody(t, f.call(f.srv.handleGetFinancialPlan, "GET", "/api/plan", nil, f.household, f.user, nil))
	byID := map[uuid.UUID]planDecisionResponse{}
	for _, d := range got.Decisions {
		byID[d.ID] = d
	}
	if !byID[first.ID].Superseded {
		t.Fatalf("old decision should read superseded: %+v", byID[first.ID])
	}
	if byID[second.ID].Superseded {
		t.Fatalf("new decision should read active: %+v", byID[second.ID])
	}

	// The proposal path: the advisor surface drafts, the household confirms.
	rec = f.call(f.srv.handleCreatePlanDecision, "POST", "/api/plan/decisions",
		map[string]any{"topic": "Draft from chat", "body": "Suggested by the advisor.",
			"status": "proposed", "source": "advisor"},
		f.household, f.spouse, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("propose: %d %s", rec.Code, rec.Body.String())
	}
	var proposal planDecisionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &proposal); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// A proposal may not supersede: retiring a decision is a decision.
	rec = f.call(f.srv.handleCreatePlanDecision, "POST", "/api/plan/decisions",
		map[string]any{"topic": "Sneaky", "body": "x", "status": "proposed",
			"supersedes": second.ID.String()},
		f.household, f.user, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("proposal superseding should 400, got %d", rec.Code)
	}

	// Edit the proposal, then confirm it.
	rec = f.call(f.srv.handleUpdatePlanDecision, "PATCH", "/api/plan/decisions/"+proposal.ID.String(),
		map[string]any{"topic": "Edited draft", "body": "Confirmed wording."},
		f.household, f.user, map[string]string{"decisionID": proposal.ID.String()})
	if rec.Code != http.StatusOK {
		t.Fatalf("edit proposal: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.call(f.srv.handleUpdatePlanDecision, "PATCH", "/api/plan/decisions/"+proposal.ID.String(),
		map[string]any{"confirm": true}, f.household, f.user,
		map[string]string{"decisionID": proposal.ID.String()})
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm proposal: %d %s", rec.Code, rec.Body.String())
	}
	var confirmed planDecisionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &confirmed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if confirmed.Status != "confirmed" {
		t.Fatalf("should be confirmed: %+v", confirmed)
	}

	// Discarding a second proposal still works; discarding a confirmed one
	// does not.
	rec = f.call(f.srv.handleCreatePlanDecision, "POST", "/api/plan/decisions",
		map[string]any{"topic": "Throwaway", "body": "x", "status": "proposed"},
		f.household, f.user, nil)
	var toss planDecisionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &toss); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rec = f.call(f.srv.handleDeletePlanDecision, "DELETE", "/api/plan/decisions/"+toss.ID.String(),
		nil, f.household, f.user, map[string]string{"decisionID": toss.ID.String()})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("discard proposal: %d", rec.Code)
	}
}

// Household scope on decisions: another household's decision does not exist,
// on read or as a supersede target.
func TestPlanDecisionHouseholdScope(t *testing.T) {
	f := newPlanFixture(t)

	rec := f.call(f.srv.handleCreatePlanDecision, "POST", "/api/plan/decisions",
		map[string]any{"topic": "Theirs", "body": "x"},
		f.otherHousehold, f.otherUser, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d", rec.Code)
	}
	var theirs planDecisionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &theirs); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Cross-household PATCH → 404, not 403: "may not" and "does not exist"
	// are the same answer.
	rec = f.call(f.srv.handleUpdatePlanDecision, "PATCH", "/api/plan/decisions/"+theirs.ID.String(),
		map[string]any{"confirm": true}, f.household, f.user,
		map[string]string{"decisionID": theirs.ID.String()})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-household patch should 404, got %d", rec.Code)
	}

	// Superseding another household's decision: the scoped read declines.
	rec = f.call(f.srv.handleCreatePlanDecision, "POST", "/api/plan/decisions",
		map[string]any{"topic": "Mine", "body": "x", "supersedes": theirs.ID.String()},
		f.household, f.user, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-household supersede should 400, got %d", rec.Code)
	}
}

// The review stamp lands, and the briefing carries the plan opened — the chat
// and the Plan page quote one plan.
func TestPlanReviewStampAndBriefingDigest(t *testing.T) {
	f := newPlanFixture(t)

	if rec := f.call(f.srv.handleSavePlanSection, "PUT", "/api/plan/sections",
		map[string]any{"kind": "strategy", "body": "Deliberately three months, not six."},
		f.household, f.user, nil); rec.Code != http.StatusOK {
		t.Fatalf("seed section: %d", rec.Code)
	}
	if rec := f.call(f.srv.handleCreatePlanDecision, "POST", "/api/plan/decisions",
		map[string]any{"topic": "529 until 2031", "body": "Surplus goes to the 529."},
		f.household, f.user, nil); rec.Code != http.StatusCreated {
		t.Fatalf("seed decision: %d", rec.Code)
	}

	rec := f.call(f.srv.handleReviewPlan, "POST", "/api/plan/review", nil, f.household, f.user, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("stamp: %d %s", rec.Code, rec.Body.String())
	}
	got := planBody(t, f.call(f.srv.handleGetFinancialPlan, "GET", "/api/plan", nil, f.household, f.user, nil))
	if got.ReviewedAt == nil {
		t.Fatal("review stamp did not land")
	}

	// The briefing carries the opened digest with the strategy and the active
	// decision. A household with no plan must carry NO plan key at all —
	// absent, not empty.
	b, err := advisor.BuildBriefing(f.ctx, f.srv.Queries, f.household, time.Now().UTC())
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if !b.Plan.Exists || len(b.Plan.Sections) == 0 || len(b.Plan.Decisions) != 1 {
		t.Fatalf("briefing digest: %+v", b.Plan)
	}
	view := f.srv.openPlanDigest(b.Plan)
	if view == nil || view.Strategy != "Deliberately three months, not six." {
		t.Fatalf("opened digest: %+v", view)
	}
	tool := planDigestToolResult(view)
	if tool["strategy"] != "Deliberately three months, not six." {
		t.Fatalf("tool digest: %+v", tool)
	}

	noPlan, err := advisor.BuildBriefing(f.ctx, f.srv.Queries, f.otherHousehold, time.Now().UTC())
	if err != nil {
		t.Fatalf("briefing: %v", err)
	}
	if noPlan.Plan.Exists {
		t.Fatal("household without a plan must have no digest")
	}
	if v := f.srv.openPlanDigest(noPlan.Plan); v != nil {
		t.Fatal("no-plan digest must open to nil")
	}
}
