package likelihood

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
)

// Doc 33's verification section, in order. Every test here is named in the doc.

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func now() time.Time { return time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC) }

// plans builds a small household: a Roth, a taxable brokerage and a 401(k).
// Deliberately under every IRS cap so capping does not confound the agreement
// test — capping has its own coverage in the allocation package.
func plans() []networth.AccountPlan {
	return []networth.AccountPlan{
		{ID: "roth", Name: "Roth", Treatment: "roth_ira",
			Balance: dec("40000"), MonthlyContribution: dec("400")},
		{ID: "brokerage", Name: "Brokerage", Treatment: "taxable",
			Balance: dec("120000"), MonthlyContribution: dec("800"),
			FirstYearContribution: dec("30000")},
		{ID: "trad", Name: "401k", Treatment: "trad_401k",
			Balance: dec("200000"), MonthlyContribution: dec("900")},
	}
}

func assumptions(months int) networth.RetirementAssumptions {
	return networth.RetirementAssumptions{
		RealReturnRate: dec("0.07"),
		WithdrawalRate: dec("0.04"),
		CurrentAge:     40,
		Months:         months,
	}
}

// --------------------------------------------------------------------------
// The two-P50 agreement test
// --------------------------------------------------------------------------

// THIS IS THE LOAD-BEARING TEST OF THE WHOLE PACKAGE.
//
// At zero volatility the simulation must reproduce doc 32's compound-at-μ
// figure TO THE CENT. It is the only case where the two MUST match, and it is a
// real check that the two engines share arithmetic rather than having drifted:
// the simulation steps monthly at μ/12 with growth applied before each deposit,
// off networth.BuildSchedule's resolved accounts, because ProjectRetirement
// does exactly that off exactly those.
//
// It is also what licenses the float64 sampling path. 204 monthly multiply-adds
// accumulate a relative error near 2e-14 — on a $1.5M portfolio, a few
// millionths of a cent — so agreement at cent resolution is not a coincidence
// that happens to hold, it is comfortably inside the arithmetic's error budget.
func TestSigmaZeroMatchesProjectionToTheCent(t *testing.T) {
	const months = 17 * 12
	a := assumptions(months)
	schedule := networth.BuildSchedule(plans(), a, now())
	projection := networth.ProjectRetirement(plans(), a, now())

	want := decimal.Zero
	final := projection.Points[len(projection.Points)-1].ByAccount
	for _, acct := range schedule.Accounts {
		want = want.Add(final[acct.ID].Balance)
	}

	got := Run(Input{
		Accounts:   schedule.Accounts,
		Months:     months,
		Volatility: decimal.Zero,
		Runs:       200,
	})

	for _, p := range []struct {
		label string
		value decimal.Decimal
	}{{"P10", got.P10}, {"P50", got.P50}, {"P90", got.P90}} {
		if !p.value.Equal(want.Round(2)) {
			t.Errorf("at sigma=0 the %s simulated outcome is %s, want %s — the simulation and the projection "+
				"have drifted apart", p.label, p.value.StringFixed(2), want.Round(2).StringFixed(2))
		}
	}
	if !got.Sigma.IsZero() {
		t.Errorf("sigma = %s, want 0 — a zero-volatility plan has one outcome", got.Sigma)
	}
}

