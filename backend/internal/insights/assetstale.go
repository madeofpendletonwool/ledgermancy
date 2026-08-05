package insights

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
)

// asset_stale — a manually entered asset value has aged past the point where it
// still describes reality.
//
// This is the highest-value part of doc 26 and it needs no external API at all.
// For most households the home is the largest line on the net-worth sheet, and
// it is wrong within months of being typed in. Nobody opens the net-worth page
// to re-enter a house value, so the number quietly drifts and every projection
// built on it inherits the drift, compounded over decades.
//
// Detection is a date comparison — no heuristic, no model. The feed already
// reaches a phone, so the nudge costs nothing beyond noticing.
//
// Note what does NOT appear here: bonds. A savings bond revalues itself every
// month from published rates (jobs.RevalueBondsWorker), so nudging about one
// would be asking the user to confirm arithmetic the app has already done. The
// query excludes them.

const (
	// maxStaleAssetCandidates keeps a household that has filed a dozen assets
	// from filling the feed with them. Past a handful, the net-worth page
	// sorted by staleness is the right surface, not one row each.
	maxStaleAssetCandidates = 4

	// staleAssetUrgentMonths is when a stale value stops being a note. Two
	// years without a review is long enough that the figure is more likely
	// wrong than right.
	staleAssetUrgentMonths = 24
)

type assetStaleProducer struct{}

func (assetStaleProducer) Kind() string { return "asset_stale" }

func (assetStaleProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	rows, err := q.ListStaleAssets(ctx, dbgen.ListStaleAssetsParams{
		HouseholdID: householdID,
		StaleBefore: networth.StaleBefore(now),
	})
	if err != nil {
		return nil, err
	}

	out := make([]Candidate, 0, len(rows))
	for i, row := range rows {
		if i >= maxStaleAssetCandidates {
			break
		}

		months := monthsSince(row.AsOf, now)
		priority := 2
		if months >= staleAssetUrgentMonths {
			priority = 3
		}

		out = append(out, Candidate{
			Kind:     "asset_stale",
			Priority: priority,
			Title:    fmt.Sprintf("%s hasn't been revalued in %s", row.Name, humanMonths(months)),
			Body: fmt.Sprintf(
				"You set %s to %s %s ago, and it has counted as that in your net worth ever since. "+
					"%s Updating it takes a moment and makes every figure built on it honest again.",
				row.Name, money(row.Value), humanMonths(months), staleHint(row.Kind)),
			Data: map[string]any{
				"asset_id":     row.ID.String(),
				"asset_name":   row.Name,
				"asset_kind":   row.Kind,
				"value":        row.Value.StringFixed(2),
				"as_of":        row.AsOf.Format(time.DateOnly),
				"months_stale": months,
			},
			// The as_of date is part of the identity, so revaluing the asset
			// retires this insight and a later drift raises a fresh one —
			// rather than the same row resurfacing every day forever.
			DedupeKey: fmt.Sprintf("asset_stale:%s:%s", row.ID, row.AsOf.Format(time.DateOnly)),
		})
	}
	return out, nil
}

// staleHint says something class-specific about why the number has moved,
// because "update your asset" is advice nobody acts on.
func staleHint(kind string) string {
	switch kind {
	case "home":
		return "House prices move a lot in a year."
	case "vehicle":
		return "Cars depreciate on a fairly predictable curve, and we can suggest a figure."
	case "collectible":
		return "Collectible values drift, and this app has no source for them."
	default:
		return "Values drift."
	}
}

func monthsSince(from, now time.Time) int {
	from, now = from.UTC(), now.UTC()
	months := (now.Year()-from.Year())*12 + int(now.Month()) - int(from.Month())
	if now.Day() < from.Day() {
		months--
	}
	if months < 0 {
		return 0
	}
	return months
}

// humanMonths renders a month count the way a person would say it.
func humanMonths(months int) string {
	switch {
	case months < 12:
		return fmt.Sprintf("%d months", months)
	case months < 24:
		return "over a year"
	default:
		return fmt.Sprintf("over %d years", months/12)
	}
}
