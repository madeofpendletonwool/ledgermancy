package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/reporting"
)

// The Investments surface. Every figure crosses the wire as a decimal STRING,
// and every figure that could not be computed crosses as null with a sentence
// saying why — the page renders the caveat, never a blank where a number should
// be.

// --------------------------------------------------------------------------
// Overview
// --------------------------------------------------------------------------

type investmentAccountResponse struct {
	ID              uuid.UUID        `json:"id"`
	Name            string           `json:"name"`
	Mask            *string          `json:"mask"`
	Subtype         *string          `json:"subtype"`
	InstitutionName *string          `json:"institution_name"`
	Balance         *decimal.Decimal `json:"balance"`
	Currency        string           `json:"currency"`
	// TaxTreatment is null until the user confirms one. The page must not
	// present a suggestion as a fact.
	TaxTreatment *string `json:"tax_treatment"`
	// SuggestedTaxTreatment is inferred from the Plaid subtype, empty when the
	// subtype cannot distinguish (a 401k is reported the same whether it is
	// traditional or Roth).
	SuggestedTaxTreatment string `json:"suggested_tax_treatment"`
	IsManaged             *bool  `json:"is_managed"`
}

type investmentOverviewResponse struct {
	TotalValue decimal.Decimal `json:"total_value"`
	// CostBasis and UnrealisedGain cover only the holdings that report a basis.
	// BasisCoverage says how much value that is, so a partial figure is never
	// read as a whole one.
	CostBasis        decimal.Decimal  `json:"cost_basis"`
	UnrealisedGain   *decimal.Decimal `json:"unrealised_gain"`
	UnrealisedGainPc *decimal.Decimal `json:"unrealised_gain_pct"`
	BasisCoverage    decimal.Decimal  `json:"basis_coverage_value"`
	BasisExcluded    int              `json:"basis_excluded_holdings"`

	Accounts []investmentAccountResponse `json:"accounts"`
	// UntaggedAccounts counts accounts with no confirmed tax treatment. The page
	// puts the tagging prompt in front of the user while this is non-zero.
	UntaggedAccounts int `json:"untagged_accounts"`

	// HistoryDays is how long the app has been recording values. Zero means the
	// performance section has nothing to show yet.
	HistoryDays int `json:"history_days"`
}

func (s *Server) handleInvestmentOverview(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	accounts, err := s.Queries.ListInvestmentAccounts(ctx, dbgen.ListInvestmentAccountsParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list investment accounts", err)
		return
	}

	holdings, err := s.Queries.ListVisibleHoldingsDetailed(ctx, dbgen.ListVisibleHoldingsDetailedParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list holdings", err)
		return
	}

	resp := investmentOverviewResponse{Accounts: make([]investmentAccountResponse, 0, len(accounts))}

	for _, a := range accounts {
		item := investmentAccountResponse{
			ID: a.ID, Name: a.Name, Mask: a.Mask, Subtype: a.Subtype,
			InstitutionName: a.InstitutionName, Currency: a.Currency,
			TaxTreatment: a.TaxTreatment, IsManaged: a.IsManaged,
			Balance: nullDecimal(a.CurrentBalance),
		}
		if a.TaxTreatment == nil {
			resp.UntaggedAccounts++
			if a.Subtype != nil {
				item.SuggestedTaxTreatment = reporting.SuggestTaxTreatment(*a.Subtype)
			}
		}
		if a.CurrentBalance.Valid {
			resp.TotalValue = resp.TotalValue.Add(a.CurrentBalance.Decimal)
		}
		resp.Accounts = append(resp.Accounts, item)
	}

	// Unrealised gain is computed only over holdings where BOTH value and basis
	// are known. Summing a basis that covers 60% of the portfolio against a
	// value that covers all of it would report a gain that is mostly an
	// artefact of the missing data.
	for _, h := range holdings {
		if h.InstitutionValue.Valid && h.CostBasis.Valid {
			resp.CostBasis = resp.CostBasis.Add(h.CostBasis.Decimal)
			resp.BasisCoverage = resp.BasisCoverage.Add(h.InstitutionValue.Decimal)
			continue
		}
		resp.BasisExcluded++
	}
	if resp.CostBasis.IsPositive() {
		gain := resp.BasisCoverage.Sub(resp.CostBasis)
		pct := gain.Div(resp.CostBasis).Mul(decimal.NewFromInt(100)).Round(2)
		resp.UnrealisedGain, resp.UnrealisedGainPc = &gain, &pct
	}

	if earliest, ok, err := reporting.EarliestHistory(ctx, s.Queries, identity.HouseholdID, identity.UserID); err != nil {
		s.internalError(w, "earliest investment history", err)
		return
	} else if ok {
		resp.HistoryDays = int(time.Since(earliest).Hours() / 24)
	}

	writeJSON(w, http.StatusOK, resp)
}