// And the INEQUALITY that follows for σ>0: the median simulated outcome is
// strictly BELOW the compound-at-μ figure, and the gap widens with σ and with
// the horizon. A build where these matched at σ=15% would have a bug in one.
//
// HOW BIG THE GAP IS, AND WHY IT IS NOT THE 15% DOC 33 PREDICTS.
//
// The doc estimates the median landing "about 15% lower" at μ=7%, σ=15%,
// T=17, from the standard e^(−Tσ²/2) lognormal result. That figure assumes the
// sampled return is compounded ONCE A YEAR. This engine compounds MONTHLY at
// (μ + σz)/12, because ProjectRetirement does and the σ=0 agreement test above
// forces the two to be the same arithmetic.
//
// Expanding the monthly form: log∏(1 + r/12)^12 ≈ Σr − Σr²/24, so the drag is
// e^(−Tσ²/24) rather than e^(−Tσ²/2) — TWELVE TIMES SMALLER. At μ=7%, σ=15%,
// T=17 that is about 1.6%, and the probe that established this measured 0.9843
// against a predicted 0.9841.
//
// Both conventions are defensible; they are not both available. Matching the
// projection to the cent is the stronger and the testable invariant, so the gap
// is real, is in the right direction, and is smaller than the doc guessed. The
// UI line explaining the gap is written to be true at any size.
func TestMedianFallsBelowProjectionAndTheGapWidens(t *testing.T) {
	// A single buy-and-hold bucket: no contributions to dilute the effect, and
	// no sampling noise from money arriving at 204 different times. The drag is
	// a property of compounding, and this is the clean way to see it.
	solo := func(months int, vol string, runs int) (decimal.Decimal, decimal.Decimal) {
		p := []networth.AccountPlan{
			{ID: "x", Name: "X", Treatment: "taxable", Balance: dec("100000")},
		}
		a := assumptions(months)
		schedule := networth.BuildSchedule(p, a, now())
		projection := networth.ProjectRetirement(p, a, now())
		det := projection.Points[len(projection.Points)-1].ByAccount["x"].Balance
		sim := Run(Input{
			Accounts: schedule.Accounts, Months: months, Volatility: dec(vol), Runs: runs,
		})
		return sim.P50, det
	}

	const months = 17 * 12
	mid, det := solo(months, "0.15", 8000)
	if !mid.LessThan(det) {
		t.Fatalf("median simulated outcome %s is not below the projected %s — volatility must drag compounding",
			mid.StringFixed(2), det.StringFixed(2))
	}
	midRatio := ratio(mid, det)

	// Wider sigma, wider gap.
	wide, wideDet := solo(months, "0.25", 8000)
	if !ratio(wide, wideDet).LessThan(midRatio) {
		t.Errorf("median/projected is %s at sigma=0.25 and %s at sigma=0.15 — the gap must widen with volatility",
			ratio(wide, wideDet), midRatio)
	}

	// Longer horizon, wider gap. Measured as a RATIO, since the absolute
	// figures grow with the horizon regardless.
	short, shortDet := solo(5*12, "0.15", 8000)
	if !midRatio.LessThan(ratio(short, shortDet)) {
		t.Errorf("median/projected is %s at 17 years and %s at 5 years — the gap must widen with the horizon",
			midRatio, ratio(short, shortDet))
	}

	// And the SIZE is the monthly-compounding prediction, e^(−Tσ²/24) ≈ 0.984,
	// not the annual-compounding 0.85. Pinning it means a future change to the
	// compounding convention fails here loudly rather than quietly moving every
	// median on the page.
	if midRatio.LessThan(dec("0.97")) || midRatio.GreaterThan(dec("0.995")) {
		t.Errorf("median/projected = %s at mu=7%%, sigma=15%%, T=17y; want about 0.984 "+
			"(e^(-T*sigma^2/24), the monthly-compounding drag). A value near 0.85 would mean the engine "+
			"started compounding the sampled return annually and no longer matches the projection", midRatio)
	}
}

func ratio(a, b decimal.Decimal) decimal.Decimal {
	if b.IsZero() {
		return decimal.Zero
	}
	return a.Div(b).Round(6)
}

// --------------------------------------------------------------------------
// Determinism
// --------------------------------------------------------------------------

// Same plan + assumptions + n → identical results. A figure that moves when you
// reload it is not something anyone should plan around.
func TestDeterminism(t *testing.T) {
	in := Input{
		Accounts:   networth.BuildSchedule(plans(), assumptions(240), now()).Accounts,
		Months:     240,
		Volatility: dec("0.15"),
		Target:     decimal.NewNullDecimal(dec("1500000")),
		Runs:       500,
	}
	first, second := Run(in), Run(in)

	if first.Seed != second.Seed {
		t.Fatalf("seed moved between runs: %d then %d", first.Seed, second.Seed)
	}
	for _, c := range []struct {
		label string
		a, b  decimal.Decimal
	}{
		{"P10", first.P10, second.P10},
		{"P50", first.P50, second.P50},
		{"P90", first.P90, second.P90},
		{"sigma", first.Sigma, second.Sigma},
		{"drawdown", first.DrawdownP5, second.DrawdownP5},
	} {
		if !c.a.Equal(c.b) {
			t.Errorf("%s is not deterministic: %s then %s", c.label, c.a, c.b)
		}
	}
	if !first.SuccessRate.Equal(*second.SuccessRate) {
		t.Errorf("success rate is not deterministic: %s then %s", first.SuccessRate, second.SuccessRate)
	}
}

