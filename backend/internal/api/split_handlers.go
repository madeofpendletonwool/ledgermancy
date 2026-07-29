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

// Bill split and the household reimbursement ledger.
//
// THE RULE THIS FILE EXISTS TO PROTECT: a split is an ATTRIBUTION OVERLAY. The
// transaction happened once, on one account, and household spending totals are
// unchanged by splitting it. Nothing here is joined into a spending aggregate.
// Only per-person views read these rows.
//
// Shares are stored as RESOLVED AMOUNTS, never percentages. A 3-way split of
// $100 is 33.34/33.33/33.33 and the remainder has to land somewhere
// deterministic; resolving in the API and asserting the sum means a stored
// split can never disagree with its own transaction.

const splitListLimit = 200

type splitShareResponse struct {
	ID         uuid.UUID  `json:"id"`
	PersonID   uuid.UUID  `json:"person_id"`
	PersonName string     `json:"person_name"`
	Amount     string     `json:"amount"`
	SettledAt  *time.Time `json:"settled_at"`
}

type transactionSplitsResponse struct {
	TransactionID     uuid.UUID            `json:"transaction_id"`
	TransactionName   string               `json:"transaction_name"`
	TransactionAmount string               `json:"transaction_amount"`
	Date              string               `json:"date"`
	Shares            []splitShareResponse `json:"shares"`
}

