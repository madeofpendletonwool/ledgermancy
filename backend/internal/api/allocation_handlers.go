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
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/goals"
)

// The allocation planner surface: /api/allocation and /api/accounts/idle-cash.
//
// TWO RULES, and the first one is the reason this file is short.
//
// NOTHING HERE WRITES ANYTHING EXCEPT A SAVED PLAN. POST /api/allocation/plan
// runs a projection and returns it; it touches no account, no goal, no
// contribution and no assumption. The engine is a pure function of a baseline it
// was handed, so the only way this could mutate the household is if a handler
// went out of its way to. None does, and the verification snapshots every
// relevant table before and after to prove it.
//
// AND A SAVED PLAN STORES INPUTS, NEVER RESULTS. Opening a plan recomputes it
// against the live baseline, so a plan saved in March reads against today's
// balances rather than March's — a stored projection is a figure that quietly
// stops being true and keeps being displayed.

// --------------------------------------------------------------------------
// Request / response shapes
// --------------------------------------------------------------------------

// allocationSplitRequest is one bucket line. Every money-ish value is a STRING
// on the wire, like every other decimal in this API: a JSON number would hand
// the browser a float to re-round.
type allocationSplitRequest struct {
	AccountID string `json:"account_id"`
	// Percentages, 0–100.
	LumpPct    string `json:"lump_pct"`
	MonthlyPct string `json:"monthly_pct"`
	// RealReturnRate is a fraction ("0.06"); omit or send "" to use the
	// household's assumed rate.
	RealReturnRate *string `json:"real_return_rate"`
}

type allocationRunRequest struct {
	Lump          string  `json:"lump"`
	Monthly       string  `json:"monthly"`
	HorizonYears  int     `json:"horizon_years"`
	TargetNestEgg *string `json:"target_nest_egg"`
	FamilyHSA     bool    `json:"family_hsa"`

	Splits []allocationSplitRequest `json:"splits"`
}

// savePlanRequest is a run request plus a name. The plan is saved from the same
// shape it was run from, so a saved plan cannot differ from the one the user
// just looked at.
type savePlanRequest struct {
	Name string `json:"name"`
	allocationRunRequest
}

// bucketOption is one allocatable bucket, for the UI's picker. It carries what a
// user needs to choose: what the account is, what it holds, and — for an
// investment account — the cap and eligibility it is already subject to.
type bucketOption struct {
	AccountID   uuid.UUID `json:"account_id"`
	Name        string    `json:"name"`
	Institution string    `json:"institution,omitempty"`
	Kind        string    `json:"kind"`
	Treatment   string    `json:"treatment,omitempty"`
	Subtype     string    `json:"subtype,omitempty"`
	Balance     string    `json:"balance"`
	// ExistingMonthly is what is already being paid in, so a user is not
	// surprised when their "50% of $1,800" is capped by a contribution they
	// forgot they had set.
	ExistingMonthly string  `json:"existing_monthly"`
	DepositAPY      *string `json:"deposit_apy,omitempty"`
	// APR and MinimumPayment are the debt terms, null where unknown — never
	// zero, which is a genuine 0% card.
	APR            *string `json:"apr,omitempty"`
	MinimumPayment *string `json:"minimum_payment,omitempty"`
}

type allocationBucketsResponse struct {
	Buckets []bucketOption `json:"buckets"`
	// The household's own assumptions, so the UI can pre-fill the per-bucket
	// return inputs rather than inventing a default.
	RealReturnRate string `json:"real_return_rate"`
	InflationRate  string `json:"inflation_rate"`
	// Excluded investment accounts, named. An account silently missing from the
	// picker is a user wondering where their brokerage went.
	ExcludedAccounts []string `json:"excluded_accounts"`
	FilingStatus     string   `json:"filing_status,omitempty"`
	MAGIKnown        bool     `json:"magi_known"`
	AgeKnown         bool     `json:"age_known"`
}

type planResponse struct {
	ID           uuid.UUID                    `json:"id"`
	Name         string                       `json:"name"`
	InputVersion int                          `json:"input_version"`
	Inputs       allocation.StoredInputs      `json:"inputs"`
	Assumptions  allocation.StoredAssumptions `json:"assumptions"`
	CreatedAt    string                       `json:"created_at"`
	UpdatedAt    string                       `json:"updated_at"`
	// Result is the plan RECOMPUTED against the live baseline. Present on a
	// single-plan read; omitted from the list, where recomputing every plan
	// would be a projection per row for a screen that only needs names.
	Result *allocation.Result `json:"result,omitempty"`
	// ResultError carries a plan that no longer runs — an account that has since
	// been closed, say. The plan still opens and still shows its inputs, because
	// a saved decision the user can no longer read is worse than one that cannot
	// be projected.
	ResultError string `json:"result_error,omitempty"`
}

