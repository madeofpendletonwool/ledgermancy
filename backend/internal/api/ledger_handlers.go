package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
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
	Amount       decimal.Decimal `json:"amount"`
	Currency     string          `json:"currency"`
	Pending      bool            `json:"pending"`
	AccountName  string          `json:"account_name"`
	Institution  *string         `json:"institution_name"`
	PFCPrimary   *string         `json:"plaid_category_primary"`
	PFCDetailed  *string         `json:"plaid_category_detailed"`
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

	rows, err := s.Queries.ListVisibleTransactions(r.Context(), dbgen.ListVisibleTransactionsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
		Limit:       int32(limit),
		Offset:      int32(offset),
		AccountID:   accountID,
	})
	if err != nil {
		s.internalError(w, "list transactions", err)
		return
	}

	out := make([]transactionResponse, 0, len(rows))
	for _, t := range rows {
		out = append(out, transactionResponse{
			ID:           t.ID,
			Date:         t.Date,
			Name:         t.Name,
			MerchantName: t.MerchantName,
			Amount:       t.Amount,
			Currency:     t.Currency,
			Pending:      t.Pending,
			AccountName:  t.AccountName,
			Institution:  t.InstitutionName,
			PFCPrimary:   t.PlaidPfcPrimary,
			PFCDetailed:  t.PlaidPfcDetailed,
		})
	}
	writeJSON(w, http.StatusOK, out)
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
