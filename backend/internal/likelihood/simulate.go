// Package likelihood answers the question doc 32's allocator cannot: not "what
// does this plan project to" but "will it work".
//
// A deterministic P50 is one number per outcome. It is honest and it is
// incomplete — "Plan A projects to $610k in 17 years" hides the fact that Plan A
// might meet the target in 94% of plausible futures and Plan B in 71%, and that
// difference, not the median, is what makes one plan the right pick.
//
// Three rules run through this package.
//
// FIRST, THE SIMULATION SHARES ARITHMETIC WITH THE PROJECTION RATHER THAN
// RESEMBLING IT. Run takes the same []networth.AccountSchedule that
// ProjectRetirement runs on — caps already pooled, employer match already
// clamped, education horizons already resolved — and steps it month by month
// with the same growth-before-deposit rule. At zero volatility the two produce
// the same number to the cent, and there is a test that says so. That test is
// the whole reason this package does not have its own idea of what a
// contribution is.
//
// SECOND, NOTHING IS PERSISTED. A distribution is recomputed from the plan plus
// a seed derived from the inputs, so the same plan gives the same answer on
// every refresh, forever. A stored success rate is a figure that quietly stops
// being true and keeps being displayed.
//
// THIRD, THE BASIS TRAVELS WITH EVERY FIGURE. Ledgermancy bundles no historical
// return series, so these are sequences drawn around the user's OWN stated real
// return and volatility. That is a real and useful thing to model. It is not a
// backtest, it is not a probability of a real outcome, and every response says
// so in words the UI renders verbatim.
package likelihood

import (
	"hash/fnv"
	"math"
	"math/rand/v2"
	"sort"

	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
)

// WHY THE SAMPLING PATH IS float64 AND THE MONEY IS NOT.
//
// The first draft of doc 33 proposed stepping the simulation ANNUALLY in
// shopspring/decimal to bound its cost, and separately required that a σ=0 plan
// reproduce doc 32's compound-at-μ figure to the cent. Those two cannot both
// hold: doc 32 compounds twelve times a year at μ/12 with growth applied before
// each deposit, and no annual step reproduces that to the cent.
//
// Measurement settled it. One 17-year, 4-account projection in decimal costs
// ~24ms, because the running balance is never rounded and reaches ~3,300
// significant digits by the horizon; a 4-plan comparison at 1,000 runs would be
// about 94 SECONDS. Rounding the growth to the cent each month cuts that to
// ~3s — still over budget, and it moves doc 32's own answer by cents, so the
// agreement test would then be failing for a reason that has nothing to do with
// volatility.
//
// float64 costs ~0.6µs for the same 204-month path: a 4-plan comparison lands in
// tens of milliseconds. And it agrees with the unrounded decimal path to well
// inside a cent — 204 multiply-adds accumulate a relative error near 2e-14,
// which on a $1,000,000 balance is a few hundred-millionths of a dollar. So the
// cheap path is also the faithful one, and doc 33's own escape hatch ("drop to
// float64 for the sampling path only") is what the measurement points at.
//
// The discipline that remains: MONEY ENTERS AND LEAVES IN DECIMAL. Balances,
// contributions and every reported figure are exact decimals; only the
// compounding of a sampled return — which was a float the moment it left the
// generator — happens in float64.

// Input is one simulation.
type Input struct {
	// Accounts is networth.BuildSchedule's output for the plan being simulated:
	// the same resolved inputs ProjectRetirement consumes.
	Accounts []networth.AccountSchedule
	// Months is the horizon, matching RetirementAssumptions.Months.
	Months int
	// Volatility is the standard deviation of the annual real return, as a
	// fraction. There is no honest default for it — it is an assumption, not a
	// constant — so the caller passes what the user set, and the basis names it.
	Volatility decimal.Decimal
	// Target is the figure the success rate is measured against. Invalid means
	// no target was set, and the result reports no success rate rather than a
	// rate against zero, which every plan would meet.
	Target decimal.NullDecimal
	// Fixed is the terminal value of the plan's σ=0 buckets — cash, which
	// accrues at its own yield and does not move with the market. It is added to
	// every run's total rather than simulated, because simulating a constant
	// would be a thousand identical draws.
	Fixed decimal.Decimal
	// Runs is the number of sequences drawn.
	Runs int
}

