package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/obligations"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/reporting"
)

// The bill calendar's HTTP surface. Every figure here is finished server-side in
// exact decimal and crosses the wire as a StringFixed(2) string; every due date
// is derived by the SQL expansion. Nothing on this endpoint invites the client
// to compute anything.

// obligationResponse is one recurring obligation plus the parts derived from it:
// the next date it falls due and what it costs per month. Neither is stored —
// storing a next-due date is a cache that goes stale the moment the calendar
// rolls over.
type obligationResponse struct {
	ID            uuid.UUID  `json:"id"`
	Label         string     `json:"label"`
	Amount        string     `json:"amount"`
	CategoryID    *uuid.UUID `json:"category_id"`
	AccountID     *uuid.UUID `json:"account_id"`
	IntervalCount int32      `json:"interval_count"`
	IntervalUnit  string     `json:"interval_unit"`
	Cadence       string     `json:"cadence"`
	AnchorDate    string     `json:"anchor_date"`
	EndDate       *string    `json:"end_date"`
	NextDue       *string    `json:"next_due"`
	// DaysUntilDue is null when there is no next occurrence. Computed here so
	// every surface counts the days the same way.
	DaysUntilDue    *int    `json:"days_until_due"`
	MonthlyEstimate string  `json:"monthly_estimate"`
	Source          string  `json:"source"`
	MerchantKey     *string `json:"merchant_key"`
	IsShared        bool    `json:"is_shared"`
	IsActive        bool    `json:"is_active"`
	// UserEdited tells the UI that promotion will no longer touch this row, so
	// it can say so rather than leave the user wondering if a fix will stick.
	UserEdited bool `json:"user_edited"`
	IsPersonal bool `json:"is_personal"`
}

