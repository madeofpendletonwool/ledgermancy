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

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
)

// Manual accounts, securities, holdings and investment transactions (doc 30).
//
// The organising rule of this file:
//
//	A MANUAL ENDPOINT NEVER TOUCHES A PLAID ROW.
//
// Every mutation below resolves through a query carrying `source = 'manual'`,
// so a caller holding a perfectly valid Plaid account id gets a 404 rather than
// an edit. That is not defensiveness for its own sake: a Plaid account's name,
// type and balance are the institution's, and a local edit would survive
// exactly until the next sync overwrote it — leaving the user with a change
// they made, saw applied, and then silently lost.
//
// The second rule is that these are the ONLY endpoints that write
// accounts.current_balance. Doc 12 established that Plaid owns balances; this
// doc carves out manual accounts specifically, and pairs every balance write
// with an account_balance_history row in the same transaction so the figure and
// its explanation cannot disagree.

// --------------------------------------------------------------------------
// Shared shapes
// --------------------------------------------------------------------------

// accountTypes is the vocabulary accounts.type may take. Not enforced by a DB
// constraint — Plaid supplies it and the column is plain TEXT (00001:104) — so
// validating it here is what stops a manual account being created with a type
// no report groups by, which would make it invisible everywhere but the
// accounts list.
var accountTypes = map[string]bool{
	"depository": true, "investment": true, "brokerage": true,
	"credit": true, "loan": true, "other": true,
}

// investmentTxTypes and investmentTxSubtypes mirror the vocabulary
// reporting.IsExternalFlow classifies on. A value outside it does not error
// anywhere — it is simply never counted as a flow, so a contribution recorded
// as type "purchase" would be silently absorbed into the market return rather
// than subtracted from it. Rejecting it at the edge is the only place that
// mistake is visible.
var investmentTxTypes = map[string]bool{
	"buy": true, "sell": true, "cash": true,
	"fee": true, "transfer": true, "cancel": true,
}

var investmentTxSubtypes = map[string]bool{
	"contribution": true, "deposit": true, "withdrawal": true,
	"distribution": true, "transfer": true, "send": true, "request": true,
	"dividend": true, "qualified dividend": true, "non-qualified dividend": true,
	"interest": true, "buy": true, "sell": true, "fee": true, "management fee": true,
}

type manualAccountRequest struct {
	Name         string  `json:"name"`
	Type         string  `json:"type"`
	Subtype      *string `json:"subtype"`
	Mask         *string `json:"mask"`
	Currency     string  `json:"currency"`
	TaxTreatment *string `json:"tax_treatment"`
	IsShared     *bool   `json:"is_shared"`
	// Balance crosses the wire as a decimal STRING, like every other money
	// figure in this API. A JSON number would be a float, and a float is not
	// what a balance is.
	Balance *string `json:"balance"`
}

// validate normalises and checks a create/update body, returning the message a
// 400 should carry.
func (req *manualAccountRequest) validate() string {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return "name is required"
	}
	if !accountTypes[req.Type] {
		return "type must be one of: depository, investment, brokerage, credit, loan, other"
	}
	if req.Currency == "" {
		req.Currency = "USD"
	}
	if req.TaxTreatment != nil && *req.TaxTreatment != "" &&
		!networth.ValidTaxTreatment(*req.TaxTreatment) {
		return taxTreatmentError()
	}
	return ""
}

// --------------------------------------------------------------------------
// Accounts
// --------------------------------------------------------------------------

