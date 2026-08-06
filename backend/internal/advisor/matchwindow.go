package advisor

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/payroll"
)

// THE EMPLOYER MATCH IS CALENDAR-BOUNDED, AND THAT IS WHAT MAKES IT TIER 2.
//
// A match is captured per pay period. Headroom not deferred by the last cheque
// of the tax year is not late, it is GONE — the employer does not true it up in
// January. That is the property that puts it above debt in the waterfall: a 22%
// card is worth the same next month, and an uncaptured match is worth nothing.
//
// So the option must actually disappear when the year's pay periods are
// exhausted, or the app spends December telling a household to capture money it
// can no longer reach.
const (
	matchWindowPayroll  = "your employer's pay schedule"
	matchWindowCalendar = "months left in the tax year"
)

// matchWindow reports how many pay periods remain in the tax year.
//
// TWO SOURCES, AND THE PRECISE ONE IS ONLY USED WHEN IT IS UNAMBIGUOUS.
//
// Doc 23 stores employers.pay_frequency and payroll.RemainingPayPeriods counts
// pay dates from the calendar. That is the real answer — and it is the only one
// that can return ZERO, which is what makes the option disappear in late
// December rather than lingering at "one month left".
//
// It is used only when the household has exactly ONE employer with a usable
// frequency. Nothing in the schema links a retirement account to an employer, so
// with two employers on file, attributing one's fortnightly schedule to the
// other's 401(k) would be a guess dressed as precision. A multi-employer
// household falls back to whole months left in the year, which is never zero and
// never wrong — only less precise.
//
// A household with no payroll data at all gets the calendar too, which is the
// ordinary case: paystubs are opt-in and the match option must work without
// them.
func matchWindow(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) (int, string, error) {
	employers, err := q.ListEmployers(ctx, dbgen.ListEmployersParams{
		HouseholdID: householdID, UserID: sharedUser,
	})
	if err != nil {
		return 0, "", err
	}

	if len(employers) == 1 {
		e := employers[0]
		if n, ok := payroll.RemainingPayPeriods(now, payroll.PayFrequency(e.PayFrequency), now.UTC().Year()); ok {
			return n, matchWindowPayroll, nil
		}
	}

	return monthsLeftInYear(now), matchWindowCalendar, nil
}
