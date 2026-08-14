package networth

import (
	"hash/fnv"
	"math/rand/v2"

	"github.com/shopspring/decimal"
)

// Sequence-of-returns simulation for the withdrawal phase.
//
// Two rules govern this file, and both exist because a survival percentage is
// the single most quotable number a retirement tool produces:
//
//  1. The seed is DERIVED FROM THE INPUTS and reported. The same inputs give
//     the same answer on every refresh. A figure that moves when you reload it
//     is not something anyone should plan around, and a user comparing two
//     scenarios must be seeing a difference in the scenario, not in the dice.
//
//  2. The basis is NAMED, and named accurately. "95% success" with no stated
//     basis is theatre.
//
// On that second point, read the Basis string below carefully before quoting
// this anywhere. Ledgermancy bundles NO historical return series. Shipping one
// would mean either an outbound data fetch (which the app promises not to make)
// or a table of numbers transcribed into source — and a subtly wrong historical
// series is exactly the kind of confident falsehood this app exists to avoid.
//
// So the sequences here are drawn from a normal distribution around the user's
// OWN stated real return, with a volatility they also state. That is a real and
// useful thing to model — it is what exposes sequence-of-returns risk, the
// reason two portfolios with identical average returns can end very differently
// — but it is emphatically not a historical backtest, and the UI says so.

// MonteCarloInput describes one withdrawal-phase simulation.
type MonteCarloInput struct {
	// StartBalance is the nest egg at retirement, taken from the projection.
	StartBalance decimal.Decimal
	// AnnualWithdrawal is the amount drawn each year, in today's dollars.
	// Returns are real, so this figure does not need grossing up for inflation.
	AnnualWithdrawal decimal.Decimal
	// MeanRealReturn is the user's own assumed real return.
	MeanRealReturn decimal.Decimal
	// Volatility is the standard deviation of the annual real return. There is
	// no honest default for this — it is an input, not a constant — so the
	// caller passes what the user set.
	Volatility decimal.Decimal
	// Years the money must last.
	Years int
	// Runs is the number of sequences drawn.
	Runs int
}

// MonteCarloResult is the outcome, always carrying its own basis.
type MonteCarloResult struct {
	Runs     int `json:"runs"`
	Survived int `json:"survived"`
	Years    int `json:"years"`
	// SurvivalRate is a fraction: "0.87" is 87 sequences in 100 lasting the
	// full term. Named a rate over the MODELLED SEQUENCES, never a probability
	// of a real outcome.
	SurvivalRate decimal.Decimal `json:"survival_rate"`
	// MedianEnding is the middle final balance across the runs. Zero for a
	// sequence that ran out, which is what makes the median meaningful: it says
	// what a middling outcome looked like, not what the survivors averaged.
	MedianEnding decimal.Decimal `json:"median_ending_balance"`
	// Seed is reported so a result can be reproduced exactly.
	Seed uint64 `json:"seed"`
	// Basis states, in words the UI renders verbatim, what was simulated.
	Basis string `json:"basis"`
}

// monteCarloBasis is the sentence that must travel with every survival figure.
const monteCarloBasis = "Sequences drawn from a normal distribution around the real return and volatility you set — not a historical backtest. Ledgermancy bundles no market history, so this shows how much the ORDER of returns matters at your own assumptions, nothing more."

