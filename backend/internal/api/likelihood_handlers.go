package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/allocation"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/likelihood"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/reporting"
)

// The likelihood surface (doc 33): /api/likelihood.
//
// THREE RULES, and the first is why this file computes nothing itself.
//
// EVERY FIGURE COMES FROM AN ENGINE THAT ALREADY OWNS IT. The deterministic
// projection is allocation.Run; the distribution is likelihood.Run over
// networth.BuildSchedule's output; the pick is likelihood.Rank. This file
// assembles inputs and shapes responses. A second implementation of any of them
// would be a household told two different things about one number.
//
// NOTHING HERE WRITES EXCEPT A TRACKING SNAPSHOT. Running a simulation touches
// no account, no goal and no assumption — it is a pure function of a baseline it
// was handed. The only write in the group is POST .../track, which records the
// household's own decision to start tracking a plan.
//
// AND THE GATE IS NOT AN ERROR. With RETIREMENT_MONTE_CARLO_ENABLED off, these
// endpoints return the deterministic figure and say so in the basis. A 503 would
// make the Advisor page's likelihood panel a broken tile on every instance that
// has not opted in.

// defaultVolatility is the standard deviation of the annual real return used
// when the caller does not supply one.
//
// There is no honest default for volatility — it is an assumption, not a
// constant — but the endpoint has to run without one, so this matches the value
// the retirement projection already uses for the withdrawal phase, and every
// response names what was used. Two surfaces disagreeing about the household's
// assumed volatility would be worse than either default.
var defaultVolatility = decimal.RequireFromString("0.15")

// --------------------------------------------------------------------------
// Response shapes
// --------------------------------------------------------------------------

// likelihoodResponse is one plan's distribution beside its deterministic
// projection.
type likelihoodResponse struct {
	PlanID *uuid.UUID `json:"plan_id,omitempty"`
	Name   string     `json:"name,omitempty"`

	HorizonYears int `json:"horizon_years"`
	Runs         int `json:"runs"`
	// Volatility is what was actually used, as a fraction, so a client can never
	// render a distribution without the assumption that produced it.
	Volatility string `json:"volatility"`

	// ProjectedAtAssumedReturn is doc 32's figure: the value compounded at the
	// assumed return. It is NOT a P50 and is never labelled one — see GapNote.
	ProjectedAtAssumedReturn string `json:"projected_at_assumed_return"`

	// Simulated is the distribution. Nil when the Monte Carlo gate is off, in
	// which case the deterministic figure above is the whole answer.
	Simulated *simulatedFigures `json:"simulated,omitempty"`

	// MonteCarloEnabled tells the client which of the two it is looking at,
	// rather than leaving it to infer from a null.
	MonteCarloEnabled bool `json:"monte_carlo_enabled"`

	// GapNote explains why the two headline figures disagree. It is rendered
	// verbatim beside them, because a 15% gap between two numbers that both look
	// like "the answer" is exactly the two-surfaces-disagreeing failure doc 24
	// names — and the resolution is stated convention, not a fudge.
	GapNote string `json:"gap_note"`

	Basis    string `json:"basis"`
	Estimate bool   `json:"estimate"`
}

// simulatedFigures is the distribution, with money already finished as strings.
type simulatedFigures struct {
	P10 string `json:"p10"`
	// P50 is the MEDIAN SIMULATED OUTCOME. The field is deliberately not named
	// "median" alone and the UI label is never "P50" — doc 32's figure sits
	// beside it on the same card.
	P50 string `json:"p50"`
	P90 string `json:"p90"`

	// SuccessRate is a FRACTION over the modelled sequences. The UI renders it
	// as "meets your target in 94% of 1,000 simulated futures", never as a
	// probability of a real outcome.
	SuccessRate *string `json:"success_rate"`
	Target      *string `json:"target,omitempty"`

	Sigma string `json:"sigma"`
	// DrawdownP5 is the FIFTH-PERCENTILE peak-to-trough drawdown, as a fraction,
	// with the percentile and the run count named so "25% drawdown" is never
	// ambiguous about which of a thousand futures it describes.
	DrawdownP5 string `json:"drawdown_p5"`

	ByAccount []accountOutcomeResponse `json:"by_account"`

	Seed uint64 `json:"seed"`
}

type accountOutcomeResponse struct {
	AccountID string `json:"account_id"`
	Name      string `json:"name"`
	P10       string `json:"p10"`
	P50       string `json:"p50"`
	P90       string `json:"p90"`
}