func (s *Server) handleListObligations(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()
	now := time.Now().UTC()

	rows, err := s.Queries.ListObligations(ctx, dbgen.ListObligationsParams{
		HouseholdID: identity.HouseholdID, UserID: &identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list obligations", err)
		return
	}

	next, err := obligations.NextDue(ctx, s.Queries, identity.HouseholdID, identity.UserID, rows, now)
	if err != nil {
		s.internalError(w, "derive next due dates", err)
		return
	}

	out := make([]obligationResponse, 0, len(rows))
	for _, o := range rows {
		out = append(out, buildObligationResponse(o, next[o.ID], now))
	}
	writeJSON(w, http.StatusOK, out)
}

func buildObligationResponse(o dbgen.RecurringObligation, nextDue time.Time, now time.Time) obligationResponse {
	cadence := obligations.Cadence{Count: o.IntervalCount, Unit: o.IntervalUnit}
	resp := obligationResponse{
		ID:              o.ID,
		Label:           o.Label,
		Amount:          o.Amount.StringFixed(2),
		CategoryID:      o.CategoryID,
		AccountID:       o.AccountID,
		IntervalCount:   o.IntervalCount,
		IntervalUnit:    o.IntervalUnit,
		Cadence:         cadence.Label(),
		AnchorDate:      o.AnchorDate.Format(time.DateOnly),
		MonthlyEstimate: obligations.MonthlyEstimate(o.Amount, cadence).StringFixed(2),
		Source:          o.Source,
		MerchantKey:     o.MerchantKey,
		IsShared:        o.IsShared,
		IsActive:        o.IsActive,
		UserEdited:      o.UserEdited,
		IsPersonal:      o.UserID != nil && !o.IsShared,
	}
	if o.EndDate != nil {
		e := o.EndDate.Format(time.DateOnly)
		resp.EndDate = &e
	}
	if !nextDue.IsZero() {
		d := nextDue.Format(time.DateOnly)
		resp.NextDue = &d
		days := int(nextDue.Sub(truncateToDay(now)).Hours() / 24)
		resp.DaysUntilDue = &days
	}
	return resp
}

// truncateToDay is the UTC calendar date of t, so a "days until due" count is a
// difference of calendar days rather than of clock times.
func truncateToDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// upsertObligationRequest is the manual entry form's payload. Amounts and dates
// are strings so neither ever passes through a JSON float.
type upsertObligationRequest struct {
	Label         string     `json:"label"`
	Amount        string     `json:"amount"`
	CategoryID    *uuid.UUID `json:"category_id"`
	AccountID     *uuid.UUID `json:"account_id"`
	IntervalCount int32      `json:"interval_count"`
	IntervalUnit  string     `json:"interval_unit"`
	AnchorDate    string     `json:"anchor_date"`
	EndDate       string     `json:"end_date"`
	// Personal keeps an obligation out of the household view — the same
	// per-item visibility choice institutions and goals already offer.
	Personal bool  `json:"personal"`
	IsActive *bool `json:"is_active"`
}

type validatedObligation struct {
	amount   decimal.Decimal
	anchor   time.Time
	endDate  *time.Time
	isActive bool
}

func validateObligationBody(req upsertObligationRequest) (validatedObligation, error) {
	var v validatedObligation
	if req.Label == "" {
		return v, errors.New("label is required")
	}
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil {
		return v, errors.New("amount must be a decimal number, e.g. \"1200.00\"")
	}
	if !amount.IsPositive() {
		return v, errors.New("amount must be greater than zero")
	}
	if req.IntervalCount <= 0 {
		return v, errors.New("interval_count must be at least 1")
	}
	if !obligations.IsValidUnit(req.IntervalUnit) {
		return v, errors.New("interval_unit must be one of day, week, month, year")
	}
	anchor, err := time.Parse(time.DateOnly, req.AnchorDate)
	if err != nil {
		return v, errors.New("anchor_date must be formatted YYYY-MM-DD")
	}
	v.amount = amount
	v.anchor = anchor
	v.isActive = true
	if req.IsActive != nil {
		v.isActive = *req.IsActive
	}
	if req.EndDate != "" {
		end, perr := time.Parse(time.DateOnly, req.EndDate)
		if perr != nil {
			return v, errors.New("end_date must be formatted YYYY-MM-DD")
		}
		// The table enforces this too; catching it here turns a 500 into a
		// sentence the form can show next to the field.
		if end.Before(anchor) {
			return v, errors.New("end_date must not be before the first due date")
		}
		v.endDate = &end
	}
	return v, nil
}

func (s *Server) handleCreateObligation(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req upsertObligationRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	v, err := validateObligationBody(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	o, err := s.Queries.CreateObligation(r.Context(), dbgen.CreateObligationParams{
		HouseholdID:   identity.HouseholdID,
		UserID:        &identity.UserID,
		IsShared:      !req.Personal,
		Label:         req.Label,
		Amount:        v.amount,
		CategoryID:    req.CategoryID,
		AccountID:     req.AccountID,
		IntervalCount: req.IntervalCount,
		IntervalUnit:  req.IntervalUnit,
		AnchorDate:    v.anchor,
		EndDate:       v.endDate,
	})
	if err != nil {
		s.internalError(w, "create obligation", err)
		return
	}

	now := time.Now().UTC()
	next, err := obligations.NextDue(r.Context(), s.Queries, identity.HouseholdID, identity.UserID,
		[]dbgen.RecurringObligation{o}, now)
	if err != nil {
		s.internalError(w, "derive next due date", err)
		return
	}
	writeJSON(w, http.StatusCreated, buildObligationResponse(o, next[o.ID], now))
}

func (s *Server) handleUpdateObligation(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	obligationID, err := uuid.Parse(chi.URLParam(r, "obligationID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid obligation id")
		return
	}

	var req upsertObligationRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	v, err := validateObligationBody(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	o, err := s.Queries.UpdateObligation(r.Context(), dbgen.UpdateObligationParams{
		ID:            obligationID,
		HouseholdID:   identity.HouseholdID,
		UserID:        &identity.UserID,
		Label:         req.Label,
		Amount:        v.amount,
		CategoryID:    req.CategoryID,
		AccountID:     req.AccountID,
		IntervalCount: req.IntervalCount,
		IntervalUnit:  req.IntervalUnit,
		AnchorDate:    v.anchor,
		EndDate:       v.endDate,
		IsShared:      !req.Personal,
		IsActive:      v.isActive,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "obligation not found")
		return
	}
	if err != nil {
		s.internalError(w, "update obligation", err)
		return
	}

	next, err := obligations.NextDue(r.Context(), s.Queries, identity.HouseholdID, identity.UserID,
		[]dbgen.RecurringObligation{o}, time.Now().UTC())
	if err != nil {
		s.internalError(w, "derive next due date", err)
		return
	}
	writeJSON(w, http.StatusOK, buildObligationResponse(o, next[o.ID], time.Now().UTC()))
}

// handleDeleteObligation removes a manual obligation outright and deactivates a
// detected one. The asymmetry is deliberate: deleting a detected row would only
// invite the next promotion pass to put it straight back, so "remove" for those
// means "stop counting it", which promotion honours through user_edited.
func (s *Server) handleDeleteObligation(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	obligationID, err := uuid.Parse(chi.URLParam(r, "obligationID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid obligation id")
		return
	}

	existing, err := s.Queries.GetObligation(r.Context(), dbgen.GetObligationParams{
		ID: obligationID, HouseholdID: identity.HouseholdID, UserID: &identity.UserID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "obligation not found")
		return
	}
	if err != nil {
		s.internalError(w, "load obligation", err)
		return
	}

	if existing.Source == "detected" {
		if _, err := s.Queries.SetObligationActive(r.Context(), dbgen.SetObligationActiveParams{
			ID: obligationID, HouseholdID: identity.HouseholdID,
			UserID: &identity.UserID, IsActive: false,
		}); err != nil {
			s.internalError(w, "deactivate obligation", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err := s.Queries.DeleteObligation(r.Context(), dbgen.DeleteObligationParams{
		ID: obligationID, HouseholdID: identity.HouseholdID, UserID: &identity.UserID,
	}); err != nil {
		s.internalError(w, "delete obligation", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// upcomingObligationResponse is one occurrence — an obligation on one date.
// Several rows can share obligation_id; that is what expansion produces.
type upcomingObligationResponse struct {
	ObligationID uuid.UUID  `json:"obligation_id"`
	Label        string     `json:"label"`
	Amount       string     `json:"amount"`
	CategoryID   *uuid.UUID `json:"category_id"`
	AccountID    *uuid.UUID `json:"account_id"`
	Cadence      string     `json:"cadence"`
	Source       string     `json:"source"`
	DueDate      string     `json:"due_date"`
	DaysUntilDue int        `json:"days_until_due"`
}

type upcomingResponse struct {
	From  string                       `json:"from"`
	To    string                       `json:"to"`
	Total string                       `json:"total"`
	Items []upcomingObligationResponse `json:"items"`
}

func (s *Server) handleUpcomingObligations(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	days := horizonDays(r)
	today := truncateToDay(time.Now())
	to := today.AddDate(0, 0, days)

	items, err := obligations.ListUpcoming(r.Context(), s.Queries,
		identity.HouseholdID, identity.UserID, today, to)
	if err != nil {
		s.internalError(w, "list upcoming obligations", err)
		return
	}

	total := decimal.Zero
	out := make([]upcomingObligationResponse, 0, len(items))
	for _, o := range items {
		total = total.Add(o.Amount)
		out = append(out, upcomingObligationResponse{
			ObligationID: o.ObligationID,
			Label:        o.Label,
			Amount:       o.Amount.StringFixed(2),
			CategoryID:   o.CategoryID,
			AccountID:    o.AccountID,
			Cadence:      o.Cadence.Label(),
			Source:       o.Source,
			DueDate:      o.DueDate.Format(time.DateOnly),
			DaysUntilDue: int(o.DueDate.Sub(today).Hours() / 24),
		})
	}

	writeJSON(w, http.StatusOK, upcomingResponse{
		From:  today.Format(time.DateOnly),
		To:    to.Format(time.DateOnly),
		Total: total.StringFixed(2),
		Items: out,
	})
}

type projectionPointResponse struct {
	Date    string `json:"date"`
	Balance string `json:"balance"`
	Due     string `json:"due"`
	// EstimatedBalance layers a trailing-median income/spending estimate on top
	// of Balance. Absent when the household has no income history to estimate
	// from — see EstimatedProjection.HasIncomeHistory.
	EstimatedBalance string `json:"estimated_balance,omitempty"`
}

type accountProjectionResponse struct {
	AccountID       *uuid.UUID                `json:"account_id"`
	Name            string                    `json:"name"`
	Mask            *string                   `json:"mask"`
	InstitutionName *string                   `json:"institution_name"`
	CurrentBalance  string                    `json:"current_balance"`
	LowestBalance   string                    `json:"lowest_balance"`
	LowestDate      string                    `json:"lowest_date"`
	GoesNegative    bool                      `json:"goes_negative"`
	Points          []projectionPointResponse `json:"points"`
}

// projectionEstimateResponse is the trailing-median estimate layered on top of
// the known-obligations line — see EstimatedProjection. income_months lets the
// UI caveat a thin history the same way Safe to Spend does; has_income_history
// tells it when to not draw the estimate line at all.
type projectionEstimateResponse struct {
	ExpectedMonthlyIncome string `json:"expected_monthly_income"`
	ExtraMonthlySpend     string `json:"extra_monthly_spend"`
	IncomeMonths          int    `json:"income_months"`
	HasIncomeHistory      bool   `json:"has_income_history"`
}

// balanceProjectionResponse is deliberately explicit that combined/accounts is
// a projection from KNOWN obligations. unassigned_total is part of that
// honesty: it is the money the per-account lines cannot show because no
// account was named. estimate and each point's estimated_balance are the
// separate, clearly-labeled guess layered on top — see projectionEstimateResponse.
type balanceProjectionResponse struct {
	From            string                      `json:"from"`
	To              string                      `json:"to"`
	Combined        accountProjectionResponse   `json:"combined"`
	Accounts        []accountProjectionResponse `json:"accounts"`
	UnassignedTotal string                      `json:"unassigned_total"`
	TotalDue        string                      `json:"total_due"`
	Estimate        projectionEstimateResponse  `json:"estimate"`
}

func (s *Server) handleObligationProjection(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	est, err := reporting.BuildEstimatedProjection(r.Context(), s.Queries,
		identity.HouseholdID, identity.UserID, time.Now(), horizonDays(r))
	if err != nil {
		s.internalError(w, "project balances", err)
		return
	}
	proj := est.Known

	accounts := make([]accountProjectionResponse, 0, len(proj.Accounts))
	for _, a := range proj.Accounts {
		id := a.AccountID
		accounts = append(accounts, buildAccountProjection(&id, a, est.EstimatedAccounts[a.AccountID]))
	}

	writeJSON(w, http.StatusOK, balanceProjectionResponse{
		From:            proj.From.Format(time.DateOnly),
		To:              proj.To.Format(time.DateOnly),
		Combined:        buildAccountProjection(nil, proj.Combined, est.EstimatedCombined),
		Accounts:        accounts,
		UnassignedTotal: proj.UnassignedTotal.StringFixed(2),
		TotalDue:        proj.TotalDue.StringFixed(2),
		Estimate: projectionEstimateResponse{
			ExpectedMonthlyIncome: est.ExpectedMonthlyIncome.StringFixed(2),
			ExtraMonthlySpend:     est.ExtraMonthlySpend.StringFixed(2),
			IncomeMonths:          est.IncomeMonths,
			HasIncomeHistory:      est.HasIncomeHistory,
		},
	})
}

// buildAccountProjection zips a's known Points with the parallel estimated
// balances BuildEstimatedProjection computed for the same series (nil, or
// shorter than Points, when there's no income history to estimate from).
func buildAccountProjection(id *uuid.UUID, a obligations.AccountProjection, estimated []decimal.Decimal) accountProjectionResponse {
	points := make([]projectionPointResponse, 0, len(a.Points))
	for i, p := range a.Points {
		point := projectionPointResponse{
			Date:    p.Date.Format(time.DateOnly),
			Balance: p.Balance.StringFixed(2),
			Due:     p.Due.StringFixed(2),
		}
		if i < len(estimated) {
			point.EstimatedBalance = estimated[i].StringFixed(2)
		}
		points = append(points, point)
	}
	return accountProjectionResponse{
		AccountID:       id,
		Name:            a.Name,
		Mask:            a.Mask,
		InstitutionName: a.InstitutionName,
		CurrentBalance:  a.CurrentBalance.StringFixed(2),
		LowestBalance:   a.LowestBalance.StringFixed(2),
		LowestDate:      a.LowestDate.Format(time.DateOnly),
		GoesNegative:    a.GoesNegative,
		Points:          points,
	}
}

// horizonDays reads the ?days= window, clamped to what a projection from known
// obligations can honestly cover.
func horizonDays(r *http.Request) int {
	days := obligations.DefaultHorizonDays
	if raw := r.URL.Query().Get("days"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			days = n
		}
	}
	if days > obligations.MaxHorizonDays {
		days = obligations.MaxHorizonDays
	}
	return days
}