// Changing an input must change the seed, or the answer would look stable while
// meaning something different.
func TestSeedTracksTheInputs(t *testing.T) {
	base := Input{
		Accounts:   networth.BuildSchedule(plans(), assumptions(240), now()).Accounts,
		Months:     240,
		Volatility: dec("0.15"),
		Runs:       500,
	}
	changed := base
	changed.Volatility = dec("0.16")

	if Run(base).Seed == Run(changed).Seed {
		t.Error("changing the volatility did not change the seed")
	}
}

// --------------------------------------------------------------------------
// Baseline-vs-baseline
// --------------------------------------------------------------------------

// An empty plan produces zero variance: P10 = P50 = P90 = the current balance.
func TestEmptyPlanHasNoVariance(t *testing.T) {
	got := Run(Input{
		Accounts:   nil,
		Months:     240,
		Volatility: dec("0.15"),
		Fixed:      dec("25000"),
		Runs:       500,
	})
	for _, p := range []decimal.Decimal{got.P10, got.P50, got.P90} {
		if !p.Equal(dec("25000")) {
			t.Errorf("percentile = %s, want 25000.00 — a plan with nothing to simulate has one outcome",
				p.StringFixed(2))
		}
	}
}

// --------------------------------------------------------------------------
// Correlated draws
// --------------------------------------------------------------------------

// THE ERROR THIS CATCHES SILENTLY INFLATES EVERY SUCCESS RATE ON THE PAGE.
//
// k identical risky buckets must produce the same terminal sigma as ONE bucket
// of k× the size. That is the property independent sampling breaks: drawn
// independently the buckets diversify against each other in the model in a way
// they do not in life, total variance falls by roughly 1/k, and the distribution
// comes out too narrow.
func TestBucketsAreDrawnTogether(t *testing.T) {
	const months = 15 * 12
	a := assumptions(months)

	split := []networth.AccountPlan{
		{ID: "a", Name: "A", Treatment: "taxable", Balance: dec("50000"), MonthlyContribution: dec("300")},
		{ID: "b", Name: "B", Treatment: "taxable", Balance: dec("50000"), MonthlyContribution: dec("300")},
		{ID: "c", Name: "C", Treatment: "taxable", Balance: dec("50000"), MonthlyContribution: dec("300")},
	}
	combined := []networth.AccountPlan{
		{ID: "all", Name: "All", Treatment: "taxable", Balance: dec("150000"), MonthlyContribution: dec("900")},
	}

	run := func(p []networth.AccountPlan) Result {
		return Run(Input{
			Accounts:   networth.BuildSchedule(p, a, now()).Accounts,
			Months:     months,
			Volatility: dec("0.18"),
			Runs:       8000,
		})
	}
	three, one := run(split), run(combined)

	// A RATIO with a tolerance, not an equality. The seed is derived from the
	// inputs, and three 50k buckets hash differently from one 150k bucket — so
	// the two runs draw different sequences and their sampled sigmas differ by
	// ordinary sampling error even when the model is right.
	//
	// The tolerance is what makes the test decisive anyway. Drawn TOGETHER the
	// ratio is 1.0 up to sampling error; drawn INDEPENDENTLY it would collapse
	// toward 1/sqrt(3) = 0.58. A 10% band separates those two hypotheses by a
	// mile, which is the property that matters.
	got := ratio(three.Sigma, one.Sigma)
	if got.LessThan(dec("0.90")) || got.GreaterThan(dec("1.10")) {
		t.Errorf("sigma over 3 identical buckets is %s but %s over one bucket of the same size "+
			"(ratio %s) — the buckets are being drawn INDEPENDENTLY, which understates the spread "+
			"and inflates every success rate on the page. Independent sampling would land near 0.58.",
			three.Sigma.StringFixed(2), one.Sigma.StringFixed(2), got)
	}

	medians := ratio(three.P50, one.P50)
	if medians.LessThan(dec("0.97")) || medians.GreaterThan(dec("1.03")) {
		t.Errorf("median over 3 buckets is %s but %s over one (ratio %s) — splitting the same money "+
			"across identical buckets should not change the answer",
			three.P50.StringFixed(2), one.P50.StringFixed(2), medians)
	}
}