func (s *Server) handleCreateManualAccount(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req manualAccountRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	balance, ok := optionalSignedMoney(w, req.Balance, "balance")
	if !ok {
		return
	}

	// Household and owner come from the session, never the body. There is
	// deliberately no field a caller could set to create an account somewhere
	// else.
	row, err := s.Queries.CreateManualAccount(r.Context(), dbgen.CreateManualAccountParams{
		UserID:         identity.UserID,
		HouseholdID:    identity.HouseholdID,
		IsShared:       req.IsShared == nil || *req.IsShared,
		Name:           req.Name,
		Type:           req.Type,
		Subtype:        trimmed(req.Subtype),
		Mask:           trimmed(req.Mask),
		Currency:       req.Currency,
		TaxTreatment:   trimmed(req.TaxTreatment),
		CurrentBalance: balance,
	})
	if err != nil {
		s.internalError(w, "create manual account", err)
		return
	}

	// An opening balance is a balance write like any other, so it gets its own
	// history row. Without it the account's trend would start at its second
	// balance and the opening figure would be unexplained.
	if balance.Valid {
		note := "Opening balance."
		if _, err := s.Queries.InsertAccountBalanceHistory(r.Context(),
			dbgen.InsertAccountBalanceHistoryParams{
				AccountID: row.ID,
				AsOf:      today(),
				Balance:   balance.Decimal,
				Reason:    "manual",
				Note:      &note,
			}); err != nil {
			s.internalError(w, "record opening balance", err)
			return
		}
	}

	writeJSON(w, http.StatusCreated, manualAccountResponse(row))
}