// gapNote is the sentence that makes the two headline figures legible together.
const gapNote = "The median simulated outcome is lower than the projected-at-your-assumed-return figure because " +
	"volatility drags compounding — that difference is the cost of the risk you're taking. They are answers to " +
	"different questions and neither is wrong. A plan with zero volatility makes them identical."

// muConvention is the guidance that belongs wherever a real return is entered.
// Most published "expected real return" figures are GEOMETRIC (a compound
// average), and entering one as the arithmetic mean overstates results by about
// σ²/2 a year — roughly 1.1 points at 15% volatility, compounded over a whole
// horizon.
const muConvention = "Your assumed real return is read as the AVERAGE OF YEARLY RETURNS, not a long-run compound " +
	"average. For a 7% compound expectation at 15% volatility, enter about 8.1%."

// --------------------------------------------------------------------------
// Handlers
// --------------------------------------------------------------------------

// handleLikelihood runs the simulation for one saved plan.
func (s *Server) handleLikelihood(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	planID, err := uuid.Parse(chi.URLParam(r, "planID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid plan id")
		return
	}

	now := time.Now().UTC()
	baseline, err := s.allocationBaseline(r.Context(), identity, now)
	if err != nil {
		s.internalError(w, "assemble allocation baseline", err)
		return
	}

	row, req, err := s.loadPlanRequest(r.Context(), identity, planID)
	if err != nil {
		s.writePlanLoadError(w, err)
		return
	}

	q := r.URL.Query()
	runs := s.interactiveRuns(q.Get("runs"))
	volatility := parseDecimal(q.Get("volatility"), defaultVolatility)

	resp, err := s.likelihoodFor(baseline, req, runs, volatility)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp.PlanID, resp.Name = &row.ID, row.Name
	writeJSON(w, http.StatusOK, resp)
}

// compareRequest is the guardrail's input.
type compareRequest struct {
	PlanIDs []string `json:"plan_ids"`
	// Runs is PINNED across every plan in the comparison. See handleCompare.
	Runs       int    `json:"runs"`
	Volatility string `json:"volatility"`
}

type compareResponse struct {
	Ranking likelihood.Ranking `json:"ranking"`
	// Plans carries each plan's own figures beside the ranking, so the
	// comparison view renders the cards and the pick from one payload.
	Plans []likelihoodResponse `json:"plans"`

	MonteCarloEnabled bool   `json:"monte_carlo_enabled"`
	Basis             string `json:"basis"`
	Estimate          bool   `json:"estimate"`
}

// handleCompare runs the guardrail rule over a set of plans.
//
// EVERY PLAN IS SIMULATED AT ONE RUN COUNT, decided here and applied to all of
// them. The P5 drawdown and the success rate both move with n, so a comparison
// assembled from differing run counts would rank the dice as much as the plans —
// likelihood.Rank refuses one, and this is where that promise is kept.
func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req compareRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.PlanIDs) < 2 {
		writeError(w, http.StatusBadRequest, "comparing plans needs at least two plan ids")
		return
	}
	if len(req.PlanIDs) > likelihood.MaxComparisonPlans {
		writeError(w, http.StatusBadRequest,
			"more than four plans at once is refused — each one is a full simulation, and the comparison view shows four")
		return
	}

	volatility := defaultVolatility
	if strings.TrimSpace(req.Volatility) != "" {
		v, err := decimal.NewFromString(strings.TrimSpace(req.Volatility))
		if err != nil {
			writeError(w, http.StatusBadRequest, "volatility must be a decimal fraction, e.g. \"0.15\"")
			return
		}
		volatility = v
	}
	runs := s.interactiveRuns("")
	if req.Runs > 0 {
		runs = s.clampInteractiveRuns(req.Runs)
	}

	now := time.Now().UTC()
	baseline, err := s.allocationBaseline(r.Context(), identity, now)
	if err != nil {
		s.internalError(w, "assemble allocation baseline", err)
		return
	}

	out := compareResponse{
		Plans:             make([]likelihoodResponse, 0, len(req.PlanIDs)),
		MonteCarloEnabled: s.Config.Retirement.MonteCarloEnabled,
		Basis:             likelihood.RuleText,
		Estimate:          true,
	}
	candidates := make([]likelihood.Candidate, 0, len(req.PlanIDs))

	for _, raw := range req.PlanIDs {
		planID, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			writeError(w, http.StatusBadRequest, "plan_ids must be uuids")
			return
		}
		row, planReq, err := s.loadPlanRequest(r.Context(), identity, planID)
		if err != nil {
			s.writePlanLoadError(w, err)
			return
		}

		result, sim, err := s.runPlan(baseline, planReq, runs, volatility)
		if err != nil {
			writeError(w, http.StatusBadRequest, row.Name+": "+err.Error())
			return
		}
		resp := s.shapeLikelihood(planReq, result, sim, runs, volatility)
		resp.PlanID, resp.Name = &row.ID, row.Name
		out.Plans = append(out.Plans, resp)

		candidates = append(candidates, likelihood.Candidate{
			PlanID:  row.ID,
			Name:    row.Name,
			Sim:     sim,
			GoalFit: likelihood.EvaluateGoals(goalTargets(result), sim),
		})
	}

	ranking, err := likelihood.Rank(candidates, baseline.RiskFloor)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	out.Ranking = ranking
	writeJSON(w, http.StatusOK, out)
}

