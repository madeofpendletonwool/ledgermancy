package insights

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// plan_stale — the household has a written plan and nobody has looked at it in
// six months (MAD-258).
//
// The plan's whole value is that it is KEPT CURRENT: a strategy written the
// year the first child arrived and never re-read is worse than no plan, because
// it still says what the household used to mean and nothing notices. The review
// stamp is the "I have re-read this and it still says what we mean" action on
// the Plan page; this producer is the other half of that mechanic — the nudge
// when the stamp (and every edit) has gone quiet.
//
// Staleness is measured against the NEWEST of the review stamp, the last
// section edit, and the last confirmed decision. An edit is a weaker signal
// than a review — changing a paragraph does not mean anybody re-read the rest —
// but it IS evidence somebody was there, and nudging a household that updated
// the plan yesterday about reviewing it would be noise. The clock starts from
// involvement, and the review stamp resets it properly.
//
// Detection is three cheap indexed reads (GetPlanReviewState + the two touch
// queries). No AI, no heuristics: a date comparison.

const (
	// planStaleDays is the quiet window. Six months, because a plan sits
	// between a budget (reviewed constantly) and an estate document (reviewed
	// rarely): it should be re-read roughly twice a year.
	planStaleDays = 182

	// planStaleKindDedupe is the whole dedupe key. This is a STATE claim, not
	// an event: there is one plan and it is either stale or it is not, so
	// there is at most one live row per household ever.
	planStaleKindDedupe = "plan_stale"
)

type planStaleProducer struct{}

func (planStaleProducer) Kind() string { return "plan_stale" }

// stale is this producer's whole detection, shared by Detect and LiveKeys so
// the raise and the retraction cannot disagree about what "still stale" means
// — the same shape overdueBillProducer uses.
//
// Returns the candidate's reference time (newest involvement) when the plan is
// stale, and the zero time when it is not.
func (p planStaleProducer) stale(
	ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time,
) (time.Time, bool, error) {
	state, err := q.GetPlanReviewState(ctx, householdID)
	if err != nil {
		return time.Time{}, false, err
	}
	// A proposal alone is not a plan (GetPlanReviewState's has_content counts
	// sections and confirmed decisions only), and a household with no plan has
	// nothing to review.
	if state.HasContent == nil || !*state.HasContent {
		return time.Time{}, false, nil
	}

	// The newest involvement across stamp, section edits and confirmed
	// decisions. The touch queries return ErrNoRows when that kind of content
	// does not exist, which is simply "no signal", not an error.
	ref := time.Time{}
	if state.PlanReviewedAt != nil {
		ref = *state.PlanReviewedAt
	}
	if t, err := q.LatestPlanSectionTouch(ctx, householdID); err == nil && t.After(ref) {
		ref = t
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, err
	}
	if t, err := q.LatestPlanDecisionTouch(ctx, householdID); err == nil && t.After(ref) {
		ref = t
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, err
	}

	if ref.IsZero() {
		return time.Time{}, false, nil
	}
	if now.Sub(ref) < planStaleDays*24*time.Hour {
		return time.Time{}, false, nil
	}
	return ref, true, nil
}

// LiveKeys implements Retractor because "the plan is stale" is a claim about
// the present that a single click on the Plan page falsifies: the review stamp
// lands and the row must leave the feed, exactly as a paid bill retires an
// overdue_bill.
func (p planStaleProducer) LiveKeys(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]string, error) {
	if _, stale, err := p.stale(ctx, q, householdID, now); err != nil {
		return nil, err
	} else if stale {
		return []string{planStaleKindDedupe}, nil
	}
	return nil, nil
}

func (p planStaleProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	ref, stale, err := p.stale(ctx, q, householdID, now)
	if err != nil || !stale {
		return nil, err
	}

	days := int(now.Sub(ref).Hours() / 24)
	months := days / 30
	when := fmt.Sprintf("%d months", months)
	if months < 1 {
		when = "weeks"
	}

	return []Candidate{{
		Kind:     "plan_stale",
		Priority: 2,
		Title:    "The financial plan hasn't been reviewed in a while",
		Body: fmt.Sprintf(
			"It's been about %s since the plan was last reviewed or updated. Open the Plan page, re-read it, and either stamp it as reviewed or update what has changed.",
			when),
		Data: map[string]any{
			"days_since_activity": days,
			"last_activity":       ref.Format(time.DateOnly),
		},
		DedupeKey: planStaleKindDedupe,
	}}, nil
}
