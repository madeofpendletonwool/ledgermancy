package insights

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/goals"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/obligations"
)

// overdue_bill — a known obligation fell due and the matcher cannot find a
// payment for it.
//
// This is the "you haven't paid the credit card yet this month" insight. The
// bill calendar already knew the payment was due (upcoming_bill says so ahead of
// time); this producer speaks up once that time has passed and nothing in the
// household's transactions looks like the payment.
//
// Detection is the single SQL query ListOverdueUnsatisfiedObligations, which
// expands the cadence backward over a short window and applies four
// satisfaction guards (manual mark, auto-post, transaction match, transfer
// pair). No heuristic lives here in Go — keeping the whole matcher in SQL means
// the reminder and the "is this paid?" check the Schedule page renders cannot
// drift apart. Detection is pure SQL expansion + comparison: no model, no guess
// about whether the charge will happen.
//
// The dedupe key carries the due DATE: each cycle's occurrence is its own
// insight, so a monthly bill raises one row per missed cycle rather than one
// forever, and a late payment clears it for that cycle only.

const (
	// overdueLookbackDays bounds how far back the producer looks for unpaid
	// occurrences. Long enough to catch a bill missed near the start of the
	// month once a sync lands; short enough that a long-ago bill someone chose
	// not to chase doesn't sit in the feed forever. The satisfaction guards mean
	// a genuinely-paid occurrence never appears here regardless of age.
	overdueLookbackDays = 21

	// Amount bands mirror upcoming_bill: a missed mortgage is worth a push, a
	// missed $9 streaming charge is not. The push threshold in the jobs layer is
	// priority 4, so these decide whether an overdue bill reaches a phone.
	overduePushDollars   = 200
	overdueUrgentDollars = 500

	// A household can miss several small bills in a window. Beyond a handful the
	// Reminders page is the right surface, not one feed row each.
	overdueMaxCandidates = 8

	// A bill overdue by a week or more reads as urgent even at a small amount:
	// the issue is no longer "it might post tomorrow" but "this is genuinely
	// unpaid", so it earns a push regardless of size.
	overdueUrgentDays = 7
)

type overdueBillProducer struct{}

func (overdueBillProducer) Kind() string { return "overdue_bill" }

func (overdueBillProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	from := today.AddDate(0, 0, -(overdueLookbackDays - 1))
	// Strictly past: upcoming_bill owns today forward, so the two producers
	// never raise the same occurrence.
	to := today.AddDate(0, 0, -1)

	// sharedUser (uuid.Nil) collapses the visibility OR to shared-only, matching
	// every other household feed producer: a member's private obligation must not
	// surface in the shared feed.
	rows, err := q.ListOverdueUnsatisfiedObligations(ctx, dbgen.ListOverdueUnsatisfiedObligationsParams{
		HouseholdID: householdID,
		UserID:      &sharedUser,
		Column3:     from,
		Column4:     to,
	})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	pushBand := decimal.NewFromInt(overduePushDollars)
	urgentBand := decimal.NewFromInt(overdueUrgentDollars)

	out := make([]Candidate, 0, overdueMaxCandidates)
	for i, o := range rows {
		if i >= overdueMaxCandidates {
			break
		}

		daysOverdue := int(today.Sub(o.DueDate.UTC()).Hours() / 24)
		amount := o.Amount.Round(2)

		priority := 3
		switch {
		case !amount.LessThan(urgentBand):
			priority = 5
		case !amount.LessThan(pushBand):
			priority = 4
		case daysOverdue >= overdueUrgentDays:
			priority = 4
		}

		when := fmt.Sprintf("%d days ago", daysOverdue)
		if daysOverdue == 1 {
			when = "yesterday"
		}

		out = append(out, Candidate{
			Kind:     "overdue_bill",
			Priority: priority,
			Title:    fmt.Sprintf("%s was due %s", o.Label, when),
			Body: fmt.Sprintf(
				"%s of %s was due on %s and we can't find a payment for it. If it went through, mark it paid; otherwise check that it's still set up.",
				o.Label, amount.StringFixed(2), o.DueDate.Format("Monday 2 January")),
			Data: map[string]any{
				// obligation_id + due_date are what the "mark paid" action needs
				// to satisfy this exact occurrence, so the UI does not have to
				// re-derive them.
				"obligation_id": o.ObligationID.String(),
				"label":         o.Label,
				"amount":        amount.StringFixed(2),
				"due_date":      o.DueDate.Format(time.DateOnly),
				"days_overdue":  daysOverdue,
				"cadence": obligations.Cadence{
					Count: o.IntervalCount, Unit: o.IntervalUnit,
				}.Label(),
				"source": o.Source,
			},
			// The due date is part of the identity, matching upcoming_bill: next
			// cycle's missed payment is a different insight, free to push again.
			// OverdueBillDedupeKey is shared with the mark-paid handler, which
			// dismisses the matching feed row the moment a member confirms payment.
			DedupeKey: OverdueBillDedupeKey(o.ObligationID, o.DueDate),
		})
	}
	return out, nil
}

// OverdueBillDedupeKey is the stable identity of one overdue-bill insight. It is
// shared between the producer (which raises the row) and the reminders HTTP
// surface (which dismisses the row when a member marks the occurrence paid), so
// the two cannot drift on the key's shape.
func OverdueBillDedupeKey(obligationID uuid.UUID, dueDate time.Time) string {
	return fmt.Sprintf("overdue_bill:%s:%s", obligationID, dueDate.Format(time.DateOnly))
}

