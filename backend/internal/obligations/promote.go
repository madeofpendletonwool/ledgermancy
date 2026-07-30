package obligations

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// isNoRows reports whether err is pgx's "query returned nothing".
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// RecurringLookbackMonths is the history the recurring detector reads, shared by
// every caller of GetRecurringMerchants so none of them can scan a different
// window than the others.
//
// Three years rather than one. A yearly charge has to be observed at least twice
// before its cadence is anything but a guess, and a twelve-month window can
// never see it twice.
const RecurringLookbackMonths = 36

// Promote turns the recurring detector's output into persisted, forward-looking
// obligations. It is safe to run on every insight pass:
//
//   - Upserts are keyed on (household_id, merchant_key) through a partial unique
//     index, so a re-run refreshes one row rather than adding a second.
//   - A row the user has edited is left completely alone (the WHERE on the DO
//     UPDATE), because the alternative is silently reverting every correction.
//   - Suppressed merchants never arrive: GetRecurringMerchants already excludes
//     anything in recurring_overrides. Rows promoted BEFORE a suppression are
//     retired here, so the suppression reaches the calendar too.
//   - Anything the detector no longer returns is retired at the end of the pass,
//     which is what keeps the calendar in step with the Spending page.
//
// Detection itself lives entirely in GetRecurringMerchants, including the test
// for whether a merchant has gone quiet. This function decides nothing about
// what recurs; it only persists the answer.
func Promote(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) (int, error) {
	// Household-shared scope: the calendar is a household surface, so a member's
	// private institution must not seed it.
	shared := uuid.Nil
	since := now.AddDate(0, -RecurringLookbackMonths, 0)

	if _, err := q.DeactivateSuppressedObligations(ctx, householdID); err != nil {
		return 0, fmt.Errorf("retire suppressed obligations: %w", err)
	}

	merchants, err := q.GetRecurringMerchants(ctx, dbgen.GetRecurringMerchantsParams{
		HouseholdID: householdID, UserID: shared, Date: since, Column4: dateOnly(now),
	})
	if err != nil {
		return 0, fmt.Errorf("detect recurring merchants: %w", err)
	}
	// No early return on an empty result. "Nothing recurs any more" is a real
	// answer, and acting on it means retiring every detected row rather than
	// leaving the calendar frozen at whatever it last believed.

	// The category a merchant is usually filed under. Without it every promoted
	// bill would look uncovered to the safe-to-spend split, which is exactly the
	// case where a fixed cost gets counted twice.
	catRows, err := q.GetMerchantDominantCategories(ctx, dbgen.GetMerchantDominantCategoriesParams{
		HouseholdID: householdID, UserID: shared, Date: since,
	})
	if err != nil {
		return 0, fmt.Errorf("merchant categories: %w", err)
	}
	// Both sides are keyed by the RESOLVED merchant key, so a merchant the
	// household has merged is looked up once under its entity rather than once
	// per descriptor.
	categories := make(map[string]uuid.UUID, len(catRows))
	for _, c := range catRows {
		if c.MerchantKey != "" {
			categories[c.MerchantKey] = c.CategoryID
		}
	}

	// Every key that survives to a live obligation on this pass. It drives the
	// retirement sweep below, so a merchant dropped by any of the guards in this
	// loop must NOT appear here — an unmappable cadence is as good as undetected,
	// and leaving its key in the list would strand the old row on the calendar.
	live := make([]string, 0, len(merchants))
	promoted := 0
	for _, m := range merchants {
		// Suppression is keyed by merchant_key, so an unkeyed row could never be
		// acted on.
		if m.MerchantKey == "" {
			continue
		}
		cadence, ok := CadenceForGapDays(m.AvgGapDays)
		if !ok {
			continue
		}

		var categoryID *uuid.UUID
		if id, found := categories[m.MerchantKey]; found {
			categoryID = &id
		}

		label := m.Merchant
		if label == "" {
			label = m.MerchantKey
		}

		// The anchor is the last observed charge, so the first derived occurrence
		// is the next one due rather than a re-run of one already paid.
		_, err := q.UpsertDetectedObligation(ctx, dbgen.UpsertDetectedObligationParams{
			HouseholdID:   householdID,
			Label:         label,
			Amount:        m.TypicalAmount.Round(2),
			CategoryID:    categoryID,
			IntervalCount: cadence.Count,
			IntervalUnit:  cadence.Unit,
			AnchorDate:    dateOnly(m.LastSeen),
			MerchantKey:   &m.MerchantKey,
		})
		if err != nil {
			// A user-edited row makes the DO UPDATE match nothing, which sqlc's
			// :one surfaces as no rows. That is the intended outcome, not a
			// failure — skip the write and keep going. The key still counts as
			// live: the merchant is detected, the row simply belongs to the user
			// now, and omitting it here would retire a bill they curated.
			if isNoRows(err) {
				live = append(live, m.MerchantKey)
				continue
			}
			return promoted, fmt.Errorf("promote merchant %s: %w", m.MerchantKey, err)
		}
		live = append(live, m.MerchantKey)
		promoted++
	}

	// Retire whatever the detector no longer vouches for. This is what stops a
	// merged merchant billing twice — promotion writes the entity-keyed row, and
	// this clears the raw-descriptor row the same pass.
	if _, err := q.DeactivateUndetectedObligations(ctx, dbgen.DeactivateUndetectedObligationsParams{
		HouseholdID:  householdID,
		DetectedKeys: live,
	}); err != nil {
		return promoted, fmt.Errorf("retire undetected obligations: %w", err)
	}

	return promoted, nil
}

