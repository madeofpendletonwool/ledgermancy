package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

type categoryResponse struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	Slug       string    `json:"slug"`
	Color      *string   `json:"color"`
	IsIncome   bool      `json:"is_income"`
	IsTransfer bool      `json:"is_transfer"`
	IsFixed    bool      `json:"is_fixed"`
	IsSystem   bool      `json:"is_system"`
}

func (s *Server) handleListCategories(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListCategories(r.Context(), &identity.HouseholdID)
	if err != nil {
		s.internalError(w, "list categories", err)
		return
	}

	out := make([]categoryResponse, 0, len(rows))
	for _, c := range rows {
		out = append(out, categoryResponse{
			ID:         c.ID,
			Name:       c.Name,
			Slug:       c.Slug,
			Color:      c.Color,
			IsIncome:   c.IsIncome,
			IsTransfer: c.IsTransfer,
			IsFixed:    c.IsFixed,
			IsSystem:   c.HouseholdID == nil,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type recategoriseRequest struct {
	CategoryID uuid.UUID `json:"category_id"`
	// ApplyToMerchant caches the choice so future transactions from the same
	// merchant are categorised the same way without asking again.
	ApplyToMerchant bool `json:"apply_to_merchant"`
}

// handleRecategoriseTransaction records a manual category choice.
//
// Manual choices are marked category_source = 'manual', which the sync upsert
// preserves — so Plaid can never overwrite a decision the user made.
func (s *Server) handleRecategoriseTransaction(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	transactionID, err := uuid.Parse(chi.URLParam(r, "transactionID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid transaction id")
		return
	}

	var req recategoriseRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// The UPDATE is scoped by household, so a caller cannot recategorise
	// someone else's transaction even with a valid id.
	updated, err := s.Queries.SetTransactionCategory(r.Context(), dbgen.SetTransactionCategoryParams{
		ID:          transactionID,
		HouseholdID: identity.HouseholdID,
		CategoryID:  &req.CategoryID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "transaction not found")
			return
		}
		s.internalError(w, "recategorise transaction", err)
		return
	}

	if req.ApplyToMerchant && updated.MerchantKey != nil && *updated.MerchantKey != "" {
		source := "manual"
		if err := s.Queries.UpsertMerchantCategory(r.Context(), dbgen.UpsertMerchantCategoryParams{
			HouseholdID: identity.HouseholdID,
			MerchantKey: *updated.MerchantKey,
			CategoryID:  req.CategoryID,
			Source:      source,
		}); err != nil {
			s.internalError(w, "cache merchant category", err)
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":              updated.ID,
		"category_id":     updated.CategoryID,
		"category_source": updated.CategorySource,
	})
}

type budgetResponse struct {
	BudgetID   uuid.UUID       `json:"budget_id"`
	CategoryID uuid.UUID       `json:"category_id"`
	Name       string          `json:"name"`
	Slug       string          `json:"slug"`
	Color      *string         `json:"color"`
	Budgeted   decimal.Decimal `json:"budgeted"`
	Spent      decimal.Decimal `json:"spent"`
	Remaining  decimal.Decimal `json:"remaining"`
}

func (s *Server) handleBudgetProgress(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	from, to := period(r)

	rows, err := s.Queries.GetBudgetProgress(r.Context(), dbgen.GetBudgetProgressParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
	})
	if err != nil {
		s.internalError(w, "budget progress", err)
		return
	}

	out := make([]budgetResponse, 0, len(rows))
	for _, b := range rows {
		out = append(out, budgetResponse{
			BudgetID:   b.BudgetID,
			CategoryID: b.CategoryID,
			Name:       b.CategoryName,
			Slug:       b.CategorySlug,
			Color:      b.CategoryColor,
			Budgeted:   b.Budgeted,
			Spent:      b.Spent,
			Remaining:  b.Budgeted.Sub(b.Spent),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type createBudgetRequest struct {
	CategoryID uuid.UUID `json:"category_id"`
	Amount     string    `json:"amount"`
}

func (s *Server) handleCreateBudget(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req createBudgetRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Amounts arrive as strings so they never pass through a JSON float.
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil {
		writeError(w, http.StatusBadRequest, "amount must be a decimal number, e.g. \"450.00\"")
		return
	}
	if !amount.IsPositive() {
		writeError(w, http.StatusBadRequest, "amount must be greater than zero")
		return
	}

	budget, err := s.Queries.UpsertBudget(r.Context(), dbgen.UpsertBudgetParams{
		HouseholdID: identity.HouseholdID,
		CategoryID:  req.CategoryID,
		Amount:      amount,
	})
	if err != nil {
		s.internalError(w, "create budget", err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          budget.ID,
		"category_id": budget.CategoryID,
		"amount":      budget.Amount,
	})
}

func (s *Server) handleDeleteBudget(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	budgetID, err := uuid.Parse(chi.URLParam(r, "budgetID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid budget id")
		return
	}

	if err := s.Queries.DeleteBudget(r.Context(), dbgen.DeleteBudgetParams{
		ID: budgetID, HouseholdID: identity.HouseholdID,
	}); err != nil {
		s.internalError(w, "delete budget", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
