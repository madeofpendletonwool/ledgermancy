package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/goals"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
)

// goalResponse is one goal plus its DERIVED standing. current_amount and the
// feasibility figures are computed server-side (SQL + decimal), never stored, so
// they can't drift. Money is always fixed-2 strings.
type goalResponse struct {
	ID              uuid.UUID  `json:"id"`
	Scope           string     `json:"scope"`
	PersonID        *uuid.UUID `json:"person_id"`
	Kind            string     `json:"kind"`
	Name            string     `json:"name"`
	TargetAmount    string     `json:"target_amount"`
	TargetDate      *string    `json:"target_date"`
	AccountID       *uuid.UUID `json:"account_id"`
	CategoryID      *uuid.UUID `json:"category_id"`
	CurrentAmount   string     `json:"current_amount"`
	RequiredMonthly string     `json:"required_monthly"`
	Shortfall       string     `json:"shortfall"`
	MonthsLeft      int        `json:"months_left"`
	OnTrack         bool       `json:"on_track"`
	OpenEnded       bool       `json:"open_ended"`
	Achieved        bool       `json:"achieved"`
	CreatedAt       string     `json:"created_at"`
}

// goalVisibility resolves the caller's identity into the three parameters every
// goal query takes. Kept in one place so no call site can forget one: omitting
// allPersonGoals would silently hide a child's goals from their parent, and
// omitting personID would hide a child's goals from the child.
//
// A child sees person-scoped goals only where the person is themselves. An
// adult sees all of them — a parent who cannot see their child's savings goal
// cannot help with it.
func goalVisibility(identity auth.Identity) (userID *uuid.UUID, personID *uuid.UUID, all bool) {
	return &identity.UserID, identity.PersonID, identity.IsAdult()
}

func (s *Server) handleListGoals(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()
	now := time.Now()

	userID, personID, all := goalVisibility(identity)
	rows, err := s.Queries.ListGoals(ctx, dbgen.ListGoalsParams{
		HouseholdID:    identity.HouseholdID,
		UserID:         userID,
		PersonID:       personID,
		AllPersonGoals: all,
	})
	if err != nil {
		s.internalError(w, "list goals", err)
		return
	}

	out := make([]goalResponse, 0, len(rows))
	for _, g := range rows {
		resp, err := s.buildGoalResponse(ctx, g, now)
		if err != nil {
			s.internalError(w, "derive goal standing", err)
			return
		}
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, out)
}

// buildGoalResponse derives progress and feasibility for one goal. Progress is
// the linked account's balance, or accumulated surplus since created_at when
// unlinked. monthlySurplus is the same trailing-average the projection uses, so
// a goal's on-track decision agrees with the forecast the user already sees.
func (s *Server) buildGoalResponse(ctx context.Context, g dbgen.Goal, now time.Time) (goalResponse, error) {
	current, err := s.goalProgress(ctx, g, now)
	if err != nil {
		return goalResponse{}, err
	}

	// Surplus scope matches the goal: a household goal reads shared cashflow; a
	// personal goal reads its owner's.
	//
	// A person-scoped goal reads household cashflow (uuid.Nil). A child has no
	// income or spending in the ledger — their money lives in the allowance
	// ledger, which is deliberately not joined to transactions — so scoping to
	// them would make every such goal read a surplus of zero and report
	// "never" as its completion date.
	surplusScope := uuid.Nil
	if g.Scope == "user" && g.UserID != nil {
		surplusScope = *g.UserID
	}
	surplus, err := networth.DefaultMonthlySurplus(ctx, s.Queries, g.HouseholdID, surplusScope, now)
	if err != nil {
		return goalResponse{}, err
	}

	f := goals.Compute(g.TargetAmount, current, surplus, g.TargetDate, now)

	resp := goalResponse{
		ID:              g.ID,
		Scope:           g.Scope,
		PersonID:        g.PersonID,
		Kind:            g.Kind,
		Name:            g.Name,
		TargetAmount:    g.TargetAmount.StringFixed(2),
		AccountID:       g.AccountID,
		CategoryID:      g.CategoryID,
		CurrentAmount:   current.StringFixed(2),
		RequiredMonthly: f.RequiredMonthly.StringFixed(2),
		Shortfall:       f.Shortfall.StringFixed(2),
		MonthsLeft:      f.MonthsLeft,
		OnTrack:         f.OnTrack,
		OpenEnded:       f.OpenEnded,
		Achieved:        f.Achieved,
		CreatedAt:       g.CreatedAt.UTC().Format(time.RFC3339),
	}
	if g.TargetDate != nil {
		d := g.TargetDate.Format(time.DateOnly)
		resp.TargetDate = &d
	}
	return resp, nil
}

