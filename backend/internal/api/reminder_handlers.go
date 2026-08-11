package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/insights"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/obligations"
)

// The reminders HTTP surface (MAD-85). The Reminders view itself is a filtered
// read of /api/insights, so it needs no handler here; the only writes the
// feature introduces are confirming or clearing one occurrence's "paid" state.

// satisfyRequest names the occurrence being marked paid. due_date is the
// specific cycle (YYYY-MM-DD) the satisfaction covers — the same DATE the
// cadence expansion produces and the overdue_bill insight carries.
type satisfyRequest struct {
	DueDate string `json:"due_date"`
}

// satisfactionResponse echoes back what was recorded, so the UI can confirm the
// exact occurrence it marked without re-reading the feed.
type satisfactionResponse struct {
	ObligationID string `json:"obligation_id"`
	DueDate      string `json:"due_date"`
	Source       string `json:"source"`
	SatisfiedAt  string `json:"satisfied_at"`
}

// handleSatisfyObligation marks one occurrence of a recurring obligation as
// paid. A member presses this on an overdue_bill reminder when the payment went
// through but the matcher could not find it (paid by check, under a different
// name, or just outside the match window).
//
// Recording the satisfaction stops the producer re-raising the reminder; the
// handler also dismisses the existing feed row for that occurrence so it leaves
// the Reminders view immediately rather than lingering until the next insight
// generation pass.
func (s *Server) handleSatisfyObligation(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	obligationID, err := uuid.Parse(chi.URLParam(r, "obligationID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid obligation id")
		return
	}

	var req satisfyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	due, err := time.Parse(time.DateOnly, req.DueDate)
	if err != nil {
		writeError(w, http.StatusBadRequest, "due_date must be a YYYY-MM-DD date")
		return
	}

	// MarkObligationSatisfied re-resolves the obligation through the household
	// guard, so a caller can only satisfy their own household's occurrences.
	// No rows means the obligation is invisible to this caller — report 404
	// rather than revealing whether it exists.
	row, err := s.Queries.MarkObligationSatisfied(r.Context(), dbgen.MarkObligationSatisfiedParams{
		ObligationID: obligationID,
		DueDate:      due,
		UserID:       &identity.UserID,
		HouseholdID:  identity.HouseholdID,
	})
	if err == pgx.ErrNoRows {
		writeError(w, http.StatusNotFound, "obligation not found")
		return
	}
	if err != nil {
		s.internalError(w, "mark obligation satisfied", err)
		return
	}

	// Drop the corresponding feed row now, so the member who just acted does not
	// see the reminder a moment longer than necessary. Best-effort: a failure
	// here leaves the row to age out at the next generation pass, which is
	// acceptable, so it is not propagated as an error.
	if _, err := s.Queries.DismissInsightByDedupe(r.Context(), dbgen.DismissInsightByDedupeParams{
		HouseholdID: identity.HouseholdID,
		DedupeKey:   insights.OverdueBillDedupeKey(obligationID, due),
	}); err != nil {
		// Logged by internalError's caller path only on real need; here a miss
		// is benign, so swallow.
		_ = err
	}

	writeJSON(w, http.StatusOK, satisfactionResponse{
		ObligationID: row.ObligationID.String(),
		DueDate:      row.DueDate.Format(time.DateOnly),
		Source:       row.Source,
		SatisfiedAt:  row.SatisfiedAt.UTC().Format(time.RFC3339),
	})
}

// handleClearObligationSatisfied removes a satisfaction record, re-arming the
// reminder for that occurrence. Used to undo a mark-paid that was wrong, or to
// surface an occurrence the matcher had suppressed once the member decides it
// genuinely is still unpaid.
func (s *Server) handleClearObligationSatisfied(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	obligationID, err := uuid.Parse(chi.URLParam(r, "obligationID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid obligation id")
		return
	}

	var req satisfyRequest
	if err := decodeJSONOptional(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	due, err := time.Parse(time.DateOnly, req.DueDate)
	if err != nil {
		writeError(w, http.StatusBadRequest, "due_date must be a YYYY-MM-DD date")
		return
	}

	// ClearObligationSatisfied is household-scoped through the obligation, so a
	// caller can only clear their own household's rows.
	if _, err := s.Queries.ClearObligationSatisfied(r.Context(), dbgen.ClearObligationSatisfiedParams{
		ObligationID: obligationID,
		DueDate:      due,
		HouseholdID:  identity.HouseholdID,
	}); err != nil {
		s.internalError(w, "clear obligation satisfied", err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// remindRequest is the body for the per-item reminders toggle. A single boolean
// keeps the endpoint self-describing and independent of the obligation/goal
// update forms, matching how auto-post is its own endpoint.
type remindRequest struct {
	Remind bool `json:"remind"`
}

// handleSetObligationRemind turns the reminders opt-out on or off for one
// obligation. Off stops the overdue_bill producer coaching about it; on (the
// default) resumes. Returns the updated obligation so the UI can reflect the new
// state without a separate read.
func (s *Server) handleSetObligationRemind(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	obligationID, err := uuid.Parse(chi.URLParam(r, "obligationID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid obligation id")
		return
	}

	var req remindRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	o, err := s.Queries.SetObligationRemind(r.Context(), dbgen.SetObligationRemindParams{
		ID:          obligationID,
		HouseholdID: identity.HouseholdID,
		UserID:      &identity.UserID,
		Remind:      req.Remind,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "obligation not found")
		return
	}
	if err != nil {
		s.internalError(w, "set obligation remind", err)
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

// handleSetGoalRemind turns the reminders opt-out on or off for one goal. Off
// stops the payoff_progress producer coaching it. Uses the same visibility
// resolution as every other goal write, so a member can toggle their own and
// household goals (and, as an adult, a person goal they manage).
func (s *Server) handleSetGoalRemind(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	goalID, err := uuid.Parse(chi.URLParam(r, "goalID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid goal id")
		return
	}

	var req remindRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	userID, personID, all := goalVisibility(identity)
	g, err := s.Queries.SetGoalRemind(ctx, dbgen.SetGoalRemindParams{
		ID:             goalID,
		HouseholdID:    identity.HouseholdID,
		UserID:         userID,
		PersonID:       personID,
		AllPersonGoals: all,
		Remind:         req.Remind,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "goal not found")
		return
	}
	if err != nil {
		s.internalError(w, "set goal remind", err)
		return
	}

	resp, err := s.buildGoalResponse(ctx, g, time.Now())
	if err != nil {
		s.internalError(w, "build goal response", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
