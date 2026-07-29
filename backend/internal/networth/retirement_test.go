package networth

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// Hand-computed fixtures are mandatory for this engine, and the reason is not
// ceremony: every other test in this package could pass while the projection
// quietly compounds at the wrong rate or applies a contribution a month early.
// The assertions below are cross-checked against the closed-form annuity
// formula in the comments, which is an independent calculation — not the same
// loop written twice.
//
// `now` is pinned inside a year the limits table configures (2026), so the
// tests do not start failing on a calendar boundary. That is deliberate: a test
// that depends on the wall clock is a test that will fail at midnight on New
// Year's Eve for reasons unrelated to the code.

var testNow = time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)

// dec lives in project_test.go, shared across the package's tests.

func baseAssumptions(months int) RetirementAssumptions {
	return RetirementAssumptions{
		RealReturnRate: dec("0.05"),
		WithdrawalRate: dec("0.04"),
		CurrentAge:     40,
		Months:         months,
	}
}

func mustEqual(t *testing.T, what string, got, want decimal.Decimal) {
	t.Helper()
	if !got.Equal(want) {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}

// TestProjectRetirementHandComputed is doc 15's required fixture: one account,
// $10,000, 5% real, $500/month, 120 months.
//
// Closed-form cross-check, with i = 0.05/12 and n = 120 (contributions land at
// month end, so this is an ordinary annuity):
//
//	FV = 10000·(1+i)^n + 500·((1+i)^n − 1)/i
//	   = 10000·1.6470095 + 500·155.28228
//	   = 16470.10 + 77641.14
//	   = 94111.2  ✓ matches the 94111.23 asserted below
func TestProjectRetirementHandComputed(t *testing.T) {
	plans := []AccountPlan{{
		ID: "a", Name: "Brokerage", Treatment: "taxable",
		Balance:             decimal.NewFromInt(10000),
		MonthlyContribution: decimal.NewFromInt(500),
	}}

	out := ProjectRetirement(plans, baseAssumptions(120), testNow)

	if len(out.Points) != 120 {
		t.Fatalf("points = %d, want 120", len(out.Points))
	}

	// Month 1, computable entirely by hand: growth applies to the opening
	// balance BEFORE the contribution, so the deposit earns nothing in the month
	// it arrives. 10000 × 0.05/12 = 41.67, then +500.
	mustEqual(t, "month 1 balance", out.Points[0].Retirement, dec("10541.67"))
	mustEqual(t, "month 1 growth", out.Points[0].Growth, dec("41.67"))
	mustEqual(t, "month 1 contributed", out.Points[0].Contributed, dec("500"))

	// 10541.666… × 0.05/12 = 43.92; 10541.67 + 43.92 + 500 = 11085.59.
	mustEqual(t, "month 2 balance", out.Points[1].Retirement, dec("11085.59"))

	// Month 120, against the closed form above.
	last := out.Points[119]
	mustEqual(t, "month 120 balance", last.Retirement, dec("94111.23"))
	// Contributed and Growth must still reconcile to the balance: 10000 opening
	// + 60000 saved + 24111.23 earned. If this ever drifts, the split the whole
	// page rests on has stopped meaning anything.
	mustEqual(t, "month 120 contributed", last.Contributed, dec("60000"))
	mustEqual(t, "month 120 growth", last.Growth, dec("24111.23"))
	mustEqual(t, "reconciliation",
		dec("10000").Add(last.Contributed).Add(last.Growth), last.Retirement)

	// Supported spending at 4%: 94111.23 × 0.04 = 3764.45 (3764.4492 → 3764.45).
	mustEqual(t, "supported spending", last.SupportedSpending, dec("3764.45"))
}

// TestContributionLimitBinds is the second required fixture: a limit that binds
// partway. $3,000/month into a 401(k) is $36,000 a year against a $24,500
// elective limit, so the projection must contribute $24,500 — not $36,000.
//
// Return is zeroed so the assertion is the cap and nothing else.
func TestContributionLimitBinds(t *testing.T) {
	plans := []AccountPlan{{
		ID: "b", Name: "401k", Treatment: "trad_401k",
		MonthlyContribution: decimal.NewFromInt(3000),
	}}
	a := baseAssumptions(12)
	a.RealReturnRate = decimal.Zero

	out := ProjectRetirement(plans, a, testNow)

	if !out.LimitsConfigured {
		t.Fatal("2026 limits should be configured")
	}
	mustEqual(t, "year one balance", out.Points[11].Retirement, dec("24500"))
	mustEqual(t, "year one contributed", out.Points[11].Contributed, dec("24500"))

	if len(out.CapNotes) != 1 {
		t.Fatalf("cap notes = %d, want 1 — a bound limit must be reported, not silently applied", len(out.CapNotes))
	}
	if out.CapNotes[0].Group != "401k" {
		t.Errorf("cap group = %q, want 401k", out.CapNotes[0].Group)
	}
	mustEqual(t, "planned", out.CapNotes[0].Planned, dec("36000"))
	mustEqual(t, "allowed", out.CapNotes[0].Allowed, dec("24500"))
}

// TestSharedLimitPoolsAccounts guards the thing a per-account cap gets wrong:
// two 401(k)s share ONE elective limit. $1,500/month into each is $36,000 a
// year combined, which must still be held to $24,500 in total.
func TestSharedLimitPoolsAccounts(t *testing.T) {
	plans := []AccountPlan{
		{ID: "t", Name: "Trad 401k", Treatment: "trad_401k", MonthlyContribution: decimal.NewFromInt(1500)},
		{ID: "r", Name: "Roth 401k", Treatment: "roth_401k", MonthlyContribution: decimal.NewFromInt(1500)},
	}
	a := baseAssumptions(12)
	a.RealReturnRate = decimal.Zero

	out := ProjectRetirement(plans, a, testNow)

	mustEqual(t, "combined year one", out.Points[11].Contributed, dec("24500"))
	// Scaled proportionally, so the user's own split between traditional and
	// Roth survives: half each.
	mustEqual(t, "traditional share", out.Points[11].ByAccount["t"].Contributed, dec("12250"))
	mustEqual(t, "roth share", out.Points[11].ByAccount["r"].Contributed, dec("12250"))
}

// TestEmployerMatchAtAnnualCap is the third required fixture. 6% of a $200,000
// salary is $12,000, but the plan caps the match at $5,000, so $5,000 is what
// lands — and it is reported separately from the employee's own $12,000.
func TestEmployerMatchAtAnnualCap(t *testing.T) {
	plans := []AccountPlan{{
		ID: "c", Name: "401k", Treatment: "trad_401k",
		MonthlyContribution: decimal.NewFromInt(1000),
		EmployerMatchPct:    dec("0.06"),
		AnnualSalary:        decimal.NewFromInt(200000),
		EmployerMatchLimit:  decimal.NewFromInt(5000),
	}}
	a := baseAssumptions(12)
	a.RealReturnRate = decimal.Zero

	out := ProjectRetirement(plans, a, testNow)

	last := out.Points[11]
	mustEqual(t, "employee contributed", last.Contributed, dec("12000"))
	mustEqual(t, "employer contributed", last.EmployerContributed, dec("5000"))
	mustEqual(t, "balance", last.Retirement, dec("17000"))
	// Per-account must agree with the household totals, employee money kept
	// apart from the employer's in both.
	mustEqual(t, "per-account employee", last.ByAccount["c"].Contributed, dec("12000"))
	mustEqual(t, "per-account employer", last.ByAccount["c"].Employer, dec("5000"))
}

// TestEmployerMatchNeedsSalary: a percentage with nothing to apply it to is not
// an amount, and inventing a salary to multiply it by is exactly the guess this
// feature exists to avoid.
func TestEmployerMatchNeedsSalary(t *testing.T) {
	plans := []AccountPlan{{
		ID: "d", Name: "401k", Treatment: "trad_401k",
		MonthlyContribution: decimal.NewFromInt(500),
		EmployerMatchPct:    dec("0.06"),
	}}
	a := baseAssumptions(12)
	a.RealReturnRate = decimal.Zero

	out := ProjectRetirement(plans, a, testNow)
	mustEqual(t, "employer contributed", out.Points[11].EmployerContributed, decimal.Zero)
}

// TestEmployerMatchBoundedByDeferral: nobody matches a contribution that was
// never made. A 6%-of-salary match against a $50/month deferral is $600.
func TestEmployerMatchBoundedByDeferral(t *testing.T) {
	plans := []AccountPlan{{
		ID: "e", Name: "401k", Treatment: "trad_401k",
		MonthlyContribution: decimal.NewFromInt(50),
		EmployerMatchPct:    dec("0.06"),
		AnnualSalary:        decimal.NewFromInt(200000),
	}}
	a := baseAssumptions(12)
	a.RealReturnRate = decimal.Zero

	out := ProjectRetirement(plans, a, testNow)
	mustEqual(t, "employer contributed", out.Points[11].EmployerContributed, dec("600"))
}

// TestUnconfiguredLimitYearDegrades: an unknown tax year must announce itself
// and project uncapped, never quietly apply a stale year's numbers.
func TestUnconfiguredLimitYearDegrades(t *testing.T) {
	future := time.Date(LatestConfiguredYear()+5, 1, 1, 0, 0, 0, 0, time.UTC)
	plans := []AccountPlan{{
		ID: "f", Name: "401k", Treatment: "trad_401k",
		MonthlyContribution: decimal.NewFromInt(3000),
	}}
	a := baseAssumptions(12)
	a.RealReturnRate = decimal.Zero

	out := ProjectRetirement(plans, a, future)

	if out.LimitsConfigured {
		t.Fatal("an unconfigured year must report LimitsConfigured=false")
	}
	if len(out.CapNotes) != 0 {
		t.Errorf("cap notes = %d, want 0 when no limits are known", len(out.CapNotes))
	}
	// Uncapped: the full $36,000, with the response's flag telling the UI to say
	// the headroom figures are unavailable rather than unlimited.
	mustEqual(t, "uncapped contribution", out.Points[11].Contributed, dec("36000"))
}

// TestUntaggedAccountsExcludedAndReported: silently omitting an account
// produces a confidently wrong number, so the exclusion has to be visible and
// carry its value.
func TestUntaggedAccountsExcludedAndReported(t *testing.T) {
	plans := []AccountPlan{
		{ID: "g", Name: "Brokerage", Treatment: "taxable", Balance: decimal.NewFromInt(10000)},
		{ID: "h", Name: "Mystery 401k", Balance: decimal.NewFromInt(50000)},
	}
	a := baseAssumptions(12)
	a.RealReturnRate = decimal.Zero

	out := ProjectRetirement(plans, a, testNow)

	if len(out.ExcludedAccounts) != 1 || out.ExcludedAccounts[0] != "Mystery 401k" {
		t.Fatalf("excluded = %v, want [Mystery 401k]", out.ExcludedAccounts)
	}
	mustEqual(t, "excluded value", out.ExcludedValue, dec("50000"))
	mustEqual(t, "nest egg", out.Points[11].Retirement, dec("10000"))
}

// TestEducationAccountNotInNestEgg: a 529 is not retirement money. Counting it
// overstates the household by the whole balance, and it stops compounding when
// the beneficiary reaches the age the money is for.
func TestEducationAccountNotInNestEgg(t *testing.T) {
	plans := []AccountPlan{
		{ID: "i", Name: "Brokerage", Treatment: "taxable", Balance: decimal.NewFromInt(10000)},
		{
			ID: "j", Name: "College", Treatment: "529",
			Balance:               decimal.NewFromInt(20000),
			MonthlyContribution:   decimal.NewFromInt(100),
			BeneficiaryCurrentAge: 17,
			BeneficiaryTargetAge:  18,
		},
	}
	a := baseAssumptions(36)
	a.RealReturnRate = decimal.Zero

	out := ProjectRetirement(plans, a, testNow)

	last := out.Points[35]
	mustEqual(t, "nest egg excludes the 529", last.Retirement, dec("10000"))
	// One year of $100/month, then the horizon stops it: 20000 + 1200.
	mustEqual(t, "education balance", last.Education, dec("21200"))
}

// TestSocialSecurityStartsOnTime: crediting Social Security early moves FI age
// years in the wrong direction for somebody planning around it.
func TestSocialSecurityStartsOnTime(t *testing.T) {
	plans := []AccountPlan{{
		ID: "k", Name: "Brokerage", Treatment: "taxable", Balance: decimal.NewFromInt(100000),
	}}
	a := baseAssumptions(120) // ages 40 through 49
	a.RealReturnRate = decimal.Zero
	a.AnnualSSIncome = decimal.NewFromInt(30000)
	a.SSStartAge = 45

	out := ProjectRetirement(plans, a, testNow)

	// Age 44 (month 60, index 59): portfolio only. 100000 × 0.04 = 4000.
	if out.Points[59].Age != 44 {
		t.Fatalf("month 60 age = %d, want 44", out.Points[59].Age)
	}
	mustEqual(t, "before SS", out.Points[59].SupportedSpending, dec("4000"))

	// Age 45 (month 61, index 60): 4000 + 30000.
	if out.Points[60].Age != 45 {
		t.Fatalf("month 61 age = %d, want 45", out.Points[60].Age)
	}
	mustEqual(t, "from SS start", out.Points[60].SupportedSpending, dec("34000"))
}

// TestFIAgeAlreadyReached: a household already past FI returns its current age,
// not a negative, not a zero, and not a panic.
func TestFIAgeAlreadyReached(t *testing.T) {
	plans := []AccountPlan{{
		ID: "l", Name: "Brokerage", Treatment: "taxable",
		Balance: decimal.NewFromInt(2000000),
	}}
	a := baseAssumptions(120)
	a.TargetAnnualSpending = decimal.NewFromInt(50000) // 2M × 4% = 80k, covered

	out := ProjectRetirement(plans, a, testNow)

	if !out.AlreadyFI {
		t.Fatal("AlreadyFI should be true when supported spending already covers the target")
	}
	if out.FIAge == nil {
		t.Fatal("FIAge should be set")
	}
	if *out.FIAge != 40 {
		t.Errorf("FIAge = %d, want the current age 40", *out.FIAge)
	}
}

// TestFIAgeFound scans to the first month that clears the bar.
func TestFIAgeFound(t *testing.T) {
	plans := []AccountPlan{{
		ID: "m", Name: "Brokerage", Treatment: "taxable",
		Balance:             decimal.NewFromInt(100000),
		MonthlyContribution: decimal.NewFromInt(3000),
	}}
	a := baseAssumptions(480) // to age 80
	a.TargetAnnualSpending = decimal.NewFromInt(40000)

	out := ProjectRetirement(plans, a, testNow)

	if out.AlreadyFI {
		t.Fatal("100k at 4% supports 4k, not 40k — should not be already FI")
	}
	if out.FIAge == nil || out.FIMonth == nil {
		t.Fatal("FI should be reached inside a 40-year horizon")
	}
	// Sanity, not a magic number: it must be after today and before the horizon.
	if *out.FIAge <= 40 || *out.FIAge > 80 {
		t.Errorf("FIAge = %d, outside the projected range", *out.FIAge)
	}
	// The month it names must actually clear the target.
	for _, p := range out.Points {
		if p.Month == *out.FIMonth {
			if p.SupportedSpending.LessThan(a.TargetAnnualSpending) {
				t.Errorf("FIMonth %s supports %s, below the %s target",
					p.Month, p.SupportedSpending, a.TargetAnnualSpending)
			}
			break
		}
	}
}

// TestFIAgeNotReachedIsNil: not reaching FI inside the horizon is an answer.
// Extrapolating past the projection would be a worse one.
func TestFIAgeNotReachedIsNil(t *testing.T) {
	plans := []AccountPlan{{
		ID: "n", Name: "Brokerage", Treatment: "taxable", Balance: decimal.NewFromInt(1000),
	}}
	a := baseAssumptions(120)
	a.RealReturnRate = decimal.Zero
	a.TargetAnnualSpending = decimal.NewFromInt(80000)

	out := ProjectRetirement(plans, a, testNow)

	if out.FIAge != nil {
		t.Errorf("FIAge = %d, want nil — FI is not reached here", *out.FIAge)
	}
	if out.AlreadyFI {
		t.Error("AlreadyFI should be false")
	}
}

// TestNoProjectableAccountsDoesNotPanic covers the empty-portfolio path, which
// the FI scan and the target lookup both have to survive.
func TestNoProjectableAccountsDoesNotPanic(t *testing.T) {
	out := ProjectRetirement(nil, baseAssumptions(120), testNow)
	if len(out.Points) != 0 {
		t.Errorf("points = %d, want 0", len(out.Points))
	}
	if out.FIAge != nil {
		t.Error("FIAge should be nil with nothing to project and no Social Security")
	}
}

// --------------------------------------------------------------------------
// Required savings rate
// --------------------------------------------------------------------------

// TestSolveRequiredSavingsUnreachable is the case the bisection bound exists
// for. "Save 340% of your income" is not an answer; it is a bug with a decimal
// point. The solve must say so, and must terminate.
func TestSolveRequiredSavingsUnreachable(t *testing.T) {
	plans := []AccountPlan{{
		ID: "o", Name: "Brokerage", Treatment: "taxable", Balance: decimal.Zero,
	}}
	a := baseAssumptions(0)
	a.CurrentAge = 60
	a.TargetRetirementAge = 61
	// A billion a year, one year away, from nothing.
	a.TargetAnnualSpending = decimal.NewFromInt(1000000000)

	done := make(chan SavingsRateSolve, 1)
	go func() { done <- SolveRequiredSavings(plans, a, decimal.NewFromInt(100000), testNow) }()

	select {
	case solve := <-done:
		if solve.Reachable {
			t.Fatal("this target is not reachable by saving; the solve must say so")
		}
		if solve.RequiredMonthly != nil {
			t.Errorf("RequiredMonthly = %s, want nil when unreachable", solve.RequiredMonthly)
		}
		if solve.Note == "" {
			t.Error("an unreachable solve must carry a reason")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("SolveRequiredSavings did not terminate — the bisection is unbounded")
	}
}

// TestSolveRequiredSavingsReachable: the figure returned must actually get
// there. Reporting the low side of the bracket would name an amount that does
// not reach the target.
func TestSolveRequiredSavingsReachable(t *testing.T) {
	plans := []AccountPlan{{
		ID: "p", Name: "Brokerage", Treatment: "taxable", Balance: decimal.NewFromInt(50000),
	}}
	a := baseAssumptions(0)
	a.CurrentAge = 40
	a.TargetRetirementAge = 65
	a.TargetAnnualSpending = decimal.NewFromInt(60000)

	solve := SolveRequiredSavings(plans, a, decimal.NewFromInt(120000), testNow)

	if !solve.Reachable || solve.RequiredMonthly == nil {
		t.Fatalf("expected a reachable solve, got %+v", solve)
	}

	// Verify by running the projection at the answer: it must clear the target.
	check := a
	check.Months = (a.TargetRetirementAge - a.CurrentAge) * 12
	verified := ProjectRetirement(distribute(plans, *solve.RequiredMonthly), check, testNow)
	last := verified.Points[len(verified.Points)-1]
	if last.SupportedSpending.LessThan(a.TargetAnnualSpending) {
		t.Errorf("solve returned %s/month, which supports only %s of a %s target",
			solve.RequiredMonthly, last.SupportedSpending, a.TargetAnnualSpending)
	}

	// And the rate is that figure over gross income: ×12 ÷ 120000.
	if solve.RequiredRate == nil {
		t.Fatal("RequiredRate should be present when income is known")
	}
	want := solve.RequiredMonthly.Mul(decimal.NewFromInt(12)).
		Div(decimal.NewFromInt(120000)).Round(4)
	mustEqual(t, "required rate", *solve.RequiredRate, want)
}

// TestSolveRequiredSavingsAlreadyThere returns zero, not a negative saving.
func TestSolveRequiredSavingsAlreadyThere(t *testing.T) {
	plans := []AccountPlan{{
		ID: "q", Name: "Brokerage", Treatment: "taxable", Balance: decimal.NewFromInt(3000000),
	}}
	a := baseAssumptions(0)
	a.CurrentAge = 40
	a.TargetRetirementAge = 65
	a.TargetAnnualSpending = decimal.NewFromInt(50000)

	solve := SolveRequiredSavings(plans, a, decimal.NewFromInt(120000), testNow)

	if !solve.Reachable || solve.RequiredMonthly == nil {
		t.Fatalf("expected reachable, got %+v", solve)
	}
	mustEqual(t, "required monthly", *solve.RequiredMonthly, decimal.Zero)
}

// TestSolveRequiredSavingsRateNeedsIncome: a rate with no denominator is not a
// rate. Nil is the honest return, not a division by zero.
func TestSolveRequiredSavingsRateNeedsIncome(t *testing.T) {
	plans := []AccountPlan{{
		ID: "r", Name: "Brokerage", Treatment: "taxable", Balance: decimal.NewFromInt(50000),
	}}
	a := baseAssumptions(0)
	a.CurrentAge = 40
	a.TargetRetirementAge = 65
	a.TargetAnnualSpending = decimal.NewFromInt(60000)

	solve := SolveRequiredSavings(plans, a, decimal.Zero, testNow)
	if solve.RequiredRate != nil {
		t.Errorf("RequiredRate = %s, want nil with no income to divide by", solve.RequiredRate)
	}
}

// TestSolveRequiredSavingsNeedsATarget: without an age to aim at there is no
// question to answer, and the note has to say which input is missing.
func TestSolveRequiredSavingsNeedsATarget(t *testing.T) {
	plans := []AccountPlan{{ID: "s", Name: "B", Treatment: "taxable"}}

	for _, tc := range []struct {
		name string
		mut  func(*RetirementAssumptions)
	}{
		{"no target age", func(a *RetirementAssumptions) { a.TargetRetirementAge = 0 }},
		{"target in the past", func(a *RetirementAssumptions) { a.TargetRetirementAge = 30 }},
		{"no target spending", func(a *RetirementAssumptions) { a.TargetAnnualSpending = decimal.Zero }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := baseAssumptions(0)
			a.CurrentAge = 40
			a.TargetRetirementAge = 65
			a.TargetAnnualSpending = decimal.NewFromInt(60000)
			tc.mut(&a)

			solve := SolveRequiredSavings(plans, a, decimal.NewFromInt(120000), testNow)
			if solve.Reachable {
				t.Error("should not claim reachable with a missing input")
			}
			if solve.Note == "" {
				t.Error("must name the missing input")
			}
		})
	}
}

// --------------------------------------------------------------------------
// Limits table
// --------------------------------------------------------------------------

func TestLimitsUnconfiguredYear(t *testing.T) {
	if _, ok := Limits(LatestConfiguredYear() + 10); ok {
		t.Error("a year past the table must report ok=false, never fall back to a stale year")
	}
	if _, ok := Limits(2026); !ok {
		t.Error("2026 should be configured")
	}
}

func TestAnnualLimitForCatchups(t *testing.T) {
	l, ok := Limits(2026)
	if !ok {
		t.Fatal("2026 limits missing")
	}

	for _, tc := range []struct {
		name      string
		treatment string
		age       int
		want      string
		capped    bool
	}{
		{"401k under 50", "trad_401k", 40, "24500", true},
		{"401k at 50 catch-up", "roth_401k", 52, "32500", true},
		{"401k in the 60-63 window", "trad_401k", 61, "35750", true},
		{"401k past the window falls back", "trad_401k", 64, "32500", true},
		{"IRA under 50", "roth_ira", 30, "7500", true},
		{"IRA at 50", "trad_ira", 55, "8600", true},
		{"HSA self-only", "hsa", 40, "4400", true},
		{"HSA with the age-55 catch-up", "hsa", 56, "5400", true},
		{"taxable has no cap", "taxable", 40, "", false},
		{"529 has no federal deferral cap", "529", 40, "", false},
		{"trust has no cap", "trust", 40, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, capped := AnnualLimitFor(tc.treatment, tc.age, l, false)
			if capped != tc.capped {
				t.Fatalf("capped = %v, want %v", capped, tc.capped)
			}
			if tc.capped {
				mustEqual(t, "limit", got, dec(tc.want))
			}
		})
	}
}

