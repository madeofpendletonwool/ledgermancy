package advisor

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// PER-OPTION SUPPRESSION IS NOT INSIGHT DISMISSAL, AND THE TWO MUST NOT BE
// CONFLATED.
//
// insights.dismissed_at is ONE nullable column on ONE row. An advisor insight
// carries the whole ranked list, so dismissing it dismisses all of it at once —
// which is the feed's grain, and fighting it would mean one insight per option
// flooding a feed that has to stay readable. That is the right trade for
// "I've read this week's list".
//
// "Stop suggesting I pay down this card" is a DIFFERENT need with a different
// lifetime: it outlives the week, and it should survive the household never
// dismissing anything. It is served here, out of the preferences store doc 02
// already provides, as a household-scoped list of suppressed option keys checked
// by the ranker BEFORE an option is emitted. No new table either way.
type suppression map[string]bool

func (s suppression) has(key string) bool { return s[key] }

// list returns the suppressed keys in a stable order, for echoing back to a
// settings UI.
func (s suppression) list() []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// suppressedKeys reads the household's muted option keys. A missing or
// malformed preference means "nothing suppressed" — a run must never fail, and
// failing OPEN here is the safe direction: the household sees an option it asked
// not to see, rather than silently losing the highest-value thing the app can
// tell them.
func suppressedKeys(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID) suppression {
	out := suppression{}
	raw, ok := readPref(ctx, q, householdID, suppressedKey)
	if !ok {
		return out
	}
	var keys []string
	if err := json.Unmarshal(raw, &keys); err != nil {
		return out
	}
	for _, k := range keys {
		if k != "" {
			out[k] = true
		}
	}
	return out
}

// OptionKey builds the stable identity of one option.
//
// (kind, subject_id), with the subject omitted for the singleton options. It is
// exported because the settings endpoint that writes a suppression list must
// spell a key the same way the ranker reads it, and two spellings of the same
// key is a mute button that does nothing.
func OptionKey(kind, subjectID string) string {
	if subjectID == "" {
		return kind
	}
	return kind + ":" + subjectID
}