// goalProgress derives current progress toward a goal without storing it.
func (s *Server) goalProgress(ctx context.Context, g dbgen.Goal, now time.Time) (decimal.Decimal, error) {
	if g.AccountID != nil {
		return s.Queries.GetGoalAccountBalance(ctx, dbgen.GetGoalAccountBalanceParams{
			ID: *g.AccountID, HouseholdID: g.HouseholdID,
		})
	}

	// Unlinked: household surplus (income − spending) accumulated since the goal
	// was created, using the same definitions as the spending summary.
	scope := uuid.Nil
	if g.Scope == "user" && g.UserID != nil {
		scope = *g.UserID
	}
	sum, err := s.Queries.GetSpendingSummary(ctx, dbgen.GetSpendingSummaryParams{
		HouseholdID: g.HouseholdID, UserID: scope, Date: g.CreatedAt, Date_2: now,
	})
	if err != nil {
		return decimal.Zero, err
	}
	surplus := sum.Income.Sub(sum.Spending)
	if surplus.IsNegative() {
		surplus = decimal.Zero
	}
	return surplus, nil
}

type upsertGoalRequest struct {
	Scope        string     `json:"scope"` // "household" (default) | "user" | "person"
	PersonID     *uuid.UUID `json:"person_id"` // required iff scope == "person"
	Kind         string     `json:"kind"`  // "savings" (default)
	Name         string     `json:"name"`
	TargetAmount string     `json:"target_amount"`
	TargetDate   string     `json:"target_date"` // YYYY-MM-DD, optional
	AccountID    *uuid.UUID `json:"account_id"`
	CategoryID   *uuid.UUID `json:"category_id"`
}

// validateGoalBody parses and checks a create/update body, returning the pieces
// the queries need. Amounts are decimal strings so they never pass through a
// float; the date, when present, must parse as a calendar date.
func validateGoalBody(req upsertGoalRequest) (amount decimal.Decimal, date *time.Time, scope, kind string, err error) {
	if len(req.Name) == 0 {
		return decimal.Zero, nil, "", "", errors.New("name is required")
	}
	amount, err = decimal.NewFromString(req.TargetAmount)
	if err != nil {
		return decimal.Zero, nil, "", "", errors.New("target_amount must be a decimal number, e.g. \"10000.00\"")
	}
	if !amount.IsPositive() {
		return decimal.Zero, nil, "", "", errors.New("target_amount must be greater than zero")
	}
	if req.TargetDate != "" {
		t, perr := time.Parse(time.DateOnly, req.TargetDate)
		if perr != nil {
			return decimal.Zero, nil, "", "", errors.New("target_date must be formatted YYYY-MM-DD")
		}
		date = &t
	}
	scope = req.Scope
	if scope == "" {
		scope = "household"
	}
	switch scope {
	case "household", "user":
		if req.PersonID != nil {
			return decimal.Zero, nil, "", "", errors.New("person_id is only valid with scope \"person\"")
		}
	case "person":
		// Mirrors the goals_scope_target constraint, so a missing person is a
		// readable 400 rather than a constraint violation.
		if req.PersonID == nil {
			return decimal.Zero, nil, "", "", errors.New("person_id is required for a person-scoped goal")
		}
	default:
		return decimal.Zero, nil, "", "", errors.New("scope must be \"household\", \"user\" or \"person\"")
	}
	kind = req.Kind
	if kind == "" {
		kind = "savings"
	}
	return amount, date, scope, kind, nil
}

