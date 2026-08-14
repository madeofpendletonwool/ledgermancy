package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Transaction links: an explicit, typed connection between two transactions.
//
// WHAT THIS FILE MUST NOT BECOME. A link is an ANNOTATION. Nothing here writes
// to `transactions` — not the amount, not the date, not the category, not a
// flag. "This $80 credit refunds that $80 charge" is a statement ABOUT two rows,
// and the moment it starts editing one of them the user loses the ability to
// take it back, and the ledger stops matching the bank. The only thing a link
// changes is what a REPORT is asked to compute, and only when the reader asks:
// see `net_refunds` in queries/reports.sql and netRefundsRequested in
// report_handlers.go.
//
// DIRECTION IS THE HARD PART. A link type carries two phrasings of one edge —
// `outward` as the source reads it ("refunds"), `inward` as the target reads it
// ("is refunded by") — and one row serves both. Everything in this file that
// looks like duplication between the two directions is the same edge being read
// from its other end, never a second edge.
//
// SCOPE. A link names two transactions, so every statement scopes BOTH through
// account_access; see the header of queries/transaction_links.sql for what that
// buys and what a member of the same household therefore cannot see.

// linkTypeResponse is one relationship the household may use.
type linkTypeResponse struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	// Outward is how the SOURCE end reads ("refunds"); Inward is how the TARGET
	// end reads ("is refunded by"). A symmetric relationship has the same string
	// twice, which is a property of the vocabulary and not a special case.
	Outward string `json:"outward"`
	Inward  string `json:"inward"`
	// NetsSpend is true when a link of this type lets the source's inflow cancel
	// the target's spending in a report the reader has asked to net. Read-only:
	// only the seeded `refund` type has it, and nothing a household creates ever
	// will. See migration 00067 for why that is not a settable option.
	NetsSpend bool `json:"nets_spend"`
	// IsSystem marks one of the three shipped relationships, which cannot be
	// renamed or deleted — worth knowing before offering an edit affordance.
	IsSystem  bool   `json:"is_system"`
	CreatedAt string `json:"created_at"`
}