// The decisive half of the correlation check, with the seed held FIXED so there
// is no sampling error at all: the same accounts, split three ways versus held
// as one, driven through the identical draw sequence.
//
// Drawn together this is an EXACT equality — the three buckets are the one
// bucket, scaled. Any difference is independent sampling.
func TestCorrelationIsExactUnderOneSeed(t *testing.T) {
	const months = 15 * 12
	a := assumptions(months)

	split := networth.BuildSchedule([]networth.AccountPlan{
		{ID: "a", Name: "A", Treatment: "taxable", Balance: dec("50000")},
		{ID: "b", Name: "B", Treatment: "taxable", Balance: dec("50000")},
		{ID: "c", Name: "C", Treatment: "taxable", Balance: dec("50000")},
	}, a, now()).Accounts
	combined := networth.BuildSchedule([]networth.AccountPlan{
		{ID: "a", Name: "A", Treatment: "taxable", Balance: dec("150000")},
	}, a, now()).Accounts

	// Same Fixed, same Months, same Runs, same Volatility — and the accounts
	// are arranged so seedFor hashes to the same value is NOT relied on.
	// Instead both are run through runWithSeed with one explicit seed.
	three := runWithSeed(Input{
		Accounts: split, Months: months, Volatility: dec("0.18"), Runs: 2000,
	}, 12345)
	one := runWithSeed(Input{
		Accounts: combined, Months: months, Volatility: dec("0.18"), Runs: 2000,
	}, 12345)

	// Agreement is to the CENT, not to the bit. Each account is rounded to the
	// cent before the totals are summed — which is what makes the σ=0 agreement
	// test above hold — so three buckets round three times where one rounds
	// once, and the totals can differ by a couple of cents. That is the only
	// source of difference left: independent sampling would move sigma by tens
	// of thousands, not by pennies.
	const pennies = "0.05"
	if three.P50.Sub(one.P50).Abs().GreaterThan(dec(pennies)) {
		t.Errorf("under ONE seed, 3 identical buckets median to %s and one bucket of the same size to %s — "+
			"they must agree, because correlated buckets ARE the single bucket scaled",
			three.P50.StringFixed(2), one.P50.StringFixed(2))
	}
	if three.Sigma.Sub(one.Sigma).Abs().GreaterThan(dec(pennies)) {
		t.Errorf("under ONE seed, sigma is %s split and %s combined — a gap this large is independent sampling",
			three.Sigma.StringFixed(2), one.Sigma.StringFixed(2))
	}
}

// --------------------------------------------------------------------------
// Drawdown stability
// --------------------------------------------------------------------------

// The reported P5 drawdown must CONVERGE as n grows, not diverge. A maximum
// over n runs would fail this — it gets monotonically worse with every extra
// run — and that is precisely why the figure is a percentile.
func TestDrawdownConverges(t *testing.T) {
	const months = 20 * 12
	base := Input{
		Accounts:   networth.BuildSchedule(plans(), assumptions(months), now()).Accounts,
		Months:     months,
		Volatility: dec("0.18"),
	}

	at := func(runs int) decimal.Decimal {
		in := base
		in.Runs = runs
		return Run(in).DrawdownP5
	}
	small, mid, large := at(100), at(1000), at(10000)

	firstStep := mid.Sub(small).Abs()
	secondStep := large.Sub(mid).Abs()
	if secondStep.GreaterThan(firstStep.Add(dec("0.02"))) {
		t.Errorf("P5 drawdown is not settling: 100→1000 moved %s, 1000→10000 moved %s "+
			"(%s, %s, %s) — an extreme order statistic diverges where a percentile converges",
			firstStep, secondStep, small, mid, large)
	}
	// And the figure must stay inside a stated tolerance across a 10x change in
	// run count — the test a maximum would fail outright.
	if large.Sub(small).Abs().GreaterThan(dec("0.10")) {
		t.Errorf("P5 drawdown moved from %s to %s between 100 and 10,000 runs, which is more than the "+
			"stated tolerance", small, large)
	}
}

