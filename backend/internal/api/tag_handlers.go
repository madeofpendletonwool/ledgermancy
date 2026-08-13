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

// Tags: free-form labels that cut ACROSS categories.
//
// THE DISTINCTION THIS FILE MUST NOT BLUR. A category answers "what kind of
// spending is this?" and a transaction has exactly one. A tag answers "what was
// this FOR?" and a transaction can carry several — a vacation spans Food,
// Travel and Lodging, and a remodel spans fees, materials and appliances.
// Nothing here touches internal/categorize: tags are a second axis, not a step
// in category resolution, and adding one to a transaction must never change
// which category it resolves to.
//
// SCOPE. A tag belongs to the household; which TAGGED TRANSACTIONS a member can
// see still runs through the same account_access predicate as every other read.
// So both members see the tag "Summer Vacation", and each member's total for it
// counts only the rows they were already allowed to see. See queries/tags.sql —
// every statement there carries both scopes, and this file never re-implements
// either in Go.

// tagResponse is one tag plus the two DERIVED figures, both computed for the
// calling viewer. Money is always a fixed-2 string, never a JSON number.
type tagResponse struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	// ExpectedAmount is the envelope's target ("this trip is meant to cost
	// 3,000"), or null when the household set none. Null and "0.00" are
	// different facts: one renders no progress bar, the other renders a full
	// one.
	ExpectedAmount *string `json:"expected_amount"`
	// TransactionCount is how many visible transactions carry the tag — every
	// one of them, including income and pending rows, because it is a fact about
	// the LABEL.
	TransactionCount int64 `json:"transaction_count"`
	// Total is what the tag has COST, all-time, under the same money rules as
	// every other spending figure in the app. It is the figure that reads
	// against ExpectedAmount.
	Total     string `json:"total"`
	CreatedAt string `json:"created_at"`
}

func tagFromRow(row dbgen.ListTagsRow) tagResponse {
	resp := tagResponse{
		ID:               row.ID.String(),
		Name:             row.Name,
		Description:      row.Description,
		TransactionCount: row.TransactionCount,
		Total:            row.Total.StringFixed(2),
		CreatedAt:        row.CreatedAt.UTC().Format(time.RFC3339),
	}
	if row.ExpectedAmount.Valid {
		s := row.ExpectedAmount.Decimal.StringFixed(2)
		resp.ExpectedAmount = &s
	}
	return resp
}

func (s *Server) handleListTags(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListTags(r.Context(), dbgen.ListTagsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list tags", err)
		return
	}
	out := make([]tagResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, tagFromRow(row))
	}
	writeJSON(w, http.StatusOK, out)
}

// readTag re-reads one tag through ListTags, which is the ONLY definition of a
// tag's count and total. Create, update and the tag detail view all render
// through here, so a freshly written tag is described by the same SQL as one
// that has existed for a year.
func (s *Server) readTag(ctx context.Context, id uuid.UUID, identity auth.Identity) (tagResponse, error) {
	rows, err := s.Queries.ListTags(ctx, dbgen.ListTagsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		ID:          &id,
	})
	if err != nil {
		return tagResponse{}, err
	}
	if len(rows) == 0 {
		return tagResponse{}, pgx.ErrNoRows
	}
	return tagFromRow(rows[0]), nil
}

type upsertTagRequest struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
	// ExpectedAmount is a decimal STRING so it never passes through a JSON
	// float. An empty string or null means "no target".
	ExpectedAmount *string `json:"expected_amount"`
}

// parsed holds the validated, normalised fields shared by create and update.
type parsedTag struct {
	name        string
	description *string
	expected    decimal.NullDecimal
}

// parse validates and normalises the request, returning a human-readable
// message on the first problem for the handler to send as a 400.
func (req upsertTagRequest) parse() (parsedTag, string) {
	var p parsedTag

	p.name = strings.TrimSpace(req.Name)
	if p.name == "" {
		return p, "a tag name is required"
	}
	// The column is TEXT and the UI renders these as chips; a novel pasted into
	// one is a rendering problem on every surface that shows it.
	if len([]rune(p.name)) > 64 {
		return p, "a tag name must be 64 characters or fewer"
	}
	if req.Description != nil {
		p.description = nilIfEmpty(strings.TrimSpace(*req.Description))
	}

	if req.ExpectedAmount != nil {
		raw := strings.TrimSpace(*req.ExpectedAmount)
		if raw != "" {
			d, err := decimal.NewFromString(raw)
			if err != nil {
				return p, "expected_amount must be a decimal number, e.g. \"3000.00\""
			}
			// Zero is rejected rather than stored: an envelope with a target of
			// nothing is not a target, it is the absence of one, and that has
			// its own representation (null).
			if !d.IsPositive() {
				return p, "expected_amount must be greater than zero"
			}
			p.expected = decimal.NullDecimal{Decimal: d, Valid: true}
		}
	}
	return p, ""
}

