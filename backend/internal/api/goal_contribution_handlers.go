package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/jobs"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/webhooks"
)

// Goal contributions: who funded what.
//
// THE DISTINCTION THIS FILE MUST NOT BLUR. A contribution is ATTRIBUTION. It is
// not progress.
//
//   - For an ACCOUNT-LINKED goal, progress is and remains the linked account's
//     balance (the 00012 header rule: progress is DERIVED, never stored).
//     Contributions here record who put money in; they do not move the bar. A
//     goal linked to a joint savings account shows the account balance whether
//     or not anybody logged a contribution.
//   - For an UNLINKED goal, the balance derives from accumulated household
//     surplus, and contributions are the natural way to say who supplied it.
//     They are still reported separately rather than replacing that figure.
//
// Summing contributions into progress would create a second source of truth
// that drifts from the first the moment somebody forgets to log one.

type contributionResponse struct {
	ID         uuid.UUID `json:"id"`
	GoalID     uuid.UUID `json:"goal_id"`
	PersonID   uuid.UUID `json:"person_id"`
	PersonName string    `json:"person_name"`
	Amount     string    `json:"amount"`
	OccurredOn string    `json:"occurred_on"`
	Note       *string   `json:"note"`
	CreatedAt  string    `json:"created_at"`
}

type contributorTotalResponse struct {
	PersonID   uuid.UUID `json:"person_id"`
	PersonName string    `json:"person_name"`
	Total      string    `json:"total"`
	// Share of all contributions, as a fixed-1 percentage string. Computed here
	// from exact decimals so the client never divides money.
	SharePct string `json:"share_pct"`
}

type goalContributionsResponse struct {
	GoalID uuid.UUID `json:"goal_id"`
	// Total is the sum of logged contributions. It is NOT the goal's progress —
	// see the file header. The client renders it as "funded by", beside
	// progress, never as progress.
	Total        string                     `json:"total"`
	Contributors []contributorTotalResponse `json:"contributors"`
	History      []contributionResponse     `json:"history"`
}

// requireGoalInHousehold resolves a goal id from the URL under the caller's
// visibility rules.
func (s *Server) requireGoalInHousehold(w http.ResponseWriter, r *http.Request,
	identity auth.Identity,
) (dbgen.Goal, bool) {
	goalID, err := uuid.Parse(chi.URLParam(r, "goalID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid goal id")
		return dbgen.Goal{}, false
	}

	userID, personID, all := goalVisibility(identity)
	g, err := s.Queries.GetGoal(r.Context(), dbgen.GetGoalParams{
		ID:             goalID,
		HouseholdID:    identity.HouseholdID,
		UserID:         userID,
		PersonID:       personID,
		AllPersonGoals: all,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "goal not found")
		return dbgen.Goal{}, false
	}
	if err != nil {
		s.internalError(w, "get goal", err)
		return dbgen.Goal{}, false
	}
	return g, true
}

