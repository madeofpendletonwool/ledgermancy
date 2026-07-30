package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// categoryDetailResponse is one category over a period: the headline numbers, the
// shape over time, who the money went to, and the charges behind all three.
//
// The counterpart of merchantDetailResponse, and bundled for the same reason —
// the page shows all of it at once, and four round trips would let the four halves
// of one answer disagree with each other while they loaded.
type categoryDetailResponse struct {
	CategoryID uuid.UUID `json:"category_id"`
	Name       string    `json:"name"`
	Slug       string    `json:"slug"`
	Color      *string   `json:"color"`
	// IsFixed is how the reporting layer splits fixed from discretionary spend, so
	// the page can say which side of that line the category sits on.
	IsFixed bool `json:"is_fixed"`
	// IsSystem marks a built-in category, which the Categories page cannot rename
	// or delete — worth knowing before offering an edit affordance.
	IsSystem bool `json:"is_system"`

	From string `json:"from"`
	To   string `json:"to"`

	Total            decimal.Decimal `json:"total"`
	TransactionCount int64           `json:"transaction_count"`
	Average          decimal.Decimal `json:"average"`
	Largest          decimal.Decimal `json:"largest"`
	FirstSeen        *string         `json:"first_seen"`
	LastSeen         *string         `json:"last_seen"`

	Monthly      []categoryMonthPoint `json:"monthly"`
	Merchants    []categoryMerchant   `json:"merchants"`
	Transactions []categoryDetailTxn  `json:"transactions"`
}

// categoryMonthPoint is shaped identically to merchantMonthPoint so both detail
// pages feed the same MonthlyBars chart.
type categoryMonthPoint struct {
	Month            string          `json:"month"`
	Total            decimal.Decimal `json:"total"`
	TransactionCount int64           `json:"transaction_count"`
}

// categoryMerchant matches the merchant-spend rows the reports API already
// returns, so the frontend renders them with the same row component the merchant
// explorer uses. merchant_key is the RESOLVED key, so every row links.
type categoryMerchant struct {
	Merchant         string          `json:"merchant"`
	MerchantKey      string          `json:"merchant_key"`
	Total            decimal.Decimal `json:"total"`
	TransactionCount int64           `json:"transaction_count"`
}

type categoryDetailTxn struct {
	ID         uuid.UUID       `json:"id"`
	Date       string          `json:"date"`
	Amount     decimal.Decimal `json:"amount"`
	Descriptor string          `json:"descriptor"`
	// Merchant and MerchantKey let a charge link to its merchant — the reverse of
	// the merchant page's category column. Key is empty when the descriptor carried
	// too little signal to key on, and the row then renders as plain text.
	Merchant    string `json:"merchant"`
	MerchantKey string `json:"merchant_key"`
	AccountName string `json:"account_name"`
}

// categoryDetailTxnLimit caps the transaction list, matching the merchant page.
// The summary above it is exact over every match, so a long history truncates in
// the list without the totals going wrong.
const categoryDetailTxnLimit = 500

// categoryTopMerchantLimit is how many merchants the "where it goes" list shows.
// Enough to see the shape of a category without turning the section into a second
// merchant explorer.
const categoryTopMerchantLimit = 15

// handleCategoryDetail reports one category over a period.
//
// Addressed by id as a path segment, which a category can afford and a merchant
// cannot: a category id is a UUID, whereas a raw merchant descriptor routinely
// contains a slash and has to travel as a query parameter.
func (s *Server) handleCategoryDetail(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	categoryID, err := uuid.Parse(chi.URLParam(r, "categoryID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid category id")
		return
	}
	from, to := period(r)

	category, err := s.Queries.GetCategoryByID(ctx, dbgen.GetCategoryByIDParams{
		ID:          categoryID,
		HouseholdID: &identity.HouseholdID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no such category")
		return
	}
	if err != nil {
		s.internalError(w, "category identity", err)
		return
	}

	summary, err := s.Queries.GetCategorySummary(ctx, dbgen.GetCategorySummaryParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
		CategoryID:  &categoryID,
	})
	if err != nil {
		s.internalError(w, "category summary", err)
		return
	}

	months, err := s.Queries.GetCategoryMonthlySpend(ctx, dbgen.GetCategoryMonthlySpendParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
		CategoryID:  &categoryID,
	})
	if err != nil {
		s.internalError(w, "category monthly spend", err)
		return
	}

	merchants, err := s.Queries.GetTopMerchantsInCategory(ctx, dbgen.GetTopMerchantsInCategoryParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
		CategoryID:  &categoryID,
		Lim:         categoryTopMerchantLimit,
	})
	if err != nil {
		s.internalError(w, "category top merchants", err)
		return
	}

	txns, err := s.Queries.ListCategoryTransactions(ctx, dbgen.ListCategoryTransactionsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
		CategoryID:  &categoryID,
		Lim:         categoryDetailTxnLimit,
	})
	if err != nil {
		s.internalError(w, "category transactions", err)
		return
	}

	out := categoryDetailResponse{
		CategoryID:       category.ID,
		Name:             category.Name,
		Slug:             category.Slug,
		Color:            category.Color,
		IsFixed:          category.IsFixed,
		IsSystem:         category.HouseholdID == nil,
		From:             from.Format(time.DateOnly),
		To:               to.Format(time.DateOnly),
		Total:            summary.Total,
		TransactionCount: summary.TransactionCount,
		Average:          summary.Average,
		Largest:          summary.Largest,
		FirstSeen:        emptyToNil(summary.FirstSeen),
		LastSeen:         emptyToNil(summary.LastSeen),
		Monthly:          make([]categoryMonthPoint, 0, len(months)),
		Merchants:        make([]categoryMerchant, 0, len(merchants)),
		Transactions:     make([]categoryDetailTxn, 0, len(txns)),
	}

	for _, m := range months {
		out.Monthly = append(out.Monthly, categoryMonthPoint{
			Month:            m.Month.Format(time.DateOnly),
			Total:            m.Total,
			TransactionCount: m.TransactionCount,
		})
	}
	for _, m := range merchants {
		out.Merchants = append(out.Merchants, categoryMerchant{
			Merchant:         m.Merchant,
			MerchantKey:      m.MerchantKey,
			Total:            m.Total,
			TransactionCount: m.TransactionCount,
		})
	}
	for _, t := range txns {
		out.Transactions = append(out.Transactions, categoryDetailTxn{
			ID:          t.ID,
			Date:        t.Date.Format(time.DateOnly),
			Amount:      t.Amount,
			Descriptor:  t.Descriptor,
			Merchant:    t.Merchant,
			MerchantKey: t.ResolvedMerchantKey,
			AccountName: t.AccountName,
		})
	}

	writeJSON(w, http.StatusOK, out)
}
