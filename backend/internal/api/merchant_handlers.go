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
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/jobs"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/merchants"
)

// merchantAliasResponse is one raw descriptor mapped to a canonical merchant,
// carrying the evidence a person needs to judge a proposed merge. Two strings
// alone are not enough to decide with — "is this the same business?" is really
// "does this spend look like the same business?" — so counts, totals and the
// date range travel with them.
type merchantAliasResponse struct {
	MerchantKey string          `json:"merchant_key"`
	SampleName  string          `json:"sample_name"`
	Source      string          `json:"source"`
	Confidence  *float64        `json:"confidence"`
	Count       int64           `json:"transaction_count"`
	Total       decimal.Decimal `json:"total_amount"`
	FirstSeen   *string         `json:"first_seen"`
	LastSeen    *string         `json:"last_seen"`
}

type merchantEntityResponse struct {
	ID            uuid.UUID  `json:"id"`
	CanonicalName string     `json:"canonical_name"`
	CategoryID    *uuid.UUID `json:"default_category_id"`
	// Members are the descriptors already merged into this merchant.
	Members []merchantAliasResponse `json:"members"`
	// Suggested are descriptors proposed for it and awaiting review. They affect
	// nothing until confirmed — no total on this page or any other includes them.
	Suggested []merchantAliasResponse `json:"suggested"`
	Count     int64                   `json:"transaction_count"`
	Total     decimal.Decimal         `json:"total_amount"`
}

func dateString(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.DateOnly)
	return &s
}

func aliasResponse(row dbgen.ListMerchantAliasStatsRow) merchantAliasResponse {
	out := merchantAliasResponse{
		MerchantKey: row.MerchantKey,
		SampleName:  row.SampleName,
		Source:      row.Source,
		Count:       row.TransactionCount,
		Total:       row.TotalAmount.Round(2),
		FirstSeen:   dateString(row.FirstSeen),
		LastSeen:    dateString(row.LastSeen),
	}
	if row.Confidence.Valid {
		c, _ := row.Confidence.Decimal.Float64()
		out.Confidence = &c
	}
	return out
}

// handleListMerchants returns the household's canonical merchants and the
// pending merge suggestions, in one payload. They come from one query and the
// UI shows them on one page, so splitting them across two endpoints would only
// mean two round trips to render a screen.
func (s *Server) handleListMerchants(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListMerchantAliasStats(r.Context(), dbgen.ListMerchantAliasStatsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list merchants", err)
		return
	}

	byEntity := make(map[uuid.UUID]*merchantEntityResponse)
	order := make([]uuid.UUID, 0)
	for _, row := range rows {
		entity, ok := byEntity[row.EntityID]
		if !ok {
			entity = &merchantEntityResponse{
				ID:            row.EntityID,
				CanonicalName: row.CanonicalName,
				CategoryID:    row.DefaultCategoryID,
				Members:       make([]merchantAliasResponse, 0, 2),
				Suggested:     make([]merchantAliasResponse, 0),
				Total:         decimal.Zero,
			}
			byEntity[row.EntityID] = entity
			order = append(order, row.EntityID)
		}

		alias := aliasResponse(row)
		if row.Source == "suggested" {
			entity.Suggested = append(entity.Suggested, alias)
			continue
		}
		// Only confirmed members contribute to the merchant's totals. A
		// suggestion that inflated the number it is asking you to approve would
		// be arguing its own case.
		entity.Members = append(entity.Members, alias)
		entity.Count += alias.Count
		entity.Total = entity.Total.Add(alias.Total)
	}

	out := make([]merchantEntityResponse, 0, len(order))
	for _, id := range order {
		out = append(out, *byEntity[id])
	}
	writeJSON(w, http.StatusOK, out)
}