func (s *Server) handleUpdateManualAccount(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	accountID, ok := accountIDParam(w, r)
	if !ok {
		return
	}

	var req manualAccountRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	row, err := s.Queries.UpdateManualAccount(r.Context(), dbgen.UpdateManualAccountParams{
		ID:           accountID,
		HouseholdID:  identity.HouseholdID,
		UserID:       identity.UserID,
		Name:         req.Name,
		Type:         req.Type,
		Subtype:      trimmed(req.Subtype),
		Mask:         trimmed(req.Mask),
		Currency:     req.Currency,
		TaxTreatment: trimmed(req.TaxTreatment),
		IsShared:     req.IsShared == nil || *req.IsShared,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		s.manualAccountNotFound(w, r, accountID)
		return
	}
	if err != nil {
		s.internalError(w, "update manual account", err)
		return
	}
	writeJSON(w, http.StatusOK, manualAccountResponse(row))
}

func (s *Server) handleDeleteManualAccount(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	accountID, ok := accountIDParam(w, r)
	if !ok {
		return
	}

	n, err := s.Queries.DeleteManualAccount(r.Context(), dbgen.DeleteManualAccountParams{
		ID: accountID, HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if err != nil {
		s.internalError(w, "delete manual account", err)
		return
	}
	if n == 0 {
		s.manualAccountNotFound(w, r, accountID)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// manualAccountNotFound distinguishes "no such account" from "that account is
// Plaid-linked", because the two need different things from the user. The
// second is a 409 with the action that would actually work.
func (s *Server) manualAccountNotFound(w http.ResponseWriter, r *http.Request, accountID uuid.UUID) {
	identity := auth.MustFromContext(r.Context())

	row, err := s.Queries.GetVisibleAccount(r.Context(), dbgen.GetVisibleAccountParams{
		ID: accountID, HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if err == nil && row.Source == "plaid" {
		writeError(w, http.StatusConflict,
			"this account is linked through Plaid; manage it from the institution it belongs to")
		return
	}
	writeError(w, http.StatusNotFound, "account not found")
}

// --------------------------------------------------------------------------
// Balance
// --------------------------------------------------------------------------

type setBalanceRequest struct {
	Balance string  `json:"balance"`
	AsOf    *string `json:"as_of"`
	Reason  string  `json:"reason"`
	Note    *string `json:"note"`
}

var balanceReasons = map[string]bool{
	"manual": true, "holding_revalue": true, "fee": true, "dividend": true,
}

// handleSetManualBalance records a new balance for a manual account.
//
// Both writes happen in one transaction, for the same reason
// handleCreateValuation's do (asset_handlers.go): accounts.current_balance is
// the figure every net-worth and investment query reads, and
// account_balance_history is the trail that explains it. A failure leaving one
// without the other would make the headline number and its own explanation
// disagree, with nothing to indicate which is right.
func (s *Server) handleSetManualBalance(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	accountID, ok := accountIDParam(w, r)
	if !ok {
		return
	}

	var req setBalanceRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	balance, err := decimal.NewFromString(req.Balance)
	if err != nil {
		writeError(w, http.StatusBadRequest, `balance must be a decimal number, e.g. "1250.00"`)
		return
	}
	if req.Reason == "" {
		req.Reason = "manual"
	}
	// 'scheduled' is absent on purpose: it is the worker's marker, and a user
	// able to write it by hand would make the audit trail unable to answer
	// "did the app do this or did I".
	if !balanceReasons[req.Reason] {
		writeError(w, http.StatusBadRequest,
			"reason must be one of: manual, holding_revalue, fee, dividend")
		return
	}

	asOf, ok := dateOrToday(w, req.AsOf)
	if !ok {
		return
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		s.internalError(w, "begin balance write", err)
		return
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)

	// The account update goes first because it carries the household and
	// source guards. Writing history for an account the caller cannot reach,
	// and only then discovering it, would leave a rolled-back row that briefly
	// existed — harmless here, but the ordering makes the guard unmistakable.
	row, err := qtx.SetManualAccountBalance(ctx, dbgen.SetManualAccountBalanceParams{
		ID: accountID, HouseholdID: identity.HouseholdID, UserID: identity.UserID,
		Balance: decimal.NullDecimal{Decimal: balance, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		s.manualAccountNotFound(w, r, accountID)
		return
	}
	if err != nil {
		s.internalError(w, "set manual balance", err)
		return
	}

	if _, err := qtx.InsertAccountBalanceHistory(ctx, dbgen.InsertAccountBalanceHistoryParams{
		AccountID: accountID,
		AsOf:      asOf,
		Balance:   balance,
		Reason:    req.Reason,
		Note:      trimmed(req.Note),
	}); err != nil {
		s.internalError(w, "record balance history", err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		s.internalError(w, "commit balance write", err)
		return
	}
	writeJSON(w, http.StatusOK, manualAccountResponse(row))
}

type balanceHistoryResponse struct {
	AsOf    string          `json:"as_of"`
	Balance decimal.Decimal `json:"balance"`
	Reason  string          `json:"reason"`
	Note    *string         `json:"note"`
}

func (s *Server) handleListBalanceHistory(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	accountID, ok := accountIDParam(w, r)
	if !ok {
		return
	}

	// Optional range bounds, matching /api/networth/history. Absent means
	// unbounded, which is what the manual balance editor wants (every entry it
	// recorded); a chart caller passes a window so a year of daily Plaid
	// snapshots is not pulled in full every render.
	var from, to *time.Time
	q := r.URL.Query()
	if raw := q.Get("from"); raw != "" {
		if v, err := time.Parse(time.DateOnly, raw); err == nil {
			from = &v
		}
	}
	if raw := q.Get("to"); raw != "" {
		if v, err := time.Parse(time.DateOnly, raw); err == nil {
			to = &v
		}
	}

	rows, err := s.Queries.ListAccountBalanceHistory(r.Context(),
		dbgen.ListAccountBalanceHistoryParams{
			AccountID: accountID, HouseholdID: identity.HouseholdID, UserID: identity.UserID,
			From: from, To: to,
		})
	if err != nil {
		s.internalError(w, "list balance history", err)
		return
	}

	out := make([]balanceHistoryResponse, 0, len(rows))
	for _, h := range rows {
		out = append(out, balanceHistoryResponse{
			AsOf:    h.AsOf.Format(time.DateOnly),
			Balance: h.Balance,
			Reason:  h.Reason,
			Note:    h.Note,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// --------------------------------------------------------------------------
// Securities
// --------------------------------------------------------------------------

type securityResponse struct {
	ID           uuid.UUID        `json:"id"`
	Ticker       *string          `json:"ticker"`
	Name         *string          `json:"name"`
	Type         *string          `json:"type"`
	ClosePrice   *decimal.Decimal `json:"close_price"`
	ClosePriceAt *string          `json:"close_price_as_of"`
	Currency     string           `json:"currency"`
	Source       string           `json:"source"`
}

func (s *Server) handleListSecurities(w http.ResponseWriter, r *http.Request) {
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	var searchArg *string
	if search != "" {
		searchArg = &search
	}

	limit := int32(50)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 200 {
			limit = int32(n)
		}
	}

	rows, err := s.Queries.ListSecurities(r.Context(), dbgen.ListSecuritiesParams{
		Search: searchArg, Limit: limit,
	})
	if err != nil {
		s.internalError(w, "list securities", err)
		return
	}

	out := make([]securityResponse, 0, len(rows))
	for _, sec := range rows {
		out = append(out, securityResponse{
			ID:           sec.ID,
			Ticker:       sec.Ticker,
			Name:         sec.Name,
			Type:         sec.Type,
			ClosePrice:   nullDecimal(sec.ClosePrice),
			ClosePriceAt: formatDate(sec.ClosePriceAsOf),
			Currency:     sec.Currency,
			Source:       sec.Source,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type createSecurityRequest struct {
	Ticker           string  `json:"ticker"`
	Name             *string `json:"name"`
	Type             *string `json:"type"`
	Cusip            *string `json:"cusip"`
	Isin             *string `json:"isin"`
	ClosePrice       *string `json:"close_price"`
	ClosePriceAsOf   *string `json:"close_price_as_of"`
	Currency         string  `json:"currency"`
	IsCashEquivalent bool    `json:"is_cash_equivalent"`
}

func (s *Server) handleCreateManualSecurity(w http.ResponseWriter, r *http.Request) {
	var req createSecurityRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	req.Ticker = strings.ToUpper(strings.TrimSpace(req.Ticker))
	if req.Ticker == "" {
		writeError(w, http.StatusBadRequest, "ticker is required")
		return
	}
	if req.Currency == "" {
		req.Currency = "USD"
	}

	price, ok := optionalSignedMoney(w, req.ClosePrice, "close_price")
	if !ok {
		return
	}
	priceAsOf, ok := optionalDateOrNil(w, req.ClosePriceAsOf)
	if !ok {
		return
	}
	// A price with no date is a number nobody can judge the staleness of, so it
	// is dated today rather than left blank.
	if price.Valid && priceAsOf == nil {
		t := today()
		priceAsOf = &t
	}

	row, err := s.Queries.CreateManualSecurity(r.Context(), dbgen.CreateManualSecurityParams{
		Ticker:           req.Ticker,
		Name:             trimmed(req.Name),
		Type:             trimmed(req.Type),
		Cusip:            trimmed(req.Cusip),
		Isin:             trimmed(req.Isin),
		ClosePrice:       price,
		ClosePriceAsOf:   priceAsOf,
		Currency:         req.Currency,
		IsCashEquivalent: req.IsCashEquivalent,
	})
	if err != nil {
		s.internalError(w, "create manual security", err)
		return
	}

	writeJSON(w, http.StatusCreated, securityResponse{
		ID:           row.ID,
		Ticker:       row.Ticker,
		Name:         row.Name,
		Type:         row.Type,
		ClosePrice:   nullDecimal(row.ClosePrice),
		ClosePriceAt: formatDate(row.ClosePriceAsOf),
		Currency:     row.Currency,
		Source:       row.Source,
	})
}

// --------------------------------------------------------------------------
// Holdings
// --------------------------------------------------------------------------

type upsertHoldingRequest struct {
	SecurityID       uuid.UUID `json:"security_id"`
	Quantity         string    `json:"quantity"`
	CostBasis        *string   `json:"cost_basis"`
	InstitutionPrice *string   `json:"institution_price"`
	AsOf             *string   `json:"as_of"`
}

func (s *Server) handleUpsertManualHolding(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	accountID, ok := accountIDParam(w, r)
	if !ok {
		return
	}

	var req upsertHoldingRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.SecurityID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "security_id is required")
		return
	}

	// Quantity is NUMERIC(28,10), not an integer: fractional shares are the
	// norm in a retirement plan, where a fixed dollar contribution buys whatever
	// fraction the price implies.
	quantity, err := decimal.NewFromString(req.Quantity)
	if err != nil {
		writeError(w, http.StatusBadRequest, `quantity must be a decimal number, e.g. "12.3456"`)
		return
	}

	basis, ok := optionalSignedMoney(w, req.CostBasis, "cost_basis")
	if !ok {
		return
	}
	price, ok := optionalSignedMoney(w, req.InstitutionPrice, "institution_price")
	if !ok {
		return
	}
	asOf, ok := dateOrToday(w, req.AsOf)
	if !ok {
		return
	}

	row, err := s.Queries.UpsertManualHolding(r.Context(), dbgen.UpsertManualHoldingParams{
		AccountID:        accountID,
		HouseholdID:      identity.HouseholdID,
		UserID:           identity.UserID,
		SecurityID:       req.SecurityID,
		Quantity:         quantity,
		CostBasis:        basis,
		InstitutionPrice: price,
		AsOf:             &asOf,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		s.manualAccountNotFound(w, r, accountID)
		return
	}
	if err != nil {
		s.internalError(w, "upsert manual holding", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":                row.ID,
		"account_id":        row.AccountID,
		"security_id":       row.SecurityID,
		"quantity":          row.Quantity,
		"cost_basis":        nullDecimal(row.CostBasis),
		"institution_price": nullDecimal(row.InstitutionPrice),
		"institution_value": nullDecimal(row.InstitutionValue),
	})
}

func (s *Server) handleDeleteManualHolding(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "holdingID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid holding id")
		return
	}

	n, err := s.Queries.DeleteManualHolding(r.Context(), dbgen.DeleteManualHoldingParams{
		ID: id, HouseholdID: identity.HouseholdID, UserID: identity.UserID,
	})
	if err != nil {
		s.internalError(w, "delete manual holding", err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "holding not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --------------------------------------------------------------------------
// Investment transactions
// --------------------------------------------------------------------------

type investmentTxRequest struct {
	AccountID  uuid.UUID  `json:"account_id"`
	SecurityID *uuid.UUID `json:"security_id"`
	Type       string     `json:"type"`
	Subtype    *string    `json:"subtype"`
	Amount     string     `json:"amount"`
	Quantity   *string    `json:"quantity"`
	Price      *string    `json:"price"`
	Fees       *string    `json:"fees"`
	Date       string     `json:"date"`
	Name       *string    `json:"name"`
}

func (s *Server) handleCreateManualInvestmentTx(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req investmentTxRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.AccountID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "account_id is required")
		return
	}
	if !investmentTxTypes[req.Type] {
		writeError(w, http.StatusBadRequest,
			"type must be one of: buy, sell, cash, fee, transfer, cancel")
		return
	}
	if req.Subtype != nil && *req.Subtype != "" && !investmentTxSubtypes[*req.Subtype] {
		writeError(w, http.StatusBadRequest,
			"subtype is not one this app's return and dividend reporting recognises")
		return
	}

	// The sign convention, restated because it is the easy thing to get wrong
	// and nothing downstream will complain: money INTO the portfolio is
	// NEGATIVE here, matching Plaid and reporting.ExternalFlows. A caller
	// entering a $500 contribution sends "-500".
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil {
		writeError(w, http.StatusBadRequest,
			`amount must be a decimal number, e.g. "-500.00" for money into the account`)
		return
	}

	date, err := time.Parse(time.DateOnly, req.Date)
	if err != nil {
		writeError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
		return
	}

	quantity, ok := optionalSignedMoney(w, req.Quantity, "quantity")
	if !ok {
		return
	}
	price, ok := optionalSignedMoney(w, req.Price, "price")
	if !ok {
		return
	}
	fees, ok := optionalSignedMoney(w, req.Fees, "fees")
	if !ok {
		return
	}

	row, err := s.Queries.CreateManualInvestmentTransaction(r.Context(),
		dbgen.CreateManualInvestmentTransactionParams{
			AccountID:   req.AccountID,
			HouseholdID: identity.HouseholdID,
			UserID:      identity.UserID,
			SecurityID:  req.SecurityID,
			Source:      "manual",
			Type:        req.Type,
			Subtype:     trimmed(req.Subtype),
			Amount:      amount,
			Quantity:    quantity,
			Price:       price,
			Fees:        fees,
			Date:        date,
			Name:        trimmed(req.Name),
		})
	if errors.Is(err, pgx.ErrNoRows) {
		s.manualAccountNotFound(w, r, req.AccountID)
		return
	}
	if err != nil {
		s.internalError(w, "create manual investment transaction", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":      row.ID,
		"date":    row.Date.Format(time.DateOnly),
		"type":    row.Type,
		"subtype": row.Subtype,
		"amount":  row.Amount,
	})
}

func (s *Server) handleDeleteManualInvestmentTx(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "txID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid transaction id")
		return
	}

	n, err := s.Queries.DeleteManualInvestmentTransaction(r.Context(),
		dbgen.DeleteManualInvestmentTransactionParams{
			ID: id, HouseholdID: identity.HouseholdID, UserID: identity.UserID,
		})
	if err != nil {
		s.internalError(w, "delete manual investment transaction", err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "transaction not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type investmentTxResponse struct {
	ID           uuid.UUID        `json:"id"`
	Date         string           `json:"date"`
	Source       string           `json:"source"`
	Type         string           `json:"type"`
	Subtype      *string          `json:"subtype"`
	Amount       decimal.Decimal  `json:"amount"`
	Quantity     *decimal.Decimal `json:"quantity"`
	Price        *decimal.Decimal `json:"price"`
	Fees         *decimal.Decimal `json:"fees"`
	Name         *string          `json:"name"`
	Ticker       *string          `json:"ticker"`
	SecurityName *string          `json:"security_name"`
}

func (s *Server) handleListAccountInvestmentTx(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	accountID, ok := accountIDParam(w, r)
	if !ok {
		return
	}

	rows, err := s.Queries.ListInvestmentTransactionsForAccount(r.Context(),
		dbgen.ListInvestmentTransactionsForAccountParams{
			AccountID: accountID, HouseholdID: identity.HouseholdID, UserID: identity.UserID,
		})
	if err != nil {
		s.internalError(w, "list investment transactions", err)
		return
	}

	out := make([]investmentTxResponse, 0, len(rows))
	for _, t := range rows {
		out = append(out, investmentTxResponse{
			ID:           t.ID,
			Date:         t.Date.Format(time.DateOnly),
			Source:       t.Source,
			Type:         t.Type,
			Subtype:      t.Subtype,
			Amount:       t.Amount,
			Quantity:     nullDecimal(t.Quantity),
			Price:        nullDecimal(t.Price),
			Fees:         nullDecimal(t.Fees),
			Name:         t.Name,
			Ticker:       t.Ticker,
			SecurityName: t.SecurityName,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func accountIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "accountID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid account id")
		return uuid.Nil, false
	}
	return id, true
}

func today() time.Time { return time.Now().UTC().Truncate(24 * time.Hour) }

// optionalSignedMoney parses a money field that may be absent. An empty string
// is treated as absent, so clearing a form field records nothing rather than
// zero — "unknown" and "$0.00" are very different claims about a cost basis.
//
// Deliberately NOT projection_handlers.go's optionalMoney, which rejects
// negatives. Every figure it guards is a projection input where a negative is
// nonsense; here a negative is the normal case — investment_transactions.amount
// is negative for money moving INTO the portfolio, and a credit or loan account
// carries a negative balance by definition.
func optionalSignedMoney(w http.ResponseWriter, raw *string, field string) (decimal.NullDecimal, bool) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return decimal.NullDecimal{}, true
	}
	d, err := decimal.NewFromString(strings.TrimSpace(*raw))
	if err != nil {
		writeError(w, http.StatusBadRequest, field+" must be a decimal number")
		return decimal.NullDecimal{}, false
	}
	return decimal.NullDecimal{Decimal: d, Valid: true}, true
}

// dateOrToday defaults an absent date to today, for the fields where "now" is
// the obvious meaning: a balance a user is entering right now, a position they
// are recording as it stands.
func dateOrToday(w http.ResponseWriter, raw *string) (time.Time, bool) {
	if raw == nil || *raw == "" {
		return today(), true
	}
	d, err := time.Parse(time.DateOnly, *raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
		return time.Time{}, false
	}
	return d, true
}

// optionalDateOrNil leaves an absent date absent, for the fields where there is
// no sensible default.
func optionalDateOrNil(w http.ResponseWriter, raw *string) (*time.Time, bool) {
	if raw == nil || *raw == "" {
		return nil, true
	}
	d, err := time.Parse(time.DateOnly, *raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
		return nil, false
	}
	return &d, true
}

func formatDate(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.DateOnly)
	return &s
}

func manualAccountResponse(a dbgen.Account) map[string]any {
	return map[string]any{
		"id":              a.ID,
		"name":            a.Name,
		"type":            a.Type,
		"subtype":         a.Subtype,
		"mask":            a.Mask,
		"currency":        a.Currency,
		"tax_treatment":   a.TaxTreatment,
		"current_balance": nullDecimal(a.CurrentBalance),
		"is_shared":       a.IsShared,
		"source":          a.Source,
	}
}
