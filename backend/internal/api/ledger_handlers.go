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

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/audit"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/plaid"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/rules"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/search"
)

type accountResponse struct {
	ID uuid.UUID `json:"id"`
	// ItemID is what the accounts page groups by. Institution *name* is not a
	// key: two household members can each link the same bank, and grouping by
	// name merges their accounts into every one of those institutions' cards.
	//
	// NULL for a manual account, which has no Plaid item to belong to. The page
	// groups those under their own heading rather than inventing an item id.
	ItemID           *uuid.UUID       `json:"item_id"`
	Name             string           `json:"name"`
	Mask             *string          `json:"mask"`
	Type             string           `json:"type"`
	Subtype          *string          `json:"subtype"`
	Institution      *string          `json:"institution_name"`
	CurrentBalance   *decimal.Decimal `json:"current_balance"`
	AvailableBalance *decimal.Decimal `json:"available_balance"`
	Currency         string           `json:"currency"`
	IsOwn            bool             `json:"is_own"`
	// Source decides which affordances the row offers: a Plaid account syncs
	// and reconnects, a manual one is edited and has its balance set by hand.
	Source string `json:"source"`
	// What the manual-account editor round-trips. Both are meaningful for Plaid
	// accounts too; they were simply not needed by this endpoint before.
	TaxTreatment *string `json:"tax_treatment"`
	IsShared     bool    `json:"is_shared"`
	// DepositAPY is the user-entered yield on a deposit account, as a PERCENT
	// string ("4.50"). Null means nobody has entered one — UNKNOWN, never zero.
	// Doc 32's cash-drag detector stays silent on a null rather than reporting a
	// high-yield savings account as the household's worst drag.
	DepositAPY *string `json:"deposit_apy"`
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
			ItemID:           a.PlaidItemID,
			Name:             a.Name,
			Mask:             a.Mask,
			Type:             a.Type,
			Subtype:          a.Subtype,
			Institution:      a.InstitutionName,
			CurrentBalance:   nullDecimal(a.CurrentBalance),
			AvailableBalance: nullDecimal(a.AvailableBalance),
			Currency:         a.Currency,
			// OwnerID and Shared are the RESOLVED values from account_access —
			// the manual account's own owner, or the Plaid item's. Reading
			// accounts.user_id/is_shared directly would report every Plaid
			// account as unowned and shared.
			IsOwn:        a.OwnerID == identity.UserID,
			Source:       a.Source,
			TaxTreatment: a.TaxTreatment,
			IsShared:     a.Shared,
			DepositAPY:   percentString(a.DepositApy),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type transactionResponse struct {
	ID           uuid.UUID `json:"id"`
	Date         time.Time `json:"date"`
	Name         string    `json:"name"`
	MerchantName *string   `json:"merchant_name"`
	// Merchant is the name to DISPLAY for the row: the canonical merchant name
	// when the descriptor has been grouped or renamed, otherwise the raw
	// merchant_name/name the bank sent. This is the field the row and recent
	// activity render — MerchantName/Name above stay raw so search and the manual
	// editor keep working against the bank's own text. Mirrors the COALESCE every
	// report reads, so a rename from the row menu is reflected here immediately.
	Merchant string `json:"merchant"`
	// MerchantKey is the normalized key the app caches categories by. Present
	// even when MerchantName is null (it falls back to the raw name), and empty
	// when the description carried too little signal to key on. The UI shows the
	// "apply to all from this merchant" option exactly when this is set.
	MerchantKey *string `json:"merchant_key"`
	// MerchantKeyResolved addresses the BUSINESS rather than the descriptor: an
	// entity id for a merchant the household has grouped, the raw key otherwise.
	// This is the one the merchant detail view and the ?merchant= filter take —
	// MerchantKey above would miss every fragment of a grouped merchant but one.
	// Empty when the description carried too little signal to key on, in which
	// case the UI must render the name as plain text.
	MerchantKeyResolved string          `json:"merchant_key_resolved"`
	Amount              decimal.Decimal `json:"amount"`
	Currency            string          `json:"currency"`
	Pending             bool            `json:"pending"`
	AccountID           uuid.UUID       `json:"account_id"`
	AccountName         string          `json:"account_name"`
	Institution         *string         `json:"institution_name"`
	PFCPrimary          *string         `json:"plaid_category_primary"`
	PFCDetailed         *string         `json:"plaid_category_detailed"`
	CategoryID          *uuid.UUID      `json:"category_id"`
	Notes               *string         `json:"notes"`
	// Source distinguishes hand-entered rows from aggregator feeds. The UI shows
	// edit/delete only on 'manual' rows; Plaid rows stay read-only except for
	// category, which has its own PATCH path.
	Source string `json:"source"`
	// PossibleDuplicate flags a manual row that a later Plaid-synced charge now
	// appears to match, so the UI can offer a one-click "delete my manual entry".
	PossibleDuplicate bool `json:"possible_duplicate"`
	// The two counting flags, so a row can render its own state and offer the
	// toggle. See transactionFlagsRequest for what each one means.
	ExcludedFromReports bool `json:"excluded_from_reports"`
	IsOneTime           bool `json:"is_one_time"`
	// Tags are the free-form labels on this row, orthogonal to CategoryID above:
	// a transaction has one category and any number of tags. Always present,
	// empty when nothing is labelled, so the row never has to handle null.
	Tags []transactionTag `json:"tags"`
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

	// Optional multi-account filter, as a comma-separated `accounts` list (or a
	// single legacy `account_id`). Malformed ids are dropped, which reads as
	// "all"; household/shared scoping in the query means a foreign id matches
	// nothing rather than leaking another household's rows. An empty slice is
	// passed through as "all accounts".
	accountIDs := parseUUIDList(q.Get("accounts"))
	if len(accountIDs) == 0 {
		accountIDs = parseUUIDList(q.Get("account_id"))
	}

	// Optional single-category filter.
	var categoryID *uuid.UUID
	if v := q.Get("category_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			categoryID = &id
		}
	}

	// Optional "only rows still needing a category" filter, for draining the
	// backlog. Any other value (or absent) leaves it off.
	var uncategorised *bool
	if q.Get("uncategorised") == "true" {
		t := true
		uncategorised = &t
	}

	// Optional canonical merchant filter, for drilling in from a merchant page.
	// Matched against the resolved key in SQL, so a grouped merchant returns every
	// descriptor's charges. A key that matches nothing yields an empty list rather
	// than an error: a stale link should read as "no charges", not as a failure.
	merchantKey := trimmedParam(q.Get("merchant"))

	// Optional "show the rows I've excluded from reports". Off by default so the
	// ledger reads like the reports it feeds; on, it is the only way to find an
	// excluded row again and clear the flag.
	var includeExcluded *bool
	if q.Get("include_excluded") == "true" {
		t := true
		includeExcluded = &t
	}

	// Optional tag filter, as a comma-separated `tags` list. Malformed ids are
	// dropped exactly as in the account filter, which reads as "all"; the query
	// re-checks each id against the household, so a foreign tag matches nothing
	// rather than leaking another household's labelling.
	tagIDs := parseUUIDList(q.Get("tags"))

	// Optional "only rows nobody has labelled yet" — the tag counterpart of the
	// uncategorised filter, and how a household finds the charges that still
	// belong to a trip or a project.
	var untagged *bool
	if q.Get("untagged") == "true" {
		t := true
		untagged = &t
	}

	params := dbgen.ListVisibleTransactionsParams{
		HouseholdID:     identity.HouseholdID,
		UserID:          identity.UserID,
		Date:            from,
		Date_2:          to,
		Limit:           int32(limit),
		Offset:          int32(offset),
		AccountIds:      accountIDs,
		CategoryID:      categoryID,
		MerchantKey:     merchantKey,
		Uncategorised:   uncategorised,
		IncludeExcluded: includeExcluded,
		TagIds:          tagIDs,
		Untagged:        untagged,
	}

	// `q` is a search query in the grammar internal/search parses: bare words are
	// free text (which is all it used to be, so old links keep working) and
	// `key:value` terms narrow further. Anything with operators in it cannot be
	// expressed as nargs, so that path resolves the query to an ordered page of
	// ids and lets this query project them — see transaction_search.go.
	if raw := strings.TrimSpace(q.Get("q")); raw != "" {
		parsed := search.Parse(raw)
		ids, err := s.searchTransactionIDs(r.Context(), transactionSearchParams{
			householdID: identity.HouseholdID,
			userID:      identity.UserID,
			query:       parsed,
			from:        from,
			to:          to,
			// A query that names its own dates owns the window. The page always
			// sends a from/to (a rolling year by default), so ANDing that with
			// `since:2019-01-01` would clip the search to the last year and answer
			// "nothing found" for a query that was perfectly good. Every other
			// kind of term still intersects with the window.
			applyWindow:   !parsed.HasDateTerm(),
			accountIDs:    accountIDs,
			categoryID:    categoryID,
			merchantKey:   merchantKey,
			uncategorised: uncategorised != nil && *uncategorised,
			// Excluded rows are hidden unless asked for, so a query that names
			// them has to turn that default off — otherwise `is_excluded` is a
			// term that can only ever match nothing.
			includeExcluded: (includeExcluded != nil && *includeExcluded) ||
				parsed.MentionsField("is_excluded"),
			limit:  limit,
			offset: offset,
			now:    time.Now(),
		})
		var invalid invalidSearchError
		switch {
		case errors.As(err, &invalid):
			// A value the user can fix (`over:banana`). Its own message is the
			// most useful thing to say.
			writeError(w, http.StatusBadRequest, invalid.Error())
			return
		case err != nil:
			s.internalError(w, "search transactions", err)
			return
		}
		if len(ids) == 0 {
			// Nothing matched. Answer with an empty list rather than passing an
			// empty id array down, where a NULL/empty narg would read as "no
			// filter" and return the whole page unfiltered.
			writeJSON(w, http.StatusOK, []transactionResponse{})
			return
		}

		// The ids ARE the filter now: the search already applied the window, the
		// chips and the excluded rule while it decided which rows and in what
		// order. Everything else is opened up so the fetch re-applies visibility
		// and the projection and nothing else.
		params.TransactionIds = ids
		params.Date, params.Date_2 = searchWindowMin, searchWindowMax
		params.Limit, params.Offset = int32(len(ids)), 0
		params.AccountIds, params.CategoryID, params.MerchantKey = nil, nil, nil
		params.Uncategorised = nil
		yes := true
		params.IncludeExcluded = &yes
	}

	rows, err := s.Queries.ListVisibleTransactions(r.Context(), params)
	if err != nil {
		s.internalError(w, "list transactions", err)
		return
	}

	// Labels for the whole page in ONE query rather than one per row. The ids
	// come from the list above, which was already read under the visibility
	// predicate, so this cannot widen what the caller sees.
	ids := make([]uuid.UUID, 0, len(rows))
	for _, t := range rows {
		ids = append(ids, t.ID)
	}
	tagsByTransaction, err := s.tagsForTransactions(r.Context(), identity, ids)
	if err != nil {
		s.internalError(w, "list transaction tags", err)
		return
	}

	out := make([]transactionResponse, 0, len(rows))
	for _, t := range rows {
		out = append(out, transactionResponse{
			ID:                  t.ID,
			Date:                t.Date,
			Name:                t.Name,
			MerchantName:        t.MerchantName,
			Merchant:            t.Merchant,
			MerchantKey:         t.MerchantKey,
			MerchantKeyResolved: t.ResolvedMerchantKey,
			Amount:              t.Amount,
			Currency:            t.Currency,
			Pending:             t.Pending,
			AccountID:           t.AccountID,
			AccountName:         t.AccountName,
			Institution:         t.InstitutionName,
			PFCPrimary:          t.PlaidPfcPrimary,
			PFCDetailed:         t.PlaidPfcDetailed,
			CategoryID:          t.CategoryID,
			Notes:               t.Notes,
			Source:              t.Source,
			PossibleDuplicate:   t.IsPossibleDuplicate != nil && *t.IsPossibleDuplicate,
			ExcludedFromReports: t.ExcludedFromReports,
			IsOneTime:           t.IsOneTime,
			Tags:                tagsByTransaction[t.ID],
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
	ctx := r.Context()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		s.internalError(w, "create manual transaction", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.Queries.WithTx(tx)

	created, err := qtx.CreateManualTransaction(ctx, dbgen.CreateManualTransactionParams{
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

	// Attribute the create in the same tx — a hand-entered row is a state
	// change too, and the History panel renders it as "created by X".
	if err := audit.Record(ctx, qtx, audit.RecordParams{
		HouseholdID: identity.HouseholdID,
		ObjectKind:  audit.KindTransaction,
		ObjectID:    created.ID,
		ActorUserID: identity.UserID,
	}, audit.Created()); err != nil {
		s.internalError(w, "record transaction change", err)
		return
	}

	// Fire the household's rules at the row that just landed, in the SAME
	// transaction: a hand-entered charge is automated exactly like a synced one,
	// and a rolled-back create leaves no tags or notes behind either.
	//
	// The sticky-manual invariant does the delicate part for free here. The
	// insert above stamps category_source='manual' whenever the form carried a
	// category, so a rule cannot re-file a charge the user just filed by hand —
	// while a row left uncategorised is fair game, which is the case rules exist
	// for.
	final, err := rules.ApplyToTransaction(ctx, qtx, identity.HouseholdID, created.ID)
	if err != nil {
		s.internalError(w, "apply rules to new transaction", err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		s.internalError(w, "create manual transaction", err)
		return
	}

	// Echo the category the row ENDED UP with. Without this a rule that filed
	// the charge would be invisible until the next refetch, and the user would
	// watch their new transaction change category a beat after saving it.
	categoryID := created.CategoryID
	if final != nil {
		categoryID = final.CategoryID
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          created.ID,
		"account_id":  created.AccountID,
		"date":        created.Date,
		"amount":      created.Amount,
		"name":        created.Name,
		"category_id": categoryID,
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

	ctx := r.Context()
	// One transaction so the edit and its history row land together. The
	// before-state read shares the tx, so the diff is computed against the row
	// under the same lock and a rolled-back edit writes no history.
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		s.internalError(w, "update manual transaction", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.Queries.WithTx(tx)

	before, err := qtx.GetVisibleTransaction(ctx, dbgen.GetVisibleTransactionParams{
		ID:           transactionID,
		HouseholdID:  identity.HouseholdID,
		ViewerUserID: &identity.UserID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "manual transaction not found")
			return
		}
		s.internalError(w, "load transaction", err)
		return
	}

	// The source='manual' + household guard in the query means an id belonging
	// to another household, or to a Plaid-synced row, matches nothing → 404.
	updated, err := qtx.UpdateManualTransaction(ctx, dbgen.UpdateManualTransactionParams{
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

	if err := audit.Record(ctx, qtx, audit.RecordParams{
		HouseholdID: identity.HouseholdID,
		ObjectKind:  audit.KindTransaction,
		ObjectID:    transactionID,
		ActorUserID: identity.UserID,
	}, audit.TransactionDiff(before, updated)); err != nil {
		s.internalError(w, "record transaction change", err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
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

// percentString renders a nullable rate as a fixed-2 STRING, or JSON null.
//
// A string rather than a number for the reason every money value in this API is
// one: a JSON number hands the browser a float to re-round. Null rather than
// "0.00" for a bigger reason — a rate nobody has entered and a rate of zero are
// different facts, and doc 32's cash-drag detector depends on telling them
// apart.
func percentString(d decimal.NullDecimal) *string {
	if !d.Valid {
		return nil
	}
	s := d.Decimal.StringFixed(2)
	return &s
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

// trimmedParam turns an optional string query parameter into a nullable filter:
// nil when absent or blank, which every narg-based query reads as "no filter".
// Trimming matters because a filter of " " would otherwise match nothing and read
// as an empty result set rather than as no filter at all.
func trimmedParam(raw string) *string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return nil
	}
	return &v
}

// parseUUIDList parses a comma-separated list of UUIDs, silently dropping blank
// or malformed entries. An empty or all-junk input yields nil, which the query
// treats as "no filter".
func parseUUIDList(raw string) []uuid.UUID {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []uuid.UUID
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if id, err := uuid.Parse(part); err == nil {
			out = append(out, id)
		}
	}
	return out
}

func parseInt(raw string, fallback, min, max int) int {
	if raw == "" {
		return fallback
	}
	// Parse within 32 bits. Every caller clamps to a small range and the result
	// is later narrowed to int32 for sqlc params, so a value that could not fit
	// int32 is out of any caller's range by definition — treat it as absent (use
	// the fallback) rather than truncating. Parsing at bitSize 32 also makes the
	// downstream int32() conversions provably in-range, not just clamped ones.
	v, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return fallback
	}
	return clamp(int(v), min, max)
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
