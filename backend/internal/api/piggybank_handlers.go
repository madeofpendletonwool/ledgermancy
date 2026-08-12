package api

import (
	"context"
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

// Piggy banks: lightweight, period-independent savings envelopes that sit on an
// asset account and track a running balance via an append-only event log.
//
// THE DISTINCTION THIS FILE MUST NOT BLUR. A piggy bank is an ANNOTATION on part
// of an account balance, not a separate balance. The money is already in the
// account (it arrived via a real transaction); a deposit/withdraw here only
// earmarks a slice of it. Net worth never moves when a piggy bank does, and the
// available-for-piggy figure (account balance − sum of piggy banks on it) is
// what stops a household earmarking the same dollars twice.
//
// current_amount is DERIVED from the event log (deposits − withdrawals, summed
// in SQL), never stored — see 00061_piggy_banks.sql. A stored running balance
// would be a second source of truth that drifts from the events the moment a
// code path forgets to update it; the event log is the only authority.

// piggyBankResponse is one piggy bank plus its DERIVED current balance. Money is
// always fixed-2 strings; target_amount is nil for an open-ended jar.
type piggyBankResponse struct {
	ID            string  `json:"id"`
	AccountID     string  `json:"account_id"`
	Name          string  `json:"name"`
	TargetAmount  *string `json:"target_amount"`
	CurrentAmount string  `json:"current_amount"`
	CreatedAt     string  `json:"created_at"`
}

// piggyResponse renders the shared fields. current is the SQL-derived balance
// the caller already holds, so create/update/deposit/withdraw all pass through
// one shape and none can invent a figure.
func piggyResponse(id, accountID uuid.UUID, name string, target decimal.NullDecimal,
	createdAt time.Time, current decimal.Decimal) piggyBankResponse {
	resp := piggyBankResponse{
		ID:            id.String(),
		AccountID:     accountID.String(),
		Name:          name,
		CurrentAmount: current.StringFixed(2),
		CreatedAt:     createdAt.UTC().Format(time.RFC3339),
	}
	if target.Valid {
		s := target.Decimal.StringFixed(2)
		resp.TargetAmount = &s
	}
	return resp
}

// piggyEventResponse is one row of the append-only event log.
type piggyEventResponse struct {
	ID            string  `json:"id"`
	Type          string  `json:"type"`
	Amount        string  `json:"amount"`
	TransactionID *string `json:"transaction_id"`
	CreatedAt     string  `json:"created_at"`
}

// accountAllocation carries the funding account's unassigned balance. It is the
// shape both the deposit/withdraw responses and the available-for-piggy endpoint
// return, because the warning it carries ("you've earmarked more than this
// account holds") is the same answer in both places.
type accountAllocation struct {
	AccountID      string `json:"account_id"`
	AccountBalance string `json:"account_balance"`
	Assigned       string `json:"assigned"`
	Available      string `json:"available"`
	OverAllocated  bool   `json:"over_allocated"`
}

// piggyTransactionResponse is the result of a deposit or withdraw: the event
// that was appended, the piggy bank's new standing, and the funding account's
// allocation afterward so the UI can warn about over-allocation immediately.
type piggyTransactionResponse struct {
	Event      piggyEventResponse `json:"event"`
	PiggyBank  piggyBankResponse  `json:"piggy_bank"`
	Allocation accountAllocation  `json:"allocation"`
}

func (s *Server) handleListPiggyBanks(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	rows, err := s.Queries.ListPiggyBanks(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "list piggy banks", err)
		return
	}
	out := make([]piggyBankResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, piggyResponse(
			row.ID, row.AccountID, row.Name, row.TargetAmount, row.CreatedAt, row.CurrentAmount,
		))
	}
	writeJSON(w, http.StatusOK, out)
}

type upsertPiggyBankRequest struct {
	Name         string `json:"name"`
	AccountID    string `json:"account_id"`
	TargetAmount string `json:"target_amount"`
}

