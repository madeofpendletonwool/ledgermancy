package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/obligations"
)

// period resolves the from/to query parameters, defaulting to the current
// calendar month — the window the dashboard opens on.
func period(r *http.Request) (from, to time.Time) {
	now := time.Now()
	defaultFrom := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	// Last day of the current month.
	defaultTo := defaultFrom.AddDate(0, 1, -1)

	q := r.URL.Query()
	return parseDate(q.Get("from"), defaultFrom), parseDate(q.Get("to"), defaultTo)
}

type summaryResponse struct {
	From                  string          `json:"from"`
	To                    string          `json:"to"`
	Income                decimal.Decimal `json:"income"`
	Spending              decimal.Decimal `json:"spending"`
	FixedSpending         decimal.Decimal `json:"fixed_spending"`
	DiscretionarySpending decimal.Decimal `json:"discretionary_spending"`
	// Leftover is income minus spending: what was available to invest.
	Leftover decimal.Decimal `json:"leftover"`
	// SavingsRate is leftover as a share of income, 0–1. Null when there is no
	// income in the period, because the ratio would be meaningless.
	SavingsRate      *decimal.Decimal `json:"savings_rate"`
	TransactionCount int64            `json:"transaction_count"`
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	from, to := period(r)

	row, err := s.Queries.GetSpendingSummary(r.Context(), dbgen.GetSpendingSummaryParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
	})
	if err != nil {
		s.internalError(w, "spending summary", err)
		return
	}

	writeJSON(w, http.StatusOK, buildSummary(from, to, row))
}

// buildSummary derives leftover and savings rate. Kept separate so the maths
// is testable without a database.
func buildSummary(from, to time.Time, row dbgen.GetSpendingSummaryRow) summaryResponse {
	leftover := row.Income.Sub(row.Spending)

	var savingsRate *decimal.Decimal
	// Guard the division: a period with no income has no meaningful rate, and
	// dividing would panic.
	if row.Income.IsPositive() {
		rate := leftover.Div(row.Income).Round(4)
		savingsRate = &rate
	}

	return summaryResponse{
		From:                  from.Format(time.DateOnly),
		To:                    to.Format(time.DateOnly),
		Income:                row.Income,
		Spending:              row.Spending,
		FixedSpending:         row.FixedSpending,
		DiscretionarySpending: row.DiscretionarySpending,
		Leftover:              leftover,
		SavingsRate:           savingsRate,
		TransactionCount:      row.TransactionCount,
	}
}

type categorySpendResponse struct {
	CategoryID       uuid.UUID       `json:"category_id"`
	Name             string          `json:"name"`
	Slug             string          `json:"slug"`
	Color            *string         `json:"color"`
	IsFixed          bool            `json:"is_fixed"`
	Total            decimal.Decimal `json:"total"`
	TransactionCount int64           `json:"transaction_count"`
}