func TestAnnualLimitForFamilyHSA(t *testing.T) {
	l, _ := Limits(2026)
	got, _ := AnnualLimitFor("hsa", 40, l, true)
	mustEqual(t, "family HSA", got, dec("8750"))
}

// --------------------------------------------------------------------------
// Monte Carlo
// --------------------------------------------------------------------------

// TestMonteCarloIsDeterministic is the whole reason the seed is derived from the
// inputs. A survival rate that moves when you refresh the page is not a figure
// anyone can plan around.
func TestMonteCarloIsDeterministic(t *testing.T) {
	in := MonteCarloInput{
		StartBalance:     decimal.NewFromInt(1000000),
		AnnualWithdrawal: decimal.NewFromInt(40000),
		MeanRealReturn:   dec("0.05"),
		Volatility:       dec("0.15"),
		Years:            30,
		Runs:             500,
	}

	first := SimulateWithdrawals(in)
	second := SimulateWithdrawals(in)

	if first.Seed != second.Seed {
		t.Errorf("seed changed between runs: %d vs %d", first.Seed, second.Seed)
	}
	if first.Survived != second.Survived {
		t.Errorf("survived %d then %d — the same inputs must give the same answer",
			first.Survived, second.Survived)
	}
	if !first.SurvivalRate.Equal(second.SurvivalRate) {
		t.Errorf("survival rate %s then %s", first.SurvivalRate, second.SurvivalRate)
	}
	if !first.MedianEnding.Equal(second.MedianEnding) {
		t.Errorf("median ending %s then %s", first.MedianEnding, second.MedianEnding)
	}
	if first.Basis == "" {
		t.Error("a survival rate must always carry its basis")
	}
}