func linkTypeFromRow(row dbgen.ListLinkTypesRow) linkTypeResponse {
	return linkTypeResponse{
		ID:        row.ID.String(),
		Slug:      row.Slug,
		Name:      row.Name,
		Outward:   row.Outward,
		Inward:    row.Inward,
		NetsSpend: row.NetsSpend,
		IsSystem:  row.IsSystem,
		CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func (s *Server) handleListLinkTypes(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	hh := identity.HouseholdID
	rows, err := s.Queries.ListLinkTypes(r.Context(), dbgen.ListLinkTypesParams{HouseholdID: &hh})
	if err != nil {
		s.internalError(w, "list link types", err)
		return
	}
	out := make([]linkTypeResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, linkTypeFromRow(row))
	}
	writeJSON(w, http.StatusOK, out)
}

// readLinkType re-reads one type through ListLinkTypes, which is the only
// definition of what a link type looks like on the wire — so a type created a
// second ago renders through the same SQL as one that shipped with the app.
func (s *Server) readLinkType(ctx context.Context, id uuid.UUID, identity auth.Identity) (linkTypeResponse, error) {
	hh := identity.HouseholdID
	rows, err := s.Queries.ListLinkTypes(ctx, dbgen.ListLinkTypesParams{HouseholdID: &hh, ID: &id})
	if err != nil {
		return linkTypeResponse{}, err
	}
	if len(rows) == 0 {
		return linkTypeResponse{}, pgx.ErrNoRows
	}
	return linkTypeFromRow(rows[0]), nil
}

// upsertLinkTypeRequest creates or edits a household's own relationship.
//
// nets_spend is absent on purpose and is not merely ignored — decodeJSON
// rejects unknown fields, so a client that tries to send it gets a 400 rather
// than a silent no-op. A household-defined relationship never moves a reported
// figure.
type upsertLinkTypeRequest struct {
	Name    string `json:"name"`
	Outward string `json:"outward"`
	Inward  string `json:"inward"`
}

type parsedLinkType struct {
	name    string
	outward string
	inward  string
}

// linkTypePhraseLimit caps every string on a link type. These are rendered
// inline in a sentence next to a transaction ("refunds Home Depot · $80"), so a
// paragraph in one is a layout problem on every surface that shows a link.
const linkTypePhraseLimit = 64

// parse validates and normalises, returning a human-readable message on the
// first problem for the handler to send as a 400.
func (req upsertLinkTypeRequest) parse() (parsedLinkType, string) {
	var p parsedLinkType

	fields := []struct {
		label string
		raw   string
		dst   *string
	}{
		{"a name", req.Name, &p.name},
		{"an outward phrase (how the first transaction reads, e.g. \"refunds\")", req.Outward, &p.outward},
		{"an inward phrase (how the second reads, e.g. \"is refunded by\")", req.Inward, &p.inward},
	}
	for _, f := range fields {
		v := strings.TrimSpace(f.raw)
		if v == "" {
			return p, "a link type needs " + f.label
		}
		if len([]rune(v)) > linkTypePhraseLimit {
			return p, fmt.Sprintf("%s must be %d characters or fewer", f.label, linkTypePhraseLimit)
		}
		*f.dst = v
	}
	return p, ""
}

// shadowsSystemLinkType reports whether a name collides, case-folded, with one
// of the three shipped relationships.
//
// The unique index cannot answer this: a system row's household_id is NULL and a
// household's is not, so they are different keys and both "Refund"s may exist as
// far as Postgres is concerned. See CountSystemLinkTypesByName in
// queries/transaction_links.sql for why the duplicate label is worth a query.
func (s *Server) shadowsSystemLinkType(ctx context.Context, name string) (bool, error) {
	n, err := s.Queries.CountSystemLinkTypesByName(ctx, name)
	return n > 0, err
}

// uniqueLinkTypeSlug derives a slug from the name and appends -2, -3, … until it
// is free. A slug used by a system type counts as taken, so a household cannot
// create something that resolves to `refund` — the slug the netting query keys
// on. Same shape as uniqueCategorySlug.
func (s *Server) uniqueLinkTypeSlug(ctx context.Context, householdID uuid.UUID, name string) (string, error) {
	base := slugify(name)
	if base == "" {
		base = "link"
	}
	candidate := base
	for i := 2; ; i++ {
		n, err := s.Queries.CountLinkTypesBySlug(ctx, dbgen.CountLinkTypesBySlugParams{
			Slug: candidate, HouseholdID: &householdID,
		})
		if err != nil {
			return "", err
		}
		if n == 0 {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
}

func (s *Server) handleCreateLinkType(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req upsertLinkTypeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, msg := req.parse()
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	shadows, err := s.shadowsSystemLinkType(r.Context(), p.name)
	if err != nil {
		s.internalError(w, "check system link type names", err)
		return
	}
	if shadows {
		writeError(w, http.StatusConflict, "a link type with that name already exists")
		return
	}

	slug, err := s.uniqueLinkTypeSlug(r.Context(), identity.HouseholdID, p.name)
	if err != nil {
		s.internalError(w, "derive link type slug", err)
		return
	}

	hh := identity.HouseholdID
	id, err := s.Queries.CreateLinkType(r.Context(), dbgen.CreateLinkTypeParams{
		HouseholdID: &hh,
		Slug:        slug,
		Name:        p.name,
		Outward:     p.outward,
		Inward:      p.inward,
	})
	if err != nil {
		// Name uniqueness folds case, and the system types are in the same
		// namespace: "Refund" collides with the shipped one. Saying so beats
		// creating a second relationship the picker lists twice.
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "a link type with that name already exists")
			return
		}
		s.internalError(w, "create link type", err)
		return
	}

	created, err := s.readLinkType(r.Context(), id, identity)
	if err != nil {
		s.internalError(w, "read created link type", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateLinkType(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "linkTypeID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid link type id")
		return
	}
	var req upsertLinkTypeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, msg := req.parse()
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	// A rename can shadow a system name just as a create can.
	shadows, err := s.shadowsSystemLinkType(r.Context(), p.name)
	if err != nil {
		s.internalError(w, "check system link type names", err)
		return
	}
	if shadows {
		writeError(w, http.StatusConflict, "a link type with that name already exists")
		return
	}

	// The household guard in the UPDATE is what makes the three system types
	// immutable: their household_id is NULL and never equals the caller's, so
	// they match no rows and fall out here as a 404.
	hh := identity.HouseholdID
	if _, err := s.Queries.UpdateLinkType(r.Context(), dbgen.UpdateLinkTypeParams{
		ID:          id,
		HouseholdID: &hh,
		Name:        p.name,
		Outward:     p.outward,
		Inward:      p.inward,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "link type not found")
			return
		}
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "a link type with that name already exists")
			return
		}
		s.internalError(w, "update link type", err)
		return
	}

	updated, err := s.readLinkType(r.Context(), id, identity)
	if err != nil {
		s.internalError(w, "read updated link type", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteLinkType(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "linkTypeID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid link type id")
		return
	}
	// transaction_links cascades: the edges go, both transactions stay exactly
	// as they were. Deleting a relationship can therefore never destroy money
	// data, only the statements someone made about it.
	hh := identity.HouseholdID
	rows, err := s.Queries.DeleteLinkType(r.Context(), dbgen.DeleteLinkTypeParams{
		ID: id, HouseholdID: &hh,
	})
	if err != nil {
		s.internalError(w, "delete link type", err)
		return
	}
	if rows == 0 {
		writeError(w, http.StatusNotFound, "link type not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// linkedTransaction is the far end of a link as the panel renders it: enough to
// recognise the charge and open it. The same trimmed shape a tag's transaction
// list uses, and deliberately not the full transaction — a link panel has no use
// for the other row's tags or Plaid categories.
type linkedTransaction struct {
	ID          string     `json:"id"`
	Date        time.Time  `json:"date"`
	Name        string     `json:"name"`
	Merchant    string     `json:"merchant"`
	Amount      string     `json:"amount"`
	Currency    string     `json:"currency"`
	CategoryID  *uuid.UUID `json:"category_id"`
	AccountName string     `json:"account_name"`
}

// transactionLinkResponse is one edge as seen FROM the transaction in the URL.
//
// The same stored row yields a different Direction and a different Relation
// depending on which end it is read from, and that is the point: the client
// renders "refunds …" or "is refunded by …" without knowing which column of the
// link type to reach for, and the two readings cannot drift because there is
// only one row behind them.
type transactionLinkResponse struct {
	ID           string `json:"id"`
	LinkTypeID   string `json:"link_type_id"`
	LinkTypeSlug string `json:"link_type_slug"`
	LinkTypeName string `json:"link_type_name"`
	// Direction is "outward" when the transaction in the URL is the link's
	// source, "inward" when it is the target.
	Direction string `json:"direction"`
	// Relation is the verb already oriented for this end — the outward phrase or
	// the inward one, chosen in SQL.
	Relation string `json:"relation"`
	// NetsSpend says whether this link is one the netting view acts on, so the
	// panel can explain why a figure moved.
	NetsSpend   bool              `json:"nets_spend"`
	Transaction linkedTransaction `json:"transaction"`
	CreatedAt   string            `json:"created_at"`
}

const (
	directionOutward = "outward"
	directionInward  = "inward"
)

func transactionLinkFromRow(row dbgen.ListTransactionLinksRow) transactionLinkResponse {
	direction := directionInward
	if row.IsOutward {
		direction = directionOutward
	}
	return transactionLinkResponse{
		ID:           row.ID.String(),
		LinkTypeID:   row.LinkTypeID.String(),
		LinkTypeSlug: row.LinkTypeSlug,
		LinkTypeName: row.LinkTypeName,
		Direction:    direction,
		Relation:     row.Relation,
		NetsSpend:    row.NetsSpend,
		Transaction: linkedTransaction{
			ID:          row.OtherTransactionID.String(),
			Date:        row.OtherDate,
			Name:        row.OtherName,
			Merchant:    row.OtherMerchant,
			Amount:      row.OtherAmount.StringFixed(2),
			Currency:    row.OtherCurrency,
			CategoryID:  row.OtherCategoryID,
			AccountName: row.OtherAccountName,
		},
		CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// linksFor reads one transaction's links in both directions. The single
// definition of what a link looks like on the wire: list, create and delete all
// respond through it, so a freshly written link is described by the same SQL as
// one made a year ago.
func (s *Server) linksFor(ctx context.Context, id uuid.UUID, identity auth.Identity) ([]transactionLinkResponse, error) {
	rows, err := s.Queries.ListTransactionLinks(ctx, dbgen.ListTransactionLinksParams{
		TransactionID: id,
		HouseholdID:   identity.HouseholdID,
		UserID:        identity.UserID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]transactionLinkResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, transactionLinkFromRow(row))
	}
	return out, nil
}

// visibleTransaction resolves one transaction under the caller's own visibility.
//
// The link statements each re-check both ends themselves, so this is not the
// enforcement — it is what turns "you cannot see this row" into an honest 404
// instead of a write that silently affects nothing and reports success. Same
// reasoning as handleSetTransactionTags.
func (s *Server) visibleTransaction(ctx context.Context, id uuid.UUID, identity auth.Identity) error {
	_, err := s.Queries.GetVisibleTransaction(ctx, dbgen.GetVisibleTransactionParams{
		ID:           id,
		HouseholdID:  identity.HouseholdID,
		ViewerUserID: &identity.UserID,
	})
	return err
}

func (s *Server) handleListTransactionLinks(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "transactionID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid transaction id")
		return
	}
	// Resolve the anchor first so an invisible transaction is a 404 rather than
	// an empty list, which the client would render as "no links yet".
	if err := s.visibleTransaction(r.Context(), id, identity); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "transaction not found")
			return
		}
		s.internalError(w, "load transaction", err)
		return
	}

	links, err := s.linksFor(r.Context(), id, identity)
	if err != nil {
		s.internalError(w, "list transaction links", err)
		return
	}
	writeJSON(w, http.StatusOK, links)
}

