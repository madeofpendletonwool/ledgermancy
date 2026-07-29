package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Allowance: a schedule a parent sets, and a ledger a child watches.
//
// SIGN CONVENTION: an entry amount is POSITIVE for money INTO the child's
// balance and NEGATIVE for money out — the OPPOSITE of transactions.amount.
// The API speaks the same convention as the column so there is exactly one
// place to get it wrong. See the migration comment for why it is inverted.
//
// Nothing here touches `transactions`. These rows are not transactions, there
// is no account behind them, and they never reach household spending totals.

// allowanceEntryKinds mirrors the CHECK constraint, so a bad kind is a readable
// 400 rather than a 500 from a constraint violation.
var allowanceEntryKinds = map[string]bool{
	"allowance": true, "chore": true, "gift": true, "spend": true, "correction": true,
}

// creditKinds are the entries only an adult may record. A child records what
// they spent; a child who could record what they were given would be recording
// their own pocket money into existence.
var creditKinds = map[string]bool{
	"allowance": true, "chore": true, "gift": true, "correction": true,
}

type allowanceResponse struct {
	PersonID uuid.UUID `json:"person_id"`
	// Amount and Cadence are both nil or both set — an amount without a cadence
	// is not a schedule.
	Amount       *string `json:"amount"`
	Cadence      *string `json:"cadence"`
	MonthlyLimit *string `json:"monthly_limit"`
	AutoPost     bool    `json:"auto_post"`
	// Balance and SpentThisMonth are DERIVED by summing the ledger. There is no
	// stored balance, for the same reason goals store no current_amount.
	Balance        string `json:"balance"`
	SpentThisMonth string `json:"spent_this_month"`
	// LimitRemaining is nil when no limit is set — which is different from a
	// remaining balance of zero, and the UI must show it differently.
	LimitRemaining *string `json:"limit_remaining"`
}

type allowanceEntryResponse struct {
	ID         uuid.UUID `json:"id"`
	PersonID   uuid.UUID `json:"person_id"`
	Kind       string    `json:"kind"`
	Amount     string    `json:"amount"`
	OccurredOn string    `json:"occurred_on"`
	Note       *string   `json:"note"`
	CreatedAt  string    `json:"created_at"`
}

