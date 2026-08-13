package search

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The date grammar has three shapes, in the order they are tried:
//
//	keyword   today, yesterday, start-of-this-month, end-of-last-year
//	relative  -30d, -6m, +2w, 1y   (bare means the past: 30d == -30d)
//	absolute  2026-01-01
//
// Keywords may be written with spaces, hyphens or underscores between words, so
// `since:"start of this month"` and `since:start-of-this-month` are the same
// thing — the hyphenated form exists so the common case needs no quoting.
//
// Everything resolves to a calendar DATE, because t.date is a DATE. There is no
// time-of-day in this grammar and no timezone conversion: the reference `now` the
// caller passes in decides what "today" means, and the answer is that day.

var relativeDate = regexp.MustCompile(`^([+-]?)(\d+)([dwmy])$`)

// ParseDate resolves one date value against a reference time. It is exported
// because the rules engine needs the same grammar for a trigger like
// "created since the start of last month".
func ParseDate(value string, now time.Time) (time.Time, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return time.Time{}, fmt.Errorf("empty date")
	}

	// Absolute first: it is the only unambiguous form, and trying it first means
	// a keyword table can never accidentally shadow a real date.
	if d, err := time.ParseInLocation(time.DateOnly, raw, now.Location()); err == nil {
		return dayOf(d), nil
	}

	today := dayOf(now)
	key := normaliseDateWords(raw)

	if d, ok := dateKeyword(key, today); ok {
		return d, nil
	}

	// Matched against the raw value, not the normalised key: normalisation eats
	// leading separators, and the `-` in `-30d` is the sign, not a word break.
	if m := relativeDate.FindStringSubmatch(strings.ToLower(raw)); m != nil {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			// Unreachable for a \d+ capture short of an overflow, and an
			// overflowing offset is not a date anybody meant.
			return time.Time{}, fmt.Errorf("date offset %q is out of range", raw)
		}
		if m[1] != "+" {
			// No sign means the past. "30d" in a search box is "the last thirty
			// days" every time; nobody searches forward by default.
			n = -n
		}
		switch m[3] {
		case "d":
			return today.AddDate(0, 0, n), nil
		case "w":
			return today.AddDate(0, 0, 7*n), nil
		case "m":
			return today.AddDate(0, n, 0), nil
		default:
			return today.AddDate(n, 0, 0), nil
		}
	}

	return time.Time{}, fmt.Errorf("%q is not a date: use 2026-01-01, -30d, or today", value)
}

// dateKeyword resolves the named days and period edges.
func dateKeyword(key string, today time.Time) (time.Time, bool) {
	switch key {
	case "today", "now":
		return today, true
	case "yesterday":
		return today.AddDate(0, 0, -1), true
	case "tomorrow":
		return today.AddDate(0, 0, 1), true
	}

	// Period edges, spelled `<start|end>_of_<this|last|next>_<week|month|year>`.
	parts := strings.Split(key, "_")
	if len(parts) != 4 || parts[1] != "of" {
		return time.Time{}, false
	}
	edge, which, unit := parts[0], parts[2], parts[3]
	if edge != "start" && edge != "end" {
		return time.Time{}, false
	}

	var offset int
	switch which {
	case "this":
		offset = 0
	case "last", "previous":
		offset = -1
	case "next":
		offset = 1
	default:
		return time.Time{}, false
	}

	switch unit {
	case "week":
		// Weeks start Monday. Go's Weekday has Sunday at 0, so Sunday is day 7
		// of the week that began six days earlier, not the start of a new one.
		back := int(today.Weekday()) - int(time.Monday)
		if back < 0 {
			back += 7
		}
		start := today.AddDate(0, 0, -back+7*offset)
		if edge == "start" {
			return start, true
		}
		return start.AddDate(0, 0, 6), true

	case "month":
		start := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, today.Location()).
			AddDate(0, offset, 0)
		if edge == "start" {
			return start, true
		}
		// The last day of the month: the day before the first of the next one.
		// Never day-28-plus-arithmetic, which gets February wrong.
		return start.AddDate(0, 1, -1), true

	case "year":
		start := time.Date(today.Year()+offset, time.January, 1, 0, 0, 0, 0, today.Location())
		if edge == "start" {
			return start, true
		}
		return time.Date(today.Year()+offset, time.December, 31, 0, 0, 0, 0, today.Location()), true
	}
	return time.Time{}, false
}

// normaliseDateWords lowercases and reduces every run of spaces, hyphens and
// underscores to a single underscore, so all three spellings of a keyword land
// on the same table entry.
func normaliseDateWords(s string) string {
	var b strings.Builder
	prevSep := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch r {
		case ' ', '\t', '-', '_':
			if !prevSep && b.Len() > 0 {
				b.WriteByte('_')
			}
			prevSep = true
		default:
			b.WriteRune(r)
			prevSep = false
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}

// dayOf strips the clock, leaving the calendar day in its own location.
func dayOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}
