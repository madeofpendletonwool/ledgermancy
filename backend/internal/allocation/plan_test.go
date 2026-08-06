package allocation

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/goals"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
)

// Run is a pure function of a Baseline and a Request, which is what lets every
// test in this file be an in-memory one with no database anywhere near it. That
// is not a convenience; it is the property the "no real-data mutation" rule
// rests on. AssembleBaseline does all the I/O, Run does none, and a plan cannot
// change the household because it has nothing to change it with.

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func nd(s string) decimal.NullDecimal { return decimal.NewNullDecimal(dec(s)) }

// now2026 is inside a configured tax year, so nothing here depends on the clock.
var now2026 = time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)

var (
	rothID  = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	brokID  = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	cardID  = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	cashID  = uuid.MustParse("44444444-4444-4444-8444-444444444444")
	chkID   = uuid.MustParse("55555555-5555-4555-8555-555555555555")
	planID  = uuid.MustParse("66666666-6666-4666-8666-666666666666")
	goalID  = uuid.MustParse("77777777-7777-4777-8777-777777777777")
	colID   = uuid.MustParse("88888888-8888-4888-8888-888888888888")
	fiveTwo = uuid.MustParse("99999999-9999-4999-8999-999999999999")
)

// testBaseline is a household with one of each bucket kind.
func testBaseline() Baseline {
	return Baseline{
		Now: now2026,
		Assumptions: networth.RetirementAssumptions{
			RealReturnRate: dec("0.05"),
			WithdrawalRate: dec("0.04"),
			CurrentAge:     40,
		},
		InflationRate:    dec("0.03"),
		CollegeInflation: dec("5.50"),
		Age:              40,
		AgeKnown:         true,
		FilingStatus:     networth.FilingSingle,
		MAGI:             nd("90000"), // comfortably under the phase-out
		Buckets: []Bucket{
			{AccountID: rothID, Name: "Roth IRA", Kind: BucketInvestment, Treatment: "roth_ira", Balance: dec("20000")},
			{AccountID: brokID, Name: "Brokerage", Kind: BucketInvestment, Treatment: "taxable", Balance: dec("50000")},
			{AccountID: cardID, Name: "Visa", Kind: BucketDebt, Balance: dec("6000"), Subtype: "credit card",
				Terms: goals.DebtTerms{
					APR: dec("19.99"), APRSource: goals.TermSourceManual,
					Payment: dec("200"), PaymentSource: goals.TermSourceManual,
				}},
			{AccountID: cashID, Name: "Savings", Kind: BucketCash, Balance: dec("15000"),
				Subtype: "savings", DepositAPY: nd("4.50")},
		},
		Plans: []networth.AccountPlan{
			{ID: rothID.String(), Name: "Roth IRA", Treatment: "roth_ira", Balance: dec("20000")},
			{ID: brokID.String(), Name: "Brokerage", Treatment: "taxable", Balance: dec("50000")},
		},
		UntaggedAccounts:  []string{},
		MonthlyFixedCosts: dec("3200"),
		Goals:             []GoalRow{},
		CollegeGoals:      []CollegeGoal{},
	}
}

