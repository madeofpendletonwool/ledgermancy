package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// insightResponse is one feed row. data is passed through as raw JSON so the
// deterministic facts (money as decimal strings) reach the client as an object,
// not a base64 []byte. period/read_at/dismissed_at are nullable.
type insightResponse struct {
	ID          uuid.UUID       `json:"id"`
	Kind        string          `json:"kind"`
	Priority    int16           `json:"priority"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	Data        json.RawMessage `json:"data"`
	Period      *string         `json:"period"`
	CreatedAt   string          `json:"created_at"`
	ReadAt      *string         `json:"read_at"`
	DismissedAt *string         `json:"dismissed_at"`
}

func toInsightResponse(i dbgen.Insight) insightResponse {
	resp := insightResponse{
		ID:        i.ID,
		Kind:      i.Kind,
		Priority:  i.Priority,
		Title:     i.Title,
		Body:      i.Body,
		Data:      json.RawMessage(i.Data),
		CreatedAt: i.CreatedAt.UTC().Format(time.RFC3339),
	}
	if i.Period != nil {
		p := i.Period.Format(time.DateOnly)
		resp.Period = &p
	}
	if i.ReadAt != nil {
		s := i.ReadAt.UTC().Format(time.RFC3339)
		resp.ReadAt = &s
	}
	if i.DismissedAt != nil {
		s := i.DismissedAt.UTC().Format(time.RFC3339)
		resp.DismissedAt = &s
	}
	return resp
}

// handleListInsights serves the feed. state=all includes dismissed rows (for the
// "show dismissed" view); anything else (the default, "unread") hides them.
func (s *Server) handleListInsights(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListInsights(r.Context(), dbgen.ListInsightsParams{
		HouseholdID:      identity.HouseholdID,
		IncludeDismissed: r.URL.Query().Get("state") == "all",
	})
	if err != nil {
		s.internalError(w, "list insights", err)
		return
	}

	out := make([]insightResponse, 0, len(rows))
	for _, i := range rows {
		out = append(out, toInsightResponse(i))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleMarkInsightRead(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "insightID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid insight id")
		return
	}

	if err := s.Queries.MarkInsightRead(r.Context(), dbgen.MarkInsightReadParams{
		ID: id, HouseholdID: identity.HouseholdID,
	}); err != nil {
		s.internalError(w, "mark insight read", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDismissInsight(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "insightID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid insight id")
		return
	}

	if err := s.Queries.DismissInsight(r.Context(), dbgen.DismissInsightParams{
		ID: id, HouseholdID: identity.HouseholdID,
	}); err != nil {
		s.internalError(w, "dismiss insight", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
