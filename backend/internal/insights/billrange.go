package insights

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/obligations"
)

// bill_out_of_range — a bill was paid, but not for what the household said to
// expect (MAD-120).
//
// This is the third answer to "what happened to this bill?", and the one nothing
// in the app could give before. upcoming_bill says it is coming; overdue_bill
// says nothing paid it; this says a payment landed and it was $90 against a
// stated $40–$60.
//
// The distinction from merchant_outlier is worth stating, because both flag a
// charge that is bigger than usual. merchant_outlier is statistical — it learns a
// baseline from history and reports a deviation from it, which means it needs
// history and can only ever say "unusual for you". This producer is a comparison
// against something the member typed. It fires on the first cycle, needs no
// history at all, and can say "more than you told us to expect" — a claim the
// statistical one is not entitled to make.
//
// Detection is the single SQL query ListBillRangeExceptions, which shares its
// candidate predicate with the overdue matcher so the two can never both claim
// (or both drop) the same occurrence. No heuristic lives here in Go.
//
// The dedupe key carries the due DATE, matching upcoming_bill and overdue_bill:
// next cycle's surprise is its own insight and free to push again.

const (
	// How far back to look for a surprise. Matches overdueLookbackDays so the two
	// halves of the bill matcher cover the same window — an occurrence should not
	// be able to fall out of one and into neither.
	billRangeLookbackDays = overdueLookbackDays

	// Amount bands, in whole dollars, applied to the OVERAGE rather than to the
	// charge. A $700 mortgage payment that came in $12 high is noise; a $60 phone
	// bill that came in $50 high is the thing this feature exists to catch. The
	// push threshold in the jobs layer is priority 4.
	billRangePushDollars   = 50
	billRangeUrgentDollars = 200

	// Beyond a handful the Reminders page is the right surface, matching the
	// overdue producer's cap.
	billRangeMaxCandidates = 8
)

type billOutOfRangeProducer struct{}

func (billOutOfRangeProducer) Kind() string { return "bill_out_of_range" }

// occurrences is this producer's whole detection: every occurrence whose
// payment landed outside the stated range. Detect truncates it to the few worth
// a feed row; LiveKeys maps all of it, matching the overdue matcher it shares a
// candidate predicate with.
func (billOutOfRangeProducer) occurrences(
	ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time,
) ([]dbgen.ListBillRangeExceptionsRow, error) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	from := today.AddDate(0, 0, -(billRangeLookbackDays - 1))
	// Occurrences up to today inclusive: unlike an overdue reminder, a surprise
	// charge is worth reporting the moment it posts, and a bill can be paid on
	// its due date.
	to := today

	// sharedUser (uuid.Nil) collapses the visibility OR to shared-only, matching
	// every other household feed producer: a member's private bill must not
	// surface in the shared feed.
	return q.ListBillRangeExceptions(ctx, dbgen.ListBillRangeExceptionsParams{
		HouseholdID: householdID,
		UserID:      &sharedUser,
		Column3:     from,
		Column4:     to,
	})
}

// LiveKeys implements Retractor. This one is less obvious than its two
// siblings, because "you were charged $90 against a stated $40–$60" reads like a
// historical fact — but the range it compares against is something a member
// TYPES, and the insight's own body invites them to change it ("if it's the new
// normal, widen the range"). Taking that advice has to clear the row; before
// retraction it left the surprise standing against a range that no longer
// considered it surprising, which made the suggestion actively misleading.
//
// The same lookback caveat as overdue_bill applies: an exception ages out of
// billRangeLookbackDays and is retracted. The history view (include_dismissed)
// still holds it.
func (p billOutOfRangeProducer) LiveKeys(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]string, error) {
	rows, err := p.occurrences(ctx, q, householdID, now)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(rows))
	for _, r := range rows {
		keys = append(keys, BillOutOfRangeDedupeKey(r.ObligationID, r.DueDate))
	}
	return keys, nil
}

