package insights

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/advisor"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// advisor — the proactive cash-flow advisor riding the insight spine.
//
// ONE INSIGHT PER ADVISOR RUN, CARRYING THE WHOLE RANKED LIST, DISMISSED AS A
// UNIT. That is deliberate and it is the feed's grain. insights.dismissed_at is
// one nullable column on one row, so an insight per option would need a table to
// dismiss them separately — and one insight per option floods a feed that has to
// stay readable. Per-option suppression is a different need with a different
// lifetime and lives in the preferences store; see advisor/suppress.go.
//
// WHY A PRODUCER RATHER THAN THE WEEKLY JOB THE DOC SPECIFIES. The doc calls for
// a weekly job in internal/jobs. A producer gets the same cadence for less: the
// insight sweep already runs hourly, and a dedupe key carrying the ISO week
// means the run upserts one row per week per household — the second and later
// sweeps in a week refresh the same row rather than raising a new one. What that
// buys over a second scheduler is the whole delivery path for free: the feed,
// the high-priority push (jobs.GenerateInsightsWorker pushes on Inserted, which
// is true exactly once per week), doc 25's digest, and dismissal. A separate job
// would have had to re-implement the dedupe to avoid raising an advisor insight
// every hour, and that dedupe is the only thing "weekly" actually means here.
type advisorProducer struct{}

func (advisorProducer) Kind() string { return "advisor" }

// advisorPriority is high enough to push.
//
// The advisor is the one producer whose whole point is that it is unsolicited —
// "the most useful money advice is the advice you did not ask for" — so it sits
// above the informational producers. It is not the maximum: an overdrawn account
// or a duplicate charge is a fact about money already gone, and this is a
// proposal about money not yet spent.
const advisorPriority = 4

func (advisorProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	adv, err := advisor.Build(ctx, q, householdID, now)
	if err != nil {
		return nil, err
	}

	// Nothing to say: slack at or below the threshold, or zero or negative.
	// Never a "you have -$300 to allocate" suggestion.
	if !adv.Significant || len(adv.Options) == 0 {
		return nil, nil
	}

	top := adv.Options[0]

	// Facts, all finished. The model rewords around these; it never recomputes
	// one. Money is StringFixed(2) throughout, and the ORDER is carried
	// explicitly so a phrasing pass cannot quietly re-rank the list.
	data := map[string]any{
		"slack":        adv.Slack.StringFixed(2),
		"slack_basis":  adv.SlackBasis,
		"hurdle":       adv.Hurdle.StringFixed(2),
		"hurdle_basis": adv.HurdleBasis,
		"option_count": len(adv.Options),
	}
	for i, o := range adv.Options {
		p := fmt.Sprintf("option_%d_", i+1)
		data[p+"label"] = o.Label
		data[p+"amount"] = o.Amount.StringFixed(2)
		data[p+"value"] = o.Value.StringFixed(2)
		data[p+"value_kind"] = o.ValueKind
		data[p+"tier"] = o.Tier
		data[p+"detail"] = o.Detail
		if o.Note != "" {
			data[p+"note"] = o.Note
		}
	}

	// The headline is CONDITIONAL, and that conditional is load-bearing. The
	// Budgets page prints this identical figure as "left to spend freely"; if
	// this said "you have $400 available" the two surfaces would be giving
	// opposite instructions about the same number, and a household that noticed
	// would trust neither. "If you don't spend it" is true, is computable, and
	// contradicts nothing.
	title := fmt.Sprintf("If you don't spend $%s this month", adv.Slack.StringFixed(2))
	body := fmt.Sprintf(
		"%s Ranked by a published rule, not a judgement — %d option%s in all.",
		top.Detail, len(adv.Options), plural(len(adv.Options)),
	)

	return []Candidate{{
		Kind:     "advisor",
		Priority: advisorPriority,
		Title:    title,
		Body:     body,
		Data:     data,
		// Deliberately NO Period. A period-scoped insight is hidden once its
		// month passes (see ListInsights), and the advisor's dedupe is weekly:
		// a run in the last week of a month would vanish from the feed days
		// after it was raised.
		DedupeKey: advisorDedupeKey(householdID, now),
	}}, nil
}

// advisorDedupeKey is stable for one household for one ISO week, which is what
// makes the hourly sweep produce a weekly insight. ISO year, not calendar year:
// the two disagree in the last days of December, and a key that changes twice
// in one week would raise two advisor insights back to back.
func advisorDedupeKey(householdID uuid.UUID, now time.Time) string {
	year, week := now.UTC().ISOWeek()
	return fmt.Sprintf("advisor:%s:%d-W%02d", householdID, year, week)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
