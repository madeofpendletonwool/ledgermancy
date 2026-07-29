package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/jobs"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/merchants"
)

// merchantAliasResponse is one raw descriptor mapped to a canonical merchant,
// carrying the evidence a person needs to judge a proposed merge. Two strings
// alone are not enough to decide with — "is this the same business?" is really
// "does this spend look like the same business?" — so counts, totals and the
// date range travel with them.
type merchantAliasResponse struct {
	MerchantKey string          `json:"merchant_key"`
	SampleName  string          `json:"sample_name"`
	Source      string          `json:"source"`
	Confidence  *float64        `json:"confidence"`
	Count       int64           `json:"transaction_count"`
	Total       decimal.Decimal `json:"total_amount"`
	FirstSeen   *string         `json:"first_seen"`
	LastSeen    *string         `json:"last_seen"`
}

type merchantEntityResponse struct {
	ID            uuid.UUID  `json:"id"`
	CanonicalName string     `json:"canonical_name"`
	CategoryID    *uuid.UUID `json:"default_category_id"`
	// Members are the descriptors already merged into this merchant.
	Members []merchantAliasResponse `json:"members"`
	// Suggested are descriptors proposed for it and awaiting review. They affect
	// nothing until confirmed — no total on this page or any other includes them.
	Suggested []merchantAliasResponse `json:"suggested"`
	Count     int64                   `json:"transaction_count"`
	Total     decimal.Decimal         `json:"total_amount"`
}

func dateString(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.DateOnly)
	return &s
}

func aliasResponse(row dbgen.ListMerchantAliasStatsRow) merchantAliasResponse {
	out := merchantAliasResponse{
		MerchantKey: row.MerchantKey,
		SampleName:  row.SampleName,
		Source:      row.Source,
		Count:       row.TransactionCount,
		Total:       row.TotalAmount.Round(2),
		FirstSeen:   dateString(row.FirstSeen),
		LastSeen:    dateString(row.LastSeen),
	}
	if row.Confidence.Valid {
		c, _ := row.Confidence.Decimal.Float64()
		out.Confidence = &c
	}
	return out
}

// handleListMerchants returns the household's canonical merchants and the
// pending merge suggestions, in one payload. They come from one query and the
// UI shows them on one page, so splitting them across two endpoints would only
// mean two round trips to render a screen.
func (s *Server) handleListMerchants(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListMerchantAliasStats(r.Context(), dbgen.ListMerchantAliasStatsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list merchants", err)
		return
	}

	byEntity := make(map[uuid.UUID]*merchantEntityResponse)
	order := make([]uuid.UUID, 0)
	for _, row := range rows {
		entity, ok := byEntity[row.EntityID]
		if !ok {
			entity = &merchantEntityResponse{
				ID:            row.EntityID,
				CanonicalName: row.CanonicalName,
				CategoryID:    row.DefaultCategoryID,
				Members:       make([]merchantAliasResponse, 0, 2),
				Suggested:     make([]merchantAliasResponse, 0),
				Total:         decimal.Zero,
			}
			byEntity[row.EntityID] = entity
			order = append(order, row.EntityID)
		}

		alias := aliasResponse(row)
		if row.Source == "suggested" {
			entity.Suggested = append(entity.Suggested, alias)
			continue
		}
		// Only confirmed members contribute to the merchant's totals. A
		// suggestion that inflated the number it is asking you to approve would
		// be arguing its own case.
		entity.Members = append(entity.Members, alias)
		entity.Count += alias.Count
		entity.Total = entity.Total.Add(alias.Total)
	}

	out := make([]merchantEntityResponse, 0, len(order))
	for _, id := range order {
		out = append(out, *byEntity[id])
	}
	writeJSON(w, http.StatusOK, out)
}

