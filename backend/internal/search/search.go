// Package search turns a typed query string into a SQL predicate over
// transactions.
//
// The grammar is `key:value` terms separated by whitespace, AND-joined, with a
// leading `-` on a term to negate it, and any bare word treated as free text:
//
//	starbucks over:10 since:-30d
//	has_no_category -account:Checking
//	merchant_is:"whole foods" under:100 before:end-of-last-month
//
// # Why a grammar and not more filter parameters
//
// The Transactions page already has chips for one account list, one category,
// one date range and one free-text needle. Every new dimension costs another
// control, and combinations the chips cannot express (two merchants, "not this
// account", "between 10 and 100") stay unreachable no matter how many are
// added. A grammar is one input that composes, and the SAME parse is what the
// rules engine needs for its triggers — one vocabulary, two surfaces.
//
// # What this package does and does not do
//
// Parse produces a Query: a flat list of Terms, AND-joined. Build turns a Query
// into a SQL fragment plus its arguments, using placeholders numbered from a
// caller-supplied offset. It emits ONLY the predicate — never a FROM, never a
// visibility clause. The caller is responsible for intersecting it with the
// household visibility predicate, which is why the fragment is always safe to
// drop into an existing WHERE with an AND.
//
// Column references in the emitted SQL assume the caller's query aliases
// transactions as `t`, accounts as `a`, account_access as `v`, the resolved
// merchant entity as `me` and the row's category as `c`. See RequiredAliases.
//
// # Money
//
// Amount values are validated as decimals in Go and then passed as STRING
// arguments cast to numeric in SQL. Nothing in this package converts money to a
// float, and no comparison happens in Go — the comparison is `abs(t.amount) >
// $n::numeric`, evaluated by Postgres against the stored NUMERIC.
package search

import (
	"fmt"
	"strings"
)

// Comparator is how a term's value is compared against its field. Which ones a
// field accepts depends on its kind; see the registry in fields.go.
type Comparator string

const (
	// Text comparators. These are the "4 varieties" every text dimension has.
	CmpIs       Comparator = "is"
	CmpContains Comparator = "contains"
	CmpStarts   Comparator = "starts"
	CmpEnds     Comparator = "ends"

	// Date comparators. Both bounds are INCLUSIVE, so `since:X before:Y` reads
	// as the closed range it looks like.
	CmpOnOrAfter  Comparator = "on_or_after"
	CmpOnOrBefore Comparator = "on_or_before"
	CmpOn         Comparator = "on"

	// Amount comparators, all against the MAGNITUDE of the amount — see the
	// note on amount fields in fields.go for why.
	CmpEqual Comparator = "equal"
	CmpMore  Comparator = "more"
	CmpLess  Comparator = "less"

	// CmpSet is the only comparator a flag takes: the flag's condition holds.
	// `has_no_category` is not a separate comparator, it is CmpSet negated.
	CmpSet Comparator = "set"
)

// Term is one AND-ed condition. A free-text term has an empty Field and Op —
// it is the bare word case, matched against what the row displays.
type Term struct {
	// Field is the canonical field name (never an alias), or "" for free text.
	Field string
	Op    Comparator
	// Value is the value exactly as the user typed it, minus quotes. Date and
	// amount values are resolved/validated in Build, not here, so a Query can be
	// inspected and re-rendered without losing what was typed.
	Value string
	// Negate inverts this term alone. Note that negating a term whose column is
	// NULL still excludes the row, because SQL's NOT (NULL LIKE …) is NULL, not
	// true — Build compensates with an explicit IS NULL branch so that
	// `-notes:rent` keeps rows with no notes at all.
	Negate bool
}

// IsFreeText reports whether t is a bare word rather than a key:value term.
func (t Term) IsFreeText() bool { return t.Field == "" }

// Query is a parsed search string: terms AND-joined, in the order typed.
type Query struct {
	Terms []Term
}

// IsEmpty reports whether the query constrains nothing, which callers treat as
// "no search" rather than "match nothing".
func (q Query) IsEmpty() bool { return len(q.Terms) == 0 }

// HasDateTerm reports whether the query says anything about dates.
//
// This is load-bearing for the Transactions endpoint: the page always sends a
// from/to window (a rolling year by default), so a query that named its own
// dates would otherwise be silently clipped to that year and `since:2019-01-01`
// would return nothing. When this is true the caller drops its own window and
// lets the query own the date range.
func (q Query) HasDateTerm() bool {
	for _, t := range q.Terms {
		if f, ok := lookupField(t.Field); ok && f.kind == kindDate {
			return true
		}
	}
	return false
}

// MentionsField reports whether any term is about the named field, whichever
// comparator or polarity it used.
//
// This exists for the dimensions a caller HIDES by default. `is_excluded` is the
// one: the ledger leaves excluded rows out unless asked, so a query that names
// them has to switch that default off or the term could only ever return nothing.
func (q Query) MentionsField(name string) bool {
	for _, t := range q.Terms {
		if t.Field == name {
			return true
		}
	}
	return false
}

// String re-renders the query in the grammar it was parsed from. Round-tripping
// is not guaranteed to be byte-identical (aliases resolve to canonical names,
// spacing normalises), but re-parsing the result yields the same Query.
func (q Query) String() string {
	parts := make([]string, 0, len(q.Terms))
	for _, t := range q.Terms {
		var b strings.Builder
		if t.Negate {
			b.WriteByte('-')
		}
		if t.IsFreeText() {
			b.WriteString(quoteIfNeeded(t.Value))
			parts = append(parts, b.String())
			continue
		}
		f, ok := lookupField(t.Field)
		if !ok {
			// Cannot happen for a Query from Parse; be inert rather than wrong.
			continue
		}
		b.WriteString(f.spellingFor(t.Op))
		if f.kind != kindFlag {
			b.WriteByte(':')
			b.WriteString(quoteIfNeeded(t.Value))
		}
		parts = append(parts, b.String())
	}
	return strings.Join(parts, " ")
}

func quoteIfNeeded(v string) string {
	if v != "" && !strings.ContainsAny(v, " \t\"'") {
		return v
	}
	return fmt.Sprintf("%q", v)
}
