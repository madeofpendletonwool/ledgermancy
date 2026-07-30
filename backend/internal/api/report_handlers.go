package api

import (
	"log/slog"
	"net/http"
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
			Month:    m.Month.Format("2006-01"),
			Income:   m.Income,
			Spending: m.Spending,
			Leftover: m.Income.Sub(m.Spending),
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
			monthly = m.AverageAmount.Mul(daysPerMonth).Div(m.AvgGapDays).Round(2)
		}
		out.Lapsed = append(out.Lapsed, lapsedMerchantResponse{
			Merchant:        m.Merchant,
			MerchantKey:     m.MerchantKey,
			TypicalAmount:   m.AverageAmount.Round(2),
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
	MerchantKey     string          `json:"merchant_key"`
	Merchant        string          `json:"merchant"`
	Occurrences     int64           `json:"occurrences"`
	AverageAmount   decimal.Decimal `json:"average_amount"`
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
			monthly = m.AverageAmount.Mul(daysPerMonth).Div(m.AvgGapDays).Round(2)
		}
		out = append(out, recurringResponse{
			MerchantKey:     m.MerchantKey,
			Merchant:        m.Merchant,
			Occurrences:     m.Occurrences,
			AverageAmount:   m.AverageAmount.Round(2),
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