// --------------------------------------------------------------------------
// Performance
// --------------------------------------------------------------------------

type performanceResponse struct {
	Period string `json:"period"`
	// Computable is false when history is too thin for any figure. Caveat
	// explains it in a sentence the page shows in place of the numbers.
	Computable bool   `json:"computable"`
	Caveat     string `json:"caveat"`

	Start string `json:"start"`
	End   string `json:"end"`
	Days  int    `json:"days"`

	StartValue decimal.Decimal `json:"start_value"`
	EndValue   decimal.Decimal `json:"end_value"`
	NetFlows   decimal.Decimal `json:"net_flows"`
	Gain       decimal.Decimal `json:"gain"`

	// Returns are fractions, not percentages: 0.0734 is 7.34%. The client
	// formats; the server does not pre-multiply and then have two conventions.
	TWR        *decimal.Decimal `json:"twr"`
	Annualised *decimal.Decimal `json:"annualised"`
	MWR        *decimal.Decimal `json:"mwr"`
	// MWRNote is non-empty exactly when MWR is null, and says why. A refusal is
	// a legitimate answer for an IRR and must be shown as one.
	MWRNote string `json:"mwr_note"`
}

// handleInvestmentPerformance returns TWR / IRR / gain for one period.
func (s *Server) handleInvestmentPerformance(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	period := r.URL.Query().Get("period")
	if period == "" {
		period = string(reporting.Period1Y)
	}
	if !reporting.ValidPeriod(period) {
		writeError(w, http.StatusBadRequest, "period must be one of: ytd, 1y, 3y, 5y, inception")
		return
	}

	perf, err := reporting.BuildPortfolioPerformance(
		r.Context(), s.Queries, identity.HouseholdID, identity.UserID,
		reporting.Period(period), time.Now())
	if err != nil {
		s.internalError(w, "investment performance", err)
		return
	}

	resp := performanceResponse{
		Period:     period,
		Computable: perf.Computable,
		Caveat:     perf.HistoryCaveat,
	}
	if perf.Computable {
		resp.Start = perf.Start.Format(time.DateOnly)
		resp.End = perf.End.Format(time.DateOnly)
		resp.Days = perf.Days
		resp.StartValue = perf.StartValue
		resp.EndValue = perf.EndValue
		resp.NetFlows = perf.NetFlows
		resp.Gain = perf.Gain
		resp.TWR = perf.TWR
		resp.Annualised = perf.Annualised
		resp.MWR = perf.MWR
		resp.MWRNote = perf.MWRNote
	}
	writeJSON(w, http.StatusOK, resp)
}

// --------------------------------------------------------------------------
// Benchmark comparison
// --------------------------------------------------------------------------

type seriesPointResponse struct {
	Date  string          `json:"date"`
	Value decimal.Decimal `json:"value"`
}

type seriesResponse struct {
	Label  string                `json:"label"`
	Points []seriesPointResponse `json:"points"`
}

type benchmarkResponse struct {
	Period string `json:"period"`
	// Enabled is false when the operator has not opted into outbound price
	// fetching. The page then shows the portfolio line alone and says why there
	// is nothing to compare against, rather than looking broken.
	Enabled bool             `json:"enabled"`
	Series  []seriesResponse `json:"series"`
	// Basis states what the chart shows, so a rebased index is never mistaken
	// for a dollar value.
	Basis string `json:"basis"`
}

