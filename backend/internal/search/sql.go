package search

import (
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// freeTextExprs is what a bare word matches: the canonical merchant name the row
// displays, the raw name the bank sent, and the descriptor key.
//
// These three are not chosen freshly here — they are exactly the columns the
// `search` filter in ListVisibleTransactions already matched. `q=coffee` returned
// a particular set of rows before this grammar existed and must return the same
// set after, so this list and that one have to stay in step.
var freeTextExprs = []string{
	"t.merchant_key",
	"COALESCE(t.merchant_name, t.name)",
	"me.canonical_name",
}

// Predicate is a SQL fragment and its arguments, ready to AND into a WHERE.
type Predicate struct {
	// SQL is empty when the query constrains nothing. Callers must treat that as
	// "no filter" — never as "match nothing".
	SQL string
	// Args are positional, starting at the offset passed to Build.
	Args []any
}

// Build renders the query as SQL.
//
// argOffset is the number of placeholders the caller has already used, so the
// first one emitted here is $argOffset+1. now is the reference for the date
// grammar — passed in rather than read from the clock so the result is testable.
//
// The returned fragment is a predicate and nothing else: no FROM, no visibility
// clause, no LIMIT. It is the caller's job to AND it into a query that already
// scopes rows to the household and the viewer, which is the one thing a search
// must never be able to widen.
func (q Query) Build(now time.Time, argOffset int) (Predicate, error) {
	var (
		parts []string
		args  []any
	)
	// next reserves a placeholder for a value and returns its `$n` spelling.
	next := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", argOffset+len(args))
	}

	for _, term := range q.Terms {
		frag, err := buildTerm(term, now, next)
		if err != nil {
			return Predicate{}, err
		}
		if frag == "" {
			continue
		}
		if term.Negate {
			// NOT wraps the whole fragment, including the OR group of a
			// multi-column field: `-merchant:amazon` must exclude a row whose
			// descriptor key matches even if its canonical name does not.
			frag = "NOT " + frag
		}
		parts = append(parts, frag)
	}

	if len(parts) == 0 {
		return Predicate{}, nil
	}
	return Predicate{SQL: strings.Join(parts, " AND "), Args: args}, nil
}

func buildTerm(term Term, now time.Time, next func(any) string) (string, error) {
	if term.IsFreeText() {
		return orILike(freeTextExprs, next(likePattern(term.Value, CmpContains))), nil
	}

	f, ok := lookupField(term.Field)
	if !ok {
		// Only reachable if a caller hand-built a Term with a bogus field.
		return "", fmt.Errorf("unknown search field %q", term.Field)
	}

	switch f.kind {
	case kindText:
		return orILike(f.exprs, next(likePattern(term.Value, term.Op))), nil

	case kindFlag:
		return f.exprs[0], nil

	case kindDate:
		d, err := ParseDate(term.Value, now)
		if err != nil {
			return "", err
		}
		// The date is passed as a YYYY-MM-DD string with an explicit ::date cast
		// rather than a time.Time, so there is no timestamp-to-date conversion
		// happening anywhere but in the value this package resolved.
		ph := next(d.Format(time.DateOnly))
		op := map[Comparator]string{
			CmpOnOrAfter:  ">=",
			CmpOnOrBefore: "<=",
			CmpOn:         "=",
		}[term.Op]
		if op == "" {
			return "", fmt.Errorf("comparator %q does not apply to dates", term.Op)
		}
		return fmt.Sprintf("(%s %s %s::date)", f.exprs[0], op, ph), nil

	case kindAmount:
		// Parsed only to REJECT junk — `over:banana` must be a 400, not a SQL
		// error. The value then travels as a string and the comparison happens in
		// Postgres against NUMERIC. No money is ever compared in Go, and nothing
		// here touches a float.
		d, err := decimal.NewFromString(strings.TrimSpace(term.Value))
		if err != nil {
			return "", fmt.Errorf("%q is not an amount: use a number like 10 or 10.50", term.Value)
		}
		// A signed amount is accepted and read as its magnitude, because the
		// column is compared with abs() — see the amount field's note in
		// fields.go. Normalising here keeps `over:-10` from meaning "over minus
		// ten", which in an abs() comparison would be every row.
		ph := next(d.Abs().String())
		op := map[Comparator]string{
			CmpEqual: "=",
			CmpMore:  ">",
			CmpLess:  "<",
		}[term.Op]
		if op == "" {
			return "", fmt.Errorf("comparator %q does not apply to amounts", term.Op)
		}
		return fmt.Sprintf("(%s %s %s::numeric)", f.exprs[0], op, ph), nil
	}
	return "", fmt.Errorf("unsupported field kind for %q", term.Field)
}

// orILike matches a pattern against several expressions, any one of which
// counts. The same placeholder is reused for each — Postgres allows that, and it
// keeps a three-column field to one argument.
//
// Every expression is wrapped in an empty-string COALESCE so a NULL column
// reads as "no
// match" instead of NULL. That is what makes negation honest: without it,
// `-notes:rent` would drop every row that has no notes, since NOT (NULL ILIKE …)
// is NULL rather than true.
func orILike(exprs []string, placeholder string) string {
	clauses := make([]string, 0, len(exprs))
	for _, e := range exprs {
		clauses = append(clauses, fmt.Sprintf("COALESCE(%s, '') ILIKE %s", e, placeholder))
	}
	return "(" + strings.Join(clauses, " OR ") + ")"
}

// likePattern turns a value and a comparator into an ILIKE pattern.
//
// The value is escaped first, so a merchant with a literal % or _ in its
// descriptor is searched for as text rather than as a wildcard. `is` yields a
// pattern with no wildcards at all, which makes it case-insensitive equality.
func likePattern(value string, cmp Comparator) string {
	esc := escapeLike(value)
	switch cmp {
	case CmpIs:
		return esc
	case CmpStarts:
		return esc + "%"
	case CmpEnds:
		return "%" + esc
	default:
		return "%" + esc + "%"
	}
}

// escapeLike neutralises the three characters LIKE treats specially. Backslash
// goes first, or it would escape the escapes added after it.
func escapeLike(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "%", `\%`)
	v = strings.ReplaceAll(v, "_", `\_`)
	return v
}
