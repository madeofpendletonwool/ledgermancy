package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
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
	Merchant         string          `json:"merchant"`
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
			Total:            m.Total,
			TransactionCount: m.TransactionCount,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// recurringLookback is how far back subscription detection reads. A year is
// enough to see an annual charge twice and a monthly one many times.
const recurringLookbackMonths = 12

// recurringActiveDays gates out merchants that have gone quiet, so a cancelled
// or paid-off subscription stops lingering in the recurring table. Mirrors the
// same cutoff the subscription/new_recurring insight producers apply (they gate
// in Go too); the detection query returns everything in the lookback window and
// callers drop the stale rows.
const recurringActiveDays = 45

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
	since := now.AddDate(0, -recurringLookbackMonths, 0)
	activeCutoff := now.AddDate(0, 0, -recurringActiveDays)

	rows, err := s.Queries.GetRecurringMerchants(r.Context(), dbgen.GetRecurringMerchantsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        since,
	})
	if err != nil {
		s.internalError(w, "recurring merchants", err)
		return
	}

	out := make([]recurringResponse, 0, len(rows))
	for _, m := range rows {
		// Drop merchants that have gone quiet: a paid-off or cancelled charge
		// should not linger in the table for months. Also skip rows without a
		// merchant_key — suppression is keyed by it, so an unkeyed row could not
		// be acted on anyway.
		if m.MerchantKey == nil || m.LastSeen.Before(activeCutoff) {
			continue
		}
		// Normalise the charge to a monthly figure: amount * (month / gap).
		var monthly decimal.Decimal
		if m.AvgGapDays.IsPositive() {
			monthly = m.AverageAmount.Mul(daysPerMonth).Div(m.AvgGapDays).Round(2)
		}
		out = append(out, recurringResponse{
			MerchantKey:     *m.MerchantKey,
			Merchant:        m.Merchant,
			Occurrences:     m.Occurrences,
			AverageAmount:   m.AverageAmount.Round(2),
			AvgGapDays:      m.AvgGapDays.Round(1),
			Cadence:         cadenceLabel(m.AvgGapDays),
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
	MerchantKey  string `json:"merchant_key"`
	Merchant     string `json:"merchant"`
	SuppressedAt string `json:"suppressed_at"`
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
			MerchantKey:  m.MerchantKey,
			Merchant:     m.MerchantLabel,
			SuppressedAt: m.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// cadenceLabel turns an average day-gap into a human word. The detection query
// only returns gaps in the 6–40 day band, so three buckets cover it.
func cadenceLabel(avgGap decimal.Decimal) string {
	days := avgGap.InexactFloat64()
	switch {
	case days < 10:
		return "weekly"
	case days < 20:
		return "every 2 weeks"
	default:
		return "monthly"
	}
}
