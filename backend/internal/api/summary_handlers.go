package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
	"github.com/apex42group/ledgermancy/backend/internal/reporting"
)

// handleCapabilities tells the frontend which optional features are available,
// so it can hide AI-only surfaces (summaries, chat) when no key is configured
// rather than offering a button that only ever returns 503.
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{
		"ai_enabled":     s.AI.Enabled(),
		"notify_enabled": s.Config.NTFY.Enabled(),
	})
}

type monthlySummaryResponse struct {
	Month       string  `json:"month"`
	Label       string  `json:"label"`
	Summary     *string `json:"summary"`
	Model       string  `json:"model,omitempty"`
	GeneratedAt *string `json:"generated_at,omitempty"`
}

// monthPeriod resolves a "2006-01" month string (defaulting to the current
// month) into the first-of-month date the cache is keyed by, the inclusive
// day range for reporting queries, and a human label.
func monthPeriod(monthStr string) (monthDate, from, to time.Time, label string, err error) {
	if monthStr == "" {
		now := time.Now().UTC()
		monthDate = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	} else {
		var t time.Time
		t, err = time.Parse("2006-01", monthStr)
		if err != nil {
			return time.Time{}, time.Time{}, time.Time{}, "", errors.New("month must be formatted YYYY-MM")
		}
		monthDate = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	from = monthDate
	to = monthDate.AddDate(0, 1, -1)
	label = monthDate.Format("January 2006")
	return monthDate, from, to, label, nil
}

func (s *Server) handleGetMonthlySummary(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	monthDate, _, _, label, err := monthPeriod(r.URL.Query().Get("month"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	row, err := s.Queries.GetMonthlySummary(r.Context(), dbgen.GetMonthlySummaryParams{
		HouseholdID: identity.HouseholdID, Month: monthDate,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, monthlySummaryResponse{
			Month: monthDate.Format("2006-01"), Label: label, Summary: nil,
		})
		return
	}
	if err != nil {
		s.internalError(w, "get monthly summary", err)
		return
	}

	generatedAt := row.CreatedAt.UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, monthlySummaryResponse{
		Month:       monthDate.Format("2006-01"),
		Label:       label,
		Summary:     &row.Summary,
		Model:       row.Model,
		GeneratedAt: &generatedAt,
	})
}

func (s *Server) handleGenerateMonthlySummary(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	if !s.AI.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "AI features are not configured")
		return
	}

	monthDate, from, to, label, err := monthPeriod(r.URL.Query().Get("month"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	input, err := reporting.BuildMonthlySummaryInput(
		r.Context(), s.Queries, identity.HouseholdID, identity.UserID, from, to, label, time.Now().UTC())
	if err != nil {
		s.internalError(w, "gather summary data", err)
		return
	}

	text, err := s.AI.MonthlySummary(r.Context(), input)
	if err != nil {
		s.internalError(w, "generate monthly summary", err)
		return
	}

	row, err := s.Queries.UpsertMonthlySummary(r.Context(), dbgen.UpsertMonthlySummaryParams{
		HouseholdID: identity.HouseholdID,
		Month:       monthDate,
		Summary:     text,
		Model:       s.AI.Model(),
	})
	if err != nil {
		s.internalError(w, "cache monthly summary", err)
		return
	}

	generatedAt := row.CreatedAt.UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, monthlySummaryResponse{
		Month:       monthDate.Format("2006-01"),
		Label:       label,
		Summary:     &row.Summary,
		Model:       row.Model,
		GeneratedAt: &generatedAt,
	})
}