func (s *Server) handleGetTransactionSplits(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	txnID, err := uuid.Parse(chi.URLParam(r, "transactionID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid transaction id")
		return
	}

	txn, err := s.Queries.GetTransactionForSplit(r.Context(), dbgen.GetTransactionForSplitParams{
		ID: txnID, HouseholdID: identity.HouseholdID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "transaction not found")
		return
	}
	if err != nil {
		s.internalError(w, "get transaction for split", err)
		return
	}

	resp, err := s.buildTransactionSplits(r, txn, identity.HouseholdID)
	if err != nil {
		s.internalError(w, "list splits", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) buildTransactionSplits(r *http.Request,
	txn dbgen.GetTransactionForSplitRow, householdID uuid.UUID,
) (transactionSplitsResponse, error) {
	resp := transactionSplitsResponse{
		TransactionID:     txn.ID,
		TransactionName:   txn.Name,
		TransactionAmount: txn.Amount.StringFixed(2),
		Date:              txn.Date.Format(time.DateOnly),
		Shares:            []splitShareResponse{},
	}

	rows, err := s.Queries.ListSplitsForTransaction(r.Context(),
		dbgen.ListSplitsForTransactionParams{
			TransactionID: txn.ID, HouseholdID: householdID,
		})
	if err != nil {
		return resp, err
	}
	for _, row := range rows {
		resp.Shares = append(resp.Shares, splitShareResponse{
			ID:         row.ID,
			PersonID:   row.PersonID,
			PersonName: row.DisplayName,
			Amount:     row.Amount.StringFixed(2),
			SettledAt:  row.SettledAt,
		})
	}
	return resp, nil
}

type setSplitsRequest struct {
	// Exactly one of Shares or Equal is used. Percentages are a UI convenience
	// and are resolved client-side into exact amounts before they get here —
	// this endpoint stores what it is given and checks that it adds up.
	Shares []splitShareRequest `json:"shares"`
	// Equal splits the transaction evenly across the named people, resolving
	// the remainder server-side so the shares always sum exactly.
	Equal []uuid.UUID `json:"equal"`
}

type splitShareRequest struct {
	PersonID uuid.UUID `json:"person_id"`
	Amount   string    `json:"amount"`
}

// handleSetTransactionSplits replaces the whole split set for one transaction.
//
// Written as a set rather than per-share on purpose: a partial write could
// leave shares that no longer sum to the transaction, and a split that does not
// sum is a silent reporting error rather than a loud one.
func (s *Server) handleSetTransactionSplits(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	txnID, err := uuid.Parse(chi.URLParam(r, "transactionID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid transaction id")
		return
	}

	var req setSplitsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Shares) == 0 && len(req.Equal) == 0 {
		writeError(w, http.StatusBadRequest, "provide either shares or equal")
		return
	}
	if len(req.Shares) > 0 && len(req.Equal) > 0 {
		writeError(w, http.StatusBadRequest, "provide shares or equal, not both")
		return
	}

	txn, err := s.Queries.GetTransactionForSplit(ctx, dbgen.GetTransactionForSplitParams{
		ID: txnID, HouseholdID: identity.HouseholdID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "transaction not found")
		return
	}
	if err != nil {
		s.internalError(w, "get transaction for split", err)
		return
	}

	shares, err := resolveShares(req, txn.Amount)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// The assertion that makes the rest of the app trustworthy. It is checked
	// here, on every write, rather than only in a test: a split that does not
	// sum to its transaction would misreport every per-person figure derived
	// from it, quietly and forever.
	var total decimal.Decimal
	for _, sh := range shares {
		total = total.Add(sh.amount)
	}
	if !total.Equal(txn.Amount) {
		writeError(w, http.StatusBadRequest,
			"shares must sum to the transaction amount ("+txn.Amount.StringFixed(2)+
				"); got "+total.StringFixed(2))
		return
	}

	// Replace atomically: a failure part-way through must not leave a
	// half-written split set that no longer sums.
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		s.internalError(w, "begin split transaction", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.Queries.WithTx(tx)

	if err := qtx.ReplaceTransactionSplits(ctx, txnID); err != nil {
		s.internalError(w, "clear splits", err)
		return
	}
	for _, sh := range shares {
		if _, err := qtx.CreateTransactionSplit(ctx, dbgen.CreateTransactionSplitParams{
			TransactionID: txnID,
			PersonID:      sh.personID,
			HouseholdID:   identity.HouseholdID,
			Amount:        sh.amount,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// The person is not in this household. Refuse the whole set.
				writeError(w, http.StatusBadRequest, "unknown person in shares")
				return
			}
			s.internalError(w, "create split", err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		s.internalError(w, "commit splits", err)
		return
	}

	resp, err := s.buildTransactionSplits(r, txn, identity.HouseholdID)
	if err != nil {
		s.internalError(w, "list splits", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type resolvedShare struct {
	personID uuid.UUID
	amount   decimal.Decimal
}

// resolveShares turns a request into exact amounts.
//
// The equal-split remainder is the interesting case. $100 three ways is
// 33.333... and cents are indivisible, so somebody gets the extra penny. The
// remainder is distributed one cent at a time from the first person onward,
// which is deterministic, fully spends the amount, and never differs by more
// than a cent between people.
func resolveShares(req setSplitsRequest, total decimal.Decimal) ([]resolvedShare, error) {
	if len(req.Equal) > 0 {
		seen := map[uuid.UUID]bool{}
		for _, id := range req.Equal {
			if seen[id] {
				return nil, errors.New("the same person appears twice in equal")
			}
			seen[id] = true
		}

		n := int64(len(req.Equal))
		// Work in cents so the remainder is an exact integer rather than a
		// rounding artifact.
		cents := total.Shift(2).Round(0)
		base := cents.Div(decimal.NewFromInt(n)).Truncate(0)
		remainder := cents.Sub(base.Mul(decimal.NewFromInt(n)))

		out := make([]resolvedShare, 0, len(req.Equal))
		for i, id := range req.Equal {
			amount := base
			if decimal.NewFromInt(int64(i)).LessThan(remainder.Abs()) {
				if remainder.IsNegative() {
					amount = amount.Sub(decimal.NewFromInt(1))
				} else {
					amount = amount.Add(decimal.NewFromInt(1))
				}
			}
			out = append(out, resolvedShare{personID: id, amount: amount.Shift(-2)})
		}
		return out, nil
	}

	seen := map[uuid.UUID]bool{}
	out := make([]resolvedShare, 0, len(req.Shares))
	for _, sh := range req.Shares {
		if seen[sh.PersonID] {
			return nil, errors.New("the same person appears twice in shares")
		}
		seen[sh.PersonID] = true

		amount, err := decimal.NewFromString(strings.TrimSpace(sh.Amount))
		if err != nil {
			return nil, errors.New("each share amount must be a decimal amount")
		}
		out = append(out, resolvedShare{personID: sh.PersonID, amount: amount})
	}
	return out, nil
}

func (s *Server) handleClearTransactionSplits(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	txnID, err := uuid.Parse(chi.URLParam(r, "transactionID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid transaction id")
		return
	}

	// Resolve through the household guard first: ReplaceTransactionSplits keys
	// on the transaction id alone, so without this a valid id from another
	// household would delete their splits.
	if _, err := s.Queries.GetTransactionForSplit(r.Context(),
		dbgen.GetTransactionForSplitParams{
			ID: txnID, HouseholdID: identity.HouseholdID,
		}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "transaction not found")
			return
		}
		s.internalError(w, "get transaction for split", err)
		return
	}

	if err := s.Queries.ReplaceTransactionSplits(r.Context(), txnID); err != nil {
		s.internalError(w, "clear splits", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type splitTransactionResponse struct {
	TransactionID   uuid.UUID `json:"transaction_id"`
	Name            string    `json:"name"`
	Date            string    `json:"date"`
	Amount          string    `json:"amount"`
	PayerName       *string   `json:"payer_name"`
	SplitCount      int64     `json:"split_count"`
	UnsettledCount  int64     `json:"unsettled_count"`
	FullySettled    bool      `json:"fully_settled"`
}

func (s *Server) handleListSplitTransactions(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListSplitTransactions(r.Context(), dbgen.ListSplitTransactionsParams{
		HouseholdID: identity.HouseholdID, Limit: splitListLimit,
	})
	if err != nil {
		s.internalError(w, "list split transactions", err)
		return
	}

	out := make([]splitTransactionResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, splitTransactionResponse{
			TransactionID:  row.ID,
			Name:           row.Name,
			Date:           row.Date.Format(time.DateOnly),
			Amount:         row.Amount.StringFixed(2),
			PayerName:      row.PayerName,
			SplitCount:     row.SplitCount,
			UnsettledCount: row.UnsettledCount,
			FullySettled:   row.UnsettledCount == 0,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type ledgerEntryResponse struct {
	DebtorID     uuid.UUID `json:"debtor_id"`
	DebtorName   string    `json:"debtor_name"`
	CreditorID   uuid.UUID `json:"creditor_id"`
	CreditorName string    `json:"creditor_name"`
	Amount       string    `json:"amount"`
}

// handleHouseholdLedger answers "who owes whom".
//
// The balance is DERIVED by summing unsettled shares — there is no stored
// balance, for the same reason goals store no current_amount. Opposing pairs
// are netted here so "Sam owes Alex $40" and "Alex owes Sam $15" render as one
// line of $25 rather than two lines that a reader has to net themselves.
func (s *Server) handleHouseholdLedger(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.HouseholdLedger(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "household ledger", err)
		return
	}

	// Net each unordered pair into a single direction.
	type pairKey struct{ a, b uuid.UUID }
	type pairState struct {
		names   map[uuid.UUID]string
		balance decimal.Decimal // positive means a owes b
	}
	pairs := map[pairKey]*pairState{}
	order := []pairKey{}

	for _, row := range rows {
		a, b := row.DebtorID, row.CreditorID
		amount := row.Amount
		// Canonical ordering so both directions land on the same key.
		if b.String() < a.String() {
			a, b = b, a
			amount = amount.Neg()
		}
		key := pairKey{a, b}
		st, ok := pairs[key]
		if !ok {
			st = &pairState{names: map[uuid.UUID]string{}}
			pairs[key] = st
			order = append(order, key)
		}
		st.names[row.DebtorID] = row.DebtorName
		st.names[row.CreditorID] = row.CreditorName
		st.balance = st.balance.Add(amount)
	}

	out := make([]ledgerEntryResponse, 0, len(order))
	for _, key := range order {
		st := pairs[key]
		if st.balance.IsZero() {
			continue // fully offsetting; nobody owes anybody
		}
		debtor, creditor := key.a, key.b
		amount := st.balance
		if amount.IsNegative() {
			debtor, creditor = key.b, key.a
			amount = amount.Neg()
		}
		out = append(out, ledgerEntryResponse{
			DebtorID:     debtor,
			DebtorName:   st.names[debtor],
			CreditorID:   creditor,
			CreditorName: st.names[creditor],
			Amount:       amount.StringFixed(2),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSettleSplit(w http.ResponseWriter, r *http.Request) {
	s.setSplitSettled(w, r, true)
}

func (s *Server) handleUnsettleSplit(w http.ResponseWriter, r *http.Request) {
	s.setSplitSettled(w, r, false)
}

// setSplitSettled stamps or clears settled_at. No money moves — the app records
// that it did, which is the whole point of the ledger.
func (s *Server) setSplitSettled(w http.ResponseWriter, r *http.Request, settled bool) {
	identity := auth.MustFromContext(r.Context())

	splitID, err := uuid.Parse(chi.URLParam(r, "splitID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid split id")
		return
	}

	var rows int64
	if settled {
		rows, err = s.Queries.SettleSplit(r.Context(), dbgen.SettleSplitParams{
			ID: splitID, HouseholdID: identity.HouseholdID,
		})
	} else {
		rows, err = s.Queries.UnsettleSplit(r.Context(), dbgen.UnsettleSplitParams{
			ID: splitID, HouseholdID: identity.HouseholdID,
		})
	}
	if err != nil {
		s.internalError(w, "settle split", err)
		return
	}
	if rows == 0 {
		// Either it is not in this household, or it was already in the target
		// state. Both are "nothing to do" from the caller's point of view.
		writeError(w, http.StatusNotFound, "split not found or already in that state")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