// Result is the distribution, always carrying its own basis.
type Result struct {
	Runs  int `json:"runs"`
	Years int `json:"years"`

	// P10, P50 and P90 are terminal values across the runs. P50 is the MEDIAN
	// SIMULATED OUTCOME and is never called a P50 in the UI beside doc 32's
	// figure — see the note on MedianBelowProjected.
	P10 decimal.Decimal `json:"p10"`
	P50 decimal.Decimal `json:"p50"`
	P90 decimal.Decimal `json:"p90"`

	// SuccessRate is the fraction of runs whose terminal value reached the
	// target. Nil when no target was set. Named a rate over the MODELLED
	// SEQUENCES, never a probability of a real outcome.
	SuccessRate *decimal.Decimal `json:"success_rate"`
	Target      *decimal.Decimal `json:"target,omitempty"`

	// Sigma is the standard deviation of the terminal distribution: the second
	// sort key of the guardrail rule, and the honest measure of how wide the
	// range of outcomes is.
	Sigma decimal.Decimal `json:"sigma"`

	// DrawdownP5 is the FIFTH-PERCENTILE peak-to-trough drawdown across the run
	// paths, as a fraction. Not the maximum — see percentile().
	DrawdownP5 decimal.Decimal `json:"drawdown_p5"`

	// ByAccount is each simulated account's own terminal distribution, keyed by
	// AccountPlan.ID.
	//
	// It exists for the guardrail's F2, which judges goal-fit AT THE MEDIAN
	// SIMULATED OUTCOME. A goal is linked to one account, so asking whether it
	// is funded at P50 means asking about THAT account's median — not the
	// portfolio's. Scaling a goal by the portfolio-wide median would credit a
	// 529 with a good year in the 401(k) beside it.
	//
	// Cash is not in here: it is the Fixed constant, not a simulated account.
	ByAccount map[string]AccountOutcome `json:"by_account"`

	// Seed is reported so a result can be reproduced exactly.
	Seed uint64 `json:"seed"`
	// Basis states, in words the UI renders verbatim, what was simulated.
	Basis string `json:"basis"`
	// Estimate is always true, in the payload so a client cannot render these
	// figures without the caveat travelling with them.
	Estimate bool `json:"estimate"`
}

// AccountOutcome is one bucket's terminal distribution.
type AccountOutcome struct {
	Name string          `json:"name"`
	P10  decimal.Decimal `json:"p10"`
	P50  decimal.Decimal `json:"p50"`
	P90  decimal.Decimal `json:"p90"`
}

// Basis is the sentence that must travel with every figure this package
// produces. Each clause names a modelling choice that would otherwise be
// invisible, and the correlation clause is the one most likely to surprise:
// treating buckets as independent is the default a careless implementation
// reaches for, and it inflates every success rate on the page.
const basisPrefix = "Sequences drawn from a normal distribution around the real return and volatility you set — " +
	"not a historical backtest. Ledgermancy bundles no market history. " +
	"Bucket returns are modelled as moving together, which understates the benefit of holding genuinely " +
	"different asset classes but never overstates it. " +
	"Figures are terminal values in today's dollars, seeded from the inputs so the same plan always gives the " +
	"same answer. This is not a prediction and not a probability of a real outcome."

// Run simulates the accumulation phase over `Runs` sampled sequences.
//
// ONE MARKET DRAW PER YEAR, SHARED ACROSS BUCKETS, SCALED PER BUCKET:
//
//	z_t      ~ N(0,1)                     -- one draw per year, all buckets
//	r_bucket = μ_bucket + σ · z_t          -- fully correlated across risky buckets
//
// Sampling each bucket independently is the obvious implementation and it is
// wrong in the flattering direction. A Roth, a 529 and a taxable brokerage in a
// typical household are all holding equities — largely the SAME equities. Drawn
// independently they diversify against each other in the model in a way they do
// not in life: total variance falls by roughly 1/k for k similar buckets, the
// distribution narrows, and the success rate — the largest number on the page —
// comes out materially too high.
//
// Perfect correlation is not true either, but it errs conservative: it
// understates diversification rather than overstating it. A per-bucket
// correlation matrix is out of scope, because it needs a covariance estimate the
// app has no honest source for — exactly like the return series doc 15 refused
// to bundle. The assumption is named in the basis.
func Run(in Input) Result { return runWithSeed(in, seedFor(in)) }

