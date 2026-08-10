package allocation

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
)

// The college drawdown, and the error it exists to not make.
//
// The obvious model — inflate the target to the enrollment year and compare it
// to the 529 balance at that instant — is wrong in BOTH directions at once. It
// ignores that years two through four cost more, and it ignores that the balance
// keeps compounding while they are being paid. TestFourYearTotalExceedsFourTimes
// the enrollment-year cost is the assertion that catches the first half of that;
// the hand-computed walk below catches the rest.

// collegeBaseline is testBaseline plus a 529 and a college goal.
//
// yearsToEnrollment and the rates are parameters because the two fixtures below
// want opposite things from them: one wants numbers a human can check in their
// head, the other wants the doc's own case.
func collegeBaseline(balance, annualCost decimal.Decimal, yearsOut, years int) Baseline {
	b := testBaseline()
	b.Buckets = append(b.Buckets, Bucket{
		AccountID: fiveTwo, Name: "529", Kind: BucketInvestment,
		Treatment: "529", Balance: balance,
	})
	b.Plans = append(b.Plans, networth.AccountPlan{
		ID: fiveTwo.String(), Name: "529", Treatment: "529", Balance: balance,
		// The beneficiary horizon the engine freezes the account at. Set to the
		// enrollment year so the projected balance is the money actually
		// available on day one of year 1.
		BeneficiaryCurrentAge: 18 - yearsOut,
		BeneficiaryTargetAge:  18,
	})
	b.CollegeGoals = []CollegeGoal{{
		ID: colID, Name: "State university", AnnualCost: annualCost,
		Years: years, AccountID: &fiveTwo,
		YearsToEnrollment: yearsOut, HorizonKnown: true,
	}}
	return b
}

func collegeResult(t *testing.T, r Result) CollegeResult {
	t.Helper()
	if len(r.College) != 1 {
		t.Fatalf("college results = %d, want 1", len(r.College))
	}
	return r.College[0]
}

// THE FULLY HAND-CHECKABLE CASE. Every rate is zero, so the arithmetic is
// arithmetic: $25,000 against four flat $10,000 years pays years one and two,
// half of year three, and none of year four.
//
// Rates of zero are not a degenerate case being dodged. They isolate the
// DRAWDOWN ORDER — pay at the start of the year, compound what is left — from
// the compounding, so a failure here is unambiguously about the walk rather than
// about a rate somebody changed.
func TestCollegeDrawdownWalksYearByYear(t *testing.T) {
	b := collegeBaseline(dec("25000"), dec("10000"), 5, 4)
	// College inflation equal to general inflation: the real excess is zero, so
	// each year costs the same in today's dollars.
	b.CollegeInflation = dec("3.00")
	b.InflationRate = dec("0.03")
	b.Assumptions.RealReturnRate = decimal.Zero

	got := collegeResult(t, run(t, b, Request{HorizonYears: 20, Splits: []Split{}}))

	if !got.Projectable {
		t.Fatalf("not projectable: %s", got.Note)
	}
	if want := dec("25000"); !got.BalanceAtEnrollment.Equal(want) {
		t.Fatalf("balance at enrollment = %s, want %s (no growth, no contributions)",
			got.BalanceAtEnrollment, want)
	}

	wantYears := []struct{ cost, covered, shortfall string }{
		{"10000.00", "10000.00", "0.00"},
		{"10000.00", "10000.00", "0.00"},
		{"10000.00", "5000.00", "5000.00"},
		{"10000.00", "0.00", "10000.00"},
	}
	if len(got.Years_) != len(wantYears) {
		t.Fatalf("years = %d, want %d", len(got.Years_), len(wantYears))
	}
	for i, w := range wantYears {
		y := got.Years_[i]
		if y.Cost.StringFixed(2) != w.cost {
			t.Errorf("year %d cost = %s, want %s", y.Year, y.Cost, w.cost)
		}
		if y.Covered.StringFixed(2) != w.covered {
			t.Errorf("year %d covered = %s, want %s", y.Year, y.Covered, w.covered)
		}
		if y.Shortfall.StringFixed(2) != w.shortfall {
			t.Errorf("year %d shortfall = %s, want %s", y.Year, y.Shortfall, w.shortfall)
		}
	}

	if want := dec("40000"); !got.TotalCost.Equal(want) {
		t.Errorf("total cost = %s, want %s", got.TotalCost, want)
	}
	if want := dec("25000"); !got.TotalCovered.Equal(want) {
		t.Errorf("total covered = %s, want %s", got.TotalCovered, want)
	}
	if want := dec("15000"); !got.TotalShortfall.Equal(want) {
		t.Errorf("total shortfall = %s, want %s", got.TotalShortfall, want)
	}
	if want := dec("62.5"); !got.FundedPct.Equal(want) {
		t.Errorf("funded = %s%%, want %s%%", got.FundedPct, want)
	}
	// "Funded through sophomore year, short in junior year" is the sentence a
	// parent can act on, and it is the one a single-point comparison cannot
	// produce.
	if got.FirstShortfallYear != 3 {
		t.Errorf("first shortfall year = %d, want 3", got.FirstShortfallYear)
	}
}

