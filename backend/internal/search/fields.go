package search

import (
	"fmt"
	"sort"
	"strings"
)

// RequiredAliases documents the table aliases the emitted SQL references. A
// caller's query must expose all of them or the fragment will not compile.
//
//	t  transactions
//	a  accounts            (JOIN a ON a.id = t.account_id)
//	v  account_access      (JOIN v ON v.account_id = a.id)  — also institution_name
//	c  categories          (LEFT JOIN c ON c.id = t.category_id)
//	me merchant_entities   (LEFT JOIN via merchant_aliases; canonical merchant name)
const RequiredAliases = "t, a, v, c, me"

type kind int

const (
	// kindText is a string dimension. Every one of these accepts all four
	// varieties (is / contains / starts / ends) plus negation.
	kindText kind = iota
	// kindDate is t.date, compared against the date grammar in dates.go.
	kindDate
	// kindAmount is the transaction amount, compared in SQL against NUMERIC.
	kindAmount
	// kindFlag is a boolean condition with no value — `has_no_category`.
	kindFlag
)

// field is one searchable dimension.
type field struct {
	name string
	kind kind
	// exprs are the SQL expressions this field matches against. A text field
	// with several (merchant, category) matches if ANY of them does, and its
	// negation excludes rows where any of them does.
	//
	// Text expressions are always wrapped in COALESCE(…, '') when emitted, so a
	// NULL column reads as "does not match" instead of NULL. Without that,
	// `-notes:rent` would drop every row that has no notes at all, because
	// NOT (NULL LIKE …) is NULL, not true.
	exprs []string
	// defaultCmp is what a bare `field:value` means. Long free-form text
	// defaults to contains; short enumerations (currency, source) to is.
	defaultCmp Comparator
	// negSpellings are the operator names that mean "this flag, negated" —
	// has_no_category for has_category. Flags only.
	negSpellings []string
	// help is the one-line description surfaced to the UI and the docs.
	help string
}

// spelling is one operator name the parser accepts, resolved to what it means.
type spelling struct {
	field  string
	op     Comparator
	negate bool
}

var (
	fields    = map[string]field{}
	spellings = map[string]spelling{}
	// fieldOrder keeps Operators() output stable and grouped the way a person
	// would read it, rather than in map order.
	fieldOrder []string
)

func register(f field) {
	if _, dup := fields[f.name]; dup {
		panic("search: duplicate field " + f.name)
	}
	fields[f.name] = f
	fieldOrder = append(fieldOrder, f.name)
}

func addSpelling(name string, s spelling) {
	if prev, dup := spellings[name]; dup {
		panic(fmt.Sprintf("search: operator %q already means %v", name, prev))
	}
	spellings[name] = s
}

// textField registers a text dimension and its four varieties. Each variety is
// spelled `<base>_<variety>`, and the bare `<base>` is the field's default.
func textField(name string, aliases []string, defaultCmp Comparator, help string, exprs ...string) {
	register(field{name: name, kind: kindText, exprs: exprs, defaultCmp: defaultCmp, help: help})
	for _, base := range append([]string{name}, aliases...) {
		addSpelling(base, spelling{field: name, op: defaultCmp})
		for _, v := range []struct {
			suffix string
			op     Comparator
		}{
			{"_is", CmpIs},
			{"_contains", CmpContains},
			{"_starts", CmpStarts},
			{"_ends", CmpEnds},
		} {
			addSpelling(base+v.suffix, spelling{field: name, op: v.op})
		}
	}
}

// flagField registers a valueless boolean dimension plus its negative spellings.
func flagField(name string, negSpellings, aliases []string, help string, expr string) {
	register(field{
		name: name, kind: kindFlag, exprs: []string{expr},
		defaultCmp: CmpSet, negSpellings: negSpellings, help: help,
	})
	for _, s := range append([]string{name}, aliases...) {
		addSpelling(s, spelling{field: name, op: CmpSet})
	}
	for _, s := range negSpellings {
		addSpelling(s, spelling{field: name, op: CmpSet, negate: true})
	}
}