// handleListMerchantKeys returns every descriptor the household has, with its
// current mapping — the picker behind "merge these by hand".
func (s *Server) handleListMerchantKeys(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListMerchantKeyStats(r.Context(), dbgen.ListMerchantKeyStatsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list merchant keys", err)
		return
	}

	type keyResponse struct {
		MerchantKey string          `json:"merchant_key"`
		SampleName  string          `json:"sample_name"`
		Count       int64           `json:"transaction_count"`
		Total       decimal.Decimal `json:"total_amount"`
		FirstSeen   string          `json:"first_seen"`
		LastSeen    string          `json:"last_seen"`
		EntityID    *uuid.UUID      `json:"entity_id"`
		AliasSource string          `json:"alias_source"`
	}

	out := make([]keyResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, keyResponse{
			MerchantKey: row.MerchantKey,
			SampleName:  row.SampleName,
			Count:       row.TransactionCount,
			Total:       row.TotalAmount.Round(2),
			FirstSeen:   row.FirstSeen.Format(time.DateOnly),
			LastSeen:    row.LastSeen.Format(time.DateOnly),
			EntityID:    row.EntityID,
			AliasSource: row.AliasSource,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// mergeRequest merges descriptors into one canonical merchant. entity_id targets
// an existing merchant (confirming a suggestion, or adding a stray descriptor);
// omitting it creates a new one named by canonical_name.
type mergeRequest struct {
	Keys          []string   `json:"merchant_keys"`
	EntityID      *uuid.UUID `json:"entity_id"`
	CanonicalName string     `json:"canonical_name"`
}

type mergeResponse struct {
	EntityID uuid.UUID `json:"entity_id"`
	// CategoryConflict is true when the merged descriptors carried two different
	// MANUAL categories. Nothing was changed; the user picks.
	CategoryConflict bool `json:"category_conflict"`
	// CategoryApplied is the category every descriptor now resolves to, when
	// reconciliation settled on one.
	CategoryApplied *uuid.UUID `json:"category_applied"`
	// CategoryFromManual records that the surviving category was a manual choice,
	// so the UI can say the merge kept it rather than guessing.
	CategoryFromManual bool `json:"category_from_manual"`
}

func (s *Server) handleMergeMerchants(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req mergeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	entityID, outcome, err := merchants.Merge(
		r.Context(), s.Queries, identity.HouseholdID, req.Keys, req.CanonicalName, req.EntityID,
	)
	if errors.Is(err, merchants.ErrNoKeys) {
		writeError(w, http.StatusBadRequest, "a merge needs at least two descriptors")
		return
	}
	if err != nil {
		s.internalError(w, "merge merchants", err)
		return
	}

	writeJSON(w, http.StatusOK, mergeResponse{
		EntityID:           entityID,
		CategoryConflict:   outcome.Conflict,
		CategoryApplied:    outcome.Applied,
		CategoryFromManual: outcome.FromManual,
	})
}

type rejectRequest struct {
	Keys []string `json:"merchant_keys"`
}

// handleRejectMerchantSuggestion dismisses proposed descriptors and remembers
// the refusal, so the same merge is never proposed twice.
func (s *Server) handleRejectMerchantSuggestion(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	entityID, err := uuid.Parse(chi.URLParam(r, "merchantID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid merchant id")
		return
	}

	var req rejectRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if len(req.Keys) == 0 {
		writeError(w, http.StatusBadRequest, "no descriptors given")
		return
	}

	if err := merchants.Reject(r.Context(), s.Queries, identity.HouseholdID, entityID, req.Keys); err != nil {
		s.internalError(w, "reject merchant suggestion", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type splitRequest struct {
	Key string `json:"merchant_key"`
}

// handleSplitMerchant detaches a descriptor from its merchant. An over-eager
// merge has to be undoable, and undoing it also records that these two are not
// the same business — otherwise the next suggestion pass proposes it again.
func (s *Server) handleSplitMerchant(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req splitRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if req.Key == "" {
		writeError(w, http.StatusBadRequest, "no descriptor given")
		return
	}

	if err := merchants.Split(r.Context(), s.Queries, identity.HouseholdID, req.Key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type renameMerchantRequest struct {
	CanonicalName string `json:"canonical_name"`
}

func (s *Server) handleRenameMerchant(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	entityID, err := uuid.Parse(chi.URLParam(r, "merchantID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid merchant id")
		return
	}

	var req renameMerchantRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	entity, err := merchants.Rename(r.Context(), s.Queries, identity.HouseholdID, entityID, req.CanonicalName)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, merchantEntityResponse{
		ID:            entity.ID,
		CanonicalName: entity.CanonicalName,
		CategoryID:    entity.DefaultCategoryID,
		Members:       make([]merchantAliasResponse, 0),
		Suggested:     make([]merchantAliasResponse, 0),
		Total:         decimal.Zero,
	})
}

// handleScanMerchants runs a suggestion pass now instead of waiting for the
// daily sweep. The pass only ever writes inert suggestions, so this is safe to
// expose as a plain button.
func (s *Server) handleScanMerchants(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	if err := jobs.EnqueueSuggestMerchants(r.Context(), s.Jobs, identity.HouseholdID); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
