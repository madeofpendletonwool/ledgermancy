package search

import "strings"

// Parse reads a query string into a Query. It never fails.
//
// That is a deliberate contract, and it has two reasons. The first is the search
// box: it fires as the user types, so half-finished input is the normal state and
// rejecting it would mean an error flashing on nearly every keystroke. The second
// is backward compatibility — `q` was a plain free-text needle before this
// grammar existed, and anything the grammar does not recognise (including a
// `word:word` pair that is not an operator, like a pasted `AMZN:MKTP` descriptor)
// stays free text and keeps matching what it used to.
//
// Values are NOT validated here. `since:banana` parses; it fails in Build, which
// is where the caller gets a message worth showing. This split lets a query be
// stored and re-rendered without its meaning being resolved.
func Parse(input string) Query {
	var q Query
	for _, tok := range lex(input) {
		if term, ok := parseToken(tok); ok {
			q.Terms = append(q.Terms, term)
		}
	}
	return q
}

func parseToken(tok token) (Term, bool) {
	key, value, hasKey := tok.splitKey()
	if !hasKey {
		// No colon. A flag is written bare (`has_no_category`), so check the
		// vocabulary before falling back to free text.
		if s, ok := lookupSpelling(tok.text()); ok && fields[s.field].kind == kindFlag {
			return Term{Field: s.field, Op: CmpSet, Negate: tok.negate != s.negate}, true
		}
		return Term{Value: tok.text(), Negate: tok.negate}, true
	}

	s, ok := lookupSpelling(key)
	if !ok {
		// An unknown operator is not an error, it is free text — and the whole
		// token including the colon, since that is what the user was looking for.
		return Term{Value: tok.text(), Negate: tok.negate}, true
	}

	f := fields[s.field]
	negate := tok.negate != s.negate

	if f.kind == kindFlag {
		// A flag needs no value, but people write `is_pending:false` anyway.
		// Honour an explicit polarity and ignore anything else.
		if b, ok := parseBool(value); ok && !b {
			negate = !negate
		}
		return Term{Field: s.field, Op: CmpSet, Negate: negate}, true
	}

	// An empty value constrains nothing — `merchant:` is a query mid-typing, and
	// emitting a term for it would match every row and read as a broken filter.
	// (It is dropped whether or not it was written as `merchant:""`; "the
	// merchant name is the empty string" is not a question anyone asks, and
	// has_no_notes covers the real "this field is blank" intent.)
	if strings.TrimSpace(value) == "" {
		return Term{}, false
	}

	return Term{Field: s.field, Op: s.op, Value: value, Negate: negate}, true
}

func parseBool(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "y", "1":
		return true, true
	case "false", "no", "n", "0":
		return false, true
	}
	return false, false
}