// parsePiggyAmount parses a non-negative decimal amount string. An empty string
// means "not provided" (a nullable target on create, an unchanged target on
// update) and returns Valid=false; any other value must parse and be positive.
func parsePiggyAmount(raw string) (decimal.NullDecimal, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return decimal.NullDecimal{}, nil
	}
	d, err := decimal.NewFromString(raw)
	if err != nil {
		return decimal.NullDecimal{}, errors.New("amount must be a decimal number, e.g. \"1000.00\"")
	}
	if !d.IsPositive() {
		return decimal.NullDecimal{}, errors.New("amount must be greater than zero")
	}
	return decimal.NullDecimal{Decimal: d, Valid: true}, nil
}

func (s *Server) handleCreatePiggyBank(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req upsertPiggyBankRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	accountID, err := uuid.Parse(strings.TrimSpace(req.AccountID))
	if err != nil {
		writeError(w, http.StatusBadRequest, "account_id is required and must be a valid id")
		return
	}
	target, err := parsePiggyAmount(req.TargetAmount)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate the funding account through the same household-scoped,
	// asset-only read the available-for-piggy endpoint uses. ErrNoRows covers
	// three cases at once — account doesn't exist, belongs to another
	// household, or is a debt account — because a piggy bank must sit on money
	// held, not money owed, and the household must own it.
	if _, err := s.Queries.GetAccountAvailableForPiggy(r.Context(),
		dbgen.GetAccountAvailableForPiggyParams{ID: accountID, HouseholdID: identity.HouseholdID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusBadRequest,
				"pick a savings or checking account — piggy banks sit on accounts that hold money, not debts")
			return
		}
		s.internalError(w, "validate piggy bank account", err)
		return
	}

	pb, err := s.Queries.CreatePiggyBank(r.Context(), dbgen.CreatePiggyBankParams{
		HouseholdID:  identity.HouseholdID,
		AccountID:    accountID,
		Name:         strings.TrimSpace(req.Name),
		TargetAmount: target,
	})
	if err != nil {
		s.internalError(w, "create piggy bank", err)
		return
	}

	// Re-read so the response carries the derived current balance (0.00 here)
	// through the same path every other single-piggy response uses.
	created, err := s.Queries.GetPiggyBank(r.Context(), dbgen.GetPiggyBankParams{
		ID: pb.ID, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "read created piggy bank", err)
		return
	}
	writeJSON(w, http.StatusCreated, piggyResponse(
		created.ID, created.AccountID, created.Name, created.TargetAmount, created.CreatedAt, created.CurrentAmount,
	))
}

func (s *Server) handleUpdatePiggyBank(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "piggyBankID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid piggy bank id")
		return
	}
	var req upsertPiggyBankRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	target, err := parsePiggyAmount(req.TargetAmount)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 404 on a missing or other-household piggy bank before the write, so the
	// UPDATE's own household guard produces a clean not-found rather than a
	// silent no-op that the client reads as success.
	if _, err := s.Queries.GetPiggyBank(r.Context(), dbgen.GetPiggyBankParams{
		ID: id, HouseholdID: identity.HouseholdID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "piggy bank not found")
			return
		}
		s.internalError(w, "get piggy bank for update", err)
		return
	}

	pb, err := s.Queries.UpdatePiggyBank(r.Context(), dbgen.UpdatePiggyBankParams{
		ID:           id,
		HouseholdID:  identity.HouseholdID,
		Name:         strings.TrimSpace(req.Name),
		TargetAmount: target,
	})
	if err != nil {
		s.internalError(w, "update piggy bank", err)
		return
	}
	updated, err := s.Queries.GetPiggyBank(r.Context(), dbgen.GetPiggyBankParams{
		ID: pb.ID, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "read updated piggy bank", err)
		return
	}
	writeJSON(w, http.StatusOK, piggyResponse(
		updated.ID, updated.AccountID, updated.Name, updated.TargetAmount, updated.CreatedAt, updated.CurrentAmount,
	))
}

