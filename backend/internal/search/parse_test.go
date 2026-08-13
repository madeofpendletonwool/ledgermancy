package search

import (
	"testing"
)

// term is the shorthand these tests compare against, so a case reads as the
// query it describes rather than as a struct literal.
type want struct {
	field  string
	op     Comparator
	value  string
	negate bool
}

func check(t *testing.T, query string, wants ...want) {
	t.Helper()
	got := Parse(query)
	if len(got.Terms) != len(wants) {
		t.Fatalf("Parse(%q) gave %d terms, want %d: %#v", query, len(got.Terms), len(wants), got.Terms)
	}
	for i, w := range wants {
		g := got.Terms[i]
		if g.Field != w.field || g.Op != w.op || g.Value != w.value || g.Negate != w.negate {
			t.Errorf("Parse(%q) term %d = {%q %q %q neg=%v}, want {%q %q %q neg=%v}",
				query, i, g.Field, g.Op, g.Value, g.Negate, w.field, w.op, w.value, w.negate)
		}
	}
}

// A bare word is free text, which is what `q` meant before this grammar existed.
// Every old link and bookmark depends on this staying true.
func TestParseFreeText(t *testing.T) {
	check(t, "starbucks", want{value: "starbucks"})
	check(t, "  whole   foods  ",
		want{value: "whole"},
		want{value: "foods"})
	check(t, `"whole foods"`, want{value: "whole foods"})
	check(t, "-starbucks", want{value: "starbucks", negate: true})
}

// An operator name nobody registered is free text, not an error. A pasted
// descriptor like AMZN:MKTP has to keep finding the charge it names.
func TestParseUnknownOperatorIsFreeText(t *testing.T) {
	check(t, "AMZN:MKTP", want{value: "AMZN:MKTP"})
	check(t, "http://example.test", want{value: "http://example.test"})
	// Quoted, the colon is text even when the word before it IS an operator.
	check(t, `"merchant:amazon"`, want{value: "merchant:amazon"})
}

// The four varieties every text dimension has, plus the bare default.
func TestParseTextVarieties(t *testing.T) {
	check(t, "merchant:amazon", want{field: "merchant", op: CmpContains, value: "amazon"})
	check(t, "merchant_is:amazon", want{field: "merchant", op: CmpIs, value: "amazon"})
	check(t, "merchant_contains:amazon", want{field: "merchant", op: CmpContains, value: "amazon"})
	check(t, "merchant_starts:AMZN", want{field: "merchant", op: CmpStarts, value: "AMZN"})
	check(t, "merchant_ends:MKTP", want{field: "merchant", op: CmpEnds, value: "MKTP"})

	// Aliases resolve to the canonical field, and the operator name is
	// case-insensitive with `-` accepted for `_`.
	check(t, "payee:amazon", want{field: "merchant", op: CmpContains, value: "amazon"})
	check(t, "DESC:rent", want{field: "description", op: CmpContains, value: "rent"})
	check(t, "notes-contains:tax", want{field: "notes", op: CmpContains, value: "tax"})

	// Short enumerations default to exact rather than substring.
	check(t, "currency:USD", want{field: "currency", op: CmpIs, value: "USD"})
	check(t, "source:manual", want{field: "source", op: CmpIs, value: "manual"})
}

// A value with spaces survives if it is quoted, which is the only reason quotes
// exist in this grammar.
func TestParseQuotedValues(t *testing.T) {
	check(t, `account:"Joint Checking"`, want{field: "account", op: CmpContains, value: "Joint Checking"})
	check(t, `account:'Joint Checking'`, want{field: "account", op: CmpContains, value: "Joint Checking"})
	check(t, `merchant:"Trader Joe's"`, want{field: "merchant", op: CmpContains, value: "Trader Joe's"})
	check(t, `notes:"a \"quoted\" note"`, want{field: "notes", op: CmpContains, value: `a "quoted" note`})
	// An unterminated quote is a query mid-typing, not a failure.
	check(t, `account:"Joint Check`, want{field: "account", op: CmpContains, value: "Joint Check"})
}

// Flags are written bare and negate through their own spelling or a leading dash.
func TestParseFlags(t *testing.T) {
	check(t, "has_no_category", want{field: "has_category", op: CmpSet, negate: true})
	check(t, "has_category", want{field: "has_category", op: CmpSet})
	check(t, "is_pending", want{field: "is_pending", op: CmpSet})
	check(t, "is_posted", want{field: "is_pending", op: CmpSet, negate: true})
	check(t, "has-no-notes", want{field: "has_notes", op: CmpSet, negate: true})

	// A dash in front of a negative spelling is a double negative and cancels.
	check(t, "-has_no_category", want{field: "has_category", op: CmpSet})
	check(t, "-has_category", want{field: "has_category", op: CmpSet, negate: true})

	// People write a value on a flag anyway. An explicit false flips it; anything
	// else is ignored rather than rejected.
	check(t, "is_pending:false", want{field: "is_pending", op: CmpSet, negate: true})
	check(t, "is_pending:true", want{field: "is_pending", op: CmpSet})
	check(t, "is_pending:maybe", want{field: "is_pending", op: CmpSet})
}

