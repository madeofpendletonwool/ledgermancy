package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/ai"
	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
	"github.com/apex42group/ledgermancy/backend/internal/networth"
)

type netWorthResponse struct {
	AssetsTotal      decimal.Decimal    `json:"assets_total"`
	LiabilitiesTotal decimal.Decimal    `json:"liabilities_total"`
	NetWorth         decimal.Decimal    `json:"net_worth"`
	Breakdown        networth.Breakdown `json:"breakdown"`
	AsOf             string             `json:"as_of"`
}

func (s *Server) handleNetWorth(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	result, err := networth.Compute(r.Context(), s.Queries, identity.HouseholdID)
	if err != nil {
		s.internalError(w, "compute net worth", err)
		return
	}

	writeJSON(w, http.StatusOK, netWorthResponse{
		AssetsTotal:      result.AssetsTotal,
		LiabilitiesTotal: result.LiabilitiesTotal,
		NetWorth:         result.NetWorth,
		Breakdown:        result.Breakdown,
		AsOf:             time.Now().Format(time.DateOnly),
	})
}

type snapshotResponse struct {
	AsOf             string          `json:"as_of"`
	AssetsTotal      decimal.Decimal `json:"assets_total"`
	LiabilitiesTotal decimal.Decimal `json:"liabilities_total"`
	NetWorth         decimal.Decimal `json:"net_worth"`
}

