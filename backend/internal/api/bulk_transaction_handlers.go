package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/audit"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// The multi-select endpoints: one action, applied to the set of rows a user
// ticked on the transactions list.
//
// They exist rather than the client looping the single-row endpoints because a
// selection is one decision. Five hundred requests would be five hundred chances
// to half-apply it, and no way to tell the user which half. Here the whole
// selection lands in one database transaction or none of it does, and the
// response says how many rows moved.
//
// What they are NOT is a way to reach further than the row-at-a-time endpoints
// already allow. Every one of them resolves the caller's id list through
// GetVisibleTransactions FIRST and writes only to what comes back, which is the
// same visibility rule GetVisibleTransaction gives the single-row handlers. An
// id from another household, or from a private account the caller cannot read,
// silently drops out of the set — the selection narrows, and `changed` says so,
// rather than the request failing or (far worse) succeeding.

// maxBulkTransactions bounds one bulk action. It matches the list endpoint's own
// maximum page size on purpose: the selection is made by ticking rows on a page,
// so it can never legitimately be larger than the largest page.
const maxBulkTransactions = 500

// bulkResultResponse reports how many rows the action actually reached. It can
// be smaller than the number of ids sent — rows outside the caller's visibility
// drop out, and an add that was already applied changes nothing. Both are normal,
// which is why this is a count rather than a success flag.
type bulkResultResponse struct {
	Changed int `json:"changed"`
}

type bulkTagsRequest struct {
	TransactionIDs []uuid.UUID `json:"transaction_ids"`
	TagIDs         []uuid.UUID `json:"tag_ids"`
	// Action is "add" or "remove". There is deliberately no "set": the single-row
	// editor can replace a row's whole tag set because that set is exactly what
	// the user just ticked, but a selection of fifty rows carries fifty different
	// sets and replacing across them would silently strip labels put there for
	// unrelated reasons. See AddTagToTransactions in queries/tags.sql.
	Action string `json:"action"`
}

type bulkCategoryRequest struct {
	TransactionIDs []uuid.UUID `json:"transaction_ids"`
	CategoryID     uuid.UUID   `json:"category_id"`
}

type bulkFlagsRequest struct {
	TransactionIDs []uuid.UUID `json:"transaction_ids"`
	// Both pointers, exactly as in the single-row transactionFlagsRequest: absent
	// means "leave alone", so either flag can be set across a selection without a
	// read-modify-write of the other.
	ExcludedFromReports *bool `json:"excluded_from_reports"`
	IsOneTime           *bool `json:"is_one_time"`
}

// validateBulkIDs rejects the two shapes no bulk action can mean anything for.
// It reports whether the request may continue, having already written the error.
func validateBulkIDs(w http.ResponseWriter, ids []uuid.UUID) bool {
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "no transactions given")
		return false
	}
	if len(ids) > maxBulkTransactions {
		writeError(w, http.StatusBadRequest, "too many transactions")
		return false
	}
	return true
}