// handleListMerchantKeys returns every descriptor the household has, with its
// current mapping — the picker behind "merge these by hand".
func (s *Server) handleListMerchantKeys(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListMerchantKeyStats(r.Context(), dbgen.ListMerchantKeyStatsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
	})
	if err != nil {
		s.internalError(w, "list merchant keys", err)
		return
	}

	type keyResponse struct {
		MerchantKey string          `json:"merchant_key"`
		SampleName  string          `json:"sample_name"`
		Count       int64           `json:"transaction_count"`
		Total       decimal.Decimal `json:"total_amount"`
		FirstSeen   string          `json:"first_seen"`
		LastSeen    string          `json:"last_seen"`
		EntityID    *uuid.UUID      `json:"entity_id"`
		AliasSource string          `json:"alias_source"`
	}

	out := make([]keyResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, keyResponse{
			MerchantKey: row.MerchantKey,
			SampleName:  row.SampleName,
			Count:       row.TransactionCount,
			Total:       row.TotalAmount.Round(2),
			FirstSeen:   row.FirstSeen.Format(time.DateOnly),
			LastSeen:    row.LastSeen.Format(time.DateOnly),
			EntityID:    row.EntityID,
			AliasSource: row.AliasSource,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// mergeRequest merges descriptors into one canonical merchant. entity_id targets
// an existing merchant (confirming a suggestion, or adding a stray descriptor);
// omitting it creates a new one named by canonical_name.
type mergeRequest struct {
	Keys          []string   `json:"merchant_keys"`
	EntityID      *uuid.UUID `json:"entity_id"`
	CanonicalName string     `json:"canonical_name"`
	// RejectKeys are descriptors from the SAME proposal that do not belong to this
	// merchant. Confirming part of a proposal is only half an answer: the other
	// half is that the rest is something else, and saying so is what stops the next
	// pass proposing the identical grouping again.
	//
	// Sending them here rather than as a second request keeps the pair of
	// statements atomic. It also has to happen before the merge, while the entity
	// still holds every proposed descriptor, so Reject can record the refusal
	// against each descriptor that stays.
	RejectKeys []string `json:"reject_keys"`
}

type mergeResponse struct {
	EntityID uuid.UUID `json:"entity_id"`
	// CategoryConflict is true when the merged descriptors carried two different
	// MANUAL categories. Nothing was changed; the user picks.
	CategoryConflict bool `json:"category_conflict"`
	// CategoryApplied is the category every descriptor now resolves to, when
	// reconciliation settled on one.
	CategoryApplied *uuid.UUID `json:"category_applied"`
	// CategoryFromManual records that the surviving category was a manual choice,
	// so the UI can say the merge kept it rather than guessing.
	CategoryFromManual bool `json:"category_from_manual"`
}

func (s *Server) handleMergeMerchants(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req mergeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if len(req.RejectKeys) > 0 && req.EntityID == nil {
		writeError(w, http.StatusBadRequest, "reject_keys only applies when confirming an existing proposal")
		return
	}

	ctx := r.Context()

	// One transaction, so a partial confirm cannot half-apply. Splitting a
	// proposal is two edits that only make sense together: landing the merge
	// without the refusal would re-propose the same wrong grouping next pass, and
	// landing the refusal without the merge would discard the descriptors the user
	// actually wanted grouped.
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		s.internalError(w, "merge merchants", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.Queries.WithTx(tx)

	if len(req.RejectKeys) > 0 {
		if err := merchants.Reject(ctx, qtx, identity.HouseholdID, *req.EntityID, req.RejectKeys); err != nil {
			s.internalError(w, "reject merchant suggestion", err)
			return
		}
	}

	entityID, outcome, err := merchants.Merge(
		ctx, qtx, identity.HouseholdID, req.Keys, req.CanonicalName, req.EntityID,
	)
	if errors.Is(err, merchants.ErrNoKeys) {
		writeError(w, http.StatusBadRequest, "a merchant needs at least one descriptor")
		return
	}
	if err != nil {
		s.internalError(w, "merge merchants", err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		s.internalError(w, "merge merchants", err)
		return
	}

	writeJSON(w, http.StatusOK, mergeResponse{
		EntityID:           entityID,
		CategoryConflict:   outcome.Conflict,
		CategoryApplied:    outcome.Applied,
		CategoryFromManual: outcome.FromManual,
	})
}

type rejectRequest struct {
	Keys []string `json:"merchant_keys"`
}

// handleRejectMerchantSuggestion dismisses proposed descriptors and remembers
// the refusal, so the same merge is never proposed twice.
func (s *Server) handleRejectMerchantSuggestion(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	entityID, err := uuid.Parse(chi.URLParam(r, "merchantID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid merchant id")
		return
	}

	var req rejectRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if len(req.Keys) == 0 {
		writeError(w, http.StatusBadRequest, "no descriptors given")
		return
	}

	if err := merchants.Reject(r.Context(), s.Queries, identity.HouseholdID, entityID, req.Keys); err != nil {
		s.internalError(w, "reject merchant suggestion", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type splitRequest struct {
	Key string `json:"merchant_key"`
}

// handleSplitMerchant detaches a descriptor from its merchant. An over-eager
// merge has to be undoable, and undoing it also records that these two are not
// the same business — otherwise the next suggestion pass proposes it again.
func (s *Server) handleSplitMerchant(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req splitRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if req.Key == "" {
		writeError(w, http.StatusBadRequest, "no descriptor given")
		return
	}

	if err := merchants.Split(r.Context(), s.Queries, identity.HouseholdID, req.Key); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type renameMerchantRequest struct {
	CanonicalName string `json:"canonical_name"`
}

func (s *Server) handleRenameMerchant(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	entityID, err := uuid.Parse(chi.URLParam(r, "merchantID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid merchant id")
		return
	}

	var req renameMerchantRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	entity, err := merchants.Rename(r.Context(), s.Queries, identity.HouseholdID, entityID, req.CanonicalName)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, merchantEntityResponse{
		ID:            entity.ID,
		CanonicalName: entity.CanonicalName,
		CategoryID:    entity.DefaultCategoryID,
		Members:       make([]merchantAliasResponse, 0),
		Suggested:     make([]merchantAliasResponse, 0),
		Total:         decimal.Zero,
	})
}

// handleScanMerchants runs a suggestion pass now instead of waiting for the
// daily sweep. The pass only ever writes inert suggestions, so this is safe to
// expose as a plain button.
func (s *Server) handleScanMerchants(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	if err := jobs.EnqueueSuggestMerchants(r.Context(), s.Jobs, identity.HouseholdID); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// --- Merchant detail ------------------------------------------------------

// merchantDetailResponse is one merchant's whole story for a period: the numbers,
// the shape over time, how it is filed, and the charges behind all three.
//
// Bundled into a single response rather than four endpoints because the page shows
// all of it at once, and four round trips would let the four halves of one answer
// disagree with each other while they loaded.
type merchantDetailResponse struct {
	// MerchantKey is the resolved key the caller asked about, echoed back so the
	// page can build links without re-deriving it.
	MerchantKey string `json:"merchant_key"`
	Merchant    string `json:"merchant"`
	// IsGrouped reports whether this is a merged merchant rather than a lone
	// descriptor, which is what decides whether the page offers to manage the
	// grouping.
	IsGrouped bool `json:"is_grouped"`
	// Descriptors are the raw keys resolving here — one for an ungrouped merchant.
	Descriptors []string `json:"descriptors"`

	From string `json:"from"`
	To   string `json:"to"`

	Total            decimal.Decimal `json:"total"`
	TransactionCount int64           `json:"transaction_count"`
	Average          decimal.Decimal `json:"average"`
	Largest          decimal.Decimal `json:"largest"`
	FirstSeen        *string         `json:"first_seen"`
	LastSeen         *string         `json:"last_seen"`

	Monthly      []merchantMonthPoint    `json:"monthly"`
	Categories   []merchantCategorySlice `json:"categories"`
	Transactions []merchantDetailTxn     `json:"transactions"`
}

type merchantMonthPoint struct {
	Month            string          `json:"month"`
	Total            decimal.Decimal `json:"total"`
	TransactionCount int64           `json:"transaction_count"`
}

// merchantCategorySlice is shaped to match the category-spend rows the rest of
// the reporting API returns, so the frontend feeds them straight into the existing
// CategoryBars chart instead of growing a second one.
type merchantCategorySlice struct {
	CategoryID       uuid.UUID       `json:"category_id"`
	Name             string          `json:"name"`
	Slug             string          `json:"slug"`
	Color            *string         `json:"color"`
	IsFixed          bool            `json:"is_fixed"`
	Total            decimal.Decimal `json:"total"`
	TransactionCount int64           `json:"transaction_count"`
}

type merchantDetailTxn struct {
	ID           uuid.UUID       `json:"id"`
	Date         string          `json:"date"`
	Amount       decimal.Decimal `json:"amount"`
	Descriptor   string          `json:"descriptor"`
	RawKey       *string         `json:"raw_merchant_key"`
	AccountName  string          `json:"account_name"`
	CategoryName *string         `json:"category_name"`
	CategoryID   *uuid.UUID      `json:"category_id"`
}

// merchantDetailTxnLimit caps the transaction list. The summary above it is exact
// over every match, so a long history is truncated in the list without the totals
// going wrong.
const merchantDetailTxnLimit = 500

// handleMerchantDetail reports one merchant over a period.
//
// The key is a RESOLVED merchant key — an entity UUID as text for a merged
// merchant, the raw descriptor otherwise — which is what GetTopMerchants and the
// recurring detector already hand out. That choice is what makes this work for
// ungrouped merchants, and those are the majority of a household's spending.
//
// It arrives as a query parameter rather than a path segment because a raw
// descriptor can contain a slash ("park/the reserve fdesk") and a path parameter
// cannot carry one.
func (s *Server) handleMerchantDetail(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		writeError(w, http.StatusBadRequest, "a merchant key is required")
		return
	}
	from, to := period(r)

	identityRow, err := s.Queries.GetMerchantIdentity(ctx, dbgen.GetMerchantIdentityParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		ResolvedKey: key,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no such merchant")
		return
	}
	if err != nil {
		s.internalError(w, "merchant identity", err)
		return
	}

	summary, err := s.Queries.GetMerchantSummary(ctx, dbgen.GetMerchantSummaryParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
		ResolvedKey: key,
	})
	if err != nil {
		s.internalError(w, "merchant summary", err)
		return
	}

	months, err := s.Queries.GetMerchantMonthlySpend(ctx, dbgen.GetMerchantMonthlySpendParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
		ResolvedKey: key,
	})
	if err != nil {
		s.internalError(w, "merchant monthly spend", err)
		return
	}

	cats, err := s.Queries.GetMerchantCategoryBreakdown(ctx, dbgen.GetMerchantCategoryBreakdownParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
		ResolvedKey: key,
	})
	if err != nil {
		s.internalError(w, "merchant category breakdown", err)
		return
	}

	txns, err := s.Queries.ListMerchantTransactions(ctx, dbgen.ListMerchantTransactionsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		Date:        from,
		Date_2:      to,
		ResolvedKey: key,
		Lim:         merchantDetailTxnLimit,
	})
	if err != nil {
		s.internalError(w, "merchant transactions", err)
		return
	}

	out := merchantDetailResponse{
		MerchantKey:      key,
		Merchant:         identityRow.Merchant,
		IsGrouped:        identityRow.IsGrouped,
		Descriptors:      identityRow.Descriptors,
		From:             from.Format(time.DateOnly),
		To:               to.Format(time.DateOnly),
		Total:            summary.Total,
		TransactionCount: summary.TransactionCount,
		Average:          summary.Average,
		Largest:          summary.Largest,
		FirstSeen:        emptyToNil(summary.FirstSeen),
		LastSeen:         emptyToNil(summary.LastSeen),
		Monthly:          make([]merchantMonthPoint, 0, len(months)),
		Categories:       make([]merchantCategorySlice, 0, len(cats)),
		Transactions:     make([]merchantDetailTxn, 0, len(txns)),
	}

	for _, m := range months {
		out.Monthly = append(out.Monthly, merchantMonthPoint{
			Month:            m.Month.Format(time.DateOnly),
			Total:            m.Total,
			TransactionCount: m.TransactionCount,
		})
	}
	for _, c := range cats {
		out.Categories = append(out.Categories, merchantCategorySlice{
			CategoryID:       c.CategoryID,
			Name:             c.CategoryName,
			Slug:             c.CategorySlug,
			Color:            c.CategoryColor,
			IsFixed:          c.IsFixed,
			Total:            c.Total,
			TransactionCount: c.TransactionCount,
		})
	}
	for _, t := range txns {
		out.Transactions = append(out.Transactions, merchantDetailTxn{
			ID:           t.ID,
			Date:         t.Date.Format(time.DateOnly),
			Amount:       t.Amount,
			Descriptor:   t.Descriptor,
			RawKey:       t.RawMerchantKey,
			AccountName:  t.AccountName,
			CategoryName: t.CategoryName,
			CategoryID:   t.CategoryID,
		})
	}

	writeJSON(w, http.StatusOK, out)
}

// emptyToNil turns the empty-string sentinel the summary query returns for "no
// matching charges" back into a JSON null. The query coalesces to ” because
// MIN/MAX over an empty match are NULL and returning ISO text keeps the column
// non-null in the generated code; the API still wants an absent date, not "".
func emptyToNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