func entryResponse(e dbgen.AllowanceEntry) allowanceEntryResponse {
	return allowanceEntryResponse{
		ID:         e.ID,
		PersonID:   e.PersonID,
		Kind:       e.Kind,
		Amount:     e.Amount.StringFixed(2),
		OccurredOn: e.OccurredOn.Format(time.DateOnly),
		Note:       e.Note,
		CreatedAt:  e.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// monthBounds returns the half-open [start, end) of the month containing now,
// which is what the monthly-limit query wants.
func monthBounds(now time.Time) (time.Time, time.Time) {
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// buildAllowance assembles the schedule plus its derived figures for one
// person. A person with no allowance row is not an error: they simply have a
// ledger and no schedule, which is a normal way to use this.
func (s *Server) buildAllowance(r *http.Request, personID, householdID uuid.UUID,
) (allowanceResponse, error) {
	ctx := r.Context()
	now := time.Now()
	monthStart, monthEnd := monthBounds(now)

	resp := allowanceResponse{PersonID: personID}

	a, err := s.Queries.GetAllowance(ctx, dbgen.GetAllowanceParams{
		PersonID: personID, HouseholdID: householdID,
	})
	switch {
	case err == nil:
		if a.Amount.Valid {
			v := a.Amount.Decimal.StringFixed(2)
			resp.Amount = &v
		}
		resp.Cadence = a.Cadence
		if a.MonthlyLimit.Valid {
			v := a.MonthlyLimit.Decimal.StringFixed(2)
			resp.MonthlyLimit = &v
		}
		resp.AutoPost = a.AutoPost
	case errors.Is(err, pgx.ErrNoRows):
		// No schedule configured. Leave the nils.
	default:
		return resp, err
	}

	balance, err := s.Queries.GetAllowanceBalance(ctx, dbgen.GetAllowanceBalanceParams{
		PersonID: personID, HouseholdID: householdID,
	})
	if err != nil {
		return resp, err
	}
	resp.Balance = balance.StringFixed(2)

	spent, err := s.Queries.GetAllowanceSpentInMonth(ctx, dbgen.GetAllowanceSpentInMonthParams{
		PersonID: personID, HouseholdID: householdID,
		MonthStart: monthStart, MonthEnd: monthEnd,
	})
	if err != nil {
		return resp, err
	}
	resp.SpentThisMonth = spent.StringFixed(2)

	if a.MonthlyLimit.Valid {
		remaining := a.MonthlyLimit.Decimal.Sub(spent).StringFixed(2)
		resp.LimitRemaining = &remaining
	}
	return resp, nil
}

// requirePersonInHousehold resolves a person id from the URL and confirms it
// belongs to the caller's household. Returns false having already written the
// response when it does not.
func (s *Server) requirePersonInHousehold(w http.ResponseWriter, r *http.Request,
	identity auth.Identity,
) (uuid.UUID, bool) {
	personID, err := uuid.Parse(chi.URLParam(r, "personID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid person id")
		return uuid.Nil, false
	}
	if _, err := s.Queries.GetPerson(r.Context(), dbgen.GetPersonParams{
		ID: personID, HouseholdID: identity.HouseholdID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "person not found")
			return uuid.Nil, false
		}
		s.internalError(w, "get person", err)
		return uuid.Nil, false
	}
	return personID, true
}

func (s *Server) handleGetAllowance(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	personID, ok := s.requirePersonInHousehold(w, r, identity)
	if !ok {
		return
	}

	resp, err := s.buildAllowance(r, personID, identity.HouseholdID)
	if err != nil {
		s.internalError(w, "build allowance", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type upsertAllowanceRequest struct {
	Amount       *string `json:"amount"`
	Cadence      *string `json:"cadence"`
	MonthlyLimit *string `json:"monthly_limit"`
	AutoPost     bool    `json:"auto_post"`
}

func (s *Server) handleUpsertAllowance(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	personID, ok := s.requirePersonInHousehold(w, r, identity)
	if !ok {
		return
	}

	var req upsertAllowanceRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	amount, err := optionalMoney(req.Amount, "amount")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, err := optionalMoney(req.MonthlyLimit, "monthly_limit")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.Cadence != nil {
		switch *req.Cadence {
		case "weekly", "biweekly", "monthly":
		default:
			writeError(w, http.StatusBadRequest,
				"cadence must be one of: weekly, biweekly, monthly")
			return
		}
	}
	// Mirrors the allowances_amount_needs_cadence constraint, so the caller
	// gets a readable 400 instead of a constraint violation.
	if amount.Valid != (req.Cadence != nil) {
		writeError(w, http.StatusBadRequest,
			"amount and cadence must be set together, or both omitted")
		return
	}
	if req.AutoPost && !amount.Valid {
		writeError(w, http.StatusBadRequest,
			"auto_post needs an amount and a cadence to post")
		return
	}

	if _, err := s.Queries.UpsertAllowance(r.Context(), dbgen.UpsertAllowanceParams{
		PersonID:     personID,
		HouseholdID:  identity.HouseholdID,
		Amount:       amount,
		Cadence:      req.Cadence,
		MonthlyLimit: limit,
		AutoPost:     req.AutoPost,
	}); err != nil {
		s.internalError(w, "upsert allowance", err)
		return
	}

	resp, err := s.buildAllowance(r, personID, identity.HouseholdID)
	if err != nil {
		s.internalError(w, "build allowance", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListAllowanceEntries(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	personID, ok := s.requirePersonInHousehold(w, r, identity)
	if !ok {
		return
	}
	s.writeAllowanceEntries(w, r, personID, identity.HouseholdID)
}

const allowanceEntryLimit = 200

func (s *Server) writeAllowanceEntries(w http.ResponseWriter, r *http.Request,
	personID, householdID uuid.UUID,
) {
	rows, err := s.Queries.ListAllowanceEntries(r.Context(), dbgen.ListAllowanceEntriesParams{
		PersonID: personID, HouseholdID: householdID, Limit: allowanceEntryLimit,
	})
	if err != nil {
		s.internalError(w, "list allowance entries", err)
		return
	}
	out := make([]allowanceEntryResponse, 0, len(rows))
	for _, e := range rows {
		out = append(out, entryResponse(e))
	}
	writeJSON(w, http.StatusOK, out)
}

type createEntryRequest struct {
	Kind string `json:"kind"`
	// Amount is always POSITIVE in the request. The sign is derived from the
	// kind, so a client cannot accidentally credit a spend by sending a
	// negative number, and 'correction' is the one kind that may be signed.
	Amount     string  `json:"amount"`
	OccurredOn *string `json:"occurred_on"`
	Note       *string `json:"note"`
}

func (s *Server) handleCreateAllowanceEntry(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	personID, ok := s.requirePersonInHousehold(w, r, identity)
	if !ok {
		return
	}
	s.createAllowanceEntry(w, r, identity, personID)
}

func (s *Server) createAllowanceEntry(w http.ResponseWriter, r *http.Request,
	identity auth.Identity, personID uuid.UUID,
) {
	var req createEntryRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if !allowanceEntryKinds[req.Kind] {
		writeError(w, http.StatusBadRequest,
			"kind must be one of: allowance, chore, gift, spend, correction")
		return
	}
	// A child may only record spending. Credits are a parent's action.
	if !identity.IsAdult() && creditKinds[req.Kind] {
		writeError(w, http.StatusForbidden,
			"only an adult can record allowance, chores, gifts or corrections")
		return
	}

	amount, err := decimal.NewFromString(strings.TrimSpace(req.Amount))
	if err != nil {
		writeError(w, http.StatusBadRequest, "amount must be a decimal amount")
		return
	}
	if amount.IsZero() {
		writeError(w, http.StatusBadRequest, "amount cannot be zero")
		return
	}

	// Derive the sign from the kind rather than trusting the request. A
	// 'correction' is the one kind allowed to be negative, because undoing a
	// mistaken credit is exactly what it is for.
	switch req.Kind {
	case "spend":
		amount = amount.Abs().Neg()
	case "correction":
		// keep the caller's sign
	default:
		amount = amount.Abs()
	}

	occurredOn := time.Now()
	if req.OccurredOn != nil && strings.TrimSpace(*req.OccurredOn) != "" {
		occurredOn, err = time.Parse(time.DateOnly, strings.TrimSpace(*req.OccurredOn))
		if err != nil {
			writeError(w, http.StatusBadRequest, "occurred_on must be YYYY-MM-DD")
			return
		}
	}

	createdBy := identity.UserID
	e, err := s.Queries.CreateAllowanceEntry(r.Context(), dbgen.CreateAllowanceEntryParams{
		PersonID:    personID,
		HouseholdID: identity.HouseholdID,
		Kind:        req.Kind,
		Amount:      amount,
		OccurredOn:  occurredOn,
		Note:        req.Note,
		CreatedBy:   &createdBy,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "person not found")
		return
	}
	if err != nil {
		s.internalError(w, "create allowance entry", err)
		return
	}
	writeJSON(w, http.StatusCreated, entryResponse(e))
}

func (s *Server) handleDeleteAllowanceEntry(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	entryID, err := uuid.Parse(chi.URLParam(r, "entryID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid entry id")
		return
	}

	rows, err := s.Queries.DeleteAllowanceEntry(r.Context(), dbgen.DeleteAllowanceEntryParams{
		ID: entryID, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "delete allowance entry", err)
		return
	}
	if rows == 0 {
		writeError(w, http.StatusNotFound, "entry not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --------------------------------------------------------------------------
// The child's own view
// --------------------------------------------------------------------------

// myPersonID resolves the caller's own person row. Every /me handler needs it,
// and a login without one is a 404 rather than a panic — the LEFT JOIN in
// GetSessionUser deliberately tolerates that state.
func (s *Server) myPersonID(w http.ResponseWriter, identity auth.Identity) (uuid.UUID, bool) {
	if identity.PersonID == nil {
		writeError(w, http.StatusNotFound, "no person record for this login")
		return uuid.Nil, false
	}
	return *identity.PersonID, true
}

func (s *Server) handleGetMyAllowance(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	personID, ok := s.myPersonID(w, identity)
	if !ok {
		return
	}

	resp, err := s.buildAllowance(r, personID, identity.HouseholdID)
	if err != nil {
		s.internalError(w, "build allowance", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListMyAllowanceEntries(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	personID, ok := s.myPersonID(w, identity)
	if !ok {
		return
	}
	s.writeAllowanceEntries(w, r, personID, identity.HouseholdID)
}

// handleCreateMyAllowanceEntry lets a child record their own spending. The
// kind check in createAllowanceEntry refuses every credit kind for a non-adult,
// so this is a spend-only endpoint for the account it exists for.
func (s *Server) handleCreateMyAllowanceEntry(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	personID, ok := s.myPersonID(w, identity)
	if !ok {
		return
	}
	s.createAllowanceEntry(w, r, identity, personID)
}
