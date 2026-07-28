package insights

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/obligations"
)

// upcoming_bill — a known obligation is about to fall due.
//
// A surprise autopay is the most common way a household ends up overdrawn, and
// this is the one insight in the feed that can prevent it outright rather than
// explain it afterwards. Detection is pure expansion of a stored cadence: no
// heuristic, no model, no guess about whether the charge will happen.
//
// The dedupe key carries the due DATE, so the same monthly bill raises one row
// per cycle rather than one forever — and the row it raises this month is a new
// insert, which is what makes it eligible to push.

const (
	// How far ahead a bill is worth mentioning. Long enough to move money, short
	// enough that the feed is not a list of everything due this quarter.
	upcomingBillHorizonDays = 7

	// Amount bands, in whole dollars. The push threshold in the jobs layer is
	// priority 4, so these are what decide whether a bill reaches a phone: a
	// mortgage should, a $9 streaming charge should not.
	upcomingBillPushDollars   = 200
	upcomingBillUrgentDollars = 500

	// A busy household can have a dozen bills in a week. Only the largest few are
	// worth a feed row each; below that the Schedule page is the right surface.
	maxUpcomingBillCandidates = 6
)

type upcomingBillProducer struct{}

func (upcomingBillProducer) Kind() string { return "upcoming_bill" }

func (upcomingBillProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	horizon := today.AddDate(0, 0, upcomingBillHorizonDays)

	// sharedUser: the feed is a household surface, so a member's private
	// obligation must not appear in it.
	due, err := obligations.ListUpcoming(ctx, q, householdID, sharedUser, today, horizon)
	if err != nil {
		return nil, err
	}
	if len(due) == 0 {
		return nil, nil
	}

	// Largest first, so the cap keeps the bills that matter rather than the ones
	// that happen to fall soonest.
	sort.SliceStable(due, func(i, j int) bool {
		return due[j].Amount.LessThan(due[i].Amount)
	})

	total := decimal.Zero
	for _, o := range due {
		total = total.Add(o.Amount)
	}

	pushBand := decimal.NewFromInt(upcomingBillPushDollars)
	urgentBand := decimal.NewFromInt(upcomingBillUrgentDollars)

	out := make([]Candidate, 0, maxUpcomingBillCandidates)
	for i, o := range due {
		if i >= maxUpcomingBillCandidates {
			break
		}

		days := int(o.DueDate.Sub(today).Hours() / 24)
		when := fmt.Sprintf("in %d days", days)
		switch days {
		case 0:
			when = "today"
		case 1:
			when = "tomorrow"
		}

		priority := 2
		switch {
		case !o.Amount.LessThan(urgentBand):
			priority = 5
		case !o.Amount.LessThan(pushBand):
			priority = 4
		}

		amount := o.Amount.Round(2)
		out = append(out, Candidate{
			Kind:     "upcoming_bill",
			Priority: priority,
			Title:    fmt.Sprintf("%s due %s", o.Label, when),
			Body: fmt.Sprintf(
				"%s of %s is due on %s. Across the next %d days you have %s of known bills.",
				o.Label, amount.StringFixed(2), o.DueDate.Format("Monday 2 January"),
				upcomingBillHorizonDays, total.StringFixed(2)),
			Data: map[string]any{
				"label":          o.Label,
				"amount":         amount.StringFixed(2),
				"due_date":       o.DueDate.Format(time.DateOnly),
				"days_until_due": days,
				"cadence":        o.Cadence.Label(),
				"source":         o.Source,
				"window_days":    upcomingBillHorizonDays,
				"window_total":   total.StringFixed(2),
			},
			// The due date is part of the identity: next month's instance of the
			// same bill is a different insight, and must be able to push again.
			DedupeKey: fmt.Sprintf("upcoming_bill:%s:%s",
				o.ObligationID, o.DueDate.Format(time.DateOnly)),
		})
	}
	return out, nil
}