// Cadence buckets, in average days between charges. The boundaries sit in the
// gaps between real cadences rather than at them, so ordinary jitter (a bill
// that lands on the next business day) cannot tip a monthly charge into
// something else.
//
// The human word lives here too, beside the interval it describes. It used to be
// a switch statement in the API package and another in the insight producers,
// each "mirroring" this table — which is how the Spending page could call a
// merchant monthly while the Schedule page billed it quarterly. One table, one
// answer.
var cadenceBuckets = []struct {
	maxGapDays float64
	cadence    Cadence
	label      string
}{
	{10, Cadence{Count: 1, Unit: UnitWeek}, "weekly"},
	{20, Cadence{Count: 2, Unit: UnitWeek}, "every 2 weeks"},
	{45, Cadence{Count: 1, Unit: UnitMonth}, "monthly"},
	// Two months. Added when the detector's gap ceiling went from 40 days to
	// 400: without it every gap from 45 to 135 days collapsed onto "quarterly",
	// so a bi-monthly bill was projected at two thirds of its real cost.
	{75, Cadence{Count: 2, Unit: UnitMonth}, "every 2 months"},
	{135, Cadence{Count: 3, Unit: UnitMonth}, "quarterly"},
	{270, Cadence{Count: 6, Unit: UnitMonth}, "every 6 months"},
	{450, Cadence{Count: 1, Unit: UnitYear}, "yearly"},
}

// CadenceLabel is the human word for an average day-gap — what the Spending
// page's recurring table and the assistant both show. Read from the same table
// CadenceForGapDays uses, so the word and the interval always agree.
//
// Unlike CadenceForGapDays this always returns something: a label is a caption,
// and a caption that is slightly coarse is fine where a wrong bill on a calendar
// is not.
func CadenceLabel(avgGapDays decimal.Decimal) string {
	days := avgGapDays.InexactFloat64()
	for _, b := range cadenceBuckets {
		if days < b.maxGapDays {
			return b.label
		}
	}
	return cadenceBuckets[len(cadenceBuckets)-1].label
}

// CadenceForGapDays maps an average day-gap to the nearest sane cadence. A raw
// gap is deliberately not stored: "every 30.6 days" is not a bill anyone
// recognises, and expanding it would drift a monthly charge off its day of the
// month within a year.
//
// Returns false when the gap is outside anything recognisable, in which case the
// merchant is not promoted at all — a bill on the calendar that is wrong is
// worse than one that is missing.
func CadenceForGapDays(avgGapDays decimal.Decimal) (Cadence, bool) {
	days := avgGapDays.InexactFloat64()
	if days < 2 {
		return Cadence{}, false
	}
	for _, b := range cadenceBuckets {
		if days < b.maxGapDays {
			return b.cadence, true
		}
	}
	return Cadence{}, false
}