func (s *Server) handleDeletePiggyBank(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "piggyBankID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid piggy bank id")
		return
	}
	rows, err := s.Queries.DeletePiggyBank(r.Context(),
		dbgen.DeletePiggyBankParams{ID: id, HouseholdID: identity.HouseholdID})
	if err != nil {
		s.internalError(w, "delete piggy bank", err)
		return
	}
	if rows == 0 {
		writeError(w, http.StatusNotFound, "piggy bank not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// requirePiggyBank resolves a piggy bank id from the URL under the caller's
// household, returning the row with its derived current balance.
func (s *Server) requirePiggyBank(w http.ResponseWriter, r *http.Request,
	identity auth.Identity) (dbgen.GetPiggyBankRow, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "piggyBankID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid piggy bank id")
		return dbgen.GetPiggyBankRow{}, false
	}
	row, err := s.Queries.GetPiggyBank(r.Context(), dbgen.GetPiggyBankParams{
		ID: id, HouseholdID: identity.HouseholdID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "piggy bank not found")
		return dbgen.GetPiggyBankRow{}, false
	}
	if err != nil {
		s.internalError(w, "get piggy bank", err)
		return dbgen.GetPiggyBankRow{}, false
	}
	return row, true
}

type piggyMovementRequest struct {
	Amount        string  `json:"amount"`
	TransactionID *string `json:"transaction_id"`
}

// movePiggyBank is the shared deposit/withdraw body: append an event inside a
// transaction with the balance check, then return the new standing plus the
// funding account's allocation. eventType is "deposit" or "withdraw".
func (s *Server) movePiggyBank(w http.ResponseWriter, r *http.Request, eventType string) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	pb, ok := s.requirePiggyBank(w, r, identity)
	if !ok {
		return
	}

	var req piggyMovementRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	amount, err := decimal.NewFromString(strings.TrimSpace(req.Amount))
	if err != nil {
		writeError(w, http.StatusBadRequest, "amount must be a decimal amount")
		return
	}
	if !amount.IsPositive() {
		writeError(w, http.StatusBadRequest, "amount must be greater than zero")
		return
	}

	// A withdrawal can't take the jar negative — there is nothing to withdraw
	// from. This is a hard error, not a warning: unlike over-allocation across
	// jars (which is the user's call to make and the UI warns about), a single
	// jar's balance is an exact, knowable number.
	if eventType == "withdraw" && amount.GreaterThan(pb.CurrentAmount) {
		writeError(w, http.StatusBadRequest,
			"can't withdraw "+amount.StringFixed(2)+" — this piggy bank holds "+pb.CurrentAmount.StringFixed(2))
		return
	}

	// transaction_id is an optional annotation (a real transfer this event
	// corresponds to). It is not validated against the household here: the
	// event row carries only the piggy bank, which is already household-scoped,
	// and a stale or foreign transaction id is a dangling annotation rather
	// than a leak — the FK SET NULL on delete is the safety net.
	var txID *uuid.UUID
	if req.TransactionID != nil {
		s := strings.TrimSpace(*req.TransactionID)
		if s != "" {
			id, err := uuid.Parse(s)
			if err != nil {
				writeError(w, http.StatusBadRequest, "transaction_id must be a valid id")
				return
			}
			txID = &id
		}
	}

	// Balance check and event insert land in one transaction so two concurrent
	// withdrawals can't both pass the guard and leave the jar negative.
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		s.internalError(w, "begin piggy bank movement", err)
		return
	}
	defer tx.Rollback(ctx)

	qtx := s.Queries.WithTx(tx)
	event, err := qtx.CreatePiggyBankEvent(ctx, dbgen.CreatePiggyBankEventParams{
		PiggyBankID: pb.ID, Type: eventType, Amount: amount, TransactionID: txID,
	})
	if err != nil {
		s.internalError(w, "append piggy bank event", err)
		return
	}
	updated, err := qtx.GetPiggyBank(ctx, dbgen.GetPiggyBankParams{
		ID: pb.ID, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "read piggy bank after event", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.internalError(w, "commit piggy bank movement", err)
		return
	}

	alloc, err := s.accountAllocationFor(r.Context(), pb.AccountID, identity.HouseholdID)
	if err != nil {
		s.internalError(w, "read account allocation", err)
		return
	}

	var txIDStr *string
	if event.TransactionID != nil {
		s := event.TransactionID.String()
		txIDStr = &s
	}
	writeJSON(w, http.StatusCreated, piggyTransactionResponse{
		Event: piggyEventResponse{
			ID:            event.ID.String(),
			Type:          event.Type,
			Amount:        event.Amount.StringFixed(2),
			TransactionID: txIDStr,
			CreatedAt:     event.CreatedAt.UTC().Format(time.RFC3339),
		},
		PiggyBank: piggyResponse(
			updated.ID, updated.AccountID, updated.Name, updated.TargetAmount, updated.CreatedAt, updated.CurrentAmount,
		),
		Allocation: alloc,
	})
}

