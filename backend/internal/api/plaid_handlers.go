package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// errPlaidDisabled is returned when Plaid credentials are not configured.
const errPlaidDisabled = "Plaid is not configured; set PLAID_CLIENT_ID and PLAID_SECRET"

type linkTokenResponse struct {
	LinkToken string `json:"link_token"`
}

// handleCreateLinkToken issues the short-lived token the frontend hands to
// Plaid Link to open the bank-selection flow.
func (s *Server) handleCreateLinkToken(w http.ResponseWriter, r *http.Request) {
	if s.Plaid == nil {
		writeError(w, http.StatusServiceUnavailable, errPlaidDisabled)
		return
	}
	identity := auth.MustFromContext(r.Context())

	token, err := s.Plaid.CreateLinkToken(r.Context(),
		identity.UserID.String(), identity.DisplayName)
	if err != nil {
		s.internalError(w, "create link token", err)
		return
	}
	writeJSON(w, http.StatusOK, linkTokenResponse{LinkToken: token})
}

type exchangeRequest struct {
	PublicToken string `json:"public_token"`
}

type itemResponse struct {
	ID               uuid.UUID  `json:"id"`
	InstitutionName  string     `json:"institution_name"`
	Status           string     `json:"status"`
	Products         []string   `json:"products"`
	IsShared         bool       `json:"is_shared"`
	BackfillComplete bool       `json:"backfill_complete"`
	LastSyncedAt     *time.Time `json:"last_synced_at"`
	ErrorCode        *string    `json:"error_code"`
	// How much history this institution actually returned. Plaid fixes an
	// Item's history window at link time and it cannot be widened afterwards,
	// so a short span is worth showing while relinking is still an option.
	EarliestTransaction *time.Time `json:"earliest_transaction"`
	LatestTransaction   *time.Time `json:"latest_transaction"`
	HistoryDays         *int       `json:"history_days"`
}

// itemHistorySpan is the transaction date range for one item.
type itemHistorySpan struct {
	earliest *time.Time
	latest   *time.Time
}