func TestParseDatesAndAmounts(t *testing.T) {
	check(t, "since:-30d", want{field: "date", op: CmpOnOrAfter, value: "-30d"})
	check(t, "before:2026-01-01", want{field: "date", op: CmpOnOrBefore, value: "2026-01-01"})
	check(t, "on:today", want{field: "date", op: CmpOn, value: "today"})
	check(t, "until:yesterday", want{field: "date", op: CmpOnOrBefore, value: "yesterday"})

	check(t, "over:10", want{field: "amount", op: CmpMore, value: "10"})
	check(t, "under:10.50", want{field: "amount", op: CmpLess, value: "10.50"})
	check(t, "amount:99", want{field: "amount", op: CmpEqual, value: "99"})
	check(t, "amount_more:99", want{field: "amount", op: CmpMore, value: "99"})
}

// The AND-join, including a negated term, in one query — the case the feature
// exists for.
func TestParseComposedQuery(t *testing.T) {
	check(t, `starbucks over:10 since:-30d -account:Checking has_no_category`,
		want{value: "starbucks"},
		want{field: "amount", op: CmpMore, value: "10"},
		want{field: "date", op: CmpOnOrAfter, value: "-30d"},
		want{field: "account", op: CmpContains, value: "Checking", negate: true},
		want{field: "has_category", op: CmpSet, negate: true},
	)
}

// A hyphen inside a word is part of the word. Only a LEADING dash negates, or
// `T-MOBILE` and `start-of-last-month` would both come out mangled.
func TestParseInternalHyphens(t *testing.T) {
	check(t, "T-MOBILE", want{value: "T-MOBILE"})
	check(t, "since:start-of-last-month", want{field: "date", op: CmpOnOrAfter, value: "start-of-last-month"})
	check(t, "-T-MOBILE", want{value: "T-MOBILE", negate: true})
}

// An operator with nothing after the colon is someone mid-keystroke. It has to
// drop out, because an empty `contains` pattern matches every row and would read
// as the filter having broken.
func TestParseEmptyValueDrops(t *testing.T) {
	check(t, "merchant:")
	check(t, "merchant: starbucks", want{value: "starbucks"})
	check(t, "over:")
	check(t, `merchant:""`)
	// A lone dash is not a term either.
	check(t, "-")
}

func TestQueryHasDateTerm(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{"starbucks", false},
		{"over:10", false},
		{"since:-30d", true},
		{"starbucks before:today", true},
		{"has_no_category", false},
	} {
		if got := Parse(tc.query).HasDateTerm(); got != tc.want {
			t.Errorf("Parse(%q).HasDateTerm() = %v, want %v", tc.query, got, tc.want)
		}
	}
}

// String round-trips through Parse. Not byte-identical — aliases resolve and
// spacing normalises — but re-parsing has to give the same Query, which is what
// a stored rule or a shared URL depends on.
func TestQueryStringRoundTrips(t *testing.T) {
	for _, q := range []string{
		"starbucks over:10 since:-30d",
		`-account:"Joint Checking" has_no_category`,
		"payee_starts:AMZN under:25 is_expense",
		`notes:"a \"quoted\" note"`,
	} {
		first := Parse(q)
		second := Parse(first.String())
		if first.String() != second.String() {
			t.Errorf("Parse(%q).String() = %q, which re-parses to %q",
				q, first.String(), second.String())
		}
		if len(first.Terms) != len(second.Terms) {
			t.Errorf("Parse(%q) has %d terms, its String() re-parses to %d",
				q, len(first.Terms), len(second.Terms))
		}
	}
}

// Every registered field has to be reachable by at least one operator name, or it
// is a dimension that exists in the code and nowhere else. Operators() is what
// the search bar's autocomplete is built from, so an unreachable field is also an
// invisible one.
func TestEveryFieldIsReachable(t *testing.T) {
	reached := map[string]bool{}
	for _, op := range Operators() {
		s, ok := lookupSpelling(op.Name)
		if !ok {
			t.Errorf("Operators() lists %q, which the parser does not accept", op.Name)
			continue
		}
		reached[s.field] = true
		if op.Help == "" {
			t.Errorf("operator %q has no help text; the autocomplete would show a blank row", op.Name)
		}
	}
	for name := range fields {
		if !reached[name] {
			t.Errorf("field %q has no operator name, so nothing can search it", name)
		}
	}
}
