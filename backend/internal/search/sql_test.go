package search

import (
	"strings"
	"testing"
	"time"
)

func build(t *testing.T, query string, offset int) Predicate {
	t.Helper()
	p, err := Parse(query).Build(ref, offset)
	if err != nil {
		t.Fatalf("Build(%q): %v", query, err)
	}
	return p
}

// An empty query is "no filter", never "match nothing". A caller that ANDed a
// non-empty fragment in for an empty query would show a user an empty ledger the
// moment they cleared the search box.
func TestBuildEmptyQuery(t *testing.T) {
	for _, q := range []string{"", "   ", "merchant:"} {
		p := build(t, q, 0)
		if p.SQL != "" || len(p.Args) != 0 {
			t.Errorf("Build(%q) = %q with %d args, want an empty predicate", q, p.SQL, len(p.Args))
		}
	}
}

// Free text matches the three columns ListVisibleTransactions' `search` narg
// matches, and only those. `q=coffee` returned a particular set of rows before
// this grammar existed and has to keep returning it.
func TestBuildFreeTextMatchesTheLegacyColumns(t *testing.T) {
	p := build(t, "coffee", 0)
	for _, expr := range []string{"t.merchant_key", "COALESCE(t.merchant_name, t.name)", "me.canonical_name"} {
		if !strings.Contains(p.SQL, expr) {
			t.Errorf("free text does not match %s; SQL was %s", expr, p.SQL)
		}
	}
	if strings.Contains(p.SQL, "t.notes") {
		t.Errorf("free text must not reach into notes — that would widen what q= used to mean: %s", p.SQL)
	}
	if len(p.Args) != 1 || p.Args[0] != "%coffee%" {
		t.Errorf("args = %#v, want one %%coffee%% pattern", p.Args)
	}
}

// Placeholder numbering continues from the caller's offset, since the fragment is
// spliced into a query that has already bound its own parameters.
func TestBuildHonoursArgOffset(t *testing.T) {
	p := build(t, "coffee over:10", 9)
	if !strings.Contains(p.SQL, "$10") || !strings.Contains(p.SQL, "$11") {
		t.Errorf("with offset 9 the fragment should use $10 and $11: %s", p.SQL)
	}
	if strings.Contains(p.SQL, "$1 ") || strings.Contains(p.SQL, "$1)") {
		t.Errorf("fragment reused a caller placeholder: %s", p.SQL)
	}
	if len(p.Args) != 2 {
		t.Errorf("args = %#v, want 2", p.Args)
	}
}

// The four text varieties differ only in the pattern, and the pattern is the
// whole behaviour — so it is what gets asserted.
func TestBuildTextPatterns(t *testing.T) {
	for _, tc := range []struct{ query, pattern string }{
		{"merchant_is:amazon", "amazon"},
		{"merchant_contains:amazon", "%amazon%"},
		{"merchant:amazon", "%amazon%"},
		{"merchant_starts:amazon", "amazon%"},
		{"merchant_ends:amazon", "%amazon"},
	} {
		p := build(t, tc.query, 0)
		if len(p.Args) != 1 || p.Args[0] != tc.pattern {
			t.Errorf("Build(%q) args = %#v, want [%q]", tc.query, p.Args, tc.pattern)
		}
	}
}

// A % or _ in a merchant descriptor is text, not a wildcard. Without escaping,
// `merchant:100%` would match every row and read as a broken filter.
func TestBuildEscapesLikeWildcards(t *testing.T) {
	for _, tc := range []struct{ query, pattern string }{
		{"merchant_is:100%", `100\%`},
		{"merchant_is:a_b", `a\_b`},
		{`merchant_is:"back\\slash"`, `back\\slash`},
	} {
		p := build(t, tc.query, 0)
		if len(p.Args) != 1 || p.Args[0] != tc.pattern {
			t.Errorf("Build(%q) args = %#v, want [%q]", tc.query, p.Args, tc.pattern)
		}
	}
}