// itemHistorySpans returns date bounds per item in one query.
//
// Hand-written because sqlc infers min()/max() over a NOT NULL column as NOT
// NULL, while an item with no transactions yet returns NULL for both.
func (s *Server) itemHistorySpans(ctx context.Context, householdID uuid.UUID) (map[uuid.UUID]itemHistorySpan, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT i.id, min(t.date), max(t.date)
		FROM plaid_items i
		JOIN users u        ON u.id = i.user_id
		LEFT JOIN accounts a     ON a.plaid_item_id = i.id
		LEFT JOIN transactions t ON t.account_id = a.id
		WHERE u.household_id = $1
		GROUP BY i.id`, householdID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	spans := make(map[uuid.UUID]itemHistorySpan)
	for rows.Next() {
		var id uuid.UUID
		var span itemHistorySpan
		if err := rows.Scan(&id, &span.earliest, &span.latest); err != nil {
			return nil, err
		}
		spans[id] = span
	}
	return spans, rows.Err()
}

// handleExchangePublicToken completes the Link flow: it swaps the public token
// for a long-lived access token, encrypts it, and stores the item.
//
// The access token is never returned to the browser — it grants read access to
// the user's bank and only ever lives encrypted at rest or in server memory.
func (s *Server) handleExchangePublicToken(w http.ResponseWriter, r *http.Request) {
	if s.Plaid == nil {
		writeError(w, http.StatusServiceUnavailable, errPlaidDisabled)
		return
	}
	identity := auth.MustFromContext(r.Context())

	var req exchangeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.PublicToken == "" {
		writeError(w, http.StatusBadRequest, "public_token is required")
		return
	}

	ctx := r.Context()

	accessToken, plaidItemID, err := s.Plaid.ExchangePublicToken(ctx, req.PublicToken)
	if err != nil {
		s.internalError(w, "exchange public token", err)
		return
	}

	sealed, err := s.Cipher.SealString(accessToken)
	if err != nil {
		s.internalError(w, "encrypt access token", err)
		return
	}

	// A missing institution name is cosmetic; linking should still succeed.
	var institutionID, institutionName *string
	if inst, err := s.Plaid.GetInstitution(ctx, accessToken); err == nil {
		if inst.ID != "" {
			institutionID = &inst.ID
		}
		if inst.Name != "" {
			institutionName = &inst.Name
		}
	} else {
		slog.Warn("could not resolve institution", "error", err)
	}

	item, err := s.Queries.CreatePlaidItem(ctx, dbgen.CreatePlaidItemParams{
		UserID:               identity.UserID,
		PlaidItemID:          plaidItemID,
		AccessTokenEncrypted: sealed,
		InstitutionID:        institutionID,
		InstitutionName:      institutionName,
		Products:             s.Plaid.Products(),
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "that institution is already linked")
			return
		}
		s.internalError(w, "create plaid item", err)
		return
	}

	// Kick off the first sync, which performs the full historical backfill.
	s.enqueueSync(item.ID)

	writeJSON(w, http.StatusCreated, toItemResponse(item))
}

func (s *Server) handleListItems(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	items, err := s.Queries.ListVisiblePlaidItems(r.Context(), dbgen.ListVisiblePlaidItemsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list plaid items", err)
		return
	}

	// A missing span map is cosmetic; the items themselves still render.
	spans, err := s.itemHistorySpans(r.Context(), identity.HouseholdID)
	if err != nil {
		slog.Warn("load item history spans", "error", err)
	}

	out := make([]itemResponse, 0, len(items))
	for _, item := range items {
		resp := toItemResponse(item)
		if span, ok := spans[item.ID]; ok && span.earliest != nil && span.latest != nil {
			resp.EarliestTransaction = span.earliest
			resp.LatestTransaction = span.latest
			days := int(span.latest.Sub(*span.earliest).Hours() / 24)
			resp.HistoryDays = &days
		}
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, out)
}

type syncResponse struct {
	ItemID      uuid.UUID  `json:"item_id"`
	Pages       int        `json:"pages"`
	Added       int        `json:"added"`
	Modified    int        `json:"modified"`
	Removed     int        `json:"removed"`
	Accounts    int        `json:"accounts"`
	Categorised int        `json:"categorised"`
	Earliest    *time.Time `json:"earliest_transaction"`
	Latest      *time.Time `json:"latest_transaction"`
}

// handleSyncItem runs a sync inline and reports what it did. Routine syncing
// happens in the worker; this exists for an explicit "refresh now".
func (s *Server) handleSyncItem(w http.ResponseWriter, r *http.Request) {
	if s.Syncer == nil {
		writeError(w, http.StatusServiceUnavailable, errPlaidDisabled)
		return
	}
	identity := auth.MustFromContext(r.Context())

	itemID, err := uuid.Parse(chi.URLParam(r, "itemID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid item id")
		return
	}
	if !s.userOwnsItem(w, r, identity.UserID, itemID) {
		return
	}

	result, err := s.Syncer.SyncItem(r.Context(), itemID)
	if err != nil {
		s.internalError(w, "sync item", err)
		return
	}

	writeJSON(w, http.StatusOK, syncResponse{
		ItemID:      result.ItemID,
		Pages:       result.Pages,
		Added:       result.Added,
		Modified:    result.Modified,
		Removed:     result.Removed,
		Accounts:    result.AccountsUpserted,
		Categorised: result.Categorised,
		Earliest:    result.Earliest,
		Latest:      result.Latest,
	})
}

type sharingRequest struct {
	IsShared bool `json:"is_shared"`
}

// handleSetItemSharing toggles whether the other household member can see this
// item's accounts and transactions.
func (s *Server) handleSetItemSharing(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	itemID, err := uuid.Parse(chi.URLParam(r, "itemID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid item id")
		return
	}

	var req sharingRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// user_id is part of the predicate, so only the owner can change sharing.
	item, err := s.Queries.SetItemShared(r.Context(), dbgen.SetItemSharedParams{
		ID: itemID, UserID: identity.UserID, IsShared: req.IsShared,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "item not found")
			return
		}
		s.internalError(w, "set item sharing", err)
		return
	}
	writeJSON(w, http.StatusOK, toItemResponse(item))
}

func (s *Server) handleDeleteItem(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	itemID, err := uuid.Parse(chi.URLParam(r, "itemID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid item id")
		return
	}

	// Cascades to accounts and transactions.
	if err := s.Queries.DeletePlaidItem(r.Context(), dbgen.DeletePlaidItemParams{
		ID: itemID, UserID: identity.UserID,
	}); err != nil {
		s.internalError(w, "delete plaid item", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// userOwnsItem reports whether the caller owns the item, writing the error
// response itself when they do not.
func (s *Server) userOwnsItem(w http.ResponseWriter, r *http.Request, userID, itemID uuid.UUID) bool {
	item, err := s.Queries.GetPlaidItem(r.Context(), itemID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "item not found")
			return false
		}
		s.internalError(w, "load item", err)
		return false
	}
	if item.UserID != userID {
		// 404 rather than 403: a caller who does not own the item should not
		// learn that it exists.
		writeError(w, http.StatusNotFound, "item not found")
		return false
	}
	return true
}

func toItemResponse(item dbgen.PlaidItem) itemResponse {
	name := ""
	if item.InstitutionName != nil {
		name = *item.InstitutionName
	}
	return itemResponse{
		ID:               item.ID,
		InstitutionName:  name,
		Status:           item.Status,
		Products:         item.Products,
		IsShared:         item.IsShared,
		BackfillComplete: item.BackfillComplete,
		LastSyncedAt:     item.LastSyncedAt,
		ErrorCode:        item.ErrorCode,
	}
}