// trackingResponse is the drift report plus the snapshots behind the sparkline.
type trackingResponse struct {
	PlanID  uuid.UUID           `json:"plan_id"`
	Name    string              `json:"name"`
	Drift   likelihood.Tracking `json:"drift"`
	History []trackingSnapshot  `json:"history"`
}

type trackingSnapshot struct {
	AsOf          string `json:"as_of"`
	ExpectedLump  string `json:"expected_lump"`
	ExpectedTotal string `json:"expected_total"`
}

// handleTracking is the drift report for an accepted plan.
//
// READ-ONLY. Doc 33 describes Reconcile as writing a plan_trackings row, and the
// snapshot write lives on POST .../track instead: a GET that writes on every
// page load would fill the table with one row per visit and make "drift since
// the last check" mean "drift since you last looked at it".
func (s *Server) handleTracking(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	planID, err := uuid.Parse(chi.URLParam(r, "planID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid plan id")
		return
	}
	row, req, err := s.loadPlanRequest(r.Context(), identity, planID)
	if err != nil {
		s.writePlanLoadError(w, err)
		return
	}

	now := time.Now().UTC()
	baseline, err := s.allocationBaseline(r.Context(), identity, now)
	if err != nil {
		s.internalError(w, "assemble allocation baseline", err)
		return
	}

	drift, err := s.reconcilePlan(r.Context(), identity, baseline, row, req, now)
	if err != nil {
		s.internalError(w, "reconcile plan", err)
		return
	}

	history, err := s.Queries.ListPlanTrackings(r.Context(), dbgen.ListPlanTrackingsParams{
		PlanID: planID, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "list plan trackings", err)
		return
	}
	out := trackingResponse{
		PlanID: row.ID, Name: row.Name, Drift: drift,
		History: make([]trackingSnapshot, 0, len(history)),
	}
	for _, h := range history {
		out.History = append(out.History, trackingSnapshot{
			AsOf:          h.AsOf.Format(time.DateOnly),
			ExpectedLump:  h.ExpectedLump.StringFixed(2),
			ExpectedTotal: h.ExpectedTotal.StringFixed(2),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRecordTracking pins today's snapshot for a plan.
//
// Only the EXPECTED side is stored. Actuals are read live every time the drift
// is computed, so correcting a contribution recorded three months ago corrects
// the history rather than leaving a wrong figure frozen in a row.
func (s *Server) handleRecordTracking(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	planID, err := uuid.Parse(chi.URLParam(r, "planID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid plan id")
		return
	}
	row, req, err := s.loadPlanRequest(r.Context(), identity, planID)
	if err != nil {
		s.writePlanLoadError(w, err)
		return
	}

	now := time.Now().UTC()
	baseline, err := s.allocationBaseline(r.Context(), identity, now)
	if err != nil {
		s.internalError(w, "assemble allocation baseline", err)
		return
	}
	drift, err := s.reconcilePlan(r.Context(), identity, baseline, row, req, now)
	if err != nil {
		s.internalError(w, "reconcile plan", err)
		return
	}

	// The snapshot blob. MONEY INSIDE IT IS A STRING, never a JSON number —
	// export.go casts numeric COLUMNS to text but passes jsonb through
	// untouched, so the continuity guarantee only holds because the writer never
	// puts a number there. See the migration's note.
	snapshot, err := json.Marshal(allocation.SnapshotAssumptions(baseline))
	if err != nil {
		s.internalError(w, "marshal tracking snapshot", err)
		return
	}

	saved, err := s.Queries.UpsertPlanTracking(r.Context(), dbgen.UpsertPlanTrackingParams{
		PlanID:         planID,
		HouseholdID:    identity.HouseholdID,
		AsOf:           now.Truncate(24 * time.Hour),
		ExpectedLump:   drift.ExpectedLump,
		ExpectedTotal:  drift.ExpectedToDate,
		SnapshotInputs: snapshot,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "plan not found")
		return
	} else if err != nil {
		s.internalError(w, "upsert plan tracking", err)
		return
	}

	writeJSON(w, http.StatusCreated, trackingSnapshot{
		AsOf:          saved.AsOf.Format(time.DateOnly),
		ExpectedLump:  saved.ExpectedLump.StringFixed(2),
		ExpectedTotal: saved.ExpectedTotal.StringFixed(2),
	})
}

// --------------------------------------------------------------------------
// Shared machinery
// --------------------------------------------------------------------------

// errPlanNotFound and errPlanUnreadable separate "no such plan of yours" from
// "the saved blob will not parse", because they are a 404 and a 400.
var (
	errPlanNotFound = errors.New("plan not found")
)

// loadPlanRequest reads a saved plan and parses its inputs back into a request.
func (s *Server) loadPlanRequest(
	ctx context.Context, identity auth.Identity, planID uuid.UUID,
) (dbgen.AllocationPlan, allocation.Request, error) {
	row, err := s.Queries.GetAllocationPlan(ctx, dbgen.GetAllocationPlanParams{
		ID: planID, HouseholdID: identity.HouseholdID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, allocation.Request{}, errPlanNotFound
	} else if err != nil {
		return row, allocation.Request{}, err
	}
	req, err := allocation.Load(row.Inputs, int(row.InputVersion))
	if err != nil {
		return row, allocation.Request{}, err
	}
	return row, req, nil
}

func (s *Server) writePlanLoadError(w http.ResponseWriter, err error) {
	if errors.Is(err, errPlanNotFound) {
		writeError(w, http.StatusNotFound, "plan not found")
		return
	}
	// Everything else Load returns is about the saved blob — an unreadable
	// version, a missing money field — and naming it is far more useful than a
	// 500 the user cannot act on.
	writeError(w, http.StatusBadRequest, err.Error())
}

// interactiveRuns is the run count for a path a user is waiting on.
//
// Capped at DefaultMonteCarloRuns rather than at ClampRuns' 10,000 ceiling. A
// four-plan comparison is four full simulations inside one synchronous request,
// and the higher ceiling exists for a deliberate single-plan call, not for a
// page load.
func (s *Server) interactiveRuns(raw string) int {
	return s.clampInteractiveRuns(parseInt(raw, networth.DefaultMonteCarloRuns, 0, networth.DefaultMonteCarloRuns))
}

func (s *Server) clampInteractiveRuns(n int) int {
	n = networth.ClampRuns(n)
	if n > networth.DefaultMonteCarloRuns {
		n = networth.DefaultMonteCarloRuns
	}
	return n
}

// likelihoodFor runs one plan and shapes the response.
func (s *Server) likelihoodFor(
	baseline allocation.Baseline, req allocation.Request, runs int, volatility decimal.Decimal,
) (likelihoodResponse, error) {
	result, sim, err := s.runPlan(baseline, req, runs, volatility)
	if err != nil {
		return likelihoodResponse{}, err
	}
	return s.shapeLikelihood(req, result, sim, runs, volatility), nil
}

// runPlan is the whole computation: the deterministic projection, then the
// distribution over the SAME resolved accounts.
//
// networth.BuildSchedule is what makes the σ=0 agreement a structural property
// rather than a coincidence — the simulation compounds exactly the accounts
// ProjectRetirement compounds, with the caps already pooled, the employer match
// already clamped and the education horizons already resolved.
func (s *Server) runPlan(
	baseline allocation.Baseline, req allocation.Request, runs int, volatility decimal.Decimal,
) (allocation.Result, likelihood.Result, error) {
	result, err := allocation.Run(baseline, req)
	if err != nil {
		return allocation.Result{}, likelihood.Result{}, err
	}
	if !s.Config.Retirement.MonteCarloEnabled {
		// The gate is off. The deterministic figure is the whole answer, and the
		// caller reports that in the basis rather than erroring.
		return result, likelihood.Result{}, nil
	}

	plans, err := allocation.PlanAccounts(baseline, req)
	if err != nil {
		return allocation.Result{}, likelihood.Result{}, err
	}
	assumptions := baseline.Assumptions
	assumptions.Months = req.HorizonYears * 12
	assumptions.FamilyHSA = req.FamilyHSA
	schedule := networth.BuildSchedule(plans, assumptions, baseline.Now)

	// Only the accounts this plan actually simulates. Cash is σ=0 and enters as
	// the Fixed constant; debt is not an asset and never enters at all.
	sim := likelihood.Run(likelihood.Input{
		Accounts:   schedule.Accounts,
		Months:     assumptions.Months,
		Volatility: volatility,
		Target:     req.TargetNestEgg,
		Fixed:      fixedAssets(result),
		Runs:       runs,
	})
	return result, sim, nil
}

// fixedAssets is the terminal value of the plan's zero-volatility buckets.
//
// Cash accrues at its own deposit yield and does not move with the market, so
// simulating it would be a thousand identical draws. It is added to every run's
// total instead — which keeps it in the success rate, where it belongs, and out
// of the drawdown, which is measured on the buckets that can actually fall.
func fixedAssets(result allocation.Result) decimal.Decimal {
	total := decimal.Zero
	for _, b := range result.Buckets {
		if b.Kind == allocation.BucketCash {
			total = total.Add(b.ProjectedValue)
		}
	}
	return total
}

// goalTargets reduces doc 32's goal mapping to what the guardrail's F2 needs.
func goalTargets(result allocation.Result) []likelihood.GoalTarget {
	out := make([]likelihood.GoalTarget, 0, len(result.Goals))
	for _, g := range result.Goals {
		out = append(out, likelihood.GoalTarget{
			Name:      g.Name,
			AccountID: g.AccountID,
			Linked:    g.Linked,
			Achieved:  g.Achieved,
			Target:    g.Target,
		})
	}
	return out
}

// shapeLikelihood turns the two results into one payload.
func (s *Server) shapeLikelihood(
	req allocation.Request, result allocation.Result, sim likelihood.Result,
	runs int, volatility decimal.Decimal,
) likelihoodResponse {
	out := likelihoodResponse{
		HorizonYears:             req.HorizonYears,
		Runs:                     runs,
		Volatility:               volatility.String(),
		ProjectedAtAssumedReturn: result.ProjectedAssets.StringFixed(2),
		MonteCarloEnabled:        s.Config.Retirement.MonteCarloEnabled,
		GapNote:                  gapNote,
		Estimate:                 true,
	}
	if !s.Config.Retirement.MonteCarloEnabled {
		out.Runs = 0
		out.Basis = "Simulation is switched off on this instance (RETIREMENT_MONTE_CARLO_ENABLED), so this is the " +
			"projected value at your assumed return only — one number, not a distribution. " + result.Basis
		out.GapNote = ""
		return out
	}

	figures := simulatedFigures{
		P10:        sim.P10.StringFixed(2),
		P50:        sim.P50.StringFixed(2),
		P90:        sim.P90.StringFixed(2),
		Sigma:      sim.Sigma.StringFixed(2),
		DrawdownP5: sim.DrawdownP5.String(),
		Seed:       sim.Seed,
		ByAccount:  make([]accountOutcomeResponse, 0, len(sim.ByAccount)),
	}
	if sim.SuccessRate != nil {
		v := sim.SuccessRate.String()
		figures.SuccessRate = &v
	}
	if sim.Target != nil {
		v := sim.Target.StringFixed(2)
		figures.Target = &v
	}
	for id, o := range sim.ByAccount {
		figures.ByAccount = append(figures.ByAccount, accountOutcomeResponse{
			AccountID: id, Name: o.Name,
			P10: o.P10.StringFixed(2), P50: o.P50.StringFixed(2), P90: o.P90.StringFixed(2),
		})
	}
	// Map iteration is random; the payload must not be. A client diffing two
	// responses should see a change only when a figure changed.
	sortAccountOutcomes(figures.ByAccount)

	out.Simulated = &figures
	out.Basis = sim.Basis + " " + muConvention
	return out
}

func sortAccountOutcomes(items []accountOutcomeResponse) {
	for i := 1; i < len(items); i++ {
		v := items[i]
		j := i - 1
		for j >= 0 && items[j].Name > v.Name {
			items[j+1] = items[j]
			j--
		}
		items[j+1] = v
	}
}

// reconcilePlan gathers the actuals and runs the reconciler.
//
// ACTUALS ARE READ LIVE, from the same sources the rest of the app reads:
// external flows through reporting.IsExternalFlow, balance movement through
// account_balance_history. A bucket with neither is reported as UNTRACKED rather
// than as zero — "we cannot see what you paid in" and "you paid in nothing" are
// opposite findings.
func (s *Server) reconcilePlan(
	ctx context.Context, identity auth.Identity, baseline allocation.Baseline,
	row dbgen.AllocationPlan, req allocation.Request, now time.Time,
) (likelihood.Tracking, error) {
	since := row.CreatedAt.UTC()

	result, err := allocation.Run(baseline, req)
	if err != nil {
		return likelihood.Tracking{}, err
	}

	flows, err := s.Queries.ListInvestmentTransactionsInRange(ctx, dbgen.ListInvestmentTransactionsInRangeParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
		Date: since, Date_2: now,
	})
	if err != nil {
		return likelihood.Tracking{}, err
	}
	contributed := map[uuid.UUID]decimal.Decimal{}
	sawFlow := map[uuid.UUID]bool{}
	for _, f := range flows {
		subtype := ""
		if f.Subtype != nil {
			subtype = strings.ToLower(*f.Subtype)
		}
		if !reporting.IsExternalFlow(strings.ToLower(f.Type), subtype) {
			continue
		}
		// Plaid's sign convention has a cash credit negative; from the
		// investor's side money going IN is positive. The flip happens once,
		// here, exactly as reporting.ExternalFlows does it.
		contributed[f.AccountID] = contributed[f.AccountID].Add(f.Amount.Neg())
		sawFlow[f.AccountID] = true
	}

	history, err := s.Queries.ListAccountBalanceHistoryInRange(ctx, dbgen.ListAccountBalanceHistoryInRangeParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
		FromDate: since, ToDate: now,
	})
	if err != nil {
		return likelihood.Tracking{}, err
	}
	// The query is ordered by (account, as_of), so first and last per account
	// fall out of one pass.
	type span struct {
		first, last decimal.Decimal
		n           int
	}
	spans := map[uuid.UUID]*span{}
	for _, h := range history {
		sp, ok := spans[h.AccountID]
		if !ok {
			spans[h.AccountID] = &span{first: h.Balance, last: h.Balance, n: 1}
			continue
		}
		sp.last, sp.n = h.Balance, sp.n+1
	}

	in := likelihood.ReconcileInput{
		Since:          since,
		AsOf:           now,
		Lump:           req.Lump,
		RealReturnRate: baseline.Assumptions.RealReturnRate,
		HorizonMonths:  req.HorizonYears * 12,
		DriftThreshold: driftThreshold,
		Buckets:        make([]likelihood.BucketActual, 0, len(result.Buckets)),
	}
	for _, b := range result.Buckets {
		actual := likelihood.BucketActual{
			AccountID:       b.AccountID.String(),
			Name:            b.Name,
			ExpectedLump:    b.AppliedLump,
			ExpectedMonthly: b.AppliedMonthly,
		}
		switch b.Kind {
		case allocation.BucketInvestment:
			if sawFlow[b.AccountID] {
				actual.Actual, actual.Tracked = contributed[b.AccountID], true
			}
		case allocation.BucketCash:
			// Saving into a cash bucket is a balance that went UP.
			if sp, ok := spans[b.AccountID]; ok && sp.n >= 2 {
				actual.Actual, actual.Tracked = sp.last.Sub(sp.first), true
			}
		case allocation.BucketDebt:
			// Paying a debt down is a balance that went DOWN, so the paydown is
			// the movement reversed. Interest accrues against it in the same
			// series, so this is NET principal reduction — which is the honest
			// figure for "did you actually pay this down".
			if sp, ok := spans[b.AccountID]; ok && sp.n >= 2 {
				actual.Actual, actual.Tracked = sp.first.Sub(sp.last), true
			}
		}
		in.Buckets = append(in.Buckets, actual)
	}

	return likelihood.Reconcile(in), nil
}

// driftThreshold is the monthly gap past which a plan is reported as behind.
//
// Twenty-five dollars a month. Not an opinion about a good plan: it is the
// rounding-and-timing band, so a contribution that landed on the 2nd instead of
// the 30th does not flip a plan to "behind" for a month.
var driftThreshold = decimal.RequireFromString("25")
