package api

import (
	"encoding/csv"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// exportWindow resolves the range for an export, defaulting to the trailing
// twelve months — the standard window for reviewing a year.
func exportWindow(r *http.Request) (from, to time.Time) {
	now := time.Now()
	q := r.URL.Query()
	to = parseDate(q.Get("to"), now)
	return parseDate(q.Get("from"), to.AddDate(-1, 0, 0)), to
}

// beginCSV sets the headers that make a browser download the response as a
// file rather than render it, and returns a writer.
func beginCSV(w http.ResponseWriter, name string, from, to time.Time) *csv.Writer {
	filename := fmt.Sprintf("ledgermancy-%s-%s-to-%s.csv",
		name, from.Format(time.DateOnly), to.Format(time.DateOnly))

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	return csv.NewWriter(w)
}

// handleExportTransactions streams every visible transaction in the window.
//
// Amounts are written as exact decimal strings, and the sign is flipped from
// Plaid's convention to the one a human reads: negative is money out. A
// spreadsheet summing this column gets the right answer without a formula.
func (s *Server) handleExportTransactions(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	from, to := exportWindow(r)

	rows, err := s.Queries.ExportTransactions(r.Context(), dbgen.ExportTransactionsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
	})
	if err != nil {
		s.internalError(w, "export transactions", err)
		return
	}

	cw := beginCSV(w, "transactions", from, to)
	defer cw.Flush()

	if err := cw.Write([]string{
		"date", "description", "merchant", "category", "amount",
		"currency", "account", "institution", "is_transfer", "is_income",
	}); err != nil {
		slog.Error("write csv header", "error", err)
		return
	}

	for _, t := range rows {
		if err := cw.Write([]string{
			t.Date.Format(time.DateOnly),
			t.Name,
			derefStr(t.MerchantName),
			derefStr(t.CategoryName),
			t.Amount.Neg().String(),
			t.Currency,
			t.AccountName,
			derefStr(t.InstitutionName),
			boolStr(t.IsTransfer),
			boolStr(t.IsIncome),
		}); err != nil {
			// The status line is already sent, so the download will simply
			// truncate; logging is all that is left.
			slog.Error("write csv row", "error", err)
			return
		}
	}
}

// handleExportCategorySummary streams per-category totals and averages.
func (s *Server) handleExportCategorySummary(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	from, to := exportWindow(r)

	rows, err := s.Queries.GetCategoryAverages(r.Context(), dbgen.GetCategoryAveragesParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
	})
	if err != nil {
		s.internalError(w, "export category summary", err)
		return
	}

	cw := beginCSV(w, "category-summary", from, to)
	defer cw.Flush()

	_ = cw.Write([]string{"category", "fixed_or_discretionary", "total", "monthly_average", "transactions"})
	for _, c := range rows {
		kind := "discretionary"
		if c.IsFixed {
			kind = "fixed"
		}
		if err := cw.Write([]string{
			c.CategoryName,
			kind,
			c.Total.String(),
			c.MonthlyAverage.Round(2).String(),
			fmt.Sprint(c.TransactionCount),
		}); err != nil {
			slog.Error("write csv row", "error", err)
			return
		}
	}
}

// handleExportNetWorth streams the recorded net-worth history.
func (s *Server) handleExportNetWorth(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	now := time.Now()
	q := r.URL.Query()
	to := parseDate(q.Get("to"), now)
	from := parseDate(q.Get("from"), to.AddDate(-2, 0, 0))

	rows, err := s.Queries.ListNetWorthSnapshots(r.Context(), dbgen.ListNetWorthSnapshotsParams{
		HouseholdID: identity.HouseholdID,
		AsOf:        from,
		AsOf_2:      to,
	})
	if err != nil {
		s.internalError(w, "export net worth", err)
		return
	}

	cw := beginCSV(w, "net-worth", from, to)
	defer cw.Flush()

	_ = cw.Write([]string{"date", "assets", "liabilities", "net_worth"})
	for _, snap := range rows {
		if err := cw.Write([]string{
			snap.AsOf.Format(time.DateOnly),
			snap.AssetsTotal.String(),
			snap.LiabilitiesTotal.String(),
			snap.NetWorth.String(),
		}); err != nil {
			slog.Error("write csv row", "error", err)
			return
		}
	}
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