// TestMonteCarloSeedTracksInputs: change the scenario, change the sequences.
// Otherwise two different plans would be compared over identical dice, which
// hides real differences between them.
func TestMonteCarloSeedTracksInputs(t *testing.T) {
	base := MonteCarloInput{
		StartBalance:     decimal.NewFromInt(1000000),
		AnnualWithdrawal: decimal.NewFromInt(40000),
		MeanRealReturn:   dec("0.05"),
		Volatility:       dec("0.15"),
		Years:            30,
		Runs:             200,
	}
	other := base
	other.AnnualWithdrawal = decimal.NewFromInt(45000)

	if SimulateWithdrawals(base).Seed == SimulateWithdrawals(other).Seed {
		t.Error("a different scenario must draw different sequences")
	}
}

// TestMonteCarloExhaustionIsSurvivable checks the two ends of the range behave:
// an unfunded portfolio never survives, and a portfolio drawn at nothing always
// does.
func TestMonteCarloEnds(t *testing.T) {
	broke := SimulateWithdrawals(MonteCarloInput{
		StartBalance:     decimal.NewFromInt(1000),
		AnnualWithdrawal: decimal.NewFromInt(40000),
		MeanRealReturn:   dec("0.05"),
		Volatility:       dec("0.15"),
		Years:            30, Runs: 100,
	})
	if broke.Survived != 0 {
		t.Errorf("survived %d of 100 on $1,000 against $40,000 a year", broke.Survived)
	}

	safe := SimulateWithdrawals(MonteCarloInput{
		StartBalance:     decimal.NewFromInt(1000000),
		AnnualWithdrawal: decimal.Zero,
		MeanRealReturn:   dec("0.05"),
		Volatility:       decimal.Zero,
		Years:            30, Runs: 100,
	})
	if safe.Survived != 100 {
		t.Errorf("survived %d of 100 with no withdrawals at all", safe.Survived)
	}
	if !safe.SurvivalRate.Equal(decimal.NewFromInt(1)) {
		t.Errorf("survival rate = %s, want 1", safe.SurvivalRate)
	}
}

func TestClampRuns(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{0, DefaultMonteCarloRuns},
		{-5, DefaultMonteCarloRuns},
		{10, 100},
		{500, 500},
		{999999, 10000},
	} {
		if got := ClampRuns(tc.in); got != tc.want {
			t.Errorf("ClampRuns(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestProjectIsUnchanged guards the boundary doc 15 draws: the Net Worth page's
// straight-line model must keep behaving exactly as it did. This engine sits
// beside it, not on top of it.
func TestProjectIsUnchanged(t *testing.T) {
	start := Result{
		AssetsTotal:      decimal.NewFromInt(10000),
		LiabilitiesTotal: decimal.Zero,
		Breakdown:        Breakdown{Investments: decimal.NewFromInt(10000)},
	}
	points := Project(start, Assumptions{
		MonthlySurplus:   decimal.NewFromInt(500),
		AnnualReturnRate: dec("0.05"),
		Months:           1,
	})
	if len(points) != 1 {
		t.Fatalf("points = %d, want 1", len(points))
	}
	mustEqual(t, "month 1", points[0].Assets, dec("10541.67"))
}
