package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/search"
)

// The composable search bar's half of GET /api/transactions.
//
// `q` used to be a plain free-text needle and is now a query in the grammar
// documented in internal/search. A bare word still means what it always did, so
// every existing link and bookmark keeps working; operators are the new part.
//
// # Why this is two queries
//
// A `key:value` query cannot be expressed as a fixed set of sqlc nargs — the
// operators compose, negate and repeat — so its predicate has to be built at
// request time. Rather than hand-write a second copy of the row projection to go
// with it, this file resolves the search to an ordered, paged list of ids and
// hands them to ListVisibleTransactions via its transaction_ids narg. The
// projection keeps one definition, and the household/viewer scoping is applied
// twice: once here while choosing rows, once again while fetching them.
//
// # The invariant
//
// The search predicate is ANDed into a WHERE that already scopes rows to the
// household and the viewer. It is never a replacement for that scoping and never
// reaches it — internal/search emits a predicate only, with no FROM and no
// visibility clause of its own. A malicious query can narrow this list; it
// cannot widen it.

// searchFixedArgs is how many placeholders searchTransactionIDs binds before the
// search predicate's own. The predicate is built with this offset so its $n
// numbering continues from here.
const searchFixedArgs = 9

// transactionSearchParams is everything that decides which rows a search
// returns: the parsed query plus the filter chips it composes with.
type transactionSearchParams struct {
	householdID uuid.UUID
	userID      uuid.UUID
	query       search.Query
	// from/to are the page's date window. They are ignored when the query names
	// its own dates — see the note in handleListTransactions.
	from, to    time.Time
	applyWindow bool
	// The remaining fields mirror the chip filters on the ordinary path, so a
	// search composes with them rather than replacing them.
	accountIDs      []uuid.UUID
	categoryID      *uuid.UUID
	merchantKey     *string
	uncategorised   bool
	includeExcluded bool
	limit, offset   int
	// now is the reference for the date grammar, so `today` is testable.
	now time.Time
}

// invalidSearchError marks a query the user can fix — an amount or date that
// does not parse. Its message is written straight back as a 400, so it has to
// read like something a person can act on.
type invalidSearchError struct{ err error }

func (e invalidSearchError) Error() string { return e.err.Error() }
func (e invalidSearchError) Unwrap() error { return e.err }

// searchIDQuery is the visibility- and chip-scoped id lookup the search predicate
// is ANDed into.
//
// The WHERE clauses below are the same ones ListVisibleTransactions applies, in
// the same order and with the same shape, because the two have to agree on which
// rows exist before the grammar narrows them. If a filter changes there it has to
// change here; the comments explaining WHY each one reads the way it does live in
// internal/db/queries/transactions.sql and are not repeated.
//
// The merchant_aliases/merchant_entities and categories joins are here for the
// grammar's sake: `merchant:`, `category:` and the category flags compare against
// me.canonical_name, c.name and c.slug. See search.RequiredAliases.
const searchIDQuery = `
SELECT t.id
FROM transactions t
JOIN accounts a       ON a.id = t.account_id
JOIN account_access v ON v.account_id = a.id
LEFT JOIN merchant_aliases ma
       ON ma.household_id = $1
      AND ma.merchant_key = t.merchant_key
      AND ma.source <> 'suggested'
LEFT JOIN merchant_entities me ON me.id = ma.entity_id
LEFT JOIN categories c         ON c.id = t.category_id
WHERE v.household_id = $1
  AND (v.user_id = $2 OR v.is_shared)
  AND a.is_active
  AND ($3::bool IS TRUE OR NOT t.excluded_from_reports)
  AND ($4::date IS NULL OR t.date >= $4::date)
  AND ($5::date IS NULL OR t.date <= $5::date)
  AND (
    $6::uuid[] IS NULL
    OR cardinality($6::uuid[]) = 0
    OR t.account_id = ANY($6::uuid[])
  )
  AND ($7::uuid IS NULL OR t.category_id = $7::uuid)
  AND (
    $8::text IS NULL
    OR COALESCE(ma.entity_id::text, t.merchant_key) = $8::text
    OR t.merchant_key = $8::text
  )
  AND (
    $9::bool IS NOT TRUE
    OR t.category_id IS NULL
    OR t.category_id IN (SELECT id FROM categories WHERE slug = 'uncategorised')
  )`

// searchTransactionIDs returns the ids of the transactions matching the search,
// newest first, already paged.
//
// The ordering is byte-for-byte the one ListVisibleTransactions uses, so the rows
// come back in the order chosen here.
func (s *Server) searchTransactionIDs(ctx context.Context, p transactionSearchParams) ([]uuid.UUID, error) {
	pred, err := p.query.Build(p.now, searchFixedArgs)
	if err != nil {
		return nil, invalidSearchError{err}
	}

	var (
		fromArg *string
		toArg   *string
	)
	if p.applyWindow {
		from := p.from.Format(time.DateOnly)
		to := p.to.Format(time.DateOnly)
		fromArg, toArg = &from, &to
	}

	args := []any{
		p.householdID,
		p.userID,
		p.includeExcluded,
		fromArg,
		toArg,
		p.accountIDs,
		p.categoryID,
		p.merchantKey,
		p.uncategorised,
	}
	args = append(args, pred.Args...)

	var b strings.Builder
	b.WriteString(searchIDQuery)
	if pred.SQL != "" {
		b.WriteString("\n  AND ")
		b.WriteString(pred.SQL)
	}
	// LIMIT/OFFSET are numbered last so the predicate's placeholders stay
	// contiguous with the fixed ones above it.
	fmt.Fprintf(&b, "\nORDER BY t.date DESC, t.created_at DESC\nLIMIT $%d OFFSET $%d",
		len(args)+1, len(args)+2)
	args = append(args, p.limit, p.offset)

	rows, err := s.Pool.Query(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]uuid.UUID, 0, p.limit)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// searchWindowMin/Max stand in for "no date window" when the fetch re-reads the
// rows a search chose. The id list is the filter at that point, so the window has
// to pass everything; Postgres dates span 4713 BC to 5874897 AD, which leaves
// these comfortably outside any transaction's date.
var (
	searchWindowMin = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)
	searchWindowMax = time.Date(9999, time.December, 31, 0, 0, 0, 0, time.UTC)
)