func (s *Server) handleInvestmentBenchmarks(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	period := r.URL.Query().Get("period")
	if period == "" {
		period = string(reporting.Period1Y)
	}
	if !reporting.ValidPeriod(period) {
		writeError(w, http.StatusBadRequest, "period must be one of: ytd, 1y, 3y, 5y, inception")
		return
	}

	now := time.Now()
	from := reporting.PeriodStart(reporting.Period(period), now)

	resp := benchmarkResponse{
		Period:  period,
		Enabled: s.Config.Benchmarks.Enabled,
		Series:  make([]seriesResponse, 0, len(s.Config.Benchmarks.Tickers)+1),
		Basis: "Growth of 100, with your own deposits and withdrawals removed. " +
			"Plotting raw balances here would credit the market for money you paid in.",
	}

	snaps, err := s.Queries.ListInvestmentSnapshots(ctx, dbgen.ListInvestmentSnapshotsParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
		AsOf: from, AsOf_2: now,
	})
	if err != nil {
		s.internalError(w, "list investment snapshots", err)
		return
	}

	if len(snaps) >= 2 {
		vals := make([]reporting.Valuation, 0, len(snaps))
		for _, sn := range snaps {
			vals = append(vals, reporting.Valuation{Date: sn.AsOf, Value: sn.MarketValue})
		}
		flows, err := reporting.ExternalFlows(ctx, s.Queries,
			identity.HouseholdID, identity.UserID, vals[0].Date, vals[len(vals)-1].Date)
		if err != nil {
			s.internalError(w, "investment cash flows", err)
			return
		}
		resp.Series = append(resp.Series,
			toSeriesResponse("Your portfolio", reporting.PortfolioGrowthSeries(vals, flows)))
	}

	// Benchmarks are additive: with the fetch disabled, or a series missing
	// because one fetch failed, the portfolio line still renders.
	if s.Config.Benchmarks.Enabled && len(s.Config.Benchmarks.Tickers) > 0 {
		prices, err := s.Queries.ListAssetPrices(ctx, dbgen.ListAssetPricesParams{
			Column1: s.Config.Benchmarks.Tickers, AsOf: from, AsOf_2: now,
		})
		if err != nil {
			// A benchmark is decoration on this chart. Log it and serve the
			// portfolio line rather than failing the whole request.
			slog.Warn("list asset prices", "error", err, "household_id", identity.HouseholdID)
		} else {
			byTicker := map[string][]reporting.RebasedPoint{}
			for _, p := range prices {
				byTicker[p.Ticker] = append(byTicker[p.Ticker],
					reporting.RebasedPoint{Date: p.AsOf, Value: p.Close})
			}
			for _, ticker := range s.Config.Benchmarks.Tickers {
				points, ok := byTicker[strings.ToUpper(ticker)]
				if !ok {
					continue
				}
				if series, ok := reporting.Rebase(strings.ToUpper(ticker), points); ok {
					resp.Series = append(resp.Series, toSeriesResponse(series.Label, series.Points))
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

func toSeriesResponse(label string, points []reporting.RebasedPoint) seriesResponse {
	out := seriesResponse{Label: label, Points: make([]seriesPointResponse, 0, len(points))}
	for _, p := range points {
		out.Points = append(out.Points, seriesPointResponse{
			Date: p.Date.Format(time.DateOnly), Value: p.Value,
		})
	}
	return out
}

// --------------------------------------------------------------------------
// Allocation
// --------------------------------------------------------------------------

type allocationSliceResponse struct {
	Label   string          `json:"label"`
	Value   decimal.Decimal `json:"value"`
	Percent decimal.Decimal `json:"percent"`
}

type allocationResponse struct {
	ByAssetClass   []allocationSliceResponse `json:"by_asset_class"`
	ByTaxTreatment []allocationSliceResponse `json:"by_tax_treatment"`
	// Note is shown under the chart. Coverage of Plaid's asset-class data is
	// partial and the "Unknown" slice is real, not a rendering artefact.
	Note string `json:"note"`
}

func (s *Server) handleInvestmentAllocation(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListVisibleHoldingsDetailed(r.Context(), dbgen.ListVisibleHoldingsDetailedParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list holdings", err)
		return
	}

	writeJSON(w, http.StatusOK, allocationResponse{
		ByAssetClass:   toAllocationResponse(reporting.Allocation(rows)),
		ByTaxTreatment: toAllocationResponse(reporting.AllocationByTaxTreatment(rows)),
		Note: "Asset class comes from your institution and is not always reported. " +
			"Anything it did not classify is shown as Unknown rather than folded into the other slices.",
	})
}

func toAllocationResponse(slices []reporting.AllocationSlice) []allocationSliceResponse {
	out := make([]allocationSliceResponse, 0, len(slices))
	for _, s := range slices {
		out = append(out, allocationSliceResponse{Label: s.Label, Value: s.Value, Percent: s.Percent})
	}
	return out
}

// --------------------------------------------------------------------------
// Holdings
// --------------------------------------------------------------------------

type detailedHoldingResponse struct {
	ID               uuid.UUID        `json:"id"`
	AccountID        uuid.UUID        `json:"account_id"`
	AccountName      string           `json:"account_name"`
	InstitutionName  *string          `json:"institution_name"`
	SecurityName     *string          `json:"security_name"`
	Ticker           *string          `json:"ticker"`
	SecurityType     *string          `json:"security_type"`
	Quantity         decimal.Decimal  `json:"quantity"`
	CostBasis        *decimal.Decimal `json:"cost_basis"`
	Value            *decimal.Decimal `json:"value"`
	LastPrice        *decimal.Decimal `json:"last_price"`
	LastPriceAsOf    *string          `json:"last_price_as_of"`
	Gain             *decimal.Decimal `json:"gain"`
	GainPct          *decimal.Decimal `json:"gain_pct"`
	IsCashEquivalent bool             `json:"is_cash_equivalent"`
	TaxTreatment     *string          `json:"tax_treatment"`
}

func (s *Server) handleInvestmentHoldings(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListVisibleHoldingsDetailed(r.Context(), dbgen.ListVisibleHoldingsDetailedParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list holdings", err)
		return
	}
	writeJSON(w, http.StatusOK, buildHoldingResponses(rows))
}

func buildHoldingResponses(rows []dbgen.ListVisibleHoldingsDetailedRow) []detailedHoldingResponse {
	out := make([]detailedHoldingResponse, 0, len(rows))
	for _, h := range rows {
		item := detailedHoldingResponse{
			ID: h.ID, AccountID: h.AccountID, AccountName: h.AccountName,
			InstitutionName: h.InstitutionName, SecurityName: h.SecurityName,
			Ticker: h.Ticker, SecurityType: h.SecurityType, Quantity: h.Quantity,
			IsCashEquivalent: h.IsCashEquivalent, TaxTreatment: h.TaxTreatment,
			CostBasis: nullDecimal(h.CostBasis),
			Value:     nullDecimal(h.InstitutionValue),
		}
		// Prefer the institution's own price; fall back to the security's last
		// close, which may be a day or two older.
		if h.InstitutionPrice.Valid {
			item.LastPrice = &h.InstitutionPrice.Decimal
		} else if h.ClosePrice.Valid {
			item.LastPrice = &h.ClosePrice.Decimal
			if h.ClosePriceAsOf != nil {
				asOf := h.ClosePriceAsOf.Format(time.DateOnly)
				item.LastPriceAsOf = &asOf
			}
		}
		// A gain needs both sides. A basis of zero is real (a gifted position)
		// but has no meaningful percentage, so only the dollar figure is shown.
		if h.InstitutionValue.Valid && h.CostBasis.Valid {
			gain := h.InstitutionValue.Decimal.Sub(h.CostBasis.Decimal)
			item.Gain = &gain
			if h.CostBasis.Decimal.IsPositive() {
				pct := gain.Div(h.CostBasis.Decimal).Mul(decimal.NewFromInt(100)).Round(2)
				item.GainPct = &pct
			}
		}
		out = append(out, item)
	}
	return out
}

// handleExportHoldings streams the holdings table as CSV, matching the other
// exports so a spreadsheet gets exact decimal strings rather than rendered text.
func (s *Server) handleExportHoldings(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListVisibleHoldingsDetailed(r.Context(), dbgen.ListVisibleHoldingsDetailedParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if err != nil {
		s.internalError(w, "export holdings", err)
		return
	}

	now := time.Now()
	cw := beginCSV(w, "holdings", now, now)
	defer cw.Flush()

	if err := cw.Write([]string{
		"account", "institution", "tax_treatment", "security", "ticker", "type",
		"quantity", "last_price", "cost_basis", "value", "gain", "gain_pct",
	}); err != nil {
		slog.Error("write csv header", "error", err)
		return
	}

	for _, h := range buildHoldingResponses(rows) {
		if err := cw.Write([]string{
			h.AccountName,
			derefStr(h.InstitutionName),
			derefStr(h.TaxTreatment),
			derefStr(h.SecurityName),
			derefStr(h.Ticker),
			derefStr(h.SecurityType),
			h.Quantity.String(),
			decimalOrEmpty(h.LastPrice),
			decimalOrEmpty(h.CostBasis),
			decimalOrEmpty(h.Value),
			decimalOrEmpty(h.Gain),
			decimalOrEmpty(h.GainPct),
		}); err != nil {
			slog.Error("write csv row", "error", err)
			return
		}
	}
}

// decimalOrEmpty renders an optional decimal for CSV. An absent value becomes an
// empty cell, never "0" — a spreadsheet summing a column of unknowns as zero is
// exactly the quiet error the exports exist to avoid.
func decimalOrEmpty(d *decimal.Decimal) string {
	if d == nil {
		return ""
	}
	return d.String()
}

// --------------------------------------------------------------------------
// Fee drag
// --------------------------------------------------------------------------

type feeDragResponse struct {
	AnnualCost       decimal.Decimal `json:"annual_cost"`
	CoveredValue     decimal.Decimal `json:"covered_value"`
	UncoveredValue   decimal.Decimal `json:"uncovered_value"`
	CoveredHoldings  int             `json:"covered_holdings"`
	ExcludedHoldings int             `json:"excluded_holdings"`
	// Note discloses coverage in words. Required: an annual fee figure computed
	// over part of a portfolio and presented as the whole is misinformation, and
	// today the app has no expense-ratio source at all.
	Note string `json:"note"`
}

func (s *Server) handleInvestmentFees(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListVisibleHoldingsDetailed(r.Context(), dbgen.ListVisibleHoldingsDetailedParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list holdings", err)
		return
	}

	// No expense-ratio source is wired yet: Plaid does not return one and the
	// securities table has no column for it. Passing an empty map means every
	// holding is reported as excluded, which is the truth.
	drag := reporting.ComputeFeeDrag(rows, nil)

	note := "No expense-ratio source is configured, so no fee estimate can be made. " +
		"Ledgermancy will not guess at fund costs — a number covering part of a portfolio " +
		"and shown as the total would be worse than none."
	if drag.CoveredHoldings > 0 {
		note = "Covers holdings with a known expense ratio only."
		if drag.ExcludedHoldings > 0 {
			note += " Excludes " + plural(drag.ExcludedHoldings, "holding", "holdings") +
				" with no published ratio."
		}
	}

	writeJSON(w, http.StatusOK, feeDragResponse{
		AnnualCost:       drag.AnnualCost,
		CoveredValue:     drag.CoveredValue,
		UncoveredValue:   drag.UncoveredValue,
		CoveredHoldings:  drag.CoveredHoldings,
		ExcludedHoldings: drag.ExcludedHoldings,
		Note:             note,
	})
}

// --------------------------------------------------------------------------
// Dividends
// --------------------------------------------------------------------------

type dividendMonthResponse struct {
	Month string          `json:"month"`
	Total decimal.Decimal `json:"total"`
}

type dividendsResponse struct {
	Months []dividendMonthResponse `json:"months"`
	Total  decimal.Decimal         `json:"total"`
	Basis  string                  `json:"basis"`
}

// handleInvestmentDividends returns dividends received by month over the
// trailing window.
func (s *Server) handleInvestmentDividends(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	now := time.Now()
	q := r.URL.Query()
	to := parseDate(q.Get("to"), now)
	from := parseDate(q.Get("from"), to.AddDate(-2, 0, 0))

	rows, err := s.Queries.GetDividendIncome(r.Context(), dbgen.GetDividendIncomeParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
		Date: from, Date_2: to,
	})
	if err != nil {
		s.internalError(w, "dividend income", err)
		return
	}

	resp := dividendsResponse{
		Months: make([]dividendMonthResponse, 0, len(rows)),
		Basis: "From investment transactions your institution reported. " +
			"Reinvested dividends are included — the money was earned either way.",
	}
	for _, row := range rows {
		resp.Months = append(resp.Months, dividendMonthResponse{
			Month: row.Month.Format("2006-01"), Total: row.Total,
		})
		resp.Total = resp.Total.Add(row.Total)
	}
	writeJSON(w, http.StatusOK, resp)
}

// --------------------------------------------------------------------------
// Account tagging
// --------------------------------------------------------------------------

// validTaxTreatments mirrors the CHECK constraint on accounts.tax_treatment.
// Validated here so a bad value returns 400 with a readable message rather than
// a 500 from a constraint violation.
var validTaxTreatments = map[string]bool{
	"taxable": true, "trad_401k": true, "roth_401k": true,
	"trad_ira": true, "roth_ira": true, "529": true,
	"hsa": true, "trust": true, "other": true,
}

type taxTreatmentRequest struct {
	// Null clears the classification back to untagged, which is a legitimate
	// action: "I do not know" is a better state than a wrong tag.
	TaxTreatment *string `json:"tax_treatment"`
	IsManaged    *bool   `json:"is_managed"`
}

type taxTreatmentResponse struct {
	ID           uuid.UUID `json:"id"`
	Name         string    `json:"name"`
	TaxTreatment *string   `json:"tax_treatment"`
	IsManaged    *bool     `json:"is_managed"`
}

// handleSetAccountTaxTreatment confirms a classification for one account.
//
// The value is only ever written from an explicit user action. The suggestion
// derived from the Plaid subtype is served on the overview for display and is
// never persisted on the user's behalf: a traditional-vs-Roth mistake silently
// changes every retirement figure built on this column.
func (s *Server) handleSetAccountTaxTreatment(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	accountID, err := uuid.Parse(chi.URLParam(r, "accountID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid account id")
		return
	}

	var req taxTreatmentRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.TaxTreatment != nil && !validTaxTreatments[*req.TaxTreatment] {
		writeError(w, http.StatusBadRequest,
			"tax_treatment must be one of: taxable, trad_401k, roth_401k, trad_ira, roth_ira, 529, hsa, trust, other")
		return
	}

	row, err := s.Queries.SetAccountTaxTreatment(r.Context(), dbgen.SetAccountTaxTreatmentParams{
		ID:           accountID,
		HouseholdID:  identity.HouseholdID,
		UserID:       identity.UserID,
		TaxTreatment: req.TaxTreatment,
		IsManaged:    req.IsManaged,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Either it does not exist or it belongs to an institution this member
		// cannot see. Same answer for both: revealing the difference would leak
		// whether another member has an account with that id.
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	if err != nil {
		s.internalError(w, "set account tax treatment", err)
		return
	}

	writeJSON(w, http.StatusOK, taxTreatmentResponse{
		ID: row.ID, Name: row.Name,
		TaxTreatment: row.TaxTreatment, IsManaged: row.IsManaged,
	})
}

// plural renders "1 holding" / "3 holdings".
func plural(n int, one, many string) string {
	word := many
	if n == 1 {
		word = one
	}
	return strconv.Itoa(n) + " " + word
}
