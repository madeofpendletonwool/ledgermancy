package continuity

import (
	"fmt"
	"os"
	"sort"
	"time"
)

// Policy is the classic generational retention shape: keep the last N, then one
// per week, then one per month.
type Policy struct {
	Daily   int
	Weekly  int
	Monthly int
}

// Prune decides which artefacts survive. It is a pure function of the artefact
// set — it touches no disk and takes no clock.
//
// No `now` parameter, deliberately. A retention rule expressed against wall
// time ("delete anything older than 7 days") behaves very badly in exactly the
// deployment this app targets: a home server that was powered off for three
// weeks comes back, runs one sweep, and finds every backup it has is older than
// the window. Expressing the rule against the *set* instead — the newest N, then
// one per distinct week present, then one per distinct month — means a host that
// has been asleep keeps what it has and simply starts a new daily series.
//
// The same property makes it trivially testable against fabricated timestamps,
// which is the only practical way to check a policy measured in months.
func Prune(arts []Artefact, p Policy) (keep, remove []Artefact) {
	sorted := make([]Artefact, len(arts))
	copy(sorted, arts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Taken.After(sorted[j].Taken) })

	keeping := make(map[string]bool, len(sorted))
	mark := func(a Artefact) { keeping[a.Path] = true }

	// The most recent N, unconditionally. This is the tier that matters for the
	// failure people actually have — "I deleted something yesterday".
	for i := 0; i < p.Daily && i < len(sorted); i++ {
		mark(sorted[i])
	}

	// Then the newest artefact in each of the most recent N distinct weeks, and
	// likewise for months. Walking newest-first means the representative kept
	// for a period is always its latest, which is the one most likely to
	// restore cleanly against the current schema.
	markPerBucket(sorted, p.Weekly, mark, func(t time.Time) string {
		year, week := t.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", year, week)
	})
	markPerBucket(sorted, p.Monthly, mark, func(t time.Time) string {
		return t.Format("2006-01")
	})

	for _, a := range sorted {
		if keeping[a.Path] {
			keep = append(keep, a)
		} else {
			remove = append(remove, a)
		}
	}
	return keep, remove
}

// markPerBucket keeps the newest artefact in each of the first `limit` distinct
// buckets, walking newest-first.
func markPerBucket(sorted []Artefact, limit int, mark func(Artefact), bucket func(time.Time) string) {
	if limit <= 0 {
		return
	}
	seen := map[string]bool{}
	for _, a := range sorted {
		key := bucket(a.Taken.UTC())
		if seen[key] {
			continue
		}
		seen[key] = true
		mark(a)
		if len(seen) >= limit {
			return
		}
	}
}

// Apply prunes one kind in one destination and returns how many files were
// removed.
//
// A delete that fails is logged into the returned error but does not stop the
// rest: the caller has just written a fresh backup, and refusing to tidy up
// because one old file is stuck would turn a cosmetic problem into a full disk.
func Apply(root, kind string, p Policy) (removed int, err error) {
	arts, err := List(root, kind)
	if err != nil {
		return 0, err
	}
	_, doomed := Prune(arts, p)

	var firstErr error
	for _, a := range doomed {
		if rmErr := os.Remove(a.Path); rmErr != nil && !os.IsNotExist(rmErr) {
			if firstErr == nil {
				firstErr = fmt.Errorf("remove %s: %w", a.Path, rmErr)
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}