// handleNetWorthHistory returns the recorded trend, defaulting to two years.
func (s *Server) handleNetWorthHistory(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	q := r.URL.Query()
	to := parseDate(q.Get("to"), time.Now())
	from := parseDate(q.Get("from"), to.AddDate(-2, 0, 0))

	rows, err := s.Queries.ListNetWorthSnapshots(r.Context(), dbgen.ListNetWorthSnapshotsParams{
		HouseholdID: identity.HouseholdID,
		AsOf:        from,
		AsOf_2:      to,
	})
	if err != nil {
		s.internalError(w, "net worth history", err)
		return
	}

	out := make([]snapshotResponse, 0, len(rows))
	for _, s := range rows {
		out = append(out, snapshotResponse{
			AsOf:             s.AsOf.Format(time.DateOnly),
			AssetsTotal:      s.AssetsTotal,
			LiabilitiesTotal: s.LiabilitiesTotal,
			NetWorth:         s.NetWorth,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSnapshotNow records today's figure on demand, so a user does not have
// to wait for the scheduled job to see a new point appear.
func (s *Server) handleSnapshotNow(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	result, err := networth.Snapshot(r.Context(), s.Queries, identity.HouseholdID, nil)
	if err != nil {
		s.internalError(w, "snapshot net worth", err)
		return
	}

	writeJSON(w, http.StatusOK, netWorthResponse{
		AssetsTotal:      result.AssetsTotal,
		LiabilitiesTotal: result.LiabilitiesTotal,
		NetWorth:         result.NetWorth,
		Breakdown:        result.Breakdown,
		AsOf:             time.Now().Format(time.DateOnly),
	})
}

type holdingResponse struct {
	ID               uuid.UUID        `json:"id"`
	SecurityName     *string          `json:"security_name"`
	Ticker           *string          `json:"ticker"`
	SecurityType     *string          `json:"security_type"`
	Quantity         decimal.Decimal  `json:"quantity"`
	CostBasis        *decimal.Decimal `json:"cost_basis"`
	Value            *decimal.Decimal `json:"value"`
	Gain             *decimal.Decimal `json:"gain"`
	AccountName      string           `json:"account_name"`
	InstitutionName  *string          `json:"institution_name"`
	IsCashEquivalent bool             `json:"is_cash_equivalent"`
}

func (s *Server) handleListHoldings(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListVisibleHoldings(r.Context(), dbgen.ListVisibleHoldingsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list holdings", err)
		return
	}

	out := make([]holdingResponse, 0, len(rows))
	for _, h := range rows {
		item := holdingResponse{
			ID:               h.ID,
			SecurityName:     h.SecurityName,
			Ticker:           h.Ticker,
			SecurityType:     h.SecurityType,
			Quantity:         h.Quantity,
			AccountName:      h.AccountName,
			InstitutionName:  h.InstitutionName,
			IsCashEquivalent: h.IsCashEquivalent,
			CostBasis:        nullDecimal(h.CostBasis),
			Value:            nullDecimal(h.InstitutionValue),
		}
		// Unrealised gain only means something when both sides are known.
		if h.InstitutionValue.Valid && h.CostBasis.Valid {
			gain := h.InstitutionValue.Decimal.Sub(h.CostBasis.Decimal)
			item.Gain = &gain
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

type liabilityResponse struct {
	ID                 uuid.UUID        `json:"id"`
	Kind               string           `json:"kind"`
	AccountName        string           `json:"account_name"`
	Mask               *string          `json:"mask"`
	InstitutionName    *string          `json:"institution_name"`
	APR                *decimal.Decimal `json:"apr"`
	Balance            *decimal.Decimal `json:"balance"`
	MinimumPayment     *decimal.Decimal `json:"minimum_payment"`
	NextPaymentDueDate *string          `json:"next_payment_due_date"`
	IsOverdue          *bool            `json:"is_overdue"`
}

func (s *Server) handleListLiabilities(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListVisibleLiabilities(r.Context(), dbgen.ListVisibleLiabilitiesParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list liabilities", err)
		return
	}

	out := make([]liabilityResponse, 0, len(rows))
	for _, l := range rows {
		item := liabilityResponse{
			ID:              l.ID,
			Kind:            l.Kind,
			AccountName:     l.AccountName,
			Mask:            l.Mask,
			InstitutionName: l.InstitutionName,
			APR:             nullDecimal(l.Apr),
			Balance:         nullDecimal(l.Balance),
			MinimumPayment:  nullDecimal(l.MinimumPayment),
			IsOverdue:       l.IsOverdue,
		}
		// Student loans and mortgages report a rate rather than an APR.
		if item.APR == nil && l.InterestRatePercentage.Valid {
			rate := l.InterestRatePercentage.Decimal
			item.APR = &rate
		}
		if l.NextPaymentDueDate != nil {
			due := l.NextPaymentDueDate.Format(time.DateOnly)
			item.NextPaymentDueDate = &due
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

type manualAssetResponse struct {
	ID          uuid.UUID       `json:"id"`
	Name        string          `json:"name"`
	Kind        string          `json:"kind"`
	Value       decimal.Decimal `json:"value"`
	IsLiability bool            `json:"is_liability"`
	AsOf        string          `json:"as_of"`
	Notes       *string         `json:"notes"`
}

func (s *Server) handleListManualAssets(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListManualAssets(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "list manual assets", err)
		return
	}

	out := make([]manualAssetResponse, 0, len(rows))
	for _, a := range rows {
		out = append(out, manualAssetResponse{
			ID: a.ID, Name: a.Name, Kind: a.Kind, Value: a.Value,
			IsLiability: a.IsLiability, AsOf: a.AsOf.Format(time.DateOnly), Notes: a.Notes,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type manualAssetRequest struct {
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`
	Value       string  `json:"value"`
	IsLiability bool    `json:"is_liability"`
	Notes       *string `json:"notes"`
}

func (s *Server) handleCreateManualAsset(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req manualAssetRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	// Values arrive as strings so they never pass through a JSON float.
	value, err := decimal.NewFromString(req.Value)
	if err != nil {
		writeError(w, http.StatusBadRequest, "value must be a decimal number, e.g. \"425000.00\"")
		return
	}
	if req.Kind == "" {
		req.Kind = "other"
	}

	asset, err := s.Queries.CreateManualAsset(r.Context(), dbgen.CreateManualAssetParams{
		HouseholdID: identity.HouseholdID,
		CreatedBy:   &identity.UserID,
		Name:        req.Name,
		Kind:        req.Kind,
		Value:       value,
		IsLiability: req.IsLiability,
		Notes:       req.Notes,
	})
	if err != nil {
		s.internalError(w, "create manual asset", err)
		return
	}

	writeJSON(w, http.StatusCreated, manualAssetResponse{
		ID: asset.ID, Name: asset.Name, Kind: asset.Kind, Value: asset.Value,
		IsLiability: asset.IsLiability, AsOf: asset.AsOf.Format(time.DateOnly), Notes: asset.Notes,
	})
}

func (s *Server) handleDeleteManualAsset(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	assetID, err := uuid.Parse(chi.URLParam(r, "assetID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid asset id")
		return
	}

	if err := s.Queries.DeleteManualAsset(r.Context(), dbgen.DeleteManualAssetParams{
		ID: assetID, HouseholdID: identity.HouseholdID,
	}); err != nil {
		s.internalError(w, "delete manual asset", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type projectionResponse struct {
	Assumptions struct {
		MonthlySurplus    decimal.Decimal `json:"monthly_surplus"`
		AnnualReturnRate  decimal.Decimal `json:"annual_return_rate"`
		AnnualDebtPaydown decimal.Decimal `json:"annual_debt_paydown"`
		Months            int             `json:"months"`
	} `json:"assumptions"`
	Points []networth.ProjectionPoint `json:"points"`
	// Estimate is always true. It is in the payload so a client cannot render
	// these numbers without the caveat travelling with them.
	Estimate bool   `json:"estimate"`
	Basis    string `json:"basis"`
	// Narrative is an AI-written phrasing of the same milestones, present only
	// when AI is enabled and the call succeeded. Nil is normal — the numbers and
	// the caveat render without it.
	Narrative *string `json:"narrative,omitempty"`
}

// handleProjection rolls the current position forward.
//
// Defaults come from the household's own trailing-twelve-month behaviour, so
// the projection starts from what actually happened rather than an invented
// figure. Every assumption is overridable by query parameter and echoed back.
func (s *Server) handleProjection(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	current, err := networth.Compute(ctx, s.Queries, identity.HouseholdID)
	if err != nil {
		s.internalError(w, "compute net worth", err)
		return
	}

	// Default surplus: the trailing year's average monthly leftover. Shared with
	// the narrator so the two never drift.
	now := time.Now()
	defaultSurplus, err := networth.DefaultMonthlySurplus(ctx, s.Queries, identity.HouseholdID, identity.UserID, now)
	if err != nil {
		s.internalError(w, "spending summary", err)
		return
	}

	q := r.URL.Query()
	assumptions := networth.Assumptions{
		MonthlySurplus:    parseDecimal(q.Get("monthly_surplus"), defaultSurplus),
		AnnualReturnRate:  parseDecimal(q.Get("annual_return_rate"), decimal.RequireFromString("0.06")),
		AnnualDebtPaydown: parseDecimal(q.Get("annual_debt_paydown"), decimal.Zero),
		Months:            parseInt(q.Get("months"), 120, 1, 600),
	}

	resp := projectionResponse{
		Points:   networth.Project(current, assumptions),
		Estimate: true,
		Basis:    "Straight-line projection from your current balances and trailing-12-month averages. Not a market forecast.",
	}
	resp.Assumptions.MonthlySurplus = assumptions.MonthlySurplus
	resp.Assumptions.AnnualReturnRate = assumptions.AnnualReturnRate
	resp.Assumptions.AnnualDebtPaydown = assumptions.AnnualDebtPaydown
	resp.Assumptions.Months = assumptions.Months

	// Narration is a best-effort supplement: only attempted when AI is on, and a
	// slow or failed call leaves Narrative nil so the table still renders.
	if s.AI.Enabled() {
		in := s.buildForecastInput(ctx, identity, resp.Points, assumptions, resp.Basis, now)
		if text, err := s.AI.ForecastNarration(ctx, in); err != nil {
			slog.Warn("forecast narration failed", "household_id", identity.HouseholdID, "error", err)
		} else if text != "" {
			resp.Narrative = &text
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// buildForecastInput assembles the finished figures the narrator quotes: the
// milestone net-worth values (read straight from the projection, never
// recomputed) and the discretionary "lever" the household could trim, computed
// in decimal from the same reporting queries the summary uses.
func (s *Server) buildForecastInput(
	ctx context.Context,
	identity auth.Identity,
	points []networth.ProjectionPoint,
	assumptions networth.Assumptions,
	basis string,
	now time.Time,
) ai.ForecastInput {
	in := ai.ForecastInput{
		MonthlySurplus:   assumptions.MonthlySurplus.StringFixed(2),
		AnnualReturnRate: assumptions.AnnualReturnRate.String(),
		Basis:            basis,
	}
	for _, i := range networth.ForecastMilestones {
		if i < len(points) {
			in.Milestones = append(in.Milestones, ai.ForecastMilestone{
				Months:   i + 1,
				Month:    points[i].Month,
				NetWorth: points[i].NetWorth.StringFixed(2),
			})
		}
	}
	if lever, ok := s.topDiscretionaryLever(ctx, identity, now); ok {
		in.Levers = append(in.Levers, lever)
	}
	return in
}

// leverFloorDollars keeps a trivially-small overage from being narrated as a
// "lever" — noise below this is not worth suggesting the reader cut.
const leverFloorDollars = 25

// topDiscretionaryLever finds the discretionary category the household is
// spending the most above its own 6-month average this month, and reports that
// overage as a monthly saving. The subtraction is done here in decimal; the
// model only quotes the finished figure.
func (s *Server) topDiscretionaryLever(ctx context.Context, identity auth.Identity, now time.Time) (ai.ForecastLever, bool) {
	mStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	mEnd := mStart.AddDate(0, 1, -1)

	thisMonth, err := s.Queries.GetSpendingByCategory(ctx, dbgen.GetSpendingByCategoryParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID, Date: mStart, Date_2: mEnd,
	})
	if err != nil {
		return ai.ForecastLever{}, false
	}
	averages, err := s.Queries.GetCategoryAverages(ctx, dbgen.GetCategoryAveragesParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
		Date: mStart.AddDate(0, -6, 0), Date_2: mStart.AddDate(0, 0, -1),
	})
	if err != nil {
		return ai.ForecastLever{}, false
	}
	avgBySlug := make(map[string]decimal.Decimal, len(averages))
	for _, a := range averages {
		avgBySlug[a.CategorySlug] = a.MonthlyAverage
	}

	floor := decimal.NewFromInt(leverFloorDollars)
	var best ai.ForecastLever
	var bestDelta decimal.Decimal
	for _, c := range thisMonth {
		if c.IsFixed {
			continue // only discretionary categories are truly a lever
		}
		avg, ok := avgBySlug[c.CategorySlug]
		if !ok || !avg.IsPositive() {
			continue
		}
		delta := c.Total.Sub(avg)
		if delta.LessThan(floor) || !delta.GreaterThan(bestDelta) {
			continue
		}
		bestDelta = delta
		best = ai.ForecastLever{
			Label:          c.CategoryName,
			MonthlySavings: delta.Round(2).StringFixed(2),
			Basis:          "your 6-month average",
		}
	}
	return best, bestDelta.IsPositive()
}

func parseDecimal(raw string, fallback decimal.Decimal) decimal.Decimal {
	if raw == "" {
		return fallback
	}
	v, err := decimal.NewFromString(raw)
	if err != nil {
		return fallback
	}
	return v
}
