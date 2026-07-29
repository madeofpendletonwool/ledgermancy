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

// promotionLookbackMonths is the history the detector reads. Matches the
// Spending page's recurring table so the two never show different merchants.
const promotionLookbackMonths = 12

// promotionActiveDays is how recently a merchant must have charged to still be
// treated as a live obligation. A cancelled subscription stops charging; without
// this it would sit on the calendar forever, predicting money that will never
// leave.
const promotionActiveDays = 75

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
//
// Detection stays exactly as it was. This writes source='detected' rows
// alongside GetRecurringMerchants; it does not replace or consume it.
func Promote(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) (int, error) {
	// Household-shared scope: the calendar is a household surface, so a member's
	// private institution must not seed it.
	shared := uuid.Nil
	since := now.AddDate(0, -promotionLookbackMonths, 0)

	if _, err := q.DeactivateSuppressedObligations(ctx, householdID); err != nil {
		return 0, fmt.Errorf("retire suppressed obligations: %w", err)
	}

	merchants, err := q.GetRecurringMerchants(ctx, dbgen.GetRecurringMerchantsParams{
		HouseholdID: householdID, UserID: shared, Date: since,
	})
	if err != nil {
		return 0, fmt.Errorf("detect recurring merchants: %w", err)
	}
	if len(merchants) == 0 {
		return 0, nil
	}

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

	activeCutoff := dateOnly(now.AddDate(0, 0, -promotionActiveDays))
	promoted := 0
	for _, m := range merchants {
		// Suppression is keyed by merchant_key, so an unkeyed row could never be
		// acted on — and a gone-quiet merchant is a cancelled charge, not a bill.
		if m.MerchantKey == "" || dateOnly(m.LastSeen).Before(activeCutoff) {
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
			Amount:        m.AverageAmount.Round(2),
			CategoryID:    categoryID,
			IntervalCount: cadence.Count,
			IntervalUnit:  cadence.Unit,
			AnchorDate:    dateOnly(m.LastSeen),
			MerchantKey:   &m.MerchantKey,
		})
		if err != nil {
			// A user-edited row makes the DO UPDATE match nothing, which sqlc's
			// :one surfaces as no rows. That is the intended outcome, not a
			// failure — skip it and keep going.
			if isNoRows(err) {
				continue
			}
			return promoted, fmt.Errorf("promote merchant %s: %w", m.MerchantKey, err)
		}
		promoted++
	}
	return promoted, nil
}

// Cadence buckets, in average days between charges. The boundaries sit in the
// gaps between real cadences rather than at them, so ordinary jitter (a bill
// that lands on the next business day) cannot tip a monthly charge into
// something else. GetRecurringMerchants only emits gaps in 6–40 days today, but
// the full table is here because the mapping is the part that has to be right,
// not the part that has to be reachable.
var cadenceBuckets = []struct {
	maxGapDays float64
	cadence    Cadence
}{
	{10, Cadence{Count: 1, Unit: UnitWeek}},
	{20, Cadence{Count: 2, Unit: UnitWeek}},
	{45, Cadence{Count: 1, Unit: UnitMonth}},
	{135, Cadence{Count: 3, Unit: UnitMonth}},
	{270, Cadence{Count: 6, Unit: UnitMonth}},
	{450, Cadence{Count: 1, Unit: UnitYear}},
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