// SimulateWithdrawals runs the withdrawal phase over `Runs` sampled sequences
// and reports how many lasted the full term.
//
// The random draw is float64 — that is what a normal deviate is — but every
// balance is carried in exact decimal, so the money arithmetic is the same
// arithmetic as the rest of the app. Only the return sampled each year comes
// from the generator.
func SimulateWithdrawals(in MonteCarloInput) MonteCarloResult {
	result := MonteCarloResult{
		Runs: in.Runs, Years: in.Years, Basis: monteCarloBasis,
	}
	if in.Runs <= 0 || in.Years <= 0 {
		return result
	}

	seed := monteCarloSeed(in)
	result.Seed = seed
	// ChaCha8 rather than the global source: it is explicitly seeded, is not
	// shared with anything else in the process, and produces the same stream on
	// every platform and every run. A global-source result would depend on what
	// else in the process happened to draw first.
	rng := rand.New(rand.NewChaCha8(seedBytes(seed)))

	mean, _ := in.MeanRealReturn.Float64()
	vol, _ := in.Volatility.Float64()
	if vol < 0 {
		vol = 0
	}

	endings := make([]decimal.Decimal, 0, in.Runs)
	for r := 0; r < in.Runs; r++ {
		balance := in.StartBalance
		survived := true

		for y := 0; y < in.Years; y++ {
			// The withdrawal comes out FIRST, at the start of the year. Taking
			// it after a year of growth would credit the portfolio with returns
			// on money that had already been spent — and it is precisely the
			// early-year withdrawals during a downturn that sink a retirement,
			// so getting this order wrong hides the risk being measured.
			balance = balance.Sub(in.AnnualWithdrawal)
			if balance.IsNegative() {
				survived = false
				balance = decimal.Zero
				break
			}

			ret := decimal.NewFromFloat(mean + vol*rng.NormFloat64())
			balance = balance.Add(balance.Mul(ret))
			// A portfolio cannot go below zero; a bad enough year empties it.
			if balance.IsNegative() {
				survived = false
				balance = decimal.Zero
				break
			}
		}

		if survived {
			result.Survived++
		}
		endings = append(endings, balance.Round(2))
	}

	result.SurvivalRate = decimal.NewFromInt(int64(result.Survived)).
		Div(decimal.NewFromInt(int64(in.Runs))).Round(4)
	result.MedianEnding = median(endings)
	return result
}

// monteCarloSeed hashes the inputs so the seed is a function of the scenario.
// Change any assumption and the sequences change; change nothing and the answer
// is identical, forever.
func monteCarloSeed(in MonteCarloInput) uint64 {
	h := fnv.New64a()
	for _, s := range []string{
		in.StartBalance.String(),
		in.AnnualWithdrawal.String(),
		in.MeanRealReturn.String(),
		in.Volatility.String(),
	} {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	_, _ = h.Write([]byte{byte(in.Years & 0xff), byte((in.Years >> 8) & 0xff)})
	_, _ = h.Write([]byte{byte(in.Runs & 0xff), byte((in.Runs >> 8) & 0xff)})
	return h.Sum64()
}

// SeedBytes is seedBytes for callers outside this package. Doc 33's
// accumulation-phase simulation draws from the same ChaCha8 generator seeded the
// same way, so that "same inputs, same answer, forever" means one thing across
// both phases rather than two things that happen to rhyme.
func SeedBytes(seed uint64) [32]byte { return seedBytes(seed) }

// seedBytes expands a 64-bit seed into the 32 bytes ChaCha8 wants, by repeating
// it. The generator's own mixing does the rest; this only has to be a
// deterministic function of the seed.
func seedBytes(seed uint64) [32]byte {
	var b [32]byte
	for i := 0; i < 32; i++ {
		b[i] = byte(seed >> (8 * (i % 8)))
	}
	return b
}

// median of a set of ending balances. Sorted with a plain insertion into a copy
// rather than sort.Slice on the caller's data.
func median(values []decimal.Decimal) decimal.Decimal {
	if len(values) == 0 {
		return decimal.Zero
	}
	sorted := make([]decimal.Decimal, len(values))
	copy(sorted, values)
	// decimal has no sort helper, and the comparison must be decimal's own.
	for i := 1; i < len(sorted); i++ {
		v := sorted[i]
		j := i - 1
		for j >= 0 && sorted[j].GreaterThan(v) {
			sorted[j+1] = sorted[j]
			j--
		}
		sorted[j+1] = v
	}
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return sorted[mid-1].Add(sorted[mid]).Div(decimal.NewFromInt(2)).Round(2)
}

// DefaultMonteCarloRuns is a compromise: enough sequences that the survival
// rate is stable to about a percentage point, few enough that the endpoint
// stays well inside a normal request.
const DefaultMonteCarloRuns = 1000

// DefaultRetirementYears is the conventional planning horizon the survival rate
// is quoted over. Stated in the response so it is never implicit.
const DefaultRetirementYears = 30

// ClampRuns keeps a caller from turning the endpoint into a CPU sink.
func ClampRuns(n int) int {
	switch {
	case n <= 0:
		return DefaultMonteCarloRuns
	case n < 100:
		return 100
	case n > 10000:
		return 10000
	default:
		return n
	}
}