func init() {
	// ---- Text dimensions -------------------------------------------------
	//
	// merchant matches the three things a user could mean by "the merchant":
	// the canonical name the row DISPLAYS after a rename or grouping, the raw
	// name the bank sent, and the normalised descriptor key. All three, because
	// a renamed merchant must stay findable by its old wording — the same reason
	// ListVisibleTransactions' free-text filter matches all three.
	textField("merchant", []string{"payee"}, CmpContains,
		"Merchant name — canonical, as the bank sent it, or the descriptor key.",
		"COALESCE(me.canonical_name, t.merchant_name, t.name)", "t.merchant_name", "t.merchant_key")
	textField("description", []string{"desc"}, CmpContains,
		"The raw description on the transaction.", "t.name")
	textField("notes", []string{"note"}, CmpContains,
		"Notes you have written on the transaction.", "t.notes")
	textField("account", []string{"acct"}, CmpContains,
		"Account name.", "a.name")
	textField("institution", []string{"bank"}, CmpContains,
		"Institution the account belongs to.", "v.institution_name")
	textField("category", []string{"cat"}, CmpContains,
		"Category name or slug.", "c.name", "c.slug")
	textField("currency", nil, CmpIs,
		"Currency code, e.g. USD.", "t.currency")
	textField("source", nil, CmpIs,
		"Where the row came from: plaid, csv or manual.", "t.source")

	// ---- Date ------------------------------------------------------------
	//
	// One field, three comparators, several spellings each. Both bounds are
	// inclusive so `since:X before:Y` is the closed range it looks like.
	register(field{
		name: "date", kind: kindDate, exprs: []string{"t.date"}, defaultCmp: CmpOn,
		help: "Transaction date. Accepts today, start-of-this-month, -30d or 2026-01-01.",
	})
	for _, s := range []struct {
		names []string
		op    Comparator
	}{
		{[]string{"since", "after", "date_after", "from"}, CmpOnOrAfter},
		{[]string{"before", "until", "date_before", "to"}, CmpOnOrBefore},
		{[]string{"on", "date", "date_is"}, CmpOn},
	} {
		for _, n := range s.names {
			addSpelling(n, spelling{field: "date", op: s.op})
		}
	}

	// ---- Amount ----------------------------------------------------------
	//
	// Comparisons are against the MAGNITUDE of the amount, abs(t.amount).
	// Ledgermancy stores Plaid's sign convention (positive = money out), so a
	// signed `over:100` would read as "any expense over $100 and every deposit",
	// which is not what anybody means by "over a hundred dollars". Direction is
	// its own dimension: is_expense / is_income.
	register(field{
		name: "amount", kind: kindAmount, exprs: []string{"abs(t.amount)"}, defaultCmp: CmpEqual,
		help: "Amount, ignoring sign. Pair with is_expense/is_income for direction.",
	})
	for _, s := range []struct {
		names []string
		op    Comparator
	}{
		{[]string{"amount", "amount_is"}, CmpEqual},
		{[]string{"over", "amount_over", "amount_more", "more_than"}, CmpMore},
		{[]string{"under", "amount_under", "amount_less", "less_than"}, CmpLess},
	} {
		for _, n := range s.names {
			addSpelling(n, spelling{field: "amount", op: s.op})
		}
	}

	// ---- Flags -----------------------------------------------------------
	//
	// has_category mirrors the `uncategorised` filter the ledger already has: a
	// row in the fallback bucket counts as having no category, because that is
	// what the backlog view means by it.
	flagField("has_category", []string{"has_no_category", "no_category"}, nil,
		"Has a real category (not the uncategorised fallback).",
		"(t.category_id IS NOT NULL AND COALESCE(c.slug, '') <> 'uncategorised')")
	flagField("has_notes", []string{"has_no_notes", "no_notes"}, []string{"has_note"},
		"Has a non-empty note.",
		"(t.notes IS NOT NULL AND btrim(t.notes) <> '')")
	flagField("has_attachment", []string{"has_no_attachment", "no_attachment"}, []string{"has_attachments"},
		"Has a document attached.",
		"EXISTS (SELECT 1 FROM document_links dl WHERE dl.transaction_id = t.id)")
	flagField("has_split", []string{"has_no_split", "no_split"}, []string{"has_splits"},
		"Has been split between people.",
		"EXISTS (SELECT 1 FROM transaction_splits ts WHERE ts.transaction_id = t.id)")
	flagField("is_pending", []string{"is_not_pending", "is_posted"}, nil,
		"Still pending at the bank.", "t.pending")
	flagField("is_recurring", []string{"is_not_recurring"}, nil,
		"Detected as a recurring charge.", "t.is_recurring")
	flagField("is_manual", []string{"is_not_manual"}, nil,
		"Entered by hand rather than synced.", "(t.source = 'manual')")
	flagField("is_excluded", []string{"is_not_excluded"}, nil,
		"Hidden from reports.", "t.excluded_from_reports")
	flagField("is_one_time", []string{"is_not_one_time"}, nil,
		"Flagged one-time, so it is out of trailing averages.", "t.is_one_time")
	// Sign, spelled the way a person thinks about it. Zero-amount rows cannot
	// exist (the manual editor rejects them and Plaid does not send them), so
	// these two are complements in practice without needing a third case.
	flagField("is_expense", []string{"is_not_expense"}, []string{"is_outflow"},
		"Money out.", "(t.amount > 0)")
	flagField("is_income", []string{"is_not_income"}, []string{"is_inflow"},
		"Money in.", "(t.amount < 0)")
	flagField("is_transfer", []string{"is_not_transfer"}, nil,
		"Sits in a transfer category — money moving between your own accounts.",
		"COALESCE(c.is_transfer, FALSE)")
}

