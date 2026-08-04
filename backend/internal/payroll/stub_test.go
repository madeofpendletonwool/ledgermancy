package payroll

import (
	"testing"

	"github.com/shopspring/decimal"
)

func money(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

func nullMoney(s string) decimal.NullDecimal {
	return decimal.NullDecimal{Decimal: money(s), Valid: true}
}

// typicalStub is one biweekly paycheque worked out by hand, and every figure in
// the tests below is derived from it on paper rather than from the code.
//
//	Gross                       3,000.00
//	  Federal income tax          -330.00
//	  State income tax            -120.00
//	  Social Security             -179.80   (6.2% of 2,900 FICA wages)
//	  Medicare                     -42.05   (1.45% of 2,900 FICA wages)
//	  401(k) pre-tax              -300.00   pre-tax, federal only
//	  Health premium              -100.00   pre-tax, federal AND FICA
//	  401(k) employer match       +150.00   EMPLOYER money, not a deduction
//	Net                         2,028.15
//
// Federal taxable wages = 3,000 − 300 − 100 = 2,600
// FICA wages            = 3,000 − 100       = 2,900   <- the 401(k) does NOT come out
func typicalStub() Stub {
	return Stub{
		Gross: money("3000.00"),
		Net:   money("2028.15"),
		Lines: []Line{
			{Category: CatFederalIncomeTax, Label: "Federal Income Tax", Amount: money("330.00")},
			{Category: CatStateIncomeTax, Label: "State Income Tax", Amount: money("120.00")},
			{Category: CatSocialSecurity, Label: "Social Security", Amount: money("179.80")},
			{Category: CatMedicare, Label: "Medicare", Amount: money("42.05")},
			{Category: Cat401kPreTax, Label: "401(k)", Amount: money("300.00"), PreTax: true},
			{Category: CatHealthPremium, Label: "Medical PPO", Amount: money("100.00"), PreTax: true},
			{Category: Cat401kMatch, Label: "ER Match", Amount: money("150.00"), IsEmployer: true},
		},
	}
}

// TestBalanceExcludesEmployerLines is the one that would have caught the
// schema as doc 23 wrote it. Without is_employer, the match is summed as a
// deduction and this stub reads as 150.00 short — which would fail confirmation
// for the most common paystub in America.
func TestBalanceExcludesEmployerLines(t *testing.T) {
	s := typicalStub()

	if got := s.Residual(); !got.IsZero() {
		t.Errorf("residual = %s, want 0 (the employer match must not be counted as a deduction)", got)
	}
	if !s.Balances() {
		t.Error("stub should balance")
	}
}

func TestBalanceToleranceIsOneCent(t *testing.T) {
	s := typicalStub()

	// A cent out: payroll systems round each deduction independently, and
	// refusing this would mean refusing real paystubs.
	s.Net = money("2028.16")
	if !s.Balances() {
		t.Errorf("a one-cent rounding difference should still balance, residual = %s", s.Residual())
	}

	// A dollar out is a missed line, not rounding.
	s.Net = money("2029.15")
	if s.Balances() {
		t.Error("a dollar discrepancy must not balance")
	}
}

func TestTaxTotalCountsOnlyGenuineTaxes(t *testing.T) {
	s := typicalStub()

	// 330 + 120 + 179.80 + 42.05. The 401(k) and the premium leave the paycheque
	// too, but folding either into "tax" would produce a rate people compare
	// against published brackets and find alarming.
	want := money("671.85")
	if got := s.TaxTotal(); !got.Equal(want) {
		t.Errorf("tax total = %s, want %s", got, want)
	}

	rate, ok := s.EffectiveTaxRate()
	if !ok {
		t.Fatal("effective tax rate should be available")
	}
	// 671.85 / 3000 = 0.22395
	if want := money("0.22395"); !rate.Round(5).Equal(want) {
		t.Errorf("effective tax rate = %s, want %s", rate.Round(5), want)
	}
}

func TestEffectiveTaxRateUnavailableOnZeroGross(t *testing.T) {
	// A correction run, or unpaid leave with benefits still deducted. Dividing
	// by it would produce an infinity that renders as something ridiculous.
	s := Stub{Gross: decimal.Zero, Net: decimal.Zero}
	if _, ok := s.EffectiveTaxRate(); ok {
		t.Error("a zero-gross stub must report no effective tax rate")
	}
}

// TestWageBasesDiffer is the W-2 correctness rule: a 401(k) deferral is exempt
// from income tax and NOT from FICA, which is why box 1 and boxes 3/5 differ on
// every real W-2 of anybody contributing.
func TestWageBasesDiffer(t *testing.T) {
	s := typicalStub()

	if want := money("2600.00"); !s.FederalTaxableWages().Equal(want) {
		t.Errorf("federal taxable wages = %s, want %s", s.FederalTaxableWages(), want)
	}
	if want := money("2900.00"); !s.FICAWages().Equal(want) {
		t.Errorf("FICA wages = %s, want %s (a 401(k) deferral does not reduce FICA)", s.FICAWages(), want)
	}
}

// TestRothDeferralReducesNothing pins the override in grossLessPreTax: a Roth
// line flagged pre-tax by a bad import still reduces no wage base, because
// ReducesNothing is a fact about Roth money rather than a default.
func TestRothDeferralReducesNothing(t *testing.T) {
	s := Stub{
		Gross: money("1000.00"),
		Net:   money("900.00"),
		Lines: []Line{
			{Category: Cat401kRoth, Amount: money("100.00"), PreTax: true},
		},
	}
	if want := money("1000.00"); !s.FederalTaxableWages().Equal(want) {
		t.Errorf("federal taxable wages = %s, want %s", s.FederalTaxableWages(), want)
	}
	if want := money("1000.00"); !s.FICAWages().Equal(want) {
		t.Errorf("FICA wages = %s, want %s", s.FICAWages(), want)
	}
}

func TestWageBaseClampsAtZero(t *testing.T) {
	// A near-zero-gross correction run with benefits still taken. A negative
	// box 1 is meaningless and must not propagate into a summary.
	s := Stub{
		Gross: money("50.00"),
		Net:   decimal.Zero,
		Lines: []Line{{Category: CatHealthPremium, Amount: money("100.00"), PreTax: true}},
	}
	if got := s.FederalTaxableWages(); !got.IsZero() {
		t.Errorf("federal taxable wages = %s, want 0", got)
	}
}

func TestTotalCompensationAddsEmployerMoney(t *testing.T) {
	s := typicalStub()

	if want := money("150.00"); !s.EmployerTotal().Equal(want) {
		t.Errorf("employer total = %s, want %s", s.EmployerTotal(), want)
	}
	if want := money("3150.00"); !s.TotalCompensation().Equal(want) {
		t.Errorf("total compensation = %s, want %s", s.TotalCompensation(), want)
	}
}

// TestBreakdownReconciles is the chart's invariant: the bands plus net must sum
// back to gross, or the drawing needs a fudge segment to close.
func TestBreakdownReconciles(t *testing.T) {
	s := typicalStub()

	bands, net := s.Breakdown()
	total := net
	for _, b := range bands {
		total = total.Add(b.Amount)
	}
	if !total.Equal(s.Gross) {
		t.Errorf("bands + net = %s, want gross %s", total, s.Gross)
	}

	// Groups with nothing in them are dropped rather than drawn as zero-width.
	for _, b := range bands {
		if b.Amount.IsZero() {
			t.Errorf("band %q has zero amount and should have been dropped", b.Group)
		}
	}
}

func TestGroupTotalExcludesEmployerLines(t *testing.T) {
	s := typicalStub()

	// Retirement holds the 300.00 deferral only; the 150.00 match is the
	// employer's and belongs in total comp, not in the paycheque breakdown.
	if want := money("300.00"); !s.GroupTotal("retirement").Equal(want) {
		t.Errorf("retirement group total = %s, want %s", s.GroupTotal("retirement"), want)
	}
}

func TestTaxonomyCoversEveryCategory(t *testing.T) {
	// Categories() feeds the manual-entry form and every roll-up loop. A
	// category present in specs but missing from the ordered list would simply
	// never be summed.
	seen := map[Category]bool{}
	for _, c := range Categories() {
		seen[c] = true
		if !Valid(string(c)) {
			t.Errorf("category %q is in Categories() but not valid", c)
		}
	}
	for c := range specs {
		if !seen[c] {
			t.Errorf("category %q has a spec but is missing from Categories()", c)
		}
	}
}

func TestEmployerMatchIsNeverLimited(t *testing.T) {
	// An employer match falls under the section 415(c) total-additions cap, not
	// the employee's elective deferral limit. Giving it the "401k" limit group
	// would eat the user's headroom with somebody else's money.
	if group := SpecFor(Cat401kMatch).LimitGroup; group != "" {
		t.Errorf("employer match limit group = %q, want empty", group)
	}
	if !SpecFor(Cat401kMatch).EmployerOnly {
		t.Error("employer match must be employer-only")
	}
}

func TestPeriodsPerYearReportsUnknown(t *testing.T) {
	if _, ok := PeriodsPerYear(PayFrequency("fortnightly")); ok {
		t.Error("an unrecognised frequency must report ok=false rather than a guess")
	}
	if n, ok := PeriodsPerYear(Biweekly); !ok || n != 26 {
		t.Errorf("biweekly = %d, %v; want 26, true", n, ok)
	}
}