func (s *Server) handleCreateTag(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req upsertTagRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, msg := req.parse()
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	id, err := s.Queries.CreateTag(r.Context(), dbgen.CreateTagParams{
		HouseholdID:    identity.HouseholdID,
		Name:           p.name,
		Description:    p.description,
		ExpectedAmount: p.expected,
	})
	if err != nil {
		// The unique index folds case, so "Vacation" collides with "vacation".
		// Saying so is the point: silently creating the second one would split
		// the envelope's total in half with no visible cause.
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "a tag with that name already exists")
			return
		}
		s.internalError(w, "create tag", err)
		return
	}

	created, err := s.readTag(r.Context(), id, identity)
	if err != nil {
		s.internalError(w, "read created tag", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateTag(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "tagID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid tag id")
		return
	}
	var req upsertTagRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, msg := req.parse()
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	// The household guard lives in the UPDATE, so an id from another household
	// matches nothing and comes back as ErrNoRows rather than as a silent no-op
	// the client would read as success.
	if _, err := s.Queries.UpdateTag(r.Context(), dbgen.UpdateTagParams{
		ID:             id,
		HouseholdID:    identity.HouseholdID,
		Name:           p.name,
		Description:    p.description,
		ExpectedAmount: p.expected,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "tag not found")
			return
		}
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "a tag with that name already exists")
			return
		}
		s.internalError(w, "update tag", err)
		return
	}

	updated, err := s.readTag(r.Context(), id, identity)
	if err != nil {
		s.internalError(w, "read updated tag", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteTag(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "tagID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid tag id")
		return
	}
	// transaction_tags cascades: the labels go, the transactions stay.
	rows, err := s.Queries.DeleteTag(r.Context(), dbgen.DeleteTagParams{
		ID: id, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "delete tag", err)
		return
	}
	if rows == 0 {
		writeError(w, http.StatusNotFound, "tag not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// tagTransactionResponse is one row of a tag's transaction list. A trimmed
// ledger row: enough to recognise the charge and open it, not the full
// transaction shape.
type tagTransactionResponse struct {
	ID          string     `json:"id"`
	Date        time.Time  `json:"date"`
	Name        string     `json:"name"`
	Merchant    string     `json:"merchant"`
	Amount      string     `json:"amount"`
	Currency    string     `json:"currency"`
	CategoryID  *uuid.UUID `json:"category_id"`
	AccountName string     `json:"account_name"`
}

// tagDetailResponse pairs the tag with its transactions, so the detail view
// renders the header and the list from one round trip — and the header's total
// and the list below it are read under the same visibility scope in the same
// request.
type tagDetailResponse struct {
	Tag          tagResponse              `json:"tag"`
	Transactions []tagTransactionResponse `json:"transactions"`
}

// tagTransactionLimit caps the detail list. A tag is an envelope, not a second
// ledger: past a few hundred rows the answer the user wants is the total, which
// the header already carries exactly.
const tagTransactionLimit = 500

func (s *Server) handleListTagTransactions(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "tagID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid tag id")
		return
	}

	tag, err := s.readTag(r.Context(), id, identity)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "tag not found")
			return
		}
		s.internalError(w, "read tag", err)
		return
	}

	rows, err := s.Queries.ListTagTransactions(r.Context(), dbgen.ListTagTransactionsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		TagID:       id,
		Lim:         tagTransactionLimit,
	})
	if err != nil {
		s.internalError(w, "list tag transactions", err)
		return
	}

	out := make([]tagTransactionResponse, 0, len(rows))
	for _, t := range rows {
		out = append(out, tagTransactionResponse{
			ID:          t.ID.String(),
			Date:        t.Date,
			Name:        t.Name,
			Merchant:    t.Merchant,
			Amount:      t.Amount.StringFixed(2),
			Currency:    t.Currency,
			CategoryID:  t.CategoryID,
			AccountName: t.AccountName,
		})
	}
	writeJSON(w, http.StatusOK, tagDetailResponse{Tag: tag, Transactions: out})
}