// A NULL column has to read as "does not match" rather than NULL, or negation
// silently drops every row where the column is empty: NOT (NULL ILIKE …) is NULL,
// which SQL treats as false. COALESCE is what makes `-notes:rent` keep the rows
// that have no notes at all.
func TestBuildCoalescesTextColumns(t *testing.T) {
	p := build(t, "notes:rent", 0)
	if !strings.Contains(p.SQL, "COALESCE(t.notes, '')") {
		t.Errorf("text comparison must COALESCE a nullable column: %s", p.SQL)
	}
}

// A multi-column field is an OR group, and negating it has to exclude a row that
// matches ANY of the columns — so NOT wraps the whole group, not one clause.
func TestBuildNegationWrapsTheWholeGroup(t *testing.T) {
	p := build(t, "-merchant:amazon", 0)
	if !strings.HasPrefix(p.SQL, "NOT (") {
		t.Errorf("negation must wrap the whole OR group: %s", p.SQL)
	}
	if strings.Count(p.SQL, "NOT") != 1 {
		t.Errorf("expected exactly one NOT: %s", p.SQL)
	}
	if strings.Count(p.SQL, "ILIKE") != 3 {
		t.Errorf("merchant matches three columns, so three ILIKEs: %s", p.SQL)
	}
}

// Terms are AND-joined, in the order typed. Three terms whose own expressions
// contain no AND, so counting the joins actually counts the joins.
func TestBuildAndJoins(t *testing.T) {
	p := build(t, "coffee over:10 is_pending", 0)
	if got := strings.Count(p.SQL, " AND "); got != 2 {
		t.Errorf("three terms should be joined by two ANDs, got %d: %s", got, p.SQL)
	}
	order := []string{"ILIKE", "abs(t.amount)", "t.pending"}
	at := -1
	for _, marker := range order {
		i := strings.Index(p.SQL, marker)
		if i <= at {
			t.Fatalf("terms are out of typed order at %q: %s", marker, p.SQL)
		}
		at = i
	}

	// The negative spelling of a flag is its condition wrapped in NOT.
	neg := build(t, "has_no_category", 0)
	if !strings.HasPrefix(neg.SQL, "NOT (t.category_id IS NOT NULL") {
		t.Errorf("has_no_category should be the negated has_category condition: %s", neg.SQL)
	}
}

// Amount comparisons happen in SQL against NUMERIC. The value travels as a
// string with an explicit ::numeric cast, so nothing in Go ever holds this money
// as a float.
func TestBuildAmountStaysNumeric(t *testing.T) {
	for _, tc := range []struct{ query, op, arg string }{
		{"over:10", ">", "10"},
		{"under:10.50", "<", "10.5"},
		{"amount:99.99", "=", "99.99"},
		// A signed value is read as its magnitude, because the column is compared
		// with abs(). `over:-10` meaning "over minus ten" would be every row.
		{"over:-10", ">", "10"},
	} {
		p := build(t, tc.query, 0)
		wantSQL := "(abs(t.amount) " + tc.op + " $1::numeric)"
		if p.SQL != wantSQL {
			t.Errorf("Build(%q) = %q, want %q", tc.query, p.SQL, wantSQL)
		}
		if len(p.Args) != 1 || p.Args[0] != tc.arg {
			t.Errorf("Build(%q) args = %#v, want [%q] as a string", tc.query, p.Args, tc.arg)
		}
		if _, isFloat := p.Args[0].(float64); isFloat {
			t.Errorf("Build(%q) passed money as a float", tc.query)
		}
	}
}

// Dates resolve in Go (that is where the grammar lives) and are compared in SQL
// as a DATE, with both bounds inclusive.
func TestBuildDates(t *testing.T) {
	for _, tc := range []struct{ query, op, arg string }{
		{"since:-30d", ">=", "2026-02-13"},
		{"before:today", "<=", "2026-03-15"},
		{"on:2026-01-01", "=", "2026-01-01"},
		{"since:start-of-this-month", ">=", "2026-03-01"},
	} {
		p := build(t, tc.query, 0)
		wantSQL := "(t.date " + tc.op + " $1::date)"
		if p.SQL != wantSQL {
			t.Errorf("Build(%q) = %q, want %q", tc.query, p.SQL, wantSQL)
		}
		if len(p.Args) != 1 || p.Args[0] != tc.arg {
			t.Errorf("Build(%q) args = %#v, want [%q]", tc.query, p.Args, tc.arg)
		}
	}
}