func (s *Server) handleCreateGoal(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req upsertGoalRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	amount, date, scope, kind, err := validateGoalBody(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Exactly one of user_id / person_id is set, per scope, satisfying the
	// table's goals_scope_target CHECK.
	var userID, personID *uuid.UUID
	switch scope {
	case "user":
		userID = &identity.UserID
	case "person":
		// Resolve through the household guard: a valid person id from another
		// household must not become a goal here.
		if _, err := s.Queries.GetPerson(r.Context(), dbgen.GetPersonParams{
			ID: *req.PersonID, HouseholdID: identity.HouseholdID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "person not found")
				return
			}
			s.internalError(w, "get person for goal", err)
			return
		}
		personID = req.PersonID
	}

	g, err := s.Queries.CreateGoal(r.Context(), dbgen.CreateGoalParams{
		HouseholdID:  identity.HouseholdID,
		Scope:        scope,
		UserID:       userID,
		PersonID:     personID,
		Kind:         kind,
		Name:         req.Name,
		TargetAmount: amount,
		TargetDate:   date,
		AccountID:    req.AccountID,
		CategoryID:   req.CategoryID,
	})
	if err != nil {
		s.internalError(w, "create goal", err)
		return
	}

	resp, err := s.buildGoalResponse(r.Context(), g, time.Now())
	if err != nil {
		s.internalError(w, "derive goal standing", err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleUpdateGoal(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	goalID, err := uuid.Parse(chi.URLParam(r, "goalID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid goal id")
		return
	}

	var req upsertGoalRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	amount, date, _, _, err := validateGoalBody(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	userID, personID, all := goalVisibility(identity)
	g, err := s.Queries.UpdateGoal(r.Context(), dbgen.UpdateGoalParams{
		ID:             goalID,
		HouseholdID:    identity.HouseholdID,
		UserID:         userID,
		PersonID:       personID,
		AllPersonGoals: all,
		Name:           req.Name,
		TargetAmount: amount,
		TargetDate:   date,
		AccountID:    req.AccountID,
		CategoryID:   req.CategoryID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "goal not found")
		return
	}
	if err != nil {
		s.internalError(w, "update goal", err)
		return
	}

	resp, err := s.buildGoalResponse(r.Context(), g, time.Now())
	if err != nil {
		s.internalError(w, "derive goal standing", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleArchiveGoal(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	goalID, err := uuid.Parse(chi.URLParam(r, "goalID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid goal id")
		return
	}

	userID, personID, all := goalVisibility(identity)
	if err := s.Queries.ArchiveGoal(r.Context(), dbgen.ArchiveGoalParams{
		ID:             goalID,
		HouseholdID:    identity.HouseholdID,
		UserID:         userID,
		PersonID:       personID,
		AllPersonGoals: all,
	}); err != nil {
		s.internalError(w, "archive goal", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// goalProposalResponse is the confirm-before-save proposal from POST
// /api/goals/parse. Nothing is written; Confirm calls the existing create path.
type goalProposalResponse struct {
	Name         string  `json:"name"`
	TargetAmount string  `json:"target_amount"`
	TargetDate   *string `json:"target_date"`
	Kind         string  `json:"kind"`
}

func (s *Server) handleParseGoal(w http.ResponseWriter, r *http.Request) {
	if !s.AI.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "AI features are not configured")
		return
	}

	var req struct {
		Text string `json:"text"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}

	today := time.Now().Format("Monday, 2 January 2006")
	parsed, err := s.AI.ParseGoal(r.Context(), req.Text, today)
	if err != nil {
		s.internalError(w, "parse goal", err)
		return
	}

	// Re-validate the extracted figures in Go: a positive amount, and a date that
	// both parses and lies in the future. A failure is a clean 422 the UI shows
	// as "couldn't understand that", never a saved row.
	amount, err := decimal.NewFromString(parsed.TargetAmount)
	if err != nil || !amount.IsPositive() {
		writeError(w, http.StatusUnprocessableEntity, "couldn't read a savings target from that")
		return
	}

	resp := goalProposalResponse{
		Name:         parsed.Name,
		TargetAmount: amount.StringFixed(2),
		Kind:         "savings",
	}
	if parsed.TargetDate != "" {
		d, derr := time.Parse(time.DateOnly, parsed.TargetDate)
		if derr != nil || !d.After(time.Now()) {
			writeError(w, http.StatusUnprocessableEntity, "the goal date must be a real date in the future")
			return
		}
		ds := d.Format(time.DateOnly)
		resp.TargetDate = &ds
	}
	writeJSON(w, http.StatusOK, resp)
}