// visibleTransactionIDs narrows a caller's id list to the rows it is actually
// allowed to write to. This is the authorisation step every handler below runs
// before touching anything, and the reason the SQL underneath can be a plain
// set-membership UPDATE.
func (s *Server) visibleTransactionIDs(ctx context.Context, q *dbgen.Queries,
	identity auth.Identity, ids []uuid.UUID) ([]uuid.UUID, error) {
	rows, err := q.GetVisibleTransactions(ctx, dbgen.GetVisibleTransactionsParams{
		TransactionIds: ids,
		HouseholdID:    identity.HouseholdID,
		ViewerUserID:   &identity.UserID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, t := range rows {
		out = append(out, t.ID)
	}
	return out, nil
}

// handleBulkTransactionTags adds or removes named tags across a selection.
//
// Accepts Plaid-synced rows for the same reason the single-row tag endpoint
// does: a synced hotel charge is exactly what "Summer Vacation" has to land on,
// and picking twelve of them off a page is the fastest way to say so.
func (s *Server) handleBulkTransactionTags(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req bulkTagsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validateBulkIDs(w, req.TransactionIDs) {
		return
	}
	if len(req.TagIDs) == 0 {
		writeError(w, http.StatusBadRequest, "no tags given")
		return
	}
	if req.Action != "add" && req.Action != "remove" {
		writeError(w, http.StatusBadRequest, `action must be "add" or "remove"`)
		return
	}

	ctx := r.Context()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		s.internalError(w, "bulk tag transactions", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.Queries.WithTx(tx)

	ids, err := s.visibleTransactionIDs(ctx, qtx, identity, req.TransactionIDs)
	if err != nil {
		s.internalError(w, "load transactions", err)
		return
	}
	if len(ids) == 0 {
		writeError(w, http.StatusNotFound, "no matching transactions")
		return
	}

	// Check the tags belong to this household BEFORE writing, rather than
	// inferring it from rows-written afterwards. The single-row handler can read
	// a zero-row insert as "unknown tag" because it is inserting exactly one
	// pair; here a zero is ambiguous between "no such tag" and "every selected
	// row already carried it", and treating the second as an error would make
	// applying the same action twice fail. Refusing the whole request on a
	// foreign id is the single-row behaviour, kept.
	known, err := qtx.CountHouseholdTags(ctx, dbgen.CountHouseholdTagsParams{
		TagIds:      req.TagIDs,
		HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "load tags", err)
		return
	}
	if int(known) != len(req.TagIDs) {
		writeError(w, http.StatusBadRequest, "unknown tag")
		return
	}

	// Rows touched, not (row, tag) pairs written: tagging twelve rows with two
	// tags changed twelve rows, which is the number the user is counting. Adding
	// the per-tag totals would report twenty-four and read as a bug.
	changed := 0
	for _, tagID := range req.TagIDs {
		var affected int64
		if req.Action == "add" {
			affected, err = qtx.AddTagToTransactions(ctx, dbgen.AddTagToTransactionsParams{
				TagID:          tagID,
				TransactionIds: ids,
				HouseholdID:    identity.HouseholdID,
				UserID:         identity.UserID,
			})
		} else {
			affected, err = qtx.RemoveTagFromTransactions(ctx, dbgen.RemoveTagFromTransactionsParams{
				TagID:          tagID,
				TransactionIds: ids,
				HouseholdID:    identity.HouseholdID,
				UserID:         identity.UserID,
			})
		}
		if err != nil {
			s.internalError(w, "bulk tag transactions", err)
			return
		}
		if int(affected) > changed {
			changed = int(affected)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		s.internalError(w, "bulk tag transactions", err)
		return
	}
	writeJSON(w, http.StatusOK, bulkResultResponse{Changed: changed})
}

// handleBulkTransactionCategory recategorises a selection.
//
// Unlike the tag and flag actions this writes change history, because the
// single-row recategorise does: a category is a fact about the transaction, and
// a household that finds one changed is entitled to see who changed it. Doing it
// in bulk is not a reason to lose that record, so one diff is written per row —
// the same rows the per-row endpoint would have written.
//
// Deliberately no apply_to_merchant. That switch means "and remember this for
// the merchant in future", which is a statement about ONE merchant; a selection
// spans many, and honouring it would write a durable rule per merchant from a
// gesture that never mentioned them.
func (s *Server) handleBulkTransactionCategory(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req bulkCategoryRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validateBulkIDs(w, req.TransactionIDs) {
		return
	}
	if req.CategoryID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "no category given")
		return
	}

	ctx := r.Context()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		s.internalError(w, "bulk recategorise", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.Queries.WithTx(tx)

	// The before-states double as the authorisation step, so this one read both
	// decides what may be written and supplies the diffs.
	before, err := qtx.GetVisibleTransactions(ctx, dbgen.GetVisibleTransactionsParams{
		TransactionIds: req.TransactionIDs,
		HouseholdID:    identity.HouseholdID,
		ViewerUserID:   &identity.UserID,
	})
	if err != nil {
		s.internalError(w, "load transactions", err)
		return
	}
	if len(before) == 0 {
		writeError(w, http.StatusNotFound, "no matching transactions")
		return
	}

	ids := make([]uuid.UUID, 0, len(before))
	priorByID := make(map[uuid.UUID]dbgen.Transaction, len(before))
	for _, t := range before {
		ids = append(ids, t.ID)
		priorByID[t.ID] = t
	}

	updated, err := qtx.SetTransactionsCategory(ctx, dbgen.SetTransactionsCategoryParams{
		CategoryID:     &req.CategoryID,
		TransactionIds: ids,
		HouseholdID:    identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "bulk recategorise", err)
		return
	}

	for _, after := range updated {
		prior, ok := priorByID[after.ID]
		if !ok {
			continue
		}
		if err := audit.Record(ctx, qtx, audit.RecordParams{
			HouseholdID: identity.HouseholdID,
			ObjectKind:  audit.KindTransaction,
			ObjectID:    after.ID,
			ActorUserID: identity.UserID,
		}, audit.TransactionDiff(prior, after)); err != nil {
			s.internalError(w, "record transaction change", err)
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		s.internalError(w, "bulk recategorise", err)
		return
	}
	writeJSON(w, http.StatusOK, bulkResultResponse{Changed: len(updated)})
}

// handleBulkTransactionFlags sets how a selection COUNTS — the bulk form of
// PATCH /{transactionID}/flags, and the fast way to take a whole month of
// one-off charges out of the trailing averages.
//
// Writes no change history, matching the single-row endpoint: these flags are
// judgements about how a row is read, not edits to what it says.
func (s *Server) handleBulkTransactionFlags(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req bulkFlagsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validateBulkIDs(w, req.TransactionIDs) {
		return
	}
	if req.ExcludedFromReports == nil && req.IsOneTime == nil {
		writeError(w, http.StatusBadRequest, "no flags given")
		return
	}

	ctx := r.Context()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		s.internalError(w, "bulk set flags", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.Queries.WithTx(tx)

	ids, err := s.visibleTransactionIDs(ctx, qtx, identity, req.TransactionIDs)
	if err != nil {
		s.internalError(w, "load transactions", err)
		return
	}
	if len(ids) == 0 {
		writeError(w, http.StatusNotFound, "no matching transactions")
		return
	}

	changed, err := qtx.SetTransactionsFlags(ctx, dbgen.SetTransactionsFlagsParams{
		ExcludedFromReports: req.ExcludedFromReports,
		IsOneTime:           req.IsOneTime,
		TransactionIds:      ids,
		HouseholdID:         identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "bulk set flags", err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		s.internalError(w, "bulk set flags", err)
		return
	}
	writeJSON(w, http.StatusOK, bulkResultResponse{Changed: int(changed)})
}