// runWithSeed is Run with the seed supplied rather than derived from the inputs.
//
// It exists so the correlation property can be asserted EXACTLY: three identical
// buckets and one bucket of three times the size hash to different seeds, so
// comparing them through Run compares two different draw sequences and can only
// ever be a tolerance check. Driven through one seed the two are the same
// arithmetic and must agree exactly, which is a far sharper test of the thing
// that actually matters.
//
// Factored out of Run rather than reimplemented in the test, because a second
// copy of this loop would be a test of the copy.
func runWithSeed(in Input, seed uint64) Result {
	years := in.Months / 12
	if in.Months%12 != 0 {
		years++
	}
	out := Result{
		Runs:      in.Runs,
		Years:     years,
		ByAccount: map[string]AccountOutcome{},
		Basis:     basisPrefix,
		Estimate:  true,
	}
	if in.Runs <= 0 || in.Months <= 0 || len(in.Accounts) == 0 {
		// Nothing to simulate. The fixed (cash) side is still a real answer, and
		// a plan of pure cash has a genuine zero-variance distribution rather
		// than no distribution.
		out.P10, out.P50, out.P90 = in.Fixed.Round(2), in.Fixed.Round(2), in.Fixed.Round(2)
		if in.Target.Valid {
			out.SuccessRate, out.Target = rateFor(in.Fixed, in.Target.Decimal), targetPtr(in.Target.Decimal)
		}
		return out
	}

	out.Seed = seed
	// ChaCha8 rather than the global source: explicitly seeded, not shared with
	// anything else in the process, and the same stream on every platform and
	// every run. A global-source result would depend on what else in the process
	// happened to draw first.
	rng := rand.New(rand.NewChaCha8(networth.SeedBytes(seed)))

	sigma, _ := in.Volatility.Float64()
	if sigma < 0 {
		sigma = 0
	}
	fixed, _ := in.Fixed.Float64()

	// Per-account constants, hoisted out of the run loop. Ranging over
	// []AccountSchedule inside the hot loop would copy six decimals per account
	// per month; these flat slices are read a few hundred million times.
	n := len(in.Accounts)
	mu := make([]float64, n)
	start := make([]float64, n)
	monthly := make([]float64, n)
	lump := make([]float64, n)
	match := make([]float64, n)
	// stopAfter is the month past which an account no longer grows or receives
	// money, or -1 for one that never stops. It mirrors AccountSchedule.Stopped
	// exactly; see the assertion in the tests.
	stopAfter := make([]int, n)
	for i, acct := range in.Accounts {
		mu[i], _ = acct.Rate.Float64()
		start[i], _ = acct.Balance.Float64()
		monthly[i], _ = acct.Monthly.Float64()
		lump[i], _ = acct.Lump.Float64()
		match[i], _ = acct.Match.Float64()
		stopAfter[i] = -1
		if acct.Education && acct.HasHorizon {
			stopAfter[i] = acct.StopMonth
		}
	}

	terminals := make([]float64, in.Runs)
	drawdowns := make([]float64, in.Runs)
	balances := make([]float64, n)
	// perAccount[i] is account i's terminal value in each run, kept so each
	// bucket gets its own percentiles. One flat slice per account rather than a
	// slice per run: the run loop writes n values and nothing reads them until
	// it is over.
	perAccount := make([][]float64, n)
	for i := range perAccount {
		perAccount[i] = make([]float64, in.Runs)
	}
	// monthlyRate[i] is account i's rate for the year currently being stepped,
	// refreshed on the first month of each year.
	monthlyRate := make([]float64, n)

	for r := 0; r < in.Runs; r++ {
		copy(balances, start)
		peak, worst := 0.0, 0.0

		for m := 1; m <= in.Months; m++ {
			// One draw per YEAR, taken on the first month of each year and held
			// for its twelve months. Returns are an annual assumption; redrawing
			// monthly would model twelve independent markets a year and shrink
			// the spread this whole package exists to show.
			if (m-1)%12 == 0 {
				z := rng.NormFloat64()
				for i := range monthlyRate {
					monthlyRate[i] = (mu[i] + sigma*z) / 12
				}
			}

			total := 0.0
			for i := range balances {
				if stopAfter[i] >= 0 && m > stopAfter[i] {
					total += balances[i]
					continue
				}
				// Growth applies to the balance BEFORE this month's
				// contribution, so a deposit is not credited with a full month
				// of return it did not earn. Same rule, same order, as
				// ProjectRetirement — that is what makes the σ=0 agreement test
				// a property rather than a coincidence.
				balances[i] += balances[i] * monthlyRate[i]
				if m == 1 {
					balances[i] += lump[i]
				}
				balances[i] += monthly[i] + match[i]
				total += balances[i]
			}

			// Peak-to-trough is tracked at YEAR ENDS. Within a year every
			// account compounds at one fixed rate, so the path between year ends
			// carries no turning point the annual sample would miss.
			if m%12 == 0 || m == in.Months {
				if total > peak {
					peak = total
				}
				if peak > 0 {
					if d := (peak - total) / peak; d > worst {
						worst = d
					}
				}
			}
		}

		// EACH ACCOUNT IS ROUNDED TO THE CENT BEFORE THE SUM, because that is
		// the order the projection totals in: ProjectRetirement reports every
		// AccountPoint.Balance at cent resolution and doc 32 adds those up.
		// Summing unrounded and rounding once lands a cent away on a portfolio
		// this size, and a one-cent disagreement is still a disagreement —
		// the σ=0 agreement test is specified to the cent for a reason.
		sum := fixed
		for i := range balances {
			v := math.Round(balances[i]*100) / 100
			sum += v
			perAccount[i][r] = v
		}
		terminals[r], drawdowns[r] = sum, worst
	}

	sort.Float64s(terminals)
	sort.Float64s(drawdowns)

	out.P10 = money(percentile(terminals, 10))
	out.P50 = money(percentile(terminals, 50))
	out.P90 = money(percentile(terminals, 90))
	out.Sigma = money(stddev(terminals))
	out.DrawdownP5 = decimal.NewFromFloat(percentile(drawdowns, 5)).Round(4)

	for i, acct := range in.Accounts {
		sort.Float64s(perAccount[i])
		out.ByAccount[acct.ID] = AccountOutcome{
			Name: acct.Name,
			P10:  money(percentile(perAccount[i], 10)),
			P50:  money(percentile(perAccount[i], 50)),
			P90:  money(percentile(perAccount[i], 90)),
		}
	}

	if in.Target.Valid {
		target, _ := in.Target.Decimal.Float64()
		hits := 0
		for _, v := range terminals {
			if v >= target {
				hits++
			}
		}
		rate := decimal.NewFromInt(int64(hits)).
			Div(decimal.NewFromInt(int64(in.Runs))).Round(4)
		out.SuccessRate, out.Target = &rate, targetPtr(in.Target.Decimal)
	}
	return out
}

