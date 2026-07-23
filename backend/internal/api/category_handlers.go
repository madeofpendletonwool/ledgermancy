package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/ai"
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

// --------------------------------------------------------------------------
// Budget suggestions
//
// A household with a blank budget grid gets a sensible starting proposal for
// every spending category, anchored on the category's SQL-computed average. The
// average is exact SQL; only the *rounding* to a memorable target and the
// one-line rationale are the AI's job — and even those are re-validated here
// against a band + roundness guardrail, with deterministic rounding as both the
// fallback (AI off) and the substitute (a rejected AI figure). The average shown
// is ALWAYS the SQL figure, never the model's echo, so a hallucinated number can
// never reach the write API. Approval is a loop of the existing single-write
// POST /api/budgets, so validation and audit stay identical.
// --------------------------------------------------------------------------

// suggestFloor drops categories whose average monthly spend is too small to be
// worth a budget.
var suggestFloor = decimal.NewFromInt(10)

// suggestMaxCategories bounds the AI batch (and the review panel) to the biggest
// spenders. GetCategoryAverages already orders by total DESC.
const suggestMaxCategories = 20

// Guardrail band: an AI target is accepted only within this multiple of the
// average, so a rewording never drifts the number materially. Below/above the
// band, or a non-round value, falls back to deterministic rounding.
var (
	suggestBandLow  = decimal.NewFromFloat(0.8)
	suggestBandHigh = decimal.NewFromFloat(1.3)
)

type budgetProposal struct {
	CategoryID      uuid.UUID `json:"category_id"`
	CategoryName    string    `json:"category_name"`
	Slug            string    `json:"slug"`
	IsFixed         bool      `json:"is_fixed"`
	ComputedAverage string    `json:"computed_average"`
	SuggestedAmount string    `json:"suggested_amount"`
	Rationale       string    `json:"rationale"`
	AlreadyBudgeted bool      `json:"already_budgeted"`
	CurrentBudget   string    `json:"current_budget,omitempty"`
}

type suggestBudgetsResponse struct {
	PeriodMonths int              `json:"period_months"`
	AITailored   bool             `json:"ai_tailored"`
	Proposals    []budgetProposal `json:"proposals"`
}