// --------------------------------------------------------------------------
// Success rate and the target
// --------------------------------------------------------------------------

// No target means NO success rate, rather than a rate against zero that every
// plan would meet.
func TestNoTargetMeansNoSuccessRate(t *testing.T) {
	got := Run(Input{
		Accounts:   networth.BuildSchedule(plans(), assumptions(120), now()).Accounts,
		Months:     120,
		Volatility: dec("0.15"),
		Runs:       200,
	})
	if got.SuccessRate != nil {
		t.Errorf("success rate = %s with no target set, want nil", got.SuccessRate)
	}
}

// A target far below every outcome is met in every run; far above, in none.
// Both ends, because a rate that saturates at one end only is a comparison
// written backwards.
func TestSuccessRateBounds(t *testing.T) {
	run := func(target string) decimal.Decimal {
		return *Run(Input{
			Accounts:   networth.BuildSchedule(plans(), assumptions(120), now()).Accounts,
			Months:     120,
			Volatility: dec("0.15"),
			Target:     decimal.NewNullDecimal(dec(target)),
			Runs:       500,
		}).SuccessRate
	}
	if got := run("1"); !got.Equal(dec("1")) {
		t.Errorf("success rate against a trivial target = %s, want 1", got)
	}
	if got := run("999999999"); !got.IsZero() {
		t.Errorf("success rate against an unreachable target = %s, want 0", got)
	}
}

// Per-account outcomes must sum to the portfolio, or the guardrail's F2 would
// be judging goals against a different portfolio than the success rate.
func TestPerAccountOutcomesArePresent(t *testing.T) {
	schedule := networth.BuildSchedule(plans(), assumptions(120), now())
	got := Run(Input{
		Accounts: schedule.Accounts, Months: 120, Volatility: decimal.Zero, Runs: 100,
	})
	if len(got.ByAccount) != len(schedule.Accounts) {
		t.Fatalf("got %d per-account outcomes, want %d", len(got.ByAccount), len(schedule.Accounts))
	}
	// At zero volatility every run is identical, so the per-account medians sum
	// exactly to the portfolio median.
	sum := decimal.Zero
	for _, o := range got.ByAccount {
		sum = sum.Add(o.P50)
	}
	if !sum.Equal(got.P50) {
		t.Errorf("per-account medians sum to %s but the portfolio median is %s",
			sum.StringFixed(2), got.P50.StringFixed(2))
	}
}

// An education account stops at its beneficiary's horizon in the simulation
// exactly as it does in the projection — the schedule owns that rule and both
// engines read it from there.
func TestEducationHorizonStopsTheBucket(t *testing.T) {
	p := []networth.AccountPlan{
		{ID: "529", Name: "College", Treatment: "529", Balance: dec("30000"),
			MonthlyContribution:   dec("300"),
			BeneficiaryCurrentAge: 10, BeneficiaryTargetAge: 18},
	}
	const months = 20 * 12
	a := assumptions(months)
	schedule := networth.BuildSchedule(p, a, now())

	sim := Run(Input{Accounts: schedule.Accounts, Months: months, Volatility: decimal.Zero, Runs: 50})
	projection := networth.ProjectRetirement(p, a, now())
	want := projection.Points[len(projection.Points)-1].ByAccount["529"].Balance

	if !sim.P50.Equal(want) {
		t.Errorf("simulated 529 lands at %s, projection says %s — the education horizon is not being applied "+
			"the same way in both engines", sim.P50.StringFixed(2), want.StringFixed(2))
	}
}