func (s *Server) handleSpendingByCategory(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	from, to := period(r)

	rows, err := s.Queries.GetSpendingByCategory(r.Context(), dbgen.GetSpendingByCategoryParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
	})
	if err != nil {
		s.internalError(w, "spending by category", err)
		return
	}

	out := make([]categorySpendResponse, 0, len(rows))
	for _, c := range rows {
		out = append(out, categorySpendResponse{
			CategoryID:       c.CategoryID,
			Name:             c.CategoryName,
			Slug:             c.CategorySlug,
			Color:            c.CategoryColor,
			IsFixed:          c.IsFixed,
			Total:            c.Total,
			TransactionCount: c.TransactionCount,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type daySpendResponse struct {
	// Day is a calendar date, "YYYY-MM-DD".
	Day      string          `json:"day"`
	Spending decimal.Decimal `json:"spending"`
}

// handleSpendingByDay returns spend per calendar day for a period, defaulting
// to the current month. Drives the dashboard's by-day chart. Only days with
// spending are returned; the frontend fills the empty days across the month.
func (s *Server) handleSpendingByDay(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	from, to := period(r)

	rows, err := s.Queries.GetSpendingByDay(r.Context(), dbgen.GetSpendingByDayParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
	})
	if err != nil {
		s.internalError(w, "spending by day", err)
		return
	}

	out := make([]daySpendResponse, 0, len(rows))
	for _, d := range rows {
		out = append(out, daySpendResponse{
			Day:      d.Day.Format(time.DateOnly),
			Spending: d.Spending,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type trendPoint struct {
	Month    string          `json:"month"`
	Income   decimal.Decimal `json:"income"`
	Spending decimal.Decimal `json:"spending"`
	Leftover decimal.Decimal `json:"leftover"`
	// FixedSpending and DiscretionarySpending decompose Spending into the same
	// two buckets GetSpendingSummary reports for a single period — same FILTER
	// clauses in SQL, so a month's two buckets sum to that month's Spending to
	// the cent. Drives the fixed-vs-discretionary stacked bars (item #9).
	FixedSpending         decimal.Decimal `json:"fixed_spending"`
	DiscretionarySpending decimal.Decimal `json:"discretionary_spending"`
}

// handleTrend returns income/spending/leftover per month. Defaults to the
// trailing twelve months, which is the span this app is built around.
func (s *Server) handleTrend(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	now := time.Now()
	end := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, -1)
	start := end.AddDate(0, -11, 0)
	start = time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)

	q := r.URL.Query()
	from, to := parseDate(q.Get("from"), start), parseDate(q.Get("to"), end)

	rows, err := s.Queries.GetMonthlyTrend(r.Context(), dbgen.GetMonthlyTrendParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
	})
	if err != nil {
		s.internalError(w, "monthly trend", err)
		return
	}

	out := make([]trendPoint, 0, len(rows))
	for _, m := range rows {
		out = append(out, trendPoint{
			Month:                 m.Month.Format("2006-01"),
			Income:                m.Income,
			Spending:              m.Spending,
			Leftover:              m.Income.Sub(m.Spending),
			FixedSpending:         m.FixedSpending,
			DiscretionarySpending: m.DiscretionarySpending,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type categoryAverageResponse struct {
	CategoryID       uuid.UUID       `json:"category_id"`
	Name             string          `json:"name"`
	Slug             string          `json:"slug"`
	Color            *string         `json:"color"`
	IsFixed          bool            `json:"is_fixed"`
	Total            decimal.Decimal `json:"total"`
	MonthlyAverage   decimal.Decimal `json:"monthly_average"`
	TransactionCount int64           `json:"transaction_count"`
}

// handleCategoryAverages defaults to the trailing twelve months, giving the
// "typical month" figures used for planning.
func (s *Server) handleCategoryAverages(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	now := time.Now()
	defaultTo := now
	defaultFrom := now.AddDate(-1, 0, 0)

	q := r.URL.Query()
	from, to := parseDate(q.Get("from"), defaultFrom), parseDate(q.Get("to"), defaultTo)

	rows, err := s.Queries.GetCategoryAverages(r.Context(), dbgen.GetCategoryAveragesParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
	})
	if err != nil {
		s.internalError(w, "category averages", err)
		return
	}

	out := make([]categoryAverageResponse, 0, len(rows))
	for _, c := range rows {
		out = append(out, categoryAverageResponse{
			CategoryID:       c.CategoryID,
			Name:             c.CategoryName,
			Slug:             c.CategorySlug,
			Color:            c.CategoryColor,
			IsFixed:          c.IsFixed,
			Total:            c.Total,
			MonthlyAverage:   c.MonthlyAverage.Round(2),
			TransactionCount: c.TransactionCount,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type merchantResponse struct {
	Merchant string `json:"merchant"`
	// MerchantKey is the resolved key, which is what makes a row here clickable:
	// it addresses the merchant detail view whether or not the merchant is one the
	// household has grouped.
	MerchantKey      string          `json:"merchant_key"`
	Total            decimal.Decimal `json:"total"`
	TransactionCount int64           `json:"transaction_count"`
}

func (s *Server) handleTopMerchants(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	from, to := period(r)
	limit := parseInt(r.URL.Query().Get("limit"), 10, 1, 50)

	rows, err := s.Queries.GetTopMerchants(r.Context(), dbgen.GetTopMerchantsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
		Limit:       int32(limit),
	})
	if err != nil {
		s.internalError(w, "top merchants", err)
		return
	}

	out := make([]merchantResponse, 0, len(rows))
	for _, m := range rows {
		out = append(out, merchantResponse{
			Merchant:         m.Merchant,
			MerchantKey:      m.MerchantKey,
			Total:            m.Total,
			TransactionCount: m.TransactionCount,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// merchantExplorerLimit caps how many merchants one explorer response carries.
// The page searches, sorts and pages the list locally, so the cap is a backstop
// against a pathological household rather than a page size — and when it bites,
// the response says so rather than quietly presenting a partial list as the whole
// truth.
const merchantExplorerLimit = 1000

// lapsedMerchantLookbackMonths is how far back the gone-quiet list looks. Longer
// than the live-recurring lookback on purpose: a subscription cancelled ten
// months ago is exactly the one worth surfacing, and it has no charges inside a
// twelve-month window to be detected from.
const lapsedMerchantLookbackMonths = 30

type merchantExplorerRow struct {
	Merchant string `json:"merchant"`
	// MerchantKey is the resolved key — it addresses the merchant detail view for
	// a grouped merchant and a bare descriptor alike.
	MerchantKey      string          `json:"merchant_key"`
	Total            decimal.Decimal `json:"total"`
	TransactionCount int64           `json:"transaction_count"`
	Average          decimal.Decimal `json:"average"`
	FirstSeen        string          `json:"first_seen"`
	LastSeen         string          `json:"last_seen"`
	// PriorTotal is spend at this merchant over the equivalent preceding window,
	// zero when there was none. The UI computes the change from the two, and must
	// not render a percentage when this is zero — a change from nothing has no
	// meaningful ratio, and the merchant is flagged IsNew for that case anyway.
	PriorTotal decimal.Decimal `json:"prior_total"`
	// IsNew means no qualifying charge at this merchant before the window at all,
	// not merely none in the previous period.
	IsNew bool `json:"is_new"`
	// The category most of this merchant's spend lands in. Null for a merchant
	// whose charges are all uncategorised.
	CategoryID    *uuid.UUID `json:"category_id"`
	CategoryName  *string    `json:"category_name"`
	CategoryColor *string    `json:"category_color"`
}

type lapsedMerchantResponse struct {
	Merchant    string `json:"merchant"`
	MerchantKey string `json:"merchant_key"`
	// TypicalAmount and Cadence describe what the charge used to look like, so the
	// UI can say "~$17/mo" without doing arithmetic on money.
	TypicalAmount   decimal.Decimal `json:"typical_amount"`
	MonthlyEstimate decimal.Decimal `json:"monthly_estimate"`
	Cadence         string          `json:"cadence"`
	LastSeen        string          `json:"last_seen"`
	DaysQuiet       int32           `json:"days_quiet"`
}

type merchantExplorerResponse struct {
	From      string `json:"from"`
	To        string `json:"to"`
	PriorFrom string `json:"prior_from"`
	PriorTo   string `json:"prior_to"`
	// WindowTotal is everything spent in the window, unaffected by the search
	// needle, so the concentration headline stays true while the user types.
	WindowTotal decimal.Decimal `json:"window_total"`
	// MerchantCount is how many merchants matched, which can exceed len(Merchants)
	// when the row cap bites. Truncated says whether it did.
	MerchantCount int64                    `json:"merchant_count"`
	Truncated     bool                     `json:"truncated"`
	Merchants     []merchantExplorerRow    `json:"merchants"`
	Lapsed        []lapsedMerchantResponse `json:"lapsed"`
}

// handleMerchantExplorer answers the merchant explorer with one round trip: every
// merchant in the window plus the gone-quiet list.
//
// The prior window is derived here rather than accepted from the caller, so the
// comparison is always like-for-like: the same number of days immediately before
// `from`. A caller-supplied prior window would let a one-month period be compared
// against a year and the resulting percentage would be nonsense.
func (s *Server) handleMerchantExplorer(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	from, to := period(r)

	// Inclusive day count, so the prior window spans the same number of days and
	// ends the day before this one starts.
	span := to.Sub(from)
	priorTo := from.AddDate(0, 0, -1)
	priorFrom := priorTo.Add(-span)

	var categoryID *uuid.UUID
	if v := r.URL.Query().Get("category_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			categoryID = &id
		}
	}

	search := trimmedParam(r.URL.Query().Get("search"))

	params := dbgen.ListMerchantSpendParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
		PriorFrom:   priorFrom,
		PriorTo:     priorTo,
		Search:      search,
		CategoryID:  categoryID,
		Lim:         merchantExplorerLimit,
	}
	rows, err := s.Queries.ListMerchantSpend(r.Context(), params)
	if err != nil {
		s.internalError(w, "merchant explorer", err)
		return
	}

	out := merchantExplorerResponse{
		From:      from.Format(time.DateOnly),
		To:        to.Format(time.DateOnly),
		PriorFrom: priorFrom.Format(time.DateOnly),
		PriorTo:   priorTo.Format(time.DateOnly),
		Merchants: make([]merchantExplorerRow, 0, len(rows)),
		Lapsed:    []lapsedMerchantResponse{},
	}

	for _, m := range rows {
		// window_total and matched_count are constant across every row, so they are
		// read off whichever row comes last.
		out.WindowTotal = m.WindowTotal
		out.MerchantCount = m.MatchedCount

		// A merchant with no key cannot be addressed, so a row for it would render
		// as a link to nowhere. Skipping matches how the Dashboard's top-merchant
		// card already guards, and such rows carry too little signal to be useful.
		if m.MerchantKey == "" {
			continue
		}
		out.Merchants = append(out.Merchants, merchantExplorerRow{
			Merchant:         m.Merchant,
			MerchantKey:      m.MerchantKey,
			Total:            m.Total,
			TransactionCount: m.TransactionCount,
			Average:          m.Average.Round(2),
			FirstSeen:        m.FirstSeen.Format(time.DateOnly),
			LastSeen:         m.LastSeen.Format(time.DateOnly),
			PriorTotal:       m.PriorTotal,
			IsNew:            m.IsNew,
			CategoryID:       m.CategoryID,
			CategoryName:     m.CategoryName,
			CategoryColor:    m.CategoryColor,
		})
	}
	out.Truncated = out.MerchantCount > int64(len(rows))

	// window_total rides on the returned rows, so a needle that matches nothing
	// leaves it at zero — and it is meant to be the window's whole spend, needle or
	// no needle. Recover it with one unfiltered call, only in that case, so the
	// figure never silently contradicts its own contract.
	if len(rows) == 0 && search != nil {
		params.Search = nil
		if all, err := s.Queries.ListMerchantSpend(r.Context(), params); err != nil {
			slog.Error("merchant explorer window total", "error", err)
		} else if len(all) > 0 {
			out.WindowTotal = all[0].WindowTotal
		}
	}

	// The gone-quiet list is the exact complement of the live recurring list, from
	// the same query, so the two can never disagree about what "recurring" means.
	// A failure here is not worth failing the whole page over: the explorer is
	// still useful without the card, so it degrades to empty.
	now := time.Now()
	lapsed := true
	quiet, err := s.Queries.GetRecurringMerchants(r.Context(), dbgen.GetRecurringMerchantsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        now.AddDate(0, -lapsedMerchantLookbackMonths, 0),
		Column4:     now,
		Lapsed:      &lapsed,
	})
	if err != nil {
		slog.Error("lapsed merchants", "error", err)
	}
	for _, m := range quiet {
		if m.MerchantKey == "" {
			continue
		}
		var monthly decimal.Decimal
		if m.AvgGapDays.IsPositive() {
			monthly = m.TypicalAmount.Mul(daysPerMonth).Div(m.AvgGapDays).Round(2)
		}
		out.Lapsed = append(out.Lapsed, lapsedMerchantResponse{
			Merchant:        m.Merchant,
			MerchantKey:     m.MerchantKey,
			TypicalAmount:   m.TypicalAmount.Round(2),
			MonthlyEstimate: monthly,
			Cadence:         obligations.CadenceLabel(m.AvgGapDays),
			LastSeen:        m.LastSeen.Format(time.DateOnly),
			DaysQuiet:       m.DaysQuiet,
		})
	}

	writeJSON(w, http.StatusOK, out)
}

// daysPerMonth is the average calendar month, used only to normalise a
// merchant's cadence into an estimated monthly cost for display.
var daysPerMonth = decimal.NewFromFloat(30.4368)

type recurringResponse struct {
	MerchantKey string `json:"merchant_key"`
	Merchant    string `json:"merchant"`
	Occurrences int64  `json:"occurrences"`
	// The MEDIAN charge, not the mean — see GetRecurringMerchants. Named
	// "typical" rather than "average" because it is deliberately not an average:
	// a merchant's one anomalous charge (a loan payoff, an annual true-up) must
	// not move the figure the household reads as "what this bill costs".
	TypicalAmount   decimal.Decimal `json:"typical_amount"`
	AvgGapDays      decimal.Decimal `json:"avg_gap_days"`
	Cadence         string          `json:"cadence"`
	MonthlyEstimate decimal.Decimal `json:"monthly_estimate"`
	LastSeen        string          `json:"last_seen"`
}

func (s *Server) handleRecurring(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	now := time.Now()
	since := now.AddDate(0, -obligations.RecurringLookbackMonths, 0)

	rows, err := s.Queries.GetRecurringMerchants(r.Context(), dbgen.GetRecurringMerchantsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        since,
		Column4:     now,
	})
	if err != nil {
		s.internalError(w, "recurring merchants", err)
		return
	}

	out := make([]recurringResponse, 0, len(rows))
	for _, m := range rows {
		// Skip rows without a merchant_key — suppression is keyed by it, so an
		// unkeyed row could not be acted on anyway. Merchants that have gone
		// quiet are already excluded by the query.
		if m.MerchantKey == "" {
			continue
		}
		// Normalise the charge to a monthly figure: amount * (month / gap).
		var monthly decimal.Decimal
		if m.AvgGapDays.IsPositive() {
			monthly = m.TypicalAmount.Mul(daysPerMonth).Div(m.AvgGapDays).Round(2)
		}
		out = append(out, recurringResponse{
			MerchantKey:     m.MerchantKey,
			Merchant:        m.Merchant,
			Occurrences:     m.Occurrences,
			TypicalAmount:   m.TypicalAmount.Round(2),
			AvgGapDays:      m.AvgGapDays.Round(1),
			Cadence:         obligations.CadenceLabel(m.AvgGapDays),
			MonthlyEstimate: monthly,
			LastSeen:        m.LastSeen.Format(time.DateOnly),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// suppressRecurringRequest marks a merchant "not recurring". merchant is the
// label captured for the restore UI; merchant_key is what the detector is keyed
// by and what suppression acts on.
type suppressRecurringRequest struct {
	MerchantKey string `json:"merchant_key"`
	Merchant    string `json:"merchant"`
}

func (s *Server) handleSuppressRecurring(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req suppressRecurringRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.MerchantKey == "" {
		writeError(w, http.StatusBadRequest, "merchant_key is required")
		return
	}

	if err := s.Queries.SuppressRecurringMerchant(r.Context(), dbgen.SuppressRecurringMerchantParams{
		HouseholdID:   identity.HouseholdID,
		MerchantKey:   req.MerchantKey,
		MerchantLabel: req.Merchant,
	}); err != nil {
		s.internalError(w, "suppress recurring merchant", err)
		return
	}

	// Suppression has to reach the bill calendar in the same breath. The
	// promotion pass retires suppressed rows too, but waiting for it would leave
	// a merchant the user just dismissed still sitting on next week's calendar.
	if _, err := s.Queries.DeactivateSuppressedObligations(r.Context(), identity.HouseholdID); err != nil {
		s.internalError(w, "retire suppressed obligations", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUnsuppressRecurring(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	// The key can be long and contain URL-unfriendly characters, so it comes on
	// the query string rather than in the path.
	key := r.URL.Query().Get("merchant_key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "merchant_key is required")
		return
	}

	if err := s.Queries.UnsuppressRecurringMerchant(r.Context(), dbgen.UnsuppressRecurringMerchantParams{
		HouseholdID: identity.HouseholdID,
		MerchantKey: key,
	}); err != nil {
		s.internalError(w, "restore recurring merchant", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type suppressedRecurringResponse struct {
	// MerchantKey is the key suppression is recorded under — what unsuppress
	// takes. MerchantKeyResolved is the same key canonicalised, which is what
	// addresses the merchant detail view: a suppression recorded against a raw
	// descriptor before that descriptor was merged would otherwise link nowhere.
	MerchantKey         string `json:"merchant_key"`
	MerchantKeyResolved string `json:"merchant_key_resolved"`
	Merchant            string `json:"merchant"`
	SuppressedAt        string `json:"suppressed_at"`
}

func (s *Server) handleListSuppressedRecurring(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListRecurringOverrides(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "list suppressed recurring", err)
		return
	}

	out := make([]suppressedRecurringResponse, 0, len(rows))
	for _, m := range rows {
		out = append(out, suppressedRecurringResponse{
			MerchantKey:         m.MerchantKey,
			MerchantKeyResolved: m.ResolvedMerchantKey,
			Merchant:            m.MerchantLabel,
			SuppressedAt:        m.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// heatmapMonthLimit caps how many months the matrix spans. The chart this feeds
// is the last-twelve-months view, and a longer window stops reading as
// "seasonality" and starts reading as a wall of numbers. Twelve matches the
// trend chart beside it, so the two agree on what "trailing" means.
const heatmapMonthLimit = 12

// heatmapCategoryLimit is the row cap the heatmap folds to "Other" past. The
// chart's cells are compact, so this is a touch above the bar chart's 8 — the
// app's fold-to-Other rule is "past ~8", and a heatmap carries no per-row bar
// art so a couple more fit without crowding. The small-multiples chart that
// rides the same payload caps itself client-side at 8 (item #12).
const heatmapCategoryLimit = 10

type heatmapCategory struct {
	CategoryID string  `json:"category_id"`
	Name       string  `json:"name"`
	Slug       string  `json:"slug"`
	Color      *string `json:"color"`
	IsFixed    bool    `json:"is_fixed"`
	// Total is the category's whole-range spend, used for ordering and for the
	// "Other" fold. Decimal string; never summed in the client for a headline.
	Total string `json:"total"`
	// Cells maps "YYYY-MM" to that month's spend as a decimal string. Only
	// months with spend appear; the client fills the gaps with zero so an empty
	// month is indistinguishable from a month outside the data — same contract
	// as GetMerchantMonthlySpend.
	Cells map[string]string `json:"cells"`
}

type spendingHeatmapResponse struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Months is the full "YYYY-MM" axis across [From, To], ascending. Built
	// server-side so every client renders the same columns even when a month
	// had no spend in any category.
	Months []string `json:"months"`
	// Categories is sorted by Total descending; the heatmap renders the top
	// heatmapCategoryLimit and folds the remainder into a synthetic "Other" row
	// client-side, matching CategoryBars' foldToOther rule. Returned in full so
	// the small-multiples chart (item #12) can pick its own top-N.
	Categories []heatmapCategory `json:"categories"`
}

// handleSpendingHeatmap answers the spending-by-category-by-month matrix behind
// the category × month heatmap (item #8) and the category-mix small multiples
// (item #12). One endpoint, two renderings — the rows pivot into either.
//
// Defaults to the trailing twelve months to match the trend chart the heatmap
// sits beside. The pivot itself — building the month axis, ranking categories,
// keying cells by month — is in buildHeatmap, kept separate so it is testable
// without a database (see report_handlers_heatmap_test.go).
func (s *Server) handleSpendingHeatmap(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	now := time.Now()
	end := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, -1)
	start := end.AddDate(0, -(heatmapMonthLimit - 1), 0)
	start = time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)

	q := r.URL.Query()
	from, to := parseDate(q.Get("from"), start), parseDate(q.Get("to"), end)

	rows, err := s.Queries.GetCategoryMonthMatrix(r.Context(), dbgen.GetCategoryMonthMatrixParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
	})
	if err != nil {
		s.internalError(w, "category month matrix", err)
		return
	}

	writeJSON(w, http.StatusOK, buildHeatmap(from, to, rows))
}

// buildHeatmap pivots the per-(category, month) rows into the matrix response.
// Kept separate from the handler so the pivot — month axis, ranking, cell keys —
// is unit-testable without a database.
//
// Rows arrive ordered by category id then month, but the pivot does not rely on
// that ordering: it indexes categories by id and accumulates both the per-month
// cells and the whole-range total in one pass, then sorts by total descending
// so the heatmap's top rows are its biggest categories.
func buildHeatmap(from, to time.Time, rows []dbgen.GetCategoryMonthMatrixRow) spendingHeatmapResponse {
	months := monthsBetween(from, to)

	type acc struct {
		id    string
		name  string
		slug  string
		color *string
		fixed bool
		total decimal.Decimal
		cells map[string]string
	}

	byID := make(map[string]*acc)
	order := make([]string, 0) // first-seen order, so the sort has a stable tiebreak
	for _, r := range rows {
		key := r.CategoryID.String()
		a, ok := byID[key]
		if !ok {
			a = &acc{
				id:    key,
				name:  r.CategoryName,
				slug:  r.CategorySlug,
				color: r.CategoryColor,
				fixed: r.IsFixed,
				cells: make(map[string]string),
			}
			byID[key] = a
			order = append(order, key)
		}
		monthKey := r.Month.Format("2006-01")
		// Accumulate across duplicate (category, month) rows in decimal, then
		// store the canonical string. date_trunc('month') already groups in SQL,
		// so this is normally a single assignment — but summing here keeps the
		// pivot correct if a future caller feeds it pre-grouped data, and the
		// cell total then agrees with the accumulator's whole-range total.
		existing := decimal.Zero
		if v, ok := a.cells[monthKey]; ok {
			existing, _ = decimal.NewFromString(v)
		}
		a.cells[monthKey] = existing.Add(r.Total).String()
		a.total = a.total.Add(r.Total)
	}

	// Rank by total descending. The order slice preserves first-seen ordering
	// for ties (which is stable category-id ordering, since the SQL groups by
	// id), so two categories with identical totals keep their relative position
	// across runs rather than swapping on map iteration.
	ranked := make([]*acc, 0, len(order))
	for _, k := range order {
		ranked = append(ranked, byID[k])
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		return ranked[i].total.GreaterThan(ranked[j].total)
	})

	cats := make([]heatmapCategory, 0, len(ranked))
	for _, a := range ranked {
		cats = append(cats, heatmapCategory{
			CategoryID: a.id,
			Name:       a.name,
			Slug:       a.slug,
			Color:      a.color,
			IsFixed:    a.fixed,
			Total:      a.total.String(),
			Cells:      a.cells,
		})
	}

	return spendingHeatmapResponse{
		From:       from.Format(time.DateOnly),
		To:         to.Format(time.DateOnly),
		Months:     months,
		Categories: cats,
	}
}

// monthsBetween returns every "YYYY-MM" from `from`'s month through `to`'s month
// inclusive, ascending. Built from the calendar parts of the two dates rather
// than by stepping a time.Time, so a UTC midnight at month end cannot flip a
// boundary month in a negative-offset timezone. Mirrors the frontend
// monthsBetween in MonthlyBars so the two axes agree to the month.
func monthsBetween(from, to time.Time) []string {
	y, m := from.Year(), int(from.Month())
	ty, tm := to.Year(), int(to.Month())
	if y > ty || (y == ty && m > tm) {
		return nil
	}

	out := make([]string, 0, (ty-y)*12+tm-m+1)
	for {
		out = append(out, fmt.Sprintf("%04d-%02d", y, m))
		if y == ty && m == tm {
			break
		}
		m++
		if m > 12 {
			m = 1
			y++
		}
	}
	return out
}
