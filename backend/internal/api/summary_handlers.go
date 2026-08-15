package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/logos"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/reporting"
)

// handleCapabilities tells the frontend which optional features are available,
// so it can hide AI-only surfaces (summaries, chat) when no key is configured
// rather than offering a button that only ever returns 503.
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	// Whether the operator opted in at all — what Settings needs in order to
	// explain the state of the feature honestly, including the case where the
	// household's own switch is on but nothing was ever enabled to fetch with.
	logosAvailable := s.Config.MerchantLogos.Ready(s.Config.AI)

	// The model ids selectable in the Advisor chat, primary first. A
	// single-entry list is the norm; the frontend hides the selector then, so
	// the chat looks exactly as it did before additional models existed.
	writeJSON(w, http.StatusOK, map[string]any{
		"ai_enabled":     s.AI.Enabled(),
		"ai_chat_models": s.Config.AI.ChatModels(),
		"notify_enabled": s.Config.NTFY.Enabled(),
		// Whether an emailed digest can be offered at all. Off unless the
		// operator configured a mail server, so Settings does not present a
		// toggle that could never deliver anything.
		"smtp_enabled": s.Config.SMTP.Enabled(),

		"merchant_logos_available": logosAvailable,
		// Whether an avatar should actually try to load an image: both switches
		// folded into one answer, so a component rendered fifty times a page
		// asks one question, and so a household's opt-out takes effect on the
		// next render rather than when the cache is next cleared.
		"merchant_logos_enabled": logosAvailable &&
			logos.HouseholdEnabled(r.Context(), s.Queries, identity.HouseholdID),
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
