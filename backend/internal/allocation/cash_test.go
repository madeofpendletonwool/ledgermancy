package allocation

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// Cash drag, and the three silences that make it trustworthy.
//
// The detector is only worth having if a household can believe it, and the way
// to lose that is to report a drag against something invented. So: the benchmark
// is the household's OWN best rate; an account with no yield entered is skipped
// rather than read as 0%; a household with no yield entered anywhere gets no
// report at all. Each of those is a test below, and each of them is the case
// where the tempting shortcut produces a confident wrong number.

// dragBaseline is testBaseline with a checking account added, so there is a
// benchmark (the 4.50% savings) and something to measure against it.
func dragBaseline(checkingAPY decimal.NullDecimal) Baseline {
	b := testBaseline()
	b.Buckets = append(b.Buckets, Bucket{
		AccountID: chkID, Name: "Everyday checking", Kind: BucketCash,
		Balance: dec("10000"), Subtype: "checking", DepositAPY: checkingAPY,
	})
	return b
}

func itemFor(t *testing.T, r IdleCashReport, name string) (IdleCashItem, bool) {
	t.Helper()
	for _, i := range r.Items {
		if i.Name == name {
			return i, true
		}
	}
	return IdleCashItem{}, false
}

// THE DOC'S FIXTURE. A checking account at 0.4% holding $10,000, in a household
// whose own best rate is 4.50%: the gross gap is $410/yr, and the reported drag
// is that LESS one month of operating float.
//
// The exclusion is the part that makes the feature usable. Without it the
// detector tells every household to empty its current account, which is advice
// nobody can follow — and once a report contains one instruction a user knows is
// silly, they stop reading the rest of it.
func TestCashDragExcludesOneMonthOfOperatingFloat(t *testing.T) {
	got := BuildIdleCash(dragBaseline(nd("0.40")))

	if !got.HasBenchmark {
		t.Fatal("want a benchmark: the household has a 4.50% savings account")
	}
	if want := dec("4.50"); !got.Benchmark.Equal(want) {
		t.Errorf("benchmark = %s, want %s — the household's own best rate", got.Benchmark, want)
	}
	if got.BenchmarkAccount != "Savings" {
		t.Errorf("benchmark account = %q, want the account it actually came from", got.BenchmarkAccount)
	}

	item, ok := itemFor(t, got, "Everyday checking")
	if !ok {
		t.Fatalf("checking account is not in the report: %+v", got.Items)
	}

	// MonthlyFixedCosts is $3,200 in the fixture, so $6,800 of the $10,000 is
	// genuinely idle.
	if want := dec("3200"); !item.OperatingFloat.Equal(want) {
		t.Errorf("operating float = %s, want %s (one month of fixed costs)", item.OperatingFloat, want)
	}
	if want := dec("6800"); !item.IdleBalance.Equal(want) {
		t.Errorf("idle balance = %s, want %s", item.IdleBalance, want)
	}

	// $6,800 × (4.50 − 0.40)% = $278.80. The doc's $410 is the figure BEFORE the
	// exclusion, asserted here so a regression that drops the float shows up as
	// this number rather than as a plausible-looking larger one.
	if want := dec("278.80"); !item.AnnualDrag.Equal(want) {
		gross := dec("10000").Mul(dec("4.10")).Div(hundred)
		t.Errorf("annual drag = %s, want %s ($410 gross was %s, less the float)",
			item.AnnualDrag, want, gross)
	}
	if !got.TotalAnnualDrag.Equal(dec("278.80")) {
		t.Errorf("total = %s, want 278.80", got.TotalAnnualDrag)
	}

	// The figure is PRE-TAX — savings interest is ordinary income and this app
	// does not know the household's bracket. Saying so is the difference between
	// a number and a promise.
	if !strings.Contains(got.Basis, "PRE-TAX") {
		t.Errorf("basis does not state that the figure is pre-tax: %q", got.Basis)
	}
	if !strings.Contains(item.Detail, "before tax") {
		t.Errorf("item detail does not carry the pre-tax caveat: %q", item.Detail)
	}
}

// A SAVINGS account gets no float exclusion: money does not flow through it, so
// the whole balance is idle by definition. This is the $410 the doc names.
func TestCashDragOnASavingsAccountHasNoFloatExclusion(t *testing.T) {
	b := testBaseline()
	b.Buckets = append(b.Buckets, Bucket{
		AccountID: chkID, Name: "Old savings", Kind: BucketCash,
		Balance: dec("10000"), Subtype: "savings", DepositAPY: nd("0.40"),
	})

	got := BuildIdleCash(b)
	item, ok := itemFor(t, got, "Old savings")
	if !ok {
		t.Fatal("savings account missing from the report")
	}
	if !item.OperatingFloat.IsZero() {
		t.Errorf("operating float = %s, want 0 on a savings account", item.OperatingFloat)
	}
	if want := dec("410.00"); !item.AnnualDrag.Equal(want) {
		t.Errorf("annual drag = %s, want %s", item.AnnualDrag, want)
	}
}