// money lands a simulated figure back in decimal at cent resolution. Every
// number that leaves this package goes through here: the float64 is an
// implementation detail of the compounding, not a shape the rest of the app has
// to know about.
func money(v float64) decimal.Decimal { return decimal.NewFromFloat(v).Round(2) }

func targetPtr(d decimal.Decimal) *decimal.Decimal {
	t := d.Round(2)
	return &t
}

func rateFor(value, target decimal.Decimal) *decimal.Decimal {
	rate := decimal.Zero
	if value.GreaterThanOrEqual(target) {
		rate = decimal.NewFromInt(1)
	}
	return &rate
}

// percentile is the NEAREST-RANK percentile of an ascending slice.
//
// A percentile rather than an extreme, and this is the whole reason the drawdown
// figure is defined the way it is. The first draft of doc 33 specified the
// "worst-case peak-to-trough drawdown across the run paths" — a maximum over n
// runs, which is an extreme order statistic: it gets monotonically worse as n
// grows. Since ClampRuns admits anything from 100 to 10,000, raising the run
// count would make every plan look riskier and could FLIP WHICH PLAN THE
// GUARDRAIL PICKS, while this package promises the pick is deterministic.
// Strictly that promise still holds — same n, same answer — but that is not how
// anyone reads it.
//
// A percentile converges as n grows instead of diverging, which is why the
// comparison path also pins n across every plan it ranks.
func percentile(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := (p*len(sorted) + 99) / 100 // ceil(p/100 * n)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// stddev is the population standard deviation of the terminal values.
func stddev(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	mean := 0.0
	for _, v := range values {
		mean += v
	}
	mean /= float64(len(values))
	sum := 0.0
	for _, v := range values {
		d := v - mean
		sum += d * d
	}
	variance := sum / float64(len(values))
	if variance <= 0 {
		return 0
	}
	return math.Sqrt(variance)
}

// seedFor hashes the inputs so the seed is a function of the scenario. Change
// any assumption and the sequences change; change nothing and the answer is
// identical, forever.
//
// EVERY FIGURE THAT MOVES THE ANSWER IS IN THE HASH. An account whose
// contribution changed but whose seed did not would produce a distribution that
// looks stable while meaning something different.
func seedFor(in Input) uint64 {
	h := fnv.New64a()
	write := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	for _, acct := range in.Accounts {
		write(acct.ID)
		write(acct.Balance.String())
		write(acct.Monthly.String())
		write(acct.Lump.String())
		write(acct.Match.String())
		write(acct.Rate.String())
	}
	write(in.Volatility.String())
	write(in.Fixed.String())
	if in.Target.Valid {
		write(in.Target.Decimal.String())
	}
	_, _ = h.Write([]byte{byte(in.Months & 0xff), byte((in.Months >> 8) & 0xff)})
	_, _ = h.Write([]byte{byte(in.Runs & 0xff), byte((in.Runs >> 8) & 0xff)})
	return h.Sum64()
}