// payoff_progress — a debt-payoff goal whose current monthly payment won't hit
// its target date.
//
// This is the "make sure you pay the extra $700 toward the mortgage principal"
// insight. goals.ComputePayoff already derives the monthly payment required to
// clear the debt by the target date; this producer compares that to the payment
// the schedule is actually assuming and coaches the gap. The "extra $700" IS the
// shortfall — the difference between what the plan needs and what is currently
// going in.
//
// Mirrors goalProducer (savings coaching) but for kind='debt_payoff', which that
// producer deliberately skips: payoff arithmetic is amortization, not
// accumulation, and running a payoff goal through goals.Compute (the savings
// path) would read the debt's balance as progress. Only household-scoped payoff
// goals are coached here, matching the savings producer — the feed is
// household-shared, and surfacing a personal payoff goal here would leak it.
//
// Money in Data is finished StringFixed(2) — the model only rewords the nudge,
// it never re-divides a balance by months.

type payoffProgressProducer struct{}

func (payoffProgressProducer) Kind() string { return "payoff_progress" }

func (payoffProgressProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	rows, err := q.ListActiveHouseholdGoals(ctx, householdID)
	if err != nil {
		return nil, err
	}

	mStart := monthStart(now)
	var out []Candidate
	for _, g := range rows {
		// Savings goals are coached by goalProducer; this producer owns the
		// debt-payoff kind only. remind is the per-item opt-out (MAD-85).
		if g.Kind != "debt_payoff" || !g.Remind {
			continue
		}
		// A payoff goal needs a target date to be "behind": an open-ended one
		// amortizes or doesn't, but there is no deadline to miss. The Goals page
		// still shows its standing; coaching an open-ended payoff here would just
		// repeat that page in the feed.
		if g.TargetDate == nil {
			continue
		}
		// Unlinked (account deleted) payoff goal: nothing to amortize, and the
		// API already says so. Skip rather than coach from missing figures.
		if g.AccountID == nil {
			continue
		}

		row, err := q.GetGoalDebtTerms(ctx, dbgen.GetGoalDebtTermsParams{
			ID:          *g.AccountID,
			HouseholdID: householdID,
		})
		if err != nil {
			return nil, err
		}
		balance := row.Balance
		// Already cleared: the Goals page celebrates this; no nudge.
		if !balance.IsPositive() {
			continue
		}

		terms := goals.MergeDebtTerms(
			row.ManualApr, row.Apr, row.InterestRatePercentage,
			row.ObligationAmount, row.ManualMinimumPayment, row.MinimumPayment,
		)

		f := goals.ComputePayoff(balance, terms.APR, terms.Payment, g.TargetDate, now)

		// Never amortizes (payment at/below interest, or no payment known): the
		// Goals page reports "never pays off", which is the honest answer. A
		// shortfall nudge against an unamortizing debt would quote a required
		// payment that still would not clear it, so it stays quiet here.
		if f.NeverPaysOff {
			continue
		}
		// On track: the current payment clears the debt by the target date, so
		// there is nothing to coach. The target-unreachable case (RequiredMonthly
		// zero, TargetReachable false) also falls through here as "not behind in
		// a quotable way".
		if f.OnTrack || !f.TargetReachable || !f.RequiredMonthly.IsPositive() {
			continue
		}

		// How much MORE per month than is currently going in — the "extra $700".
		// RequiredMonthly already incorporates the target date, and Payment is
		// what the schedule assumes (the linked bill's amount, the typed minimum,
		// or the institution's). Both are exact decimal; this only subtracts.
		shortfall := f.RequiredMonthly.Sub(terms.Payment)
		if !shortfall.IsPositive() {
			continue
		}

		out = append(out, Candidate{
			Kind:     "payoff_progress",
			Priority: payoffPriority(shortfall, f.RequiredMonthly),
			Title:    fmt.Sprintf("%s payoff is behind", g.Name),
			Body: fmt.Sprintf(
				"To pay off %s by %s you'd need to put about %s toward it each month. Right now about %s is going in — roughly %s a month short.",
				g.Name, g.TargetDate.Format("January 2006"),
				money(f.RequiredMonthly), money(terms.Payment), money(shortfall)),
			Data: map[string]any{
				"goal_id":          g.ID.String(),
				"name":             g.Name,
				"target_date":      g.TargetDate.Format(time.DateOnly),
				"balance":          balance.StringFixed(2),
				"required_monthly": f.RequiredMonthly.StringFixed(2),
				"current_monthly":  terms.Payment.StringFixed(2),
				"shortfall":        shortfall.StringFixed(2),
				"months_left":      f.MonthsLeft,
				"status":           "behind",
			},
			Period:    &mStart,
			DedupeKey: fmt.Sprintf("payoff_progress:%s:%s", g.ID, mStart.Format("2006-01")),
		})
	}
	return out, nil
}

// payoffPriority scales with how far short the current payment is: a shortfall
// that is most of the required contribution is urgent (paying the minimum on a
// debt that needs double), a small gap is a note. Same shape as goalPriority.
func payoffPriority(shortfall, required decimal.Decimal) int {
	if !required.IsPositive() {
		return 3
	}
	ratio := shortfall.Div(required)
	switch {
	case ratio.GreaterThanOrEqual(decimal.NewFromFloat(0.75)):
		return 5
	case ratio.GreaterThanOrEqual(decimal.NewFromFloat(0.4)):
		return 4
	default:
		return 3
	}
}