func lookupField(name string) (field, bool) {
	f, ok := fields[name]
	return f, ok
}

// lookupSpelling resolves an operator name as typed. Matching is
// case-insensitive, and `-` is accepted wherever `_` is so a user can write
// has-no-category without reaching for the underscore.
func lookupSpelling(name string) (spelling, bool) {
	s, ok := spellings[normaliseOperator(name)]
	return s, ok
}

func normaliseOperator(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "-", "_")
}

// spellingFor is the canonical way to write this field with the given
// comparator, used by Query.String.
func (f field) spellingFor(op Comparator) string {
	switch f.kind {
	case kindFlag:
		return f.name
	case kindText:
		if op == f.defaultCmp {
			return f.name
		}
		return f.name + "_" + string(op)
	case kindDate:
		switch op {
		case CmpOnOrAfter:
			return "since"
		case CmpOnOrBefore:
			return "before"
		default:
			return "on"
		}
	case kindAmount:
		switch op {
		case CmpMore:
			return "over"
		case CmpLess:
			return "under"
		default:
			return "amount"
		}
	}
	return f.name
}

// Operator is one operator name the grammar accepts, for autocomplete and docs.
type Operator struct {
	// Name is the operator as typed, without the trailing colon.
	Name string `json:"name"`
	// TakesValue is false for flags, which are written bare.
	TakesValue bool `json:"takes_value"`
	// Help is the field's one-line description.
	Help string `json:"help"`
}

// Operators lists every accepted operator name, sorted, with its help text.
// This is the single source of truth for the vocabulary: the docs and any
// autocomplete list should be checked against it rather than hand-maintained.
func Operators() []Operator {
	out := make([]Operator, 0, len(spellings))
	for name, s := range spellings {
		f := fields[s.field]
		out = append(out, Operator{
			Name:       name,
			TakesValue: f.kind != kindFlag,
			Help:       f.help,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