// THE DOC'S OWN FIXTURE: a $20k 529 with $200/month at 6%, a beneficiary ten
// years from enrollment, against a $100k today's-cost ANNUAL target.
//
// The assertion the doc names explicitly is the last one — the four-year total
// must be STRICTLY GREATER than four times the enrollment-year cost, because
// years two, three and four inflate for another one, two and three years. That
// inequality is precisely the error the single-point version made.
func TestCollegeFourYearTotalExceedsFourTimesYearOne(t *testing.T) {
	b := collegeBaseline(dec("20000"), dec("100000"), 10, 4)
	b.CollegeInflation = dec("5.50")
	b.InflationRate = decimal.Zero // so the real excess IS the 5.5% the doc names
	b.Assumptions.RealReturnRate = dec("0.06")
	for i := range b.Plans {
		if b.Plans[i].ID == fiveTwo.String() {
			b.Plans[i].MonthlyContribution = dec("200")
		}
	}

	got := collegeResult(t, run(t, b, Request{HorizonYears: 20, Splits: []Split{}}))

	if !got.Projectable {
		t.Fatalf("not projectable: %s", got.Note)
	}

	// The balance at enrollment, derived independently: $20,000 compounding at
	// 6%/12 a month with $200 added after each month's growth, 120 times. Same
	// rule the engine uses ("growth before the deposit"), written as a plain loop
	// rather than read back out of the projection.
	want := dec("20000")
	monthlyRate := dec("0.06").Div(decimal.NewFromInt(12))
	for m := 0; m < 120; m++ {
		want = want.Add(want.Mul(monthlyRate)).Add(dec("200"))
	}
	if diff := got.BalanceAtEnrollment.Sub(want.Round(2)).Abs(); diff.GreaterThan(dec("0.01")) {
		t.Errorf("balance at enrollment = %s, want %s", got.BalanceAtEnrollment, want.Round(2))
	}

	// Each year costs strictly more than the one before it.
	for i := 1; i < len(got.Years_); i++ {
		if !got.Years_[i].Cost.GreaterThan(got.Years_[i-1].Cost) {
			t.Errorf("year %d (%s) does not cost more than year %d (%s) — the years are not being "+
				"inflated separately", i+1, got.Years_[i].Cost, i, got.Years_[i-1].Cost)
		}
	}

	// THE ASSERTION THE DOC CALLS OUT BY NAME.
	fourTimesYearOne := got.Years_[0].Cost.Mul(decimal.NewFromInt(4))
	if !got.TotalCost.GreaterThan(fourTimesYearOne) {
		t.Fatalf("four-year total %s is not greater than 4 × the enrollment-year cost %s — this is "+
			"exactly the error the single-point comparison made", got.TotalCost, fourTimesYearOne)
	}

	// Year one is the today's-dollars target inflated for the ten years to
	// enrollment: $100,000 × 1.055^10 ≈ $170,814.
	if got.Years_[0].Cost.LessThan(dec("170000")) || got.Years_[0].Cost.GreaterThan(dec("172000")) {
		t.Errorf("year one cost = %s, want ≈170,814 ($100k inflated 10 years at 5.5%%)", got.Years_[0].Cost)
	}

	// This 529 cannot come close, so the shortfall year is named rather than a
	// bare "underfunded".
	if got.FirstShortfallYear == 0 {
		t.Error("want a named first shortfall year")
	}
	if got.MonthlyNeeded == nil {
		t.Error("want a monthly figure that would close the gap")
	}
}