// A flag is a condition with no argument at all.
func TestBuildFlagsBindNoArgs(t *testing.T) {
	for _, tc := range []struct{ query, contains string }{
		{"is_pending", "t.pending"},
		{"is_expense", "(t.amount > 0)"},
		{"is_income", "(t.amount < 0)"},
		{"is_transfer", "COALESCE(c.is_transfer, FALSE)"},
		{"has_attachment", "document_links"},
		{"has_split", "transaction_splits"},
		{"is_manual", "(t.source = 'manual')"},
		{"is_excluded", "t.excluded_from_reports"},
		{"is_one_time", "t.is_one_time"},
		{"has_notes", "btrim(t.notes)"},
	} {
		p := build(t, tc.query, 0)
		if !strings.Contains(p.SQL, tc.contains) {
			t.Errorf("Build(%q) = %q, want it to contain %q", tc.query, p.SQL, tc.contains)
		}
		if len(p.Args) != 0 {
			t.Errorf("Build(%q) bound %d args; a flag takes none", tc.query, len(p.Args))
		}
	}
}

// A value the user can fix is an error with a message worth showing, because the
// handler writes it straight back as a 400.
func TestBuildRejectsBadValues(t *testing.T) {
	for _, q := range []string{
		"over:banana",
		"amount:1.2.3",
		"since:banana",
		"before:2026-13-45",
	} {
		if _, err := Parse(q).Build(ref, 0); err == nil {
			t.Errorf("Build(%q) succeeded, want an error the user can act on", q)
		} else if !strings.Contains(err.Error(), "banana") &&
			!strings.Contains(err.Error(), "1.2.3") &&
			!strings.Contains(err.Error(), "2026-13-45") {
			t.Errorf("Build(%q) error %q does not name the offending value", q, err)
		}
	}
}

// The predicate is a predicate and nothing else. If it ever grew a FROM or a
// household clause of its own it could widen the caller's visibility scoping
// instead of narrowing it, which is the one thing a search must not do.
func TestBuildEmitsOnlyAPredicate(t *testing.T) {
	p := build(t, `coffee over:10 since:-30d -account:Checking has_no_category is_expense`, 9)
	for _, forbidden := range []string{"FROM", "SELECT t.", "household_id", "LIMIT", "ORDER BY", ";"} {
		if strings.Contains(p.SQL, forbidden) {
			t.Errorf("predicate contains %q, which belongs to the caller's query: %s", forbidden, p.SQL)
		}
	}
	// One argument per value-taking term; the two flags bind none.
	if len(p.Args) != 4 {
		t.Errorf("args = %#v, want 4", p.Args)
	}
}

// Every operator Operators() advertises has to build. An operator the autocomplete
// suggests and the builder then rejects would be a suggestion that breaks search.
func TestEveryOperatorBuilds(t *testing.T) {
	for _, op := range Operators() {
		query := op.Name
		if op.TakesValue {
			// A value each kind accepts: dates need a date, amounts a number, and
			// text takes anything.
			switch s, _ := lookupSpelling(op.Name); fields[s.field].kind {
			case kindDate:
				query += ":2026-01-01"
			case kindAmount:
				query += ":10"
			default:
				query += ":x"
			}
		}
		parsed := Parse(query)
		if len(parsed.Terms) != 1 {
			t.Errorf("Parse(%q) gave %d terms, want 1", query, len(parsed.Terms))
			continue
		}
		p, err := parsed.Build(ref, 0)
		if err != nil {
			t.Errorf("Build(%q): %v", query, err)
			continue
		}
		if p.SQL == "" {
			t.Errorf("Build(%q) produced no SQL", query)
		}
	}
}

// The date grammar resolves against the reference the caller passes, not the wall
// clock — otherwise none of the above could be asserted at all.
func TestBuildUsesTheGivenNow(t *testing.T) {
	other := time.Date(2020, time.July, 4, 0, 0, 0, 0, time.UTC)
	p, err := Parse("on:today").Build(other, 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.Args[0] != "2020-07-04" {
		t.Errorf("args = %#v, want the reference date", p.Args)
	}
}