func (p billOutOfRangeProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	rows, err := p.occurrences(ctx, q, householdID, now)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	pushBand := decimal.NewFromInt(billRangePushDollars)
	urgentBand := decimal.NewFromInt(billRangeUrgentDollars)

	out := make([]Candidate, 0, billRangeMaxCandidates)
	for i, r := range rows {
		if i >= billRangeMaxCandidates {
			break
		}
		// The query only returns rows with both bounds set, so this is always a
		// complete range; the guard keeps the nil case honest rather than assumed.
		rng := billRange(r)
		if rng == nil {
			continue
		}

		charged := r.ChargedAmount.Round(2)

		// Which way it missed, and by how much. Both directions are reported: a
		// bill that came in far UNDER the expected floor is just as likely to be a
		// partial payment or a proration the household wants to know about.
		var direction string
		var gap decimal.Decimal
		if charged.GreaterThan(rng.Max) {
			direction, gap = "over", charged.Sub(rng.Max)
		} else {
			direction, gap = "under", rng.Min.Sub(charged)
		}
		gap = gap.Round(2)

		priority := 3
		switch {
		case !gap.LessThan(urgentBand):
			priority = 5
		case !gap.LessThan(pushBand):
			priority = 4
		}
		// An under-charge is informational, never urgent: paying less than
		// expected does not overdraw anyone. Capping it below the push threshold
		// keeps a prorated first month off a phone at 6am.
		if direction == "under" && priority > 3 {
			priority = 3
		}

		headline := fmt.Sprintf("%s came in at %s", r.Label, money(charged))
		body := fmt.Sprintf(
			"%s was charged %s on %s, %s more than the %s you expect. Check the bill if that wasn't planned; if it's the new normal, widen the range.",
			r.Label, money(charged), r.ChargedDate.Format("Monday 2 January"),
			money(gap), rng.Label())
		if direction == "under" {
			body = fmt.Sprintf(
				"%s was charged %s on %s, %s less than the %s you expect. Worth checking it was the full payment.",
				r.Label, money(charged), r.ChargedDate.Format("Monday 2 January"),
				money(gap), rng.Label())
		}

		out = append(out, Candidate{
			Kind:     "bill_out_of_range",
			Priority: priority,
			Title:    headline,
			Body:     body,
			Data: map[string]any{
				// obligation_id + due_date are what "accept this as the payment"
				// needs to satisfy this exact occurrence, matching overdue_bill so
				// the UI can drive both from one action.
				"obligation_id":  r.ObligationID.String(),
				"transaction_id": r.TransactionID.String(),
				"label":          r.Label,
				"expected_min":   rng.Min.StringFixed(2),
				"expected_max":   rng.Max.StringFixed(2),
				"charged_amount": charged.StringFixed(2),
				"charged_date":   r.ChargedDate.Format(time.DateOnly),
				"charged_name":   r.ChargedName,
				"difference":     gap.StringFixed(2),
				"direction":      direction,
				"due_date":       r.DueDate.Format(time.DateOnly),
				"cadence": obligations.Cadence{
					Count: r.IntervalCount, Unit: r.IntervalUnit,
				}.Label(),
				"source": r.Source,
			},
			DedupeKey: BillOutOfRangeDedupeKey(r.ObligationID, r.DueDate),
		})
	}
	return out, nil
}

// billRange rebuilds the stated range from one exception row. The query filters
// on amount_min IS NOT NULL and the column CHECK is both-or-neither, so a row
// reaching here always carries both bounds.
func billRange(r dbgen.ListBillRangeExceptionsRow) *obligations.AmountRange {
	if !r.AmountMin.Valid || !r.AmountMax.Valid {
		return nil
	}
	return &obligations.AmountRange{Min: r.AmountMin.Decimal, Max: r.AmountMax.Decimal}
}

// BillOutOfRangeDedupeKey is the stable identity of one out-of-range insight.
// Shared with the mark-paid handler, which dismisses the row the moment a member
// accepts the charge — the same arrangement overdue_bill has, so accepting a
// surprise charge clears it from the feed immediately rather than at the next
// generation pass.
func BillOutOfRangeDedupeKey(obligationID uuid.UUID, dueDate time.Time) string {
	return fmt.Sprintf("bill_out_of_range:%s:%s", obligationID, dueDate.Format(time.DateOnly))
}