// --------------------------------------------------------------------------
// Handlers
// --------------------------------------------------------------------------

// handleAllocationBuckets lists what money can be allocated to.
func (s *Server) handleAllocationBuckets(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	baseline, err := s.allocationBaseline(r.Context(), identity, time.Now().UTC())
	if err != nil {
		s.internalError(w, "assemble allocation baseline", err)
		return
	}

	resp := allocationBucketsResponse{
		Buckets:          make([]bucketOption, 0, len(baseline.Buckets)),
		RealReturnRate:   baseline.Assumptions.RealReturnRate.String(),
		InflationRate:    baseline.InflationRate.String(),
		ExcludedAccounts: baseline.UntaggedAccounts,
		FilingStatus:     baseline.FilingStatus,
		MAGIKnown:        baseline.MAGI.Valid,
		AgeKnown:         baseline.AgeKnown,
	}
	for _, b := range baseline.Buckets {
		opt := bucketOption{
			AccountID: b.AccountID, Name: b.Name, Institution: b.Institution,
			Kind: string(b.Kind), Treatment: b.Treatment, Subtype: b.Subtype,
			Balance:         b.Balance.StringFixed(2),
			ExistingMonthly: b.ExistingMonthly.StringFixed(2),
		}
		if b.DepositAPY.Valid {
			v := b.DepositAPY.Decimal.StringFixed(2)
			opt.DepositAPY = &v
		}
		if b.Kind == allocation.BucketDebt {
			if apr, ok := knownTerm(b.Terms.APR, b.Terms.APRSource); ok {
				opt.APR = &apr
			}
			if pay, ok := knownTerm(b.Terms.Payment, b.Terms.PaymentSource); ok {
				opt.MinimumPayment = &pay
			}
		}
		resp.Buckets = append(resp.Buckets, opt)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRunAllocation runs a plan WITHOUT saving it. Read-only in the strictest
// sense: it opens no transaction and issues no write.
func (s *Server) handleRunAllocation(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req allocationRunRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	parsed, err := parseAllocationRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	now := time.Now().UTC()
	baseline, err := s.allocationBaseline(r.Context(), identity, now)
	if err != nil {
		s.internalError(w, "assemble allocation baseline", err)
		return
	}
	result, err := allocation.Run(baseline, parsed)
	if err != nil {
		// Every error Run returns is about the REQUEST — a horizon out of
		// bounds, percentages over 100, an account that is not a bucket — so a
		// 400 naming it is the right answer, not a 500.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleSavePlan(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req savePlanRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	parsed, err := parseAllocationRequest(req.allocationRunRequest)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	now := time.Now().UTC()
	baseline, err := s.allocationBaseline(r.Context(), identity, now)
	if err != nil {
		s.internalError(w, "assemble allocation baseline", err)
		return
	}
	// Run before saving. A plan that does not compute is not worth storing, and
	// the error a user gets now is far more useful than the one they would get
	// on reopening it.
	if _, err := allocation.Run(baseline, parsed); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	inputs, err := json.Marshal(allocation.Store(parsed))
	if err != nil {
		s.internalError(w, "marshal allocation inputs", err)
		return
	}
	assumptions, err := json.Marshal(allocation.SnapshotAssumptions(baseline))
	if err != nil {
		s.internalError(w, "marshal allocation assumptions", err)
		return
	}

	userID := identity.UserID
	row, err := s.Queries.CreateAllocationPlan(r.Context(), dbgen.CreateAllocationPlanParams{
		HouseholdID:  identity.HouseholdID,
		CreatedBy:    &userID,
		Name:         name,
		Inputs:       inputs,
		InputVersion: allocation.CurrentInputVersion,
		Assumptions:  assumptions,
	})
	if err != nil {
		s.internalError(w, "create allocation plan", err)
		return
	}
	writeJSON(w, http.StatusCreated, toPlanResponse(row, nil, ""))
}

func (s *Server) handleListPlans(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListAllocationPlans(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "list allocation plans", err)
		return
	}
	out := make([]planResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, toPlanResponse(row, nil, ""))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetPlan opens a saved plan and RECOMPUTES it against the live baseline.
//
// This is the whole reason results are not stored. A plan is a decision about
// what to do with money; the projection that decision rested on is a statement
// about balances, limits and assumptions that have all moved since. Re-running
// is what keeps "your 529 hits 80% of college" true rather than merely saved.
func (s *Server) handleGetPlan(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	planID, err := uuid.Parse(chi.URLParam(r, "planID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid plan id")
		return
	}
	row, err := s.Queries.GetAllocationPlan(r.Context(), dbgen.GetAllocationPlanParams{
		ID: planID, HouseholdID: identity.HouseholdID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "plan not found")
		return
	} else if err != nil {
		s.internalError(w, "get allocation plan", err)
		return
	}

	req, err := allocation.Load(row.Inputs, int(row.InputVersion))
	if err != nil {
		writeJSON(w, http.StatusOK, toPlanResponse(row, nil, err.Error()))
		return
	}
	baseline, err := s.allocationBaseline(r.Context(), identity, time.Now().UTC())
	if err != nil {
		s.internalError(w, "assemble allocation baseline", err)
		return
	}
	result, err := allocation.Run(baseline, req)
	if err != nil {
		// The plan still opens. An account closed since the plan was saved is a
		// thing the user needs to see and edit, not a 500 they cannot act on.
		writeJSON(w, http.StatusOK, toPlanResponse(row, nil, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, toPlanResponse(row, &result, ""))
}

func (s *Server) handleDeletePlan(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	planID, err := uuid.Parse(chi.URLParam(r, "planID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid plan id")
		return
	}
	n, err := s.Queries.DeleteAllocationPlan(r.Context(), dbgen.DeleteAllocationPlanParams{
		ID: planID, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "delete allocation plan", err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "plan not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleIdleCash is the cash-drag report.
func (s *Server) handleIdleCash(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	baseline, err := s.allocationBaseline(r.Context(), identity, time.Now().UTC())
	if err != nil {
		s.internalError(w, "assemble allocation baseline", err)
		return
	}
	writeJSON(w, http.StatusOK, allocation.BuildIdleCash(baseline))
}

// handleAssetLocation is disclosure, not a recommendation. See location.go for
// why that distinction is in the payload rather than only in the copy.
func (s *Server) handleAssetLocation(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, allocation.AssetLocation())
}

// depositAPYRequest sets an account's yield. A null value CLEARS it, which is a
// real operation: "I no longer know what this earns" must be expressible, or the
// cash-drag detector would keep reporting against a rate the user has abandoned.
type depositAPYRequest struct {
	DepositAPY *string `json:"deposit_apy"`
}

func (s *Server) handleSetDepositAPY(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	accountID, err := uuid.Parse(chi.URLParam(r, "accountID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid account id")
		return
	}
	var req depositAPYRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var apy decimal.NullDecimal
	if req.DepositAPY != nil {
		if v := strings.TrimSpace(*req.DepositAPY); v != "" {
			d, err := decimal.NewFromString(v)
			if err != nil {
				writeError(w, http.StatusBadRequest,
					"deposit_apy must be a decimal percent, e.g. \"4.50\" for 4.5%")
				return
			}
			// A percent, and bounded. The upper bound is not an opinion about a
			// good rate — it rejects a user who typed 450 meaning 4.5%, which
			// would otherwise make every other account in the household look
			// like a catastrophic drag.
			if d.IsNegative() || d.GreaterThan(decimal.NewFromInt(maxDepositAPY)) {
				writeError(w, http.StatusBadRequest,
					"deposit_apy is a percent and must be between 0 and 25 — enter 4.50 for 4.5%")
				return
			}
			apy = decimal.NewNullDecimal(d)
		}
	}

	row, err := s.Queries.SetAccountDepositAPY(r.Context(), dbgen.SetAccountDepositAPYParams{
		AccountID: accountID, HouseholdID: identity.HouseholdID,
		UserID: identity.UserID, DepositApy: apy,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "account not found")
		return
	} else if err != nil {
		s.internalError(w, "set deposit apy", err)
		return
	}

	out := map[string]any{"id": row.ID, "name": row.Name, "deposit_apy": nil}
	if row.DepositApy.Valid {
		out["deposit_apy"] = row.DepositApy.Decimal.StringFixed(2)
	}
	writeJSON(w, http.StatusOK, out)
}

// maxDepositAPY bounds the yield input. See handleSetDepositAPY.
const maxDepositAPY = 25

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// allocationBaseline assembles the baseline for the calling identity.
//
// identity.UserID, not uuid.Nil: a plan is built FOR a person, so it sees their
// own private accounts and never their spouse's. The advisor's ranker uses the
// shared scope instead because it runs for a household from a job; the two
// scopes are different on purpose and each names its reason.
func (s *Server) allocationBaseline(
	ctx context.Context, identity auth.Identity, now time.Time,
) (allocation.Baseline, error) {
	return allocation.AssembleBaseline(ctx, s.Queries, identity.HouseholdID, identity.UserID, now)
}

// parseAllocationRequest turns the wire shape into the engine's request,
// rejecting anything that is not a decimal string rather than coercing it.
func parseAllocationRequest(req allocationRunRequest) (allocation.Request, error) {
	out := allocation.Request{
		HorizonYears: req.HorizonYears,
		FamilyHSA:    req.FamilyHSA,
		Splits:       make([]allocation.Split, 0, len(req.Splits)),
	}
	var err error
	if out.Lump, err = parseMoneyField(req.Lump, "lump", true); err != nil {
		return allocation.Request{}, err
	}
	if out.Monthly, err = parseMoneyField(req.Monthly, "monthly", true); err != nil {
		return allocation.Request{}, err
	}
	if req.TargetNestEgg != nil && strings.TrimSpace(*req.TargetNestEgg) != "" {
		t, err := parseMoneyField(*req.TargetNestEgg, "target_nest_egg", false)
		if err != nil {
			return allocation.Request{}, err
		}
		out.TargetNestEgg = decimal.NewNullDecimal(t)
	}

	for _, s := range req.Splits {
		id, err := uuid.Parse(s.AccountID)
		if err != nil {
			return allocation.Request{}, errors.New("splits[].account_id must be a uuid")
		}
		split := allocation.Split{AccountID: id}
		if split.LumpPct, err = parseMoneyField(s.LumpPct, "lump_pct", true); err != nil {
			return allocation.Request{}, err
		}
		if split.MonthlyPct, err = parseMoneyField(s.MonthlyPct, "monthly_pct", true); err != nil {
			return allocation.Request{}, err
		}
		if s.RealReturnRate != nil && strings.TrimSpace(*s.RealReturnRate) != "" {
			rate, err := parseRate(*s.RealReturnRate, "real_return_rate")
			if err != nil {
				return allocation.Request{}, err
			}
			split.RealReturnRate = decimal.NewNullDecimal(rate)
		}
		out.Splits = append(out.Splits, split)
	}
	return out, nil
}

// parseMoneyField reads a decimal string. An empty value is zero when the field
// is optional-with-a-zero-meaning (a lump of nothing is a real plan); it is an
// error otherwise.
func parseMoneyField(raw, field string, emptyIsZero bool) (decimal.Decimal, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if emptyIsZero {
			return decimal.Zero, nil
		}
		return decimal.Zero, errors.New(field + " is required")
	}
	v, err := decimal.NewFromString(raw)
	if err != nil {
		return decimal.Zero, errors.New(field + " must be a decimal string, e.g. \"30000.00\"")
	}
	if v.IsNegative() {
		return decimal.Zero, errors.New(field + " cannot be negative")
	}
	return v, nil
}

// knownTerm renders a debt term only when its SOURCE says somebody actually
// supplied it. A rate nobody knows comes back null, never "0.00" — a genuine 0%
// promotional card and a card whose rate is unknown are different facts, and
// collapsing them is how the most expensive debt in a household is presented as
// the cheapest.
func knownTerm(value decimal.Decimal, source string) (string, bool) {
	if source == goals.TermSourceNone {
		return "", false
	}
	return value.StringFixed(2), true
}

func toPlanResponse(row dbgen.AllocationPlan, result *allocation.Result, resultErr string) planResponse {
	out := planResponse{
		ID: row.ID, Name: row.Name,
		InputVersion: int(row.InputVersion),
		CreatedAt:    row.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:    row.UpdatedAt.UTC().Format(time.RFC3339),
		Result:       result,
		ResultError:  resultErr,
	}
	// A blob that will not unmarshal is reported through ResultError rather than
	// failing the read: the row is still the user's saved decision.
	if err := json.Unmarshal(row.Inputs, &out.Inputs); err != nil && out.ResultError == "" {
		out.ResultError = "this plan's saved inputs could not be read"
	}
	_ = json.Unmarshal(row.Assumptions, &out.Assumptions)
	return out
}