func run(t *testing.T, b Baseline, req Request) Result {
	t.Helper()
	got, err := Run(b, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return got
}

// --------------------------------------------------------------------------
// The two rules the whole package rests on
// --------------------------------------------------------------------------

// A PLAN MUST NEVER MUTATE THE BASELINE. This is the most dangerous bug
// available here — the same class as a scenario engine editing the scenario it
// was built from — and the only cheap way to be sure is to compare the whole
// thing before and after.
func TestRunDoesNotMutateTheBaseline(t *testing.T) {
	b := testBaseline()
	before := deepCopy(b)

	run(t, b, Request{
		Lump: dec("30000"), Monthly: dec("1800"), HorizonYears: 17,
		Splits: []Split{
			{AccountID: rothID, LumpPct: dec("25"), MonthlyPct: dec("30")},
			{AccountID: brokID, LumpPct: dec("50"), MonthlyPct: dec("20")},
			{AccountID: cardID, LumpPct: dec("25"), MonthlyPct: dec("50")},
		},
	})

	if !reflect.DeepEqual(b, before) {
		t.Fatal("Run mutated the baseline it was handed")
	}
}

// BASELINE VS BASELINE. A plan with no money in it must produce exactly the
// household's current position: zero delta. If it does not, the engine is not
// deterministic and every other figure it produces is suspect.
func TestZeroPlanProducesZeroDelta(t *testing.T) {
	got := run(t, testBaseline(), Request{HorizonYears: 20, Splits: []Split{}})

	if !got.Delta.IsZero() {
		t.Errorf("delta = %s, want exactly 0 for a plan that allocates nothing", got.Delta)
	}
	if !got.ProjectedAssets.Equal(got.BaselineAssets) {
		t.Errorf("projected %s vs baseline %s", got.ProjectedAssets, got.BaselineAssets)
	}
}

// --------------------------------------------------------------------------
// Eligibility is separate from the cap
// --------------------------------------------------------------------------

// Over the phase-out: a $0 projected Roth contribution, not a $7,500 one.
func TestIneligibleRothGetsZeroNotTheCap(t *testing.T) {
	b := testBaseline()
	b.MAGI = nd("250000")

	got := run(t, b, Request{
		Lump: dec("8000"), HorizonYears: 10,
		Splits: []Split{{AccountID: rothID, LumpPct: dec("100")}},
	})

	roth := bucketByID(t, got, rothID)
	if roth.Eligibility != networth.EligibilityIneligible {
		t.Fatalf("eligibility = %q, want ineligible", roth.Eligibility)
	}
	if !roth.AppliedLump.IsZero() {
		t.Errorf("applied lump = %s, want 0 — a cap is not permission", roth.AppliedLump)
	}
	if want := dec("8000"); !roth.EligibilitySpill.Equal(want) {
		t.Errorf("eligibility spill = %s, want %s", roth.EligibilitySpill, want)
	}
	// And it must be reported as an ELIGIBILITY refusal, not silently folded in
	// with the IRS cap: the two rules are different and the fix is different.
	if roth.EligibilityNote == "" {
		t.Error("no note explaining why the money could not go in")
	}
}

// Inside the phase-out: the computed reduced limit binds, not the full cap.
func TestPhasedOutRothIsHeldAtTheReducedLimit(t *testing.T) {
	b := testBaseline()
	b.MAGI = nd("160500") // half-way through the 2026 single band → $3,750

	got := run(t, b, Request{
		Lump: dec("7500"), HorizonYears: 10,
		Splits: []Split{{AccountID: rothID, LumpPct: dec("100")}},
	})

	roth := bucketByID(t, got, rothID)
	if roth.Eligibility != networth.EligibilityPhasedOut {
		t.Fatalf("eligibility = %q, want phased_out", roth.Eligibility)
	}
	if want := dec("3750"); !roth.AppliedLump.Equal(want) {
		t.Errorf("applied lump = %s, want %s", roth.AppliedLump, want)
	}
}

// NO MAGI ENTERED IS `unknown` WITH A LABELLED CAVEAT — never `eligible`, and
// never a refusal either: a household that has not filled in an optional field
// still gets a working planner.
func TestUnknownEligibilityStillProjectsButSaysSo(t *testing.T) {
	b := testBaseline()
	b.MAGI = decimal.NullDecimal{}

	got := run(t, b, Request{
		Lump: dec("5000"), HorizonYears: 10,
		Splits: []Split{{AccountID: rothID, LumpPct: dec("100")}},
	})

	roth := bucketByID(t, got, rothID)
	if roth.Eligibility != networth.EligibilityUnknown {
		t.Fatalf("eligibility = %q, want unknown", roth.Eligibility)
	}
	if want := dec("5000"); !roth.AppliedLump.Equal(want) {
		t.Errorf("applied lump = %s, want %s — unknown must not block the projection", roth.AppliedLump, want)
	}
	if roth.EligibilityNote == "" {
		t.Error("an unknown eligibility must carry the caveat that makes it honest")
	}
}

// --------------------------------------------------------------------------
// Bucket kinds
// --------------------------------------------------------------------------

// A debt bucket is amortization, and its "return" is interest avoided.
func TestDebtBucketAmortizes(t *testing.T) {
	got := run(t, testBaseline(), Request{
		Monthly: dec("400"), HorizonYears: 10,
		Splits: []Split{{AccountID: cardID, MonthlyPct: dec("100")}},
	})

	card := bucketByID(t, got, cardID)
	if card.Engine != "amortization" {
		t.Fatalf("engine = %q, want amortization — a card must never show a compound formula", card.Engine)
	}
	if card.PayoffBase == nil || card.PayoffPlan == nil {
		t.Fatal("both payoff schedules should be present")
	}
	if card.PayoffPlan.Months >= card.PayoffBase.Months {
		t.Errorf("plan payoff %d months is not sooner than the base %d",
			card.PayoffPlan.Months, card.PayoffBase.Months)
	}
	if !card.InterestAvoided.IsPositive() {
		t.Errorf("interest avoided = %s, want positive", card.InterestAvoided)
	}
	// A debt is never counted as an asset.
	if !card.ProjectedValue.IsZero() {
		t.Errorf("projected value = %s, want 0: a retired balance is not a portfolio", card.ProjectedValue)
	}
}

// A payment at or below the interest never retires the debt, and the engine says
// "never" rather than printing a large number nobody should read.
func TestDebtThatNeverPaysOffIsReportedAsSuch(t *testing.T) {
	b := testBaseline()
	for i := range b.Buckets {
		if b.Buckets[i].AccountID == cardID {
			b.Buckets[i].Balance = dec("20000")
			b.Buckets[i].Terms.Payment = dec("50") // under the monthly interest
		}
	}

	got := run(t, b, Request{
		HorizonYears: 10,
		Splits:       []Split{{AccountID: cardID, LumpPct: dec("0"), MonthlyPct: dec("0")}},
	})
	// A split with nothing in it is still a bucket the user asked about.
	card := bucketByID(t, got, cardID)
	if card.PayoffPlan == nil || !card.PayoffPlan.NeverPaysOff {
		t.Fatal("want never_pays_off at a payment below the interest")
	}
	if !card.InterestAvoided.IsZero() {
		t.Errorf("interest avoided = %s, want 0 when neither side amortizes", card.InterestAvoided)
	}
}

// A cash bucket accrues at its own yield with no volatility — and in REAL terms,
// which is the part that is easy to get flatteringly wrong.
func TestCashBucketAccruesInRealTerms(t *testing.T) {
	got := run(t, testBaseline(), Request{
		Lump: dec("10000"), HorizonYears: 10,
		Splits: []Split{{AccountID: cashID, LumpPct: dec("100")}},
	})

	cash := bucketByID(t, got, cashID)
	if cash.Engine != "accrual" {
		t.Fatalf("engine = %q, want accrual", cash.Engine)
	}
	// 4.50% nominal less 3% assumed inflation = 1.5% real, not 4.5%.
	if want := dec("0.015"); !cash.ReturnRate.Equal(want) {
		t.Errorf("return rate = %s, want %s (nominal APY less inflation)", cash.ReturnRate, want)
	}
	if !cash.ProjectedValue.GreaterThan(dec("25000")) {
		t.Errorf("projected %s should exceed the %s deposited", cash.ProjectedValue, dec("25000"))
	}
}

// A yield below inflation is a NEGATIVE real return, reported rather than
// clamped. It is the single most useful thing this bucket can tell a household
// sitting on cash.
func TestCashBelowInflationHasANegativeRealReturn(t *testing.T) {
	b := testBaseline()
	for i := range b.Buckets {
		if b.Buckets[i].AccountID == cashID {
			b.Buckets[i].DepositAPY = nd("0.40")
		}
	}
	got := run(t, b, Request{
		Lump: dec("10000"), HorizonYears: 10,
		Splits: []Split{{AccountID: cashID, LumpPct: dec("100")}},
	})

	cash := bucketByID(t, got, cashID)
	if !cash.ReturnRate.IsNegative() {
		t.Errorf("return rate = %s, want negative at 0.4%% against 3%% inflation", cash.ReturnRate)
	}
	if len(cash.Notes) == 0 {
		t.Error("a negative real return must be said out loud")
	}
}

// An untagged investment account is not offerable as a bucket at all, and naming
// it in a split is refused rather than silently dropped — a vanished line would
// leave the percentages summing to 100 while the money went nowhere.
func TestUntaggedAccountIsNotABucket(t *testing.T) {
	b := testBaseline()
	b.UntaggedAccounts = []string{"Mystery brokerage"}

	if _, err := Run(b, Request{
		Lump: dec("1000"), HorizonYears: 10,
		Splits: []Split{{AccountID: planID, LumpPct: dec("100")}},
	}); err == nil {
		t.Fatal("want an error for an account that is not an allocatable bucket")
	}

	got := run(t, b, Request{HorizonYears: 10, Splits: []Split{}})
	if len(got.ExcludedAccounts) != 1 {
		t.Errorf("excluded = %v, want the untagged account listed rather than dropped", got.ExcludedAccounts)
	}
}

// --------------------------------------------------------------------------
// Requests the engine has to refuse
// --------------------------------------------------------------------------

func TestRequestValidation(t *testing.T) {
	b := testBaseline()
	cases := []struct {
		name string
		req  Request
	}{
		{"horizon too short", Request{HorizonYears: 0}},
		{"horizon too long", Request{HorizonYears: 999}},
		{"negative lump", Request{HorizonYears: 10, Lump: dec("-1")}},
		{"lump over 100%", Request{HorizonYears: 10, Splits: []Split{
			{AccountID: rothID, LumpPct: dec("60")},
			{AccountID: brokID, LumpPct: dec("60")},
		}}},
		{"duplicate account", Request{HorizonYears: 10, Splits: []Split{
			{AccountID: rothID, LumpPct: dec("10")},
			{AccountID: rothID, LumpPct: dec("10")},
		}}},
	}
	for _, c := range cases {
		if _, err := Run(b, c.req); err == nil {
			t.Errorf("%s: want an error", c.name)
		}
	}
}

// Money the split did not place is reported, never silently absorbed into
// another bucket.
func TestUnallocatedMoneyIsReported(t *testing.T) {
	got := run(t, testBaseline(), Request{
		Lump: dec("10000"), Monthly: dec("1000"), HorizonYears: 10,
		Splits: []Split{{AccountID: brokID, LumpPct: dec("60"), MonthlyPct: dec("25")}},
	})

	if want := dec("4000"); !got.UnallocatedLump.Equal(want) {
		t.Errorf("unallocated lump = %s, want %s", got.UnallocatedLump, want)
	}
	if want := dec("750"); !got.UnallocatedMonthly.Equal(want) {
		t.Errorf("unallocated monthly = %s, want %s", got.UnallocatedMonthly, want)
	}
}

// --------------------------------------------------------------------------
// Goals and horizons
// --------------------------------------------------------------------------

// A goal due inside three years, funded into a volatile bucket, is FLAGGED —
// not refused, and not silently reallocated. The engine computes what the user
// asked for and tells them what it noticed.
func TestShortHorizonGoalInAVolatileBucketIsFlagged(t *testing.T) {
	b := testBaseline()
	soon := now2026.AddDate(1, 0, 0)
	b.Goals = []GoalRow{{
		ID: goalID, Name: "House deposit", Kind: "savings",
		AccountID: &brokID, Target: dec("40000"), Current: dec("10000"), TargetDate: &soon,
	}}

	got := run(t, b, Request{
		Monthly: dec("1000"), HorizonYears: 10,
		Splits: []Split{{AccountID: brokID, MonthlyPct: dec("100")}},
	})

	if len(got.HorizonFlags) != 1 {
		t.Fatalf("horizon flags = %v, want one", got.HorizonFlags)
	}
	if got.HorizonFlags[0].GoalName != "House deposit" {
		t.Errorf("flagged %q", got.HorizonFlags[0].GoalName)
	}

	// And the plan still ran: nothing was refused.
	if !bucketByID(t, got, brokID).AppliedMonthly.Equal(dec("1000")) {
		t.Error("the flag must not change the allocation")
	}
}

// The same goal five years out is not flagged: the horizon is long enough to
// ride out a drawdown, and flagging it would be the noise the rule exists to
// avoid.
func TestLongHorizonGoalIsNotFlagged(t *testing.T) {
	b := testBaseline()
	later := now2026.AddDate(5, 0, 0)
	b.Goals = []GoalRow{{
		ID: goalID, Name: "House deposit", Kind: "savings",
		AccountID: &brokID, Target: dec("40000"), Current: dec("10000"), TargetDate: &later,
	}}

	got := run(t, b, Request{
		Monthly: dec("1000"), HorizonYears: 10,
		Splits: []Split{{AccountID: brokID, MonthlyPct: dec("100")}},
	})
	if len(got.HorizonFlags) != 0 {
		t.Errorf("horizon flags = %v, want none five years out", got.HorizonFlags)
	}
}

// A goal with no linked account cannot be funded by the plan, and says so rather
// than guessing which bucket the user meant.
func TestUnlinkedGoalIsReportedAsUnfundable(t *testing.T) {
	b := testBaseline()
	b.Goals = []GoalRow{{
		ID: goalID, Name: "Someday", Kind: "savings",
		Target: dec("5000"), Current: dec("0"),
	}}

	got := run(t, b, Request{
		Monthly: dec("500"), HorizonYears: 10,
		Splits: []Split{{AccountID: brokID, MonthlyPct: dec("100")}},
	})

	if len(got.Goals) != 1 {
		t.Fatalf("goals = %v", got.Goals)
	}
	if got.Goals[0].Linked {
		t.Error("an unlinked goal must not be reported as linked")
	}
	if !got.Goals[0].PlanMonthly.IsZero() {
		t.Errorf("plan monthly = %s, want 0 for an unlinked goal", got.Goals[0].PlanMonthly)
	}
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func bucketByID(t *testing.T, r Result, id uuid.UUID) BucketResult {
	t.Helper()
	for _, b := range r.Buckets {
		if b.AccountID == id {
			return b
		}
	}
	t.Fatalf("no result for account %s", id)
	return BucketResult{}
}

// deepCopy clones a baseline so the mutation test compares against a genuinely
// independent value rather than against a struct sharing the same backing
// arrays — which would make the assertion pass for the wrong reason.
//
// cloneSlice rather than `append([]T(nil), src...)`, and the difference is the
// whole reason this comment exists: appending to a nil slice returns NIL when
// the source is empty, so an untouched `[]string{}` came back as `nil` and
// reflect.DeepEqual reported a mutation that had not happened. A false positive
// on the highest-priority safety test in the package is worse than no test —
// the reflex on seeing it fail is to go looking for a bug in the engine.
func deepCopy(b Baseline) Baseline {
	out := b
	out.Buckets = cloneSlice(b.Buckets)
	out.Plans = cloneSlice(b.Plans)
	out.UntaggedAccounts = cloneSlice(b.UntaggedAccounts)
	out.Goals = cloneSlice(b.Goals)
	out.CollegeGoals = cloneSlice(b.CollegeGoals)
	return out
}

// cloneSlice copies a slice, preserving the nil/empty distinction.
func cloneSlice[T any](src []T) []T {
	if src == nil {
		return nil
	}
	out := make([]T, len(src))
	copy(out, src)
	return out
}
