package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/categorize"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// maxImportRows caps one upload. Capital One (and most banks) export at most a
// year or two per file; a generous ceiling keeps a pathological payload from
// tying up a request without getting in a real backfill's way.
const maxImportRows = 20000

// importMaxBody lifts the default 1 MiB body ceiling for this endpoint only: a
// full 20k-row import is a few MiB of JSON, well under this but far over the
// default. Everything else stays at the small default.
const importMaxBody = 16 << 20 // 16 MiB

// importRequest is a pre-mapped CSV: the frontend has already picked which
// column is the date/description and computed a single SIGNED amount per row
// (positive = money out / spending, negative = money in), so the server stays
// bank-agnostic — it never sees column names or debit/credit conventions.
type importRequest struct {
	AccountID uuid.UUID   `json:"account_id"`
	Rows      []importRow `json:"rows"`
}

type importRow struct {
	Date        string `json:"date"`        // YYYY-MM-DD
	Amount      string `json:"amount"`      // signed decimal string
	Description string `json:"description"` // becomes name + merchant_name
}

type importResponse struct {
	Imported          int `json:"imported"`
	SkippedDuplicates int `json:"skipped_duplicates"`
	SkippedInvalid    int `json:"skipped_invalid"`
	Uncategorized     int `json:"uncategorized"`
}

// handleImportTransactions ingests a batch of mapped CSV rows into one account.
// Each row is de-duplicated against existing transactions (so overlapping
// Plaid's synced window is safe) and run through the same categoriser Plaid
// sync uses — so the transfer/card-payment heuristics apply on the way in.
func (s *Server) handleImportTransactions(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req importRequest
	if err := decodeJSONLimit(w, r, &req, importMaxBody); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.AccountID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "account_id is required")
		return
	}
	if len(req.Rows) == 0 {
		writeError(w, http.StatusBadRequest, "no rows to import")
		return
	}
	if len(req.Rows) > maxImportRows {
		writeError(w, http.StatusBadRequest, "too many rows in one import")
		return
	}

	account, err := s.Queries.GetImportAccount(r.Context(), dbgen.GetImportAccountParams{
		ID: req.AccountID, HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	if err != nil {
		s.internalError(w, "load import account", err)
		return
	}

	resolver, err := categorize.NewResolver(r.Context(), s.Queries, identity.HouseholdID)
	if err != nil {
		s.internalError(w, "prepare categoriser", err)
		return
	}

	var resp importResponse
	for _, row := range req.Rows {
		date, err := time.Parse("2006-01-02", strings.TrimSpace(row.Date))
		if err != nil {
			resp.SkippedInvalid++
			continue
		}
		amount, err := decimal.NewFromString(strings.TrimSpace(row.Amount))
		if err != nil {
			resp.SkippedInvalid++
			continue
		}
		name := strings.TrimSpace(row.Description)
		if name == "" {
			resp.SkippedInvalid++
			continue
		}
		merchantKey := importMerchantKey(name)

		// Deterministic categorisation only (rules → cache → heuristic). A CSV
		// row carries no Plaid PFC, so an unmatched row is left uncategorised
		// for the periodic sweep / LLM pass, exactly like a synced one.
		var categoryID *uuid.UUID
		var categorySource *string
		result, ok, err := resolver.Resolve(r.Context(), categorize.Input{
			MerchantKey:  merchantKey,
			MerchantName: name,
			Name:         name,
		})
		if err != nil {
			s.internalError(w, "categorise imported row", err)
			return
		}
		if ok {
			id := result.CategoryID
			src := string(result.Source)
			categoryID, categorySource = &id, &src
		}

		mn := name
		mk := merchantKey
		newID, err := s.Queries.InsertImportedTransactionIfNew(r.Context(), dbgen.InsertImportedTransactionIfNewParams{
			AccountID:      req.AccountID,
			Amount:         amount,
			Currency:       account.Currency,
			Date:           date,
			Name:           name,
			MerchantName:   &mn,
			MerchantKey:    &mk,
			CategoryID:     categoryID,
			CategorySource: categorySource,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			resp.SkippedDuplicates++
			continue
		}
		if err != nil {
			s.internalError(w, "insert imported transaction", err)
			return
		}
		_ = newID
		resp.Imported++
		if categoryID == nil {
			resp.Uncategorized++
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// importMerchantKey derives a stable, low-effort merchant key from a CSV
// description: lower-cased and whitespace-collapsed. It will not match Plaid's
// own keys (categorisation cache hits across the two sources are a bonus, not a
// guarantee), but it gives imported rows a consistent handle for "apply to all
// from this merchant" and for de-duping a re-import's own categorisation.
func importMerchantKey(description string) string {
	return strings.Join(strings.Fields(strings.ToLower(description)), " ")
}
