package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
	"github.com/apex42group/ledgermancy/backend/internal/plaid"
)

type accountResponse struct {
	ID               uuid.UUID        `json:"id"`
	Name             string           `json:"name"`
	Mask             *string          `json:"mask"`
	Type             string           `json:"type"`
	Subtype          *string          `json:"subtype"`
	Institution      *string          `json:"institution_name"`
	CurrentBalance   *decimal.Decimal `json:"current_balance"`
	AvailableBalance *decimal.Decimal `json:"available_balance"`
	Currency         string           `json:"currency"`
	IsOwn            bool             `json:"is_own"`
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListVisibleAccounts(r.Context(), dbgen.ListVisibleAccountsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list accounts", err)
		return
	}

	out := make([]accountResponse, 0, len(rows))
	for _, a := range rows {
		out = append(out, accountResponse{
			ID:               a.ID,
			Name:             a.Name,
			Mask:             a.Mask,
			Type:             a.Type,
			Subtype:          a.Subtype,
			Institution:      a.InstitutionName,
			CurrentBalance:   nullDecimal(a.CurrentBalance),
			AvailableBalance: nullDecimal(a.AvailableBalance),
			Currency:         a.Currency,
			IsOwn:            a.OwnerID == identity.UserID,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type transactionResponse struct {
	ID           uuid.UUID       `json:"id"`
	Date         time.Time       `json:"date"`
	Name         string          `json:"name"`
	MerchantName *string         `json:"merchant_name"`
	// MerchantKey is the normalized key the app caches categories by. Present
	// even when MerchantName is null (it falls back to the raw name), and empty
	// when the description carried too little signal to key on. The UI shows the
	// "apply to all from this merchant" option exactly when this is set.
	MerchantKey *string `json:"merchant_key"`
	Amount       decimal.Decimal `json:"amount"`
	Currency     string          `json:"currency"`
	Pending      bool            `json:"pending"`
	AccountID    uuid.UUID       `json:"account_id"`
	AccountName  string          `json:"account_name"`
	Institution  *string         `json:"institution_name"`
	PFCPrimary   *string         `json:"plaid_category_primary"`
	PFCDetailed  *string         `json:"plaid_category_detailed"`
	CategoryID   *uuid.UUID      `json:"category_id"`
	Notes        *string         `json:"notes"`
	// Source distinguishes hand-entered rows from aggregator feeds. The UI shows
	// edit/delete only on 'manual' rows; Plaid rows stay read-only except for
	// category, which has its own PATCH path.
	Source string `json:"source"`
	// PossibleDuplicate flags a manual row that a later Plaid-synced charge now
	// appears to match, so the UI can offer a one-click "delete my manual entry".
	PossibleDuplicate bool `json:"possible_duplicate"`
}

// defaultTransactionWindow is used when the caller does not supply dates: a
// rolling year, which is the span this app is built around.
const defaultTransactionWindow = 365 * 24 * time.Hour

func (s *Server) handleListTransactions(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	q := r.URL.Query()

	to := parseDate(q.Get("to"), time.Now())
	from := parseDate(q.Get("from"), to.Add(-defaultTransactionWindow))

	limit := parseInt(q.Get("limit"), 100, 1, 500)
	offset := parseInt(q.Get("offset"), 0, 0, 1_000_000)

	// Optional account filter. A malformed id is ignored, which reads as "all
	// accounts"; household/shared scoping in the query means a foreign id
	// simply matches nothing rather than leaking another household's rows.
	var accountID *uuid.UUID
	if v := q.Get("account_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			accountID = &id
		}
	}

	// Optional "only rows still needing a category" filter, for draining the
	// backlog. Any other value (or absent) leaves it off.
	var uncategorised *bool
	if q.Get("uncategorised") == "true" {
		t := true
		uncategorised = &t
	}

	rows, err := s.Queries.ListVisibleTransactions(r.Context(), dbgen.ListVisibleTransactionsParams{
		HouseholdID:   identity.HouseholdID,
		UserID:        identity.UserID,
		Date:          from,
		Date_2:        to,
		Limit:         int32(limit),
		Offset:        int32(offset),
		AccountID:     accountID,
		Uncategorised: uncategorised,
	})
	if err != nil {
		s.internalError(w, "list transactions", err)
		return
	}

	out := make([]transactionResponse, 0, len(rows))
	for _, t := range rows {
		out = append(out, transactionResponse{
			ID:                t.ID,
			Date:              t.Date,
			Name:              t.Name,
			MerchantName:      t.MerchantName,
			MerchantKey:       t.MerchantKey,
			Amount:            t.Amount,
			Currency:          t.Currency,
			Pending:           t.Pending,
			AccountID:         t.AccountID,
			AccountName:       t.AccountName,
			Institution:       t.InstitutionName,
			PFCPrimary:        t.PlaidPfcPrimary,
			PFCDetailed:       t.PlaidPfcDetailed,
			CategoryID:        t.CategoryID,
			Notes:             t.Notes,
			Source:            t.Source,
			PossibleDuplicate: t.IsPossibleDuplicate != nil && *t.IsPossibleDuplicate,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// manualTransactionRequest is the shared body for creating and editing a
// hand-entered transaction. Amount is a decimal STRING and already signed by
// the caller (the UI's expense/income toggle sets the sign: positive = money
// out, negative = a refund/credit), so it never passes through a JSON float.
type manualTransactionRequest struct {
	AccountID    uuid.UUID  `json:"account_id"`
	Date         string     `json:"date"`
	Amount       string     `json:"amount"`
	Name         string     `json:"name"`
	MerchantName *string    `json:"merchant_name"`
	CategoryID   *uuid.UUID `json:"category_id"`
	Notes        *string    `json:"notes"`
}

// parsed holds the validated, normalised fields shared by create and update.
type parsedManualTransaction struct {
	date         time.Time
	amount       decimal.Decimal
	name         string
	merchantName *string
	merchantKey  *string
	notes        *string
}

// parse validates the request and normalises the free-text fields. It returns a
// human-readable message on the first problem, which the handler sends as a 400.
func (req manualTransactionRequest) parse() (parsedManualTransaction, string) {
	var p parsedManualTransaction

	date, err := time.Parse(time.DateOnly, req.Date)
	if err != nil {
		return p, "date must be YYYY-MM-DD"
	}
	p.date = date

	amount, err := decimal.NewFromString(strings.TrimSpace(req.Amount))
	if err != nil {
		return p, "amount must be a decimal number, e.g. \"11.86\" (positive) or \"-11.86\" (refund)"
	}
	if amount.IsZero() {
		return p, "amount cannot be zero"
	}
	p.amount = amount

	// A merchant name typed with no separate description doubles as the name,
	// since name is NOT NULL and is what the list falls back to for display.
	name := strings.TrimSpace(req.Name)
	merchant := strings.TrimSpace(ptrStr(req.MerchantName))
	if name == "" {
		name = merchant
	}
	if name == "" {
		return p, "a merchant name or description is required"
	}
	p.name = name
	p.merchantName = nilIfEmpty(merchant)
	p.notes = nilIfEmpty(strings.TrimSpace(ptrStr(req.Notes)))

	// The merchant key feeds the categorisation cache and recurring detector, so
	// a manual row is seen like any other. "" means nothing meaningful survived.
	if key := plaid.MerchantKey(merchant, name); key != "" {
		p.merchantKey = &key
	}

	return p, ""
}

func (s *Server) handleCreateManualTransaction(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req manualTransactionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.AccountID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "account_id is required")
		return
	}
	p, msg := req.parse()
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	// Household scoping lives in the query's SELECT: a foreign or invisible
	// account_id inserts nothing and comes back as ErrNoRows.
	created, err := s.Queries.CreateManualTransaction(r.Context(), dbgen.CreateManualTransactionParams{
		AccountID:    req.AccountID,
		HouseholdID:  identity.HouseholdID,
		UserID:       identity.UserID,
		Amount:       p.amount,
		Date:         p.date,
		Name:         p.name,
		MerchantName: p.merchantName,
		MerchantKey:  p.merchantKey,
		CategoryID:   req.CategoryID,
		Notes:        p.notes,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "account not found")
			return
		}
		s.internalError(w, "create manual transaction", err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          created.ID,
		"account_id":  created.AccountID,
		"date":        created.Date,
		"amount":      created.Amount,
		"name":        created.Name,
		"category_id": created.CategoryID,
		"source":      created.Source,
	})
}

func (s *Server) handleUpdateManualTransaction(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	transactionID, err := uuid.Parse(chi.URLParam(r, "transactionID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid transaction id")
		return
	}

	var req manualTransactionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, msg := req.parse()
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	// The source='manual' + household guard in the query means an id belonging
	// to another household, or to a Plaid-synced row, matches nothing → 404.
	updated, err := s.Queries.UpdateManualTransaction(r.Context(), dbgen.UpdateManualTransactionParams{
		ID:           transactionID,
		HouseholdID:  identity.HouseholdID,
		Amount:       p.amount,
		Date:         p.date,
		Name:         p.name,
		MerchantName: p.merchantName,
		MerchantKey:  p.merchantKey,
		CategoryID:   req.CategoryID,
		Notes:        p.notes,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "manual transaction not found")
			return
		}
		s.internalError(w, "update manual transaction", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":          updated.ID,
		"account_id":  updated.AccountID,
		"date":        updated.Date,
		"amount":      updated.Amount,
		"name":        updated.Name,
		"category_id": updated.CategoryID,
		"source":      updated.Source,
	})
}

func (s *Server) handleDeleteManualTransaction(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	transactionID, err := uuid.Parse(chi.URLParam(r, "transactionID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid transaction id")
		return
	}

	rows, err := s.Queries.DeleteManualTransaction(r.Context(), dbgen.DeleteManualTransactionParams{
		ID:          transactionID,
		HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "delete manual transaction", err)
		return
	}
	// Zero rows means the id was not a manual transaction in this household — a
	// Plaid row, another household's row, or nothing. Report it as not found so
	// the endpoint can never delete a synced transaction.
	if rows == 0 {
		writeError(w, http.StatusNotFound, "manual transaction not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// nullDecimal converts a nullable database decimal into a pointer so it
// serialises as JSON null rather than "0".
func nullDecimal(d decimal.NullDecimal) *decimal.Decimal {
	if !d.Valid {
		return nil
	}
	return &d.Decimal
}

func parseDate(raw string, fallback time.Time) time.Time {
	if raw == "" {
		return fallback
	}
	parsed, err := time.Parse(time.DateOnly, raw)
	if err != nil {
		return fallback
	}
	return parsed
}

func parseInt(raw string, fallback, min, max int) int {
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return clamp(v, min, max)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