// The solved monthly figure must actually work. Feeding it back through the same
// drawdown is the check a closed-form solve could not offer — and it is why the
// solve bisects over the drawdown rather than inverting it.
func TestCollegeMonthlyNeededActuallyFundsIt(t *testing.T) {
	b := collegeBaseline(dec("20000"), dec("30000"), 10, 4)
	b.CollegeInflation = dec("5.50")
	b.InflationRate = dec("0.03")

	got := collegeResult(t, run(t, b, Request{HorizonYears: 20, Splits: []Split{}}))
	if got.MonthlyNeeded == nil {
		t.Fatal("no monthly figure returned for an underfunded goal")
	}

	// Re-run with that contribution actually in the plan. It should now fund
	// every year — the solved figure is the smallest that does, so a few cents
	// of slack is expected and a shortfall is not.
	funded := collegeBaseline(dec("20000"), dec("30000"), 10, 4)
	funded.CollegeInflation = dec("5.50")
	funded.InflationRate = dec("0.03")

	after := collegeResult(t, run(t, funded, Request{
		Monthly: *got.MonthlyNeeded, HorizonYears: 20,
		Splits: []Split{{AccountID: fiveTwo, MonthlyPct: dec("100")}},
	}))
	if after.TotalShortfall.IsPositive() {
		t.Errorf("still %s short at the solved contribution of %s/mo",
			after.TotalShortfall, got.MonthlyNeeded)
	}
}

// THE CONVERSION THAT IS EASY TO GET FLATTERINGLY WRONG. The bucket's balance is
// projected at a REAL return, so it is already in today's dollars. Inflating the
// cost at the full 5.5% against it would count general inflation twice and
// overstate the shortfall by several years of compounding.
func TestCollegeUsesTheExcessOverGeneralInflation(t *testing.T) {
	b := collegeBaseline(dec("50000"), dec("20000"), 5, 4)
	b.CollegeInflation = dec("5.50")
	b.InflationRate = dec("0.03")

	got := collegeResult(t, run(t, b, Request{HorizonYears: 20, Splits: []Split{}}))

	// (1.055 / 1.03) − 1 ≈ 0.024272.
	if diff := got.RealExcessRate.Sub(dec("0.024272")).Abs(); diff.GreaterThan(dec("0.00001")) {
		t.Errorf("real excess rate = %s, want ≈0.024272", got.RealExcessRate)
	}
	// And the reported rate is still the household's stated 5.5%, so both facts
	// are visible: what was set, and what was actually applied.
	if want := dec("5.50"); !got.InflationRate.Equal(want) {
		t.Errorf("inflation rate = %s, want the stored %s", got.InflationRate, want)
	}

	// The first year costs $20,000 inflated at the EXCESS for five years, not at
	// the full college rate — roughly $22,550, not the ~$26,100 the double-count
	// would produce.
	if got.Years_[0].Cost.GreaterThan(dec("24000")) {
		t.Errorf("year one cost = %s, which looks like general inflation counted twice", got.Years_[0].Cost)
	}
}