func (s *Server) handleDepositPiggyBank(w http.ResponseWriter, r *http.Request) {
	s.movePiggyBank(w, r, "deposit")
}

func (s *Server) handleWithdrawPiggyBank(w http.ResponseWriter, r *http.Request) {
	s.movePiggyBank(w, r, "withdraw")
}

func (s *Server) handleListPiggyBankEvents(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	pb, ok := s.requirePiggyBank(w, r, identity)
	if !ok {
		return
	}
	events, err := s.Queries.ListPiggyBankEvents(r.Context(), dbgen.ListPiggyBankEventsParams{
		PiggyBankID: pb.ID, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "list piggy bank events", err)
		return
	}
	out := make([]piggyEventResponse, 0, len(events))
	for _, e := range events {
		var txID *string
		if e.TransactionID != nil {
			s := e.TransactionID.String()
			txID = &s
		}
		out = append(out, piggyEventResponse{
			ID:            e.ID.String(),
			Type:          e.Type,
			Amount:        e.Amount.StringFixed(2),
			TransactionID: txID,
			CreatedAt:     e.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListAccountPiggyBanks(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	accountID, err := uuid.Parse(chi.URLParam(r, "accountID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid account id")
		return
	}
	rows, err := s.Queries.ListPiggyBanksForAccount(r.Context(), dbgen.ListPiggyBanksForAccountParams{
		HouseholdID: identity.HouseholdID, AccountID: accountID,
	})
	if err != nil {
		s.internalError(w, "list account piggy banks", err)
		return
	}
	out := make([]piggyBankResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, piggyResponse(
			row.ID, row.AccountID, row.Name, row.TargetAmount, row.CreatedAt, row.CurrentAmount,
		))
	}
	writeJSON(w, http.StatusOK, out)
}

// accountAllocationFor reads the funding account's derived allocation. It is
// separate from the handler so deposit/withdraw reuse it after a movement.
func (s *Server) accountAllocationFor(ctx context.Context, accountID uuid.UUID,
	householdID uuid.UUID) (accountAllocation, error) {
	row, err := s.Queries.GetAccountAvailableForPiggy(ctx, dbgen.GetAccountAvailableForPiggyParams{
		ID: accountID, HouseholdID: householdID,
	})
	if err != nil {
		return accountAllocation{}, err
	}
	return accountAllocation{
		AccountID:      accountID.String(),
		AccountBalance: row.AccountBalance.StringFixed(2),
		Assigned:       row.Assigned.StringFixed(2),
		Available:      row.Available.StringFixed(2),
		OverAllocated:  row.Available.IsNegative(),
	}, nil
}

func (s *Server) handleAccountAvailableForPiggy(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	accountID, err := uuid.Parse(chi.URLParam(r, "accountID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid account id")
		return
	}
	alloc, err := s.accountAllocationFor(r.Context(), accountID, identity.HouseholdID)
	if err != nil {
		// ErrNoRows is a debt account or one the household doesn't own: piggy
		// banks can't sit on it, so the honest answer is "nothing assignable".
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, accountAllocation{AccountID: accountID.String()})
			return
		}
		s.internalError(w, "read account allocation", err)
		return
	}
	writeJSON(w, http.StatusOK, alloc)
}