func (s *Server) handleSuggestBudgets(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	// Trailing twelve months, matching handleCategoryAverages exactly.
	now := time.Now()
	averages, err := s.Queries.GetCategoryAverages(ctx, dbgen.GetCategoryAveragesParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        now.AddDate(-1, 0, 0),
		Date_2:      now,
	})
	if err != nil {
		s.internalError(w, "category averages for suggestions", err)
		return
	}

	// Existing budgets for the current month, so a proposal can say "raise from
	// $X" rather than proposing blind.
	from, to := period(r)
	budgets, err := s.Queries.GetBudgetProgress(ctx, dbgen.GetBudgetProgressParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID, Date: from, Date_2: to,
	})
	if err != nil {
		s.internalError(w, "budget progress for suggestions", err)
		return
	}
	current := make(map[uuid.UUID]decimal.Decimal, len(budgets))
	for _, b := range budgets {
		current[b.CategoryID] = b.Budgeted
	}

	// Deterministic filter: skip trivially small categories, cap the batch.
	// (income/transfer are already excluded by GetCategoryAverages.)
	type kept struct {
		row dbgen.GetCategoryAveragesRow
		avg decimal.Decimal
	}
	var keep []kept
	for _, a := range averages {
		avg := a.MonthlyAverage.Round(2)
		if avg.LessThan(suggestFloor) {
			continue
		}
		keep = append(keep, kept{row: a, avg: avg})
		if len(keep) >= suggestMaxCategories {
			break
		}
	}

	// Ask the model for round targets + rationale in one batch, keyed by slug.
	// Any failure (disabled, timeout, malformed) leaves suggestions nil and the
	// loop below uses deterministic rounding + a template rationale.
	var suggestions map[string]ai.BudgetSuggestion
	aiTailored := false
	if s.AI.Enabled() && len(keep) > 0 {
		inputs := make([]ai.BudgetSuggestionInput, 0, len(keep))
		for _, k := range keep {
			inputs = append(inputs, ai.BudgetSuggestionInput{
				Name:           k.row.CategoryName,
				Slug:           k.row.CategorySlug,
				MonthlyAverage: k.avg.StringFixed(2),
				IsFixed:        k.row.IsFixed,
			})
		}
		if got, err := s.AI.SuggestBudgets(ctx, inputs); err != nil {
			slog.Debug("budget suggestions fell back to deterministic", "error", err)
		} else {
			suggestions = got
			aiTailored = true
		}
	}

	proposals := make([]budgetProposal, 0, len(keep))
	for _, k := range keep {
		target := roundBudgetTarget(k.avg)
		rationale := templateRationale(k.row.CategoryName, k.avg, target)

		// Take the AI target only if it survives the band + roundness guardrail;
		// otherwise keep the deterministic figure. The rationale is never a source
		// of truth, so it is taken as-is when present.
		if sug, ok := suggestions[k.row.CategorySlug]; ok {
			if t, err := decimal.NewFromString(sug.Target); err == nil && acceptableTarget(t, k.avg) {
				target = t.Round(2)
			}
			if r := trimRationale(sug.Rationale); r != "" {
				rationale = r
			}
		}

		p := budgetProposal{
			CategoryID:      k.row.CategoryID,
			CategoryName:    k.row.CategoryName,
			Slug:            k.row.CategorySlug,
			IsFixed:         k.row.IsFixed,
			ComputedAverage: k.avg.StringFixed(2), // ALWAYS the SQL figure
			SuggestedAmount: target.StringFixed(2),
			Rationale:       rationale,
		}
		if cur, ok := current[k.row.CategoryID]; ok {
			p.AlreadyBudgeted = true
			p.CurrentBudget = cur.StringFixed(2)
		}
		proposals = append(proposals, p)
	}

	writeJSON(w, http.StatusOK, suggestBudgetsResponse{
		PeriodMonths: 12,
		AITailored:   aiTailored,
		Proposals:    proposals,
	})
}

// budgetStep is the rounding granularity for a target of a given magnitude —
// bigger budgets round to coarser steps so every target stays memorable.
func budgetStep(d decimal.Decimal) decimal.Decimal {
	switch {
	case d.LessThan(decimal.NewFromInt(200)):
		return decimal.NewFromInt(10)
	case d.LessThan(decimal.NewFromInt(500)):
		return decimal.NewFromInt(25)
	case d.LessThan(decimal.NewFromInt(1000)):
		return decimal.NewFromInt(50)
	default:
		return decimal.NewFromInt(100)
	}
}

// roundBudgetTarget rounds an average UP to the nearest step for its magnitude,
// so a budget is never set below the historical average by construction. This is
// the deterministic fallback and the guardrail's anchor.
func roundBudgetTarget(avg decimal.Decimal) decimal.Decimal {
	step := budgetStep(avg)
	// ceil(avg/step) * step
	return avg.Div(step).Ceil().Mul(step)
}

// acceptableTarget gates an AI-proposed target: it must be positive, within the
// band around the average, and round for its magnitude. Anything else is
// discarded in favour of deterministic rounding.
func acceptableTarget(target, avg decimal.Decimal) bool {
	if !target.IsPositive() {
		return false
	}
	if target.LessThan(avg.Mul(suggestBandLow)) || target.GreaterThan(avg.Mul(suggestBandHigh)) {
		return false
	}
	step := budgetStep(target)
	return target.Mod(step).IsZero()
}

// templateRationale is the AI-off (and guardrail-substitute) reason line.
func templateRationale(name string, avg, target decimal.Decimal) string {
	return "You've averaged $" + avg.StringFixed(2) + "/mo on " + name +
		" — $" + target.StringFixed(2) + " is a round target just above that."
}

// trimRationale bounds a model rationale so a runaway response cannot bloat the
// payload, and blanks an empty one so the template stands.
func trimRationale(s string) string {
	s = strings.TrimSpace(s)
	const max = 240
	if len(s) > max {
		return s[:max]
	}
	return s
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
