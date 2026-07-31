package reporting

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/obligations"
)

// EstimatedProjection layers a trailing-median estimate of income and typical
// spending on top of obligations.Project's known-obligations baseline. The
// known series (Known) is never touched — see obligations.Project's doc
// comment for why a guess must never be blended into that line. Everything
// else on this struct IS a guess, and every field name says so.
type EstimatedProjection struct {
	Known obligations.Projection

	// ExpectedMonthlyIncome / ExtraMonthlySpend are the trailing-median inputs,
	// computed the same way BuildSafeToSpend computes them (same window, same
	// medianOf), so the two figures never disagree about what's "typical".
	ExpectedMonthlyIncome decimal.Decimal
	ExtraMonthlySpend     decimal.Decimal
	// IncomeMonths is how many of the trailing window's months had any income,
	// so the caller can caveat a thin history the same way Safe to Spend does.
	IncomeMonths int
	// HasIncomeHistory is false when the household has no income history at
	// all. The estimate is meaningless without it, so the caller should not
	// render the line rather than show "minus typical spend" mislabeled as an
	// income-aware estimate.
	HasIncomeHistory bool

	// EstimatedCombined mirrors Known.Combined.Points positionally: one balance
	// per day, income and typical spending layered on top of the same known
	// obligations.
	EstimatedCombined []decimal.Decimal
	// EstimatedAccounts mirrors Known.Accounts positionally, keyed by account ID.
	EstimatedAccounts map[uuid.UUID][]decimal.Decimal
}

// BuildEstimatedProjection computes the known-obligations projection and lays
// a trailing-median income/spending estimate on top of it as a wholly
// separate series. Two rules keep it honest:
//
//   - The estimate never touches obligations.Project's output. Known stays
//     exactly what it always was.
//   - Spend already reflected in a tracked obligation is netted out of the
//     trailing median before it is added as "extra" — otherwise a household's
//     rent would count once as a known bill and a second time as part of
//     "typical spending".
func BuildEstimatedProjection(
	ctx context.Context,
	q *dbgen.Queries,
	householdID, userID uuid.UUID,
	now time.Time,
	days int,
) (EstimatedProjection, error) {
	known, err := obligations.Project(ctx, q, householdID, userID, now, days)
	if err != nil {
		return EstimatedProjection{}, err
	}

	mStart := firstOfMonth(now)
	windowStart := mStart.AddDate(0, -safeWindowMonths, 0)
	windowEnd := mStart.AddDate(0, 0, -1)

	// Household-shared (uuid.Nil), matching BuildSafeToSpend: the trailing
	// income/spend figures describe the household, not one member's slice of
	// it, so they read the same regardless of who is looking at the schedule.
	trend, err := q.GetMonthlyTrend(ctx, dbgen.GetMonthlyTrendParams{
		HouseholdID: householdID, UserID: uuid.Nil,
		Date: windowStart, Date_2: windowEnd,
		ExcludeOneTime: true,
	})
	if err != nil {
		return EstimatedProjection{}, err
	}

	incomeMonthly := make([]decimal.Decimal, 0, len(trend))
	spendMonthly := make([]decimal.Decimal, 0, len(trend))
	for _, m := range trend {
		if m.Income.IsPositive() {
			incomeMonthly = append(incomeMonthly, m.Income)
		}
		spendMonthly = append(spendMonthly, m.Spending)
	}
	expectedIncome := medianOf(incomeMonthly)
	medianSpend := medianOf(spendMonthly)
	incomeMonths := len(incomeMonthly)

	// The obligations visible to THIS request's user (private + shared),
	// matching the visibility obligations.Project just used to build Known —
	// otherwise the netting-out below could subtract bills the known line
	// never counted in the first place.
	knownMonthly, err := knownMonthlyObligationsTotal(ctx, q, householdID, userID, now)
	if err != nil {
		return EstimatedProjection{}, err
	}

	extraSpend := netExtraSpend(medianSpend, knownMonthly)

	ep := EstimatedProjection{
		Known:                 known,
		ExpectedMonthlyIncome: expectedIncome.Round(2),
		ExtraMonthlySpend:     extraSpend.Round(2),
		IncomeMonths:          incomeMonths,
		HasIncomeHistory:      incomeMonths > 0,
		EstimatedAccounts:     make(map[uuid.UUID][]decimal.Decimal, len(known.Accounts)),
	}

	netDrift := decimal.Zero
	if ep.HasIncomeHistory {
		netDrift = expectedIncome.Sub(extraSpend)
	}

	ep.EstimatedCombined = estimateSeries(known.Combined.CurrentBalance, known.Combined.Points, netDrift)
	for _, a := range known.Accounts {
		ep.EstimatedAccounts[a.AccountID] = estimateSeries(a.CurrentBalance, a.Points, netDrift)
	}

	return ep, nil
}

// netExtraSpend is the trailing spend median with the household's known,
// tracked obligations subtracted out — what's typically spent BEYOND the bills
// already on the known-obligations line. Clamped at zero: a household whose
// tracked obligations exceed its trailing spend median (a newly added annual
// premium, say) has no "extra" left to guess at, not a negative one.
func netExtraSpend(medianSpend, knownMonthly decimal.Decimal) decimal.Decimal {
	extra := medianSpend.Sub(knownMonthly)
	if extra.IsNegative() {
		return decimal.Zero
	}
	return extra
}

// knownMonthlyObligationsTotal is what the household's tracked obligations
// cost per month on average (obligations.MonthlyEstimate, summed over active,
// unexpired rows). Netting this out of the trailing spend median is what keeps
// a tracked bill from being counted twice: once as a known due date, once as
// part of "typical spending".
func knownMonthlyObligationsTotal(ctx context.Context, q *dbgen.Queries, householdID, userID uuid.UUID, asOf time.Time) (decimal.Decimal, error) {
	rows, err := q.ListObligations(ctx, dbgen.ListObligationsParams{HouseholdID: householdID, UserID: &userID})
	if err != nil {
		return decimal.Zero, err
	}
	total := decimal.Zero
	for _, o := range rows {
		if !o.IsActive {
			continue
		}
		if o.EndDate != nil && o.EndDate.Before(asOf) {
			continue
		}
		total = total.Add(obligations.MonthlyEstimate(o.Amount, obligations.Cadence{
			Count: o.IntervalCount, Unit: o.IntervalUnit,
		}))
	}
	return total, nil
}

// estimateSeries lays a flat daily drift (a monthly figure spread over however
// many days are in that calendar month) on top of the same day-by-day due
// amounts obligations.Project already computed, so the two series only ever
// differ by the estimate, never by re-deriving what's due.
func estimateSeries(current decimal.Decimal, points []obligations.Point, netMonthlyDrift decimal.Decimal) []decimal.Decimal {
	out := make([]decimal.Decimal, len(points))
	balance := current
	for i, p := range points {
		drift := netMonthlyDrift.Div(decimal.NewFromInt(int64(daysInMonth(p.Date))))
		balance = balance.Sub(p.Due).Add(drift)
		out[i] = balance.Round(2)
	}
	return out
}

// daysInMonth is the number of days in t's calendar month, so a monthly
// estimate can be spread evenly across it regardless of length.
func daysInMonth(t time.Time) int {
	return time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
