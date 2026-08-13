package search

import "strings"

// segment is one run of a token's text, remembering whether it arrived quoted.
//
// Quotedness has to survive lexing because it decides what a colon MEANS. In
// `merchant:"whole foods"` the colon separates a key from a value; in
// `"AMZN:MKTP"` it is part of a merchant descriptor the user pasted. Both are
// one token, and only the quoting tells them apart.
type segment struct {
	text   string
	quoted bool
}

// token is one whitespace-delimited term, with its negation prefix stripped.
type token struct {
	negate   bool
	segments []segment
}

// text is the token's whole value with quotes resolved away.
func (t token) text() string {
	var b strings.Builder
	for _, s := range t.segments {
		b.WriteString(s.text)
	}
	return b.String()
}

// splitKey finds the first colon in an UNQUOTED segment and returns the key
// before it and the value after it. ok is false when the token carries no such
// colon, which makes it free text.
//
// The value keeps everything after that colon, including further colons, so
// `since:2026-01-01` and `merchant:AMZN:MKTP` both behave.
func (t token) splitKey() (key, value string, ok bool) {
	var kb, vb strings.Builder
	found := false
	for _, s := range t.segments {
		if found {
			vb.WriteString(s.text)
			continue
		}
		if s.quoted {
			kb.WriteString(s.text)
			continue
		}
		if i := strings.IndexByte(s.text, ':'); i >= 0 {
			kb.WriteString(s.text[:i])
			vb.WriteString(s.text[i+1:])
			found = true
			continue
		}
		kb.WriteString(s.text)
	}
	if !found {
		return "", "", false
	}
	return kb.String(), vb.String(), true
}

// lex splits a query string into tokens, honouring double and single quotes.
//
// It is deliberately total: there is no lex error. The search box sends a
// request as the user types, so an unterminated quote is a query mid-flight, not
// a mistake to reject — the closing quote is assumed at end of input. Anything
// this function cannot make sense of ends up as free text, which is the
// behaviour a user typing into a search box expects.
func lex(input string) []token {
	var tokens []token
	var cur token
	var seg strings.Builder
	inToken := false

	flushSegment := func(quoted bool) {
		// A quoted segment is kept even when empty: `merchant:""` is a
		// deliberate "empty value", and dropping it would silently turn the
		// term into the bare word `merchant`.
		if seg.Len() > 0 || quoted {
			cur.segments = append(cur.segments, segment{text: seg.String(), quoted: quoted})
			seg.Reset()
		}
	}
	flushToken := func() {
		flushSegment(false)
		if len(cur.segments) > 0 {
			tokens = append(tokens, cur)
		}
		cur = token{}
		inToken = false
	}

	runes := []rune(input)
	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		switch {
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r':
			flushToken()

		case ch == '"' || ch == '\'':
			quote := ch
			inToken = true
			flushSegment(false)
			i++
			for ; i < len(runes); i++ {
				// Backslash escapes the quote character and itself, so a
				// descriptor containing a quote can still be searched for.
				if runes[i] == '\\' && i+1 < len(runes) &&
					(runes[i+1] == quote || runes[i+1] == '\\') {
					i++
					seg.WriteRune(runes[i])
					continue
				}
				if runes[i] == quote {
					break
				}
				seg.WriteRune(runes[i])
			}
			flushSegment(true)

		case ch == '-' && !inToken && !cur.negate && len(cur.segments) == 0 && seg.Len() == 0:
			// A leading dash negates the term. It only counts at the very start
			// of a token, so a hyphenated word (`start-of-last-month`, `T-MOBILE`)
			// and a negative amount keep their dashes.
			cur.negate = true

		default:
			inToken = true
			seg.WriteRune(ch)
		}
	}
	flushToken()

	// A lone `-` produces a token with the negate flag and nothing to negate.
	// Drop it rather than emitting a term that matches everything but nothing.
	out := tokens[:0]
	for _, t := range tokens {
		if t.text() == "" && !hasQuotedSegment(t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

func hasQuotedSegment(t token) bool {
	for _, s := range t.segments {
		if s.quoted {
			return true
		}
	}
	return false
}
