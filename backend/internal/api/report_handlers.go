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