// A 529 with its OWN assumed real return compounds at that rate, not the
// household's. This is the fix for "3% across everything": the engine had the
// per-account plumbing (AccountPlan.RealReturnRate) but the standing college
// projection never populated it and its drawdown read the household default, so
// every account — a 529 included — compounded at the one household rate. With
// the column wired through loadInvestments and returnRateFor, an account that
// carries its own rate uses it even when the request has no split (which is the
// college_projection path: it runs with no allocation).
func TestCollegeUsesAccountOwnReturnRate(t *testing.T) {
	// Two identical households; only the 529's own rate differs.
	base := func() Baseline {
		b := collegeBaseline(dec("20000"), dec("100000"), 10, 4)
		b.CollegeInflation = dec("5.50")
		b.InflationRate = dec("0.03")
		b.Assumptions.RealReturnRate = dec("0.03") // household stays conservative
		return b
	}

	withHouseholdRate := base() // 529 plan rate unset -> household 3%
	withAccountRate := base()
	for i := range withAccountRate.Plans {
		if withAccountRate.Plans[i].ID == fiveTwo.String() {
			withAccountRate.Plans[i].RealReturnRate = dec("0.06") // the 529 earns more
		}
	}

	req := Request{HorizonYears: 20, Splits: []Split{}}
	lower := collegeResult(t, run(t, withHouseholdRate, req))
	higher := collegeResult(t, run(t, withAccountRate, req))

	if !lower.Projectable || !higher.Projectable {
		t.Fatalf("both must project: household=%v account=%v", lower.Projectable, higher.Projectable)
	}
	// A higher compounding rate grows the balance more and shrinks the monthly
	// figure needed. If these flip, the per-account rate is not being read.
	if !higher.BalanceAtEnrollment.GreaterThan(lower.BalanceAtEnrollment) {
		t.Errorf("enrollment balance at 6%% (%s) should exceed 3%% (%s)",
			higher.BalanceAtEnrollment, lower.BalanceAtEnrollment)
	}
	if higher.MonthlyNeeded == nil || lower.MonthlyNeeded == nil {
		t.Fatal("want a monthly-needed figure for both")
	}
	if !higher.MonthlyNeeded.LessThan(*lower.MonthlyNeeded) {
		t.Errorf("monthly needed at 6%% (%s) should be less than at 3%% (%s)",
			higher.MonthlyNeeded, lower.MonthlyNeeded)
	}
}


// A goal whose beneficiary has no resolvable age is REPORTED as unprojectable
// rather than projected against a guessed enrollment year. Same refusal to guess
// that networth.ResolveAge exists for.
func TestCollegeWithNoHorizonIsReportedNotGuessed(t *testing.T) {
	b := collegeBaseline(dec("20000"), dec("30000"), 10, 4)
	b.CollegeGoals[0].HorizonKnown = false

	got := collegeResult(t, run(t, b, Request{HorizonYears: 20, Splits: []Split{}}))
	if got.Projectable {
		t.Fatal("a goal with no resolvable enrollment year must not be projected")
	}
	if got.Note == "" {
		t.Error("want a note saying what is missing")
	}
	if len(got.Years_) != 0 {
		t.Error("want no per-year figures for an unprojectable goal")
	}
}

// A goal with no linked account has no balance to draw down, and says so rather
// than reporting 0% funded — which would read as a fact about the money.
func TestCollegeWithNoLinkedAccountIsReported(t *testing.T) {
	b := collegeBaseline(dec("20000"), dec("30000"), 10, 4)
	b.CollegeGoals[0].AccountID = nil

	got := collegeResult(t, run(t, b, Request{HorizonYears: 20, Splits: []Split{}}))
	if got.Projectable {
		t.Fatal("want unprojectable with no account linked")
	}
	if got.Note == "" {
		t.Error("want a note pointing at the missing link")
	}
}

// The per-goal year count is honoured: community-college transfers and five-year
// programmes exist, and four is a default rather than a law.
func TestCollegeYearCountIsPerGoal(t *testing.T) {
	for _, years := range []int{2, 4, 5} {
		b := collegeBaseline(dec("500000"), dec("10000"), 5, years)
		b.CollegeInflation = dec("3.00")
		b.InflationRate = dec("0.03")

		got := collegeResult(t, run(t, b, Request{HorizonYears: 20, Splits: []Split{}}))
		if len(got.Years_) != years {
			t.Errorf("years detail = %d, want %d", len(got.Years_), years)
		}
		if got.Years != years {
			t.Errorf("years = %d, want %d", got.Years, years)
		}
	}
}

// The summary line is built from the computed figures and nothing else — it is
// what a chat tool or an insight body quotes, so it must not be a second
// derivation of the same numbers.
func TestCollegeSummaryLineNamesTheShortfallYear(t *testing.T) {
	b := collegeBaseline(dec("25000"), dec("10000"), 5, 4)
	b.CollegeInflation = dec("3.00")
	b.InflationRate = dec("0.03")
	b.Assumptions.RealReturnRate = decimal.Zero

	got := collegeResult(t, run(t, b, Request{HorizonYears: 20, Splits: []Split{}}))
	line := got.CollegeSummaryLine()
	for _, want := range []string{"year 2", "year 3", "5000.00"} {
		if !strings.Contains(line, want) {
			t.Errorf("summary %q does not mention %q", line, want)
		}
	}
}