// createTransactionLinkRequest connects the transaction in the URL to another.
//
// Direction is stated rather than inferred. "This credit refunds that charge"
// and "this charge is refunded by that credit" describe the same pair and the
// opposite edge, and getting it backwards makes the netting view subtract the
// charge from the credit. Defaults to outward, which is how the picker phrases
// it: the row you are standing on <verb> the row you chose.
type createTransactionLinkRequest struct {
	// TransactionID is the OTHER end. The anchor is the one in the URL.
	TransactionID uuid.UUID `json:"transaction_id"`
	LinkTypeID    uuid.UUID `json:"link_type_id"`
	Direction     string    `json:"direction"`
}

func (s *Server) handleCreateTransactionLink(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	anchorID, err := uuid.Parse(chi.URLParam(r, "transactionID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid transaction id")
		return
	}
	var req createTransactionLinkRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	direction := strings.TrimSpace(req.Direction)
	if direction == "" {
		direction = directionOutward
	}
	if direction != directionOutward && direction != directionInward {
		writeError(w, http.StatusBadRequest, `direction must be "outward" or "inward"`)
		return
	}
	// The CHECK constraint catches this too. Catching it here is about the
	// message: a self-link is a mis-click, not a server error, and "a
	// transaction cannot be linked to itself" says what went wrong.
	if req.TransactionID == anchorID {
		writeError(w, http.StatusBadRequest, "a transaction cannot be linked to itself")
		return
	}

	ctx := r.Context()

	// Resolve all three inputs separately, purely so the error can say WHICH one
	// was wrong. CreateTransactionLink re-checks every one of them and is the
	// actual enforcement — see queries/transaction_links.sql.
	for _, t := range []struct {
		id  uuid.UUID
		msg string
	}{
		{anchorID, "transaction not found"},
		{req.TransactionID, "the transaction you linked to was not found"},
	} {
		if err := s.visibleTransaction(ctx, t.id, identity); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, t.msg)
				return
			}
			s.internalError(w, "load transaction", err)
			return
		}
	}
	if _, err := s.readLinkType(ctx, req.LinkTypeID, identity); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusBadRequest, "unknown link type")
			return
		}
		s.internalError(w, "load link type", err)
		return
	}

	// Outward means the anchor is the source: "this <verb> that".
	source, target := anchorID, req.TransactionID
	if direction == directionInward {
		source, target = req.TransactionID, anchorID
	}

	if _, err := s.Queries.CreateTransactionLink(ctx, dbgen.CreateTransactionLinkParams{
		SourceTransactionID: source,
		TargetTransactionID: target,
		LinkTypeID:          req.LinkTypeID,
		HouseholdID:         identity.HouseholdID,
		UserID:              identity.UserID,
	}); err != nil {
		// One link per PAIR, in either direction — see transaction_links_pair_key.
		// Two links over the same two rows say nothing a single one does not, and
		// a second refund edge would net the pair twice.
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "these transactions are already linked")
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			// The pre-checks above passed, so this means visibility changed under
			// the request. 404 is still the honest answer.
			writeError(w, http.StatusNotFound, "transaction not found")
			return
		}
		s.internalError(w, "create transaction link", err)
		return
	}

	// Echo the anchor's resulting links, read back rather than reflected from the
	// request, so the panel re-renders from what was actually stored — including
	// the verb, which the client never has to derive.
	links, err := s.linksFor(ctx, anchorID, identity)
	if err != nil {
		s.internalError(w, "read transaction links", err)
		return
	}
	writeJSON(w, http.StatusCreated, links)
}

func (s *Server) handleDeleteTransactionLink(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	anchorID, err := uuid.Parse(chi.URLParam(r, "transactionID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid transaction id")
		return
	}
	linkID, err := uuid.Parse(chi.URLParam(r, "linkID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid link id")
		return
	}

	rows, err := s.Queries.DeleteTransactionLink(r.Context(), dbgen.DeleteTransactionLinkParams{
		ID:            linkID,
		TransactionID: anchorID,
		HouseholdID:   identity.HouseholdID,
		UserID:        identity.UserID,
	})
	if err != nil {
		s.internalError(w, "delete transaction link", err)
		return
	}
	// 0 covers an unknown link, a link that does not touch this transaction, and
	// an anchor the caller cannot see. They are one answer on purpose: telling
	// them apart would confirm that a link id exists.
	if rows == 0 {
		writeError(w, http.StatusNotFound, "link not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