func (s *Server) handleListGoalContributions(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	g, ok := s.requireGoalInHousehold(w, r, identity)
	if !ok {
		return
	}

	resp := goalContributionsResponse{
		GoalID:       g.ID,
		Contributors: []contributorTotalResponse{},
		History:      []contributionResponse{},
	}

	total, err := s.Queries.SumGoalContributions(ctx, dbgen.SumGoalContributionsParams{
		GoalID: g.ID, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "sum goal contributions", err)
		return
	}
	resp.Total = total.StringFixed(2)

	byPerson, err := s.Queries.SumGoalContributionsByPerson(ctx,
		dbgen.SumGoalContributionsByPersonParams{
			GoalID: g.ID, HouseholdID: identity.HouseholdID,
		})
	if err != nil {
		s.internalError(w, "goal contributions by person", err)
		return
	}
	for _, row := range byPerson {
		share := "0.0"
		if !total.IsZero() {
			share = row.Total.Div(total).Mul(decimal.NewFromInt(100)).StringFixed(1)
		}
		resp.Contributors = append(resp.Contributors, contributorTotalResponse{
			PersonID:   row.PersonID,
			PersonName: row.DisplayName,
			Total:      row.Total.StringFixed(2),
			SharePct:   share,
		})
	}

	history, err := s.Queries.ListGoalContributions(ctx, dbgen.ListGoalContributionsParams{
		GoalID: g.ID, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "list goal contributions", err)
		return
	}
	for _, row := range history {
		resp.History = append(resp.History, contributionResponse{
			ID:         row.ID,
			GoalID:     row.GoalID,
			PersonID:   row.PersonID,
			PersonName: row.DisplayName,
			Amount:     row.Amount.StringFixed(2),
			OccurredOn: row.OccurredOn.Format(time.DateOnly),
			Note:       row.Note,
			CreatedAt:  row.CreatedAt.UTC().Format(time.RFC3339),
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

type createContributionRequest struct {
	// PersonID defaults to the caller's own person: logging your own
	// contribution is the common case and should not need the id.
	PersonID   *uuid.UUID `json:"person_id"`
	Amount     string     `json:"amount"`
	OccurredOn *string    `json:"occurred_on"`
	Note       *string    `json:"note"`
}

func (s *Server) handleCreateGoalContribution(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	g, ok := s.requireGoalInHousehold(w, r, identity)
	if !ok {
		return
	}

	var req createContributionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	personID := req.PersonID
	if personID == nil {
		if identity.PersonID == nil {
			writeError(w, http.StatusBadRequest,
				"person_id is required: this login has no person record")
			return
		}
		personID = identity.PersonID
	}

	amount, err := decimal.NewFromString(strings.TrimSpace(req.Amount))
	if err != nil {
		writeError(w, http.StatusBadRequest, "amount must be a decimal amount")
		return
	}
	if !amount.IsPositive() {
		writeError(w, http.StatusBadRequest, "amount must be greater than zero")
		return
	}

	occurredOn := time.Now()
	if req.OccurredOn != nil && strings.TrimSpace(*req.OccurredOn) != "" {
		occurredOn, err = time.Parse(time.DateOnly, strings.TrimSpace(*req.OccurredOn))
		if err != nil {
			writeError(w, http.StatusBadRequest, "occurred_on must be YYYY-MM-DD")
			return
		}
	}

	c, err := s.Queries.CreateGoalContribution(ctx, dbgen.CreateGoalContributionParams{
		GoalID:      g.ID,
		PersonID:    *personID,
		HouseholdID: identity.HouseholdID,
		Amount:      amount,
		OccurredOn:  occurredOn,
		Note:        req.Note,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The join guard found no matching person in this household.
		writeError(w, http.StatusNotFound, "person not found")
		return
	}
	if err != nil {
		s.internalError(w, "create goal contribution", err)
		return
	}

	s.emitGoalContributionWebhooks(ctx, identity.HouseholdID, g, c)

	writeJSON(w, http.StatusCreated, contributionResponse{
		ID:         c.ID,
		GoalID:     c.GoalID,
		PersonID:   c.PersonID,
		Amount:     c.Amount.StringFixed(2),
		OccurredOn: c.OccurredOn.Format(time.DateOnly),
		Note:       c.Note,
		CreatedAt:  c.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// emitGoalContributionWebhooks fans a recorded contribution out to whichever
// webhooks asked for goal.contribution.recorded.
//
// This is the one producer that runs in a request rather than a job, so two
// things about it are deliberate. It is called AFTER the contribution row
// commits, because a webhook must never announce something that then fails to
// save. And every failure is logged rather than surfaced: the contribution
// happened, the user is looking at the result of saving it, and refusing to show
// it because an integration could not be queued would be the wrong trade.
//
// The context is detached for the same reason the audit log detaches: a client
// that has already navigated away would otherwise cancel the enqueue, losing the
// event that had just been recorded.
func (s *Server) emitGoalContributionWebhooks(
	ctx context.Context,
	householdID uuid.UUID,
	goal dbgen.Goal,
	c dbgen.GoalContribution,
) {
	if !s.Config.Webhooks.Enabled {
		return
	}
	ctx = context.WithoutCancel(ctx)

	note := ""
	if c.Note != nil {
		note = *c.Note
	}
	ids, err := webhooks.Emit(ctx, s.Queries, householdID,
		webhooks.TriggerGoalContribution, c.ID.String(), c.CreatedAt, webhooks.GoalContributionData{
			ContributionID: c.ID.String(),
			GoalID:         goal.ID.String(),
			GoalName:       goal.Name,
			PersonID:       c.PersonID.String(),
			// A fixed-2 decimal STRING, exactly as it goes to the browser. Money
			// never leaves this app as a JSON number.
			Amount:     c.Amount.StringFixed(2),
			OccurredOn: c.OccurredOn.Format(time.DateOnly),
			Note:       note,
		})
	if err != nil {
		slog.Error("enqueue goal contribution webhooks", "error", err, "contribution_id", c.ID)
		return
	}
	jobs.EnqueueWebhookDeliveries(ctx, s.Jobs, ids)
}

func (s *Server) handleDeleteGoalContribution(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	contributionID, err := uuid.Parse(chi.URLParam(r, "contributionID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid contribution id")
		return
	}

	rows, err := s.Queries.DeleteGoalContribution(r.Context(),
		dbgen.DeleteGoalContributionParams{
			ID: contributionID, HouseholdID: identity.HouseholdID,
		})
	if err != nil {
		s.internalError(w, "delete goal contribution", err)
		return
	}
	if rows == 0 {
		writeError(w, http.StatusNotFound, "contribution not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --------------------------------------------------------------------------
// The child's own view
// --------------------------------------------------------------------------

// handleListMyGoals serves the caller's own goals. It is the same visibility
// rule ListGoals applies, narrowed to person scope: the child view shows their
// bike fund, not the household's mortgage.
func (s *Server) handleListMyGoals(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()
	now := time.Now()

	if identity.PersonID == nil {
		writeError(w, http.StatusNotFound, "no person record for this login")
		return
	}

	// all_person_goals is false regardless of role: this endpoint is "mine",
	// and an adult calling it wants their own, not everybody's.
	rows, err := s.Queries.ListGoals(ctx, dbgen.ListGoalsParams{
		HouseholdID:    identity.HouseholdID,
		UserID:         &identity.UserID,
		PersonID:       identity.PersonID,
		AllPersonGoals: false,
	})
	if err != nil {
		s.internalError(w, "list my goals", err)
		return
	}

	out := make([]goalResponse, 0, len(rows))
	for _, g := range rows {
		if g.Scope == "household" {
			continue // the child view is deliberately not a household view
		}
		resp, err := s.buildGoalResponse(ctx, g, now)
		if err != nil {
			s.internalError(w, "derive goal standing", err)
			return
		}
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, out)
}
