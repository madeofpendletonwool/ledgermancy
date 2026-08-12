package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// objectChangeResponse is one row of the History panel. OldValue/NewValue are
// the raw JSONB as stored (kept as json.RawMessage so the frontend renders
// exactly what was recorded); null means the field was set on create or cleared.
type objectChangeResponse struct {
	Field            string          `json:"field"`
	OldValue         json.RawMessage `json:"old_value"`
	NewValue         json.RawMessage `json:"new_value"`
	ActorUserID      *uuid.UUID      `json:"actor_user_id"`
	ActorDisplayName *string         `json:"actor_display_name"`
	CreatedAt        time.Time       `json:"created_at"`
}

// objectHistoryLimit is the most rows the History panel ever asks for. Enough
// to trace an object's edits back to its creation without paging, and bounded
// so a churning object cannot pull thousands of rows per open.
const objectHistoryLimit = 100

// changeRow is the common shape every List*Changes query returns; collapsing
// the three generated row types onto it keeps the response assembly identical
// regardless of object kind.
type changeRow struct {
	Field            string
	OldValue         []byte
	NewValue         []byte
	ActorUserID      *uuid.UUID
	ActorDisplayName *string
	CreatedAt        time.Time
}

// handleListObjectChanges serves the History panel: the field-level edits for
// one object, newest-first. A single endpoint dispatches on object_kind to the
// right visibility-scoped query, so every object kind goes through the same
// read surface and the same scoping guarantee: a member sees history on objects
// they can already see, and nothing else.
func (s *Server) handleListObjectChanges(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	q := r.URL.Query()
	kind := q.Get("object_kind")
	objectID, err := uuid.Parse(q.Get("object_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "object_id must be a UUID")
		return
	}

	var rows []changeRow

	switch kind {
	case "transaction":
		dbRows, err := s.Queries.ListTransactionChanges(r.Context(), dbgen.ListTransactionChangesParams{
			HouseholdID:  identity.HouseholdID,
			ObjectID:     objectID,
			ViewerUserID: &identity.UserID,
			LimitCount:   objectHistoryLimit,
		})
		if err != nil {
			s.internalError(w, "list transaction changes", err)
			return
		}
		for _, dr := range dbRows {
			rows = append(rows, changeRow{dr.Field, dr.OldValue, dr.NewValue, dr.ActorUserID, dr.ActorDisplayName, dr.CreatedAt})
		}

	case "budget":
		dbRows, err := s.Queries.ListBudgetChanges(r.Context(), dbgen.ListBudgetChangesParams{
			HouseholdID:  identity.HouseholdID,
			ObjectID:     objectID,
			ViewerUserID: &identity.UserID,
			LimitCount:   objectHistoryLimit,
		})
		if err != nil {
			s.internalError(w, "list budget changes", err)
			return
		}
		for _, dr := range dbRows {
			rows = append(rows, changeRow{dr.Field, dr.OldValue, dr.NewValue, dr.ActorUserID, dr.ActorDisplayName, dr.CreatedAt})
		}

	case "goal":
		userID, personID, all := goalVisibility(identity)
		dbRows, err := s.Queries.ListGoalChanges(r.Context(), dbgen.ListGoalChangesParams{
			HouseholdID:    identity.HouseholdID,
			ObjectID:       objectID,
			ViewerUserID:   userID,
			AllPersonGoals: all,
			PersonID:       personID,
			LimitCount:     objectHistoryLimit,
		})
		if err != nil {
			s.internalError(w, "list goal changes", err)
			return
		}
		for _, dr := range dbRows {
			rows = append(rows, changeRow{dr.Field, dr.OldValue, dr.NewValue, dr.ActorUserID, dr.ActorDisplayName, dr.CreatedAt})
		}

	default:
		writeError(w, http.StatusBadRequest, "object_kind must be transaction, budget, or goal")
		return
	}

	out := make([]objectChangeResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, objectChangeResponse{
			Field:            row.Field,
			OldValue:         json.RawMessage(row.OldValue),
			NewValue:         json.RawMessage(row.NewValue),
			ActorUserID:      row.ActorUserID,
			ActorDisplayName: row.ActorDisplayName,
			CreatedAt:        row.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