// A NULL yield on an account means UNKNOWN. The account is skipped and NAMED, so
// the household can see the report is partial rather than concluding the account
// is fine.
func TestCashDragIsSilentOnAnAccountWithNoYield(t *testing.T) {
	got := BuildIdleCash(dragBaseline(decimal.NullDecimal{}))

	if _, ok := itemFor(t, got, "Everyday checking"); ok {
		t.Fatal("an account with no yield entered must not be reported as dragging — " +
			"unknown is not zero")
	}
	if len(got.UnknownYieldAccounts) != 1 || got.UnknownYieldAccounts[0] != "Everyday checking" {
		t.Errorf("unknown-yield accounts = %v, want the checking account named", got.UnknownYieldAccounts)
	}
	if !strings.Contains(got.Note, "not included") {
		t.Errorf("note does not say the report is partial: %q", got.Note)
	}
}

// NO YIELD ANYWHERE IN THE HOUSEHOLD: no benchmark, no items, no invented market
// rate. This is the case the whole "use the household's own best rate" decision
// was made for — bundling a market HYSA rate would mean either an outbound fetch
// the README promises against or a transcribed constant nobody can verify.
func TestCashDragIsSilentWithNoBenchmarkAnywhere(t *testing.T) {
	b := dragBaseline(decimal.NullDecimal{})
	for i := range b.Buckets {
		if b.Buckets[i].Kind == BucketCash {
			b.Buckets[i].DepositAPY = decimal.NullDecimal{}
		}
	}

	got := BuildIdleCash(b)
	if got.HasBenchmark {
		t.Fatal("a benchmark was invented from nothing")
	}
	if !got.Benchmark.IsZero() {
		t.Errorf("benchmark = %s, want zero and unused", got.Benchmark)
	}
	if len(got.Items) != 0 {
		t.Errorf("items = %v, want none without a benchmark", got.Items)
	}
	if got.Note == "" {
		t.Error("want a note explaining why there is nothing to report")
	}
}

// SILENCE BEATS NOISE. A gap worth a few dollars a year is arithmetically real
// and not worth a household's attention, and a report that lists it trains
// people to ignore the one that matters.
func TestCashDragStaysQuietBelowTheThreshold(t *testing.T) {
	b := testBaseline()
	b.Buckets = append(b.Buckets, Bucket{
		AccountID: chkID, Name: "Nearly matching", Kind: BucketCash,
		Balance: dec("1000"), Subtype: "savings", DepositAPY: nd("4.40"),
	})

	got := BuildIdleCash(b)
	// $1,000 × 0.10% = $1/yr, well under the threshold.
	if _, ok := itemFor(t, got, "Nearly matching"); ok {
		t.Error("a $1/yr gap should not be reported")
	}
	if !strings.Contains(got.Note, "Nothing worth moving") {
		t.Errorf("note = %q, want it to say there is nothing worth moving", got.Note)
	}
}

// The benchmark account itself is never reported as dragging against itself.
func TestBenchmarkAccountIsNotItsOwnDrag(t *testing.T) {
	got := BuildIdleCash(dragBaseline(nd("0.40")))
	if _, ok := itemFor(t, got, "Savings"); ok {
		t.Error("the benchmark account cannot be behind its own rate")
	}
}

// A checking account whose whole balance is operating float has nothing idle in
// it, and is reported as nothing rather than as a negative drag.
func TestCheckingBelowOneMonthOfCostsIsNotIdle(t *testing.T) {
	b := testBaseline()
	b.Buckets = append(b.Buckets, Bucket{
		AccountID: chkID, Name: "Thin checking", Kind: BucketCash,
		Balance: dec("2000"), Subtype: "checking", DepositAPY: nd("0.40"),
	})
	// Fixed costs are $3,200 — more than the balance.

	got := BuildIdleCash(b)
	if _, ok := itemFor(t, got, "Thin checking"); ok {
		t.Error("an account holding less than one month of costs has no idle money in it")
	}
	if got.TotalAnnualDrag.IsNegative() {
		t.Errorf("total drag = %s, want never negative", got.TotalAnnualDrag)
	}
}