// setTransactionTagsRequest replaces one transaction's tag set.
//
// Replace rather than add/remove deltas, because that is what the editor in the
// UI actually is: a list of checkboxes the user confirms. A delta API would make
// the client responsible for computing the difference against a list it may have
// fetched a minute ago.
type setTransactionTagsRequest struct {
	TagIDs []uuid.UUID `json:"tag_ids"`
	// ApplyToMerchant additionally ADDS these tags to every visible transaction
	// sharing this row's merchant descriptor. It never removes: see
	// ApplyTagToMerchant in queries/tags.sql for why the merchant-wide action
	// differs from the single-row replace above.
	ApplyToMerchant bool `json:"apply_to_merchant"`
}

func (s *Server) handleSetTransactionTags(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	transactionID, err := uuid.Parse(chi.URLParam(r, "transactionID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid transaction id")
		return
	}
	var req setTransactionTagsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		s.internalError(w, "set transaction tags", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.Queries.WithTx(tx)

	// Resolve the transaction under the caller's own visibility BEFORE writing.
	// The tag statements each re-check it too, so this is not the enforcement —
	// it is what turns "you cannot see this row" into an honest 404 instead of a
	// write that silently affects nothing and reports success. It also gives us
	// the merchant key for the apply-to-merchant path.
	before, err := qtx.GetVisibleTransaction(ctx, dbgen.GetVisibleTransactionParams{
		ID:           transactionID,
		HouseholdID:  identity.HouseholdID,
		ViewerUserID: &identity.UserID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "transaction not found")
			return
		}
		s.internalError(w, "load transaction", err)
		return
	}

	if err := qtx.ClearTransactionTags(ctx, dbgen.ClearTransactionTagsParams{
		TransactionID: transactionID,
		HouseholdID:   identity.HouseholdID,
		UserID:        identity.UserID,
	}); err != nil {
		s.internalError(w, "clear transaction tags", err)
		return
	}

	for _, tagID := range req.TagIDs {
		rows, err := qtx.AddTransactionTag(ctx, dbgen.AddTransactionTagParams{
			TransactionID: transactionID,
			TagID:         tagID,
			HouseholdID:   identity.HouseholdID,
			UserID:        identity.UserID,
		})
		if err != nil {
			s.internalError(w, "add transaction tag", err)
			return
		}
		// Zero rows means the tag id is not this household's. Refusing the whole
		// request is deliberate: silently dropping it would leave the client
		// showing a tag the server never stored.
		if rows == 0 {
			writeError(w, http.StatusBadRequest, "unknown tag")
			return
		}

		if req.ApplyToMerchant && before.MerchantKey != nil && *before.MerchantKey != "" {
			if _, err := qtx.ApplyTagToMerchant(ctx, dbgen.ApplyTagToMerchantParams{
				TagID:       tagID,
				MerchantKey: before.MerchantKey,
				HouseholdID: identity.HouseholdID,
				UserID:      identity.UserID,
			}); err != nil {
				s.internalError(w, "apply tag to merchant", err)
				return
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		s.internalError(w, "set transaction tags", err)
		return
	}

	// Echo the resulting set, read back rather than reflected from the request,
	// so the row re-renders from what was actually stored.
	tags, err := s.tagsForTransactions(r.Context(), identity, []uuid.UUID{transactionID})
	if err != nil {
		s.internalError(w, "read transaction tags", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": tags[transactionID]})
}

// transactionTag is the chip shape a ledger row renders: id to act on, name to
// show. Deliberately not the full tagResponse — a row does not need the tag's
// all-time total, and computing fifty of them per page would be a report query
// per chip.
type transactionTag struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// tagsForTransactions reads the labels for a page of transactions in ONE query
// and groups them by transaction. The N+1 this avoids is the whole reason the
// bulk query exists: fifty rows must cost one round trip, not fifty.
//
// Every id in the returned map has a non-nil slice, so the caller never has to
// distinguish "no tags" from "not looked up" — and the JSON is [] rather than
// null.
func (s *Server) tagsForTransactions(ctx context.Context, identity auth.Identity,
	ids []uuid.UUID) (map[uuid.UUID][]transactionTag, error) {
	out := make(map[uuid.UUID][]transactionTag, len(ids))
	for _, id := range ids {
		out[id] = []transactionTag{}
	}
	if len(ids) == 0 {
		return out, nil
	}

	rows, err := s.Queries.ListTagsForTransactions(ctx, dbgen.ListTagsForTransactionsParams{
		TransactionIds: ids,
		HouseholdID:    identity.HouseholdID,
	})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.TransactionID] = append(out[row.TransactionID], transactionTag{
			ID:   row.TagID.String(),
			Name: row.Name,
		})
	}
	return out, nil
}
