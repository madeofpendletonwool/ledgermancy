package advisor

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/goals"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
)

// These tests drive the ranker directly over hand-built inputs. That is the
// whole point: the waterfall is the thing the feature is accountable for, and it
// is pure — every figure that places an option is already computed by the time
// rank() sees it. The database-backed agreement tests live beside these in
// advisor_db_test.go.

var testNow = time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)

func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

// baseInputs is a household that has cleared its starter emergency fund, so the
// waterfall is open past tier 1. Individual tests add the option they are about.
func baseInputs() inputs {
	return inputs{
		now: testNow,
		slack: slackResult{
			amount:     dec("400"),
			basis:      SlackBasisTypicalMonth,
			fixedCosts: dec("2000"),
		},
		hurdle:            dec("6"),
		hurdleBasis:       "the 6% floor (your assumed real return is lower)",
		realReturn:        dec("0.05"),
		liquid:            dec("9000"),
		starterTarget:     dec("2000"),
		fullTarget:        dec("6000"),
		efMonths:          3,
		matchPeriodsLeft:  20,
		matchPeriodsBasis: matchWindowCalendar,
		matchMonthsLeft:   10,
	}
}

func testDebt(name, balance, apr, payment string) debt {
	d := debt{id: uuid.New(), name: name, balance: dec(balance)}
	if apr != "" {
		d.terms.APR, d.terms.APRSource = dec(apr), goals.TermSourceManual
	} else {
		d.terms.APRSource = goals.TermSourceNone
	}
	if payment != "" {
		d.terms.Payment, d.terms.PaymentSource = dec(payment), goals.TermSourceManual
	} else {
		d.terms.PaymentSource = goals.TermSourceNone
	}
	return d
}

func keys(opts []Option) []string {
	out := make([]string, len(opts))
	for i, o := range opts {
		out[i] = o.Key
	}
	return out
}

func findKind(opts []Option, kind string) (Option, bool) {
	for _, o := range opts {
		if o.Kind == kind {
			return o, true
		}
	}
	return Option{}, false
}

// --------------------------------------------------------------------------
// The waterfall, tier by tier
// --------------------------------------------------------------------------

// TestStarterEmergencyFundIsTheOnlyOption is the tier-1 short circuit, and it is
// the test that stops the ranker draining a household's only cash into a card.
// A 24% balance is the strongest thing tiers 2–7 can offer, and it must not
// appear at all.
func TestStarterEmergencyFundIsTheOnlyOption(t *testing.T) {
	in := baseInputs()
	in.liquid = dec("300") // well under the 2000 starter target
	in.debts = []debt{testDebt("Card", "5000", "24", "150")}
	in.matches = []matchInput{{
		accountID: uuid.New().String(), name: "401(k)",
		full: dec("4000"), captured: dec("1000"), unclaimed: dec("3000"),
	}}

	got := rank(in, suppression{})
	if len(got) != 1 {
		t.Fatalf("want exactly 1 option while the starter fund is unmet, got %d: %v", len(got), keys(got))
	}
	if got[0].Kind != KindEmergencyFund || got[0].Tier != tierStarterEF {
		t.Fatalf("want the tier-1 emergency fund, got %+v", got[0])
	}
}

// TestEmployerMatchOutranksAHighAPRCard is tier 2 versus tier 3. The match is an
// instant guaranteed 50-100% AND it expires; the card is worth the same next
// month.
func TestEmployerMatchOutranksAHighAPRCard(t *testing.T) {
	in := baseInputs()
	in.debts = []debt{testDebt("Card", "5000", "22", "150")}
	in.matches = []matchInput{{
		accountID: uuid.New().String(), name: "401(k)",
		full: dec("4000"), captured: dec("1000"), unclaimed: dec("3000"),
	}}

	got := rank(in, suppression{})
	if len(got) == 0 {
		t.Fatal("want options")
	}
	if got[0].Kind != KindEmployerMatch {
		t.Fatalf("want the employer match first, got %q (%v)", got[0].Kind, keys(got))
	}
	if got[0].Tier != tierMatch {
		t.Errorf("want tier %d, got %d", tierMatch, got[0].Tier)
	}
}

// TestLowAPRMortgageNeverOutranksTaxAdvantagedHeadroom is the failure the first
// draft's "guaranteed return first" rule would have shipped: a 3.5% mortgage
// paid down ahead of a Roth. It must land in tier 7, beside taxable investing.
func TestLowAPRMortgageNeverOutranksTaxAdvantagedHeadroom(t *testing.T) {
	in := baseInputs()
	in.debts = []debt{testDebt("Mortgage", "250000", "3.5", "1400")}
	in.headroom = []headroomInput{{
		group: "ira", label: "roth_ira",
		limit: dec("7000"), planned: dec("1000"), headroom: dec("6000"),
	}}

	got := rank(in, suppression{})
	mortgage, ok := findKind(got, KindDebtPaydown)
	if !ok {
		t.Fatal("want the mortgage listed")
	}
	if mortgage.Tier != tierTradeoff {
		t.Errorf("want the 3.5%% mortgage in tier %d, got %d", tierTradeoff, mortgage.Tier)
	}
	if !mortgage.Tradeoff {
		t.Error("want the below-hurdle mortgage marked as a tradeoff, not ranked")
	}

	room, ok := findKind(got, KindTaxAdvantaged)
	if !ok {
		t.Fatal("want the tax-advantaged headroom listed")
	}
	if room.Tier >= mortgage.Tier {
		t.Errorf("want headroom (tier %d) ahead of the mortgage (tier %d)", room.Tier, mortgage.Tier)
	}
}

// TestHighAPRDebtClearsTheHurdle is the other side of the same line: above the
// hurdle a guaranteed return genuinely beats an assumed one, so the card is
// RANKED rather than presented as a toss-up.
func TestHighAPRDebtClearsTheHurdle(t *testing.T) {
	in := baseInputs()
	in.debts = []debt{testDebt("Card", "5000", "22", "150")}

	got := rank(in, suppression{})
	card, ok := findKind(got, KindDebtPaydown)
	if !ok {
		t.Fatal("want the card listed")
	}
	if card.Tier != tierDebtAbove {
		t.Errorf("want tier %d for a 22%% card against a 6%% hurdle, got %d", tierDebtAbove, card.Tier)
	}
	if card.Tradeoff {
		t.Error("an above-hurdle debt is ranked, not a tradeoff")
	}
}

// TestHurdleFollowsTheHouseholdsOwnAssumption is what makes the rule honest
// rather than a constant: the SAME debt falls on different sides of the hurdle
// for two households with different stated assumptions.
func TestHurdleFollowsTheHouseholdsOwnAssumption(t *testing.T) {
	d := testDebt("Car loan", "18000", "7", "350")

	modest := baseInputs() // 6% floor
	modest.debts = []debt{d}
	got := rank(modest, suppression{})
	car, _ := findKind(got, KindDebtPaydown)
	if car.Tier != tierDebtAbove {
		t.Errorf("7%% against a 6%% hurdle should rank: want tier %d, got %d", tierDebtAbove, car.Tier)
	}

	optimistic := baseInputs()
	optimistic.realReturn = dec("0.09")
	optimistic.hurdle, optimistic.hurdleBasis = resolveHurdle(optimistic.realReturn)
	optimistic.debts = []debt{d}
	got = rank(optimistic, suppression{})
	car, _ = findKind(got, KindDebtPaydown)
	if car.Tier != tierTradeoff {
		t.Errorf("7%% against a 9%% hurdle is a toss-up: want tier %d, got %d", tierTradeoff, car.Tier)
	}
}

// TestResolveHurdleFloor pins the floor and the unit conversion. The stored rate
// is a FRACTION and the hurdle is a PERCENTAGE; conflating them is the trap the
// whole comparison rests on.
func TestResolveHurdleFloor(t *testing.T) {
	for _, tc := range []struct {
		realReturn string
		want       string
	}{
		{"0.02", "6"}, // below the floor
		{"0.06", "6"}, // exactly the floor
		{"0.07", "7"}, // above
		{"0.0725", "7.25"},
	} {
		got, _ := resolveHurdle(dec(tc.realReturn))
		if !got.Equal(dec(tc.want)) {
			t.Errorf("resolveHurdle(%s) = %s, want %s", tc.realReturn, got, tc.want)
		}
	}
}

// --------------------------------------------------------------------------
// Employer match
// --------------------------------------------------------------------------

// TestEmployerMatchAgreesWithNetworth is the doc's agreement requirement: the
// captured figure must equal networth.AnnualMatch for the same account, to the
// cent. It holds because there is one implementation, and this fails the moment
// somebody forks it.
func TestEmployerMatchAgreesWithNetworth(t *testing.T) {
	p := networth.AccountPlan{
		ID: "a", Name: "401(k)", Treatment: "trad_401k",
		MonthlyContribution: dec("100"),  // defers 1,200/yr
		EmployerMatchPct:    dec("0.05"), // 5% of salary
		AnnualSalary:        dec("80000"),
	}

	full := networth.FullAnnualMatch(p) // 4,000
	captured := networth.AnnualMatch(p) // clamped to the 1,200 deferred

	if !full.Equal(dec("4000")) {
		t.Errorf("full match = %s, want 4000", full)
	}
	if !captured.Equal(dec("1200")) {
		t.Errorf("captured match = %s, want 1200 (clamped to what was deferred)", captured)
	}
	if unclaimed := full.Sub(captured); !unclaimed.Equal(dec("2800")) {
		t.Errorf("unclaimed = %s, want 2800", unclaimed)
	}
}

// TestEmployerMatchRefusesToGuess: a match percentage with no salary behind it
// is not an amount. It must produce NO option rather than a zero or a guess.
func TestEmployerMatchRefusesToGuess(t *testing.T) {
	p := networth.AccountPlan{
		ID: "a", Name: "401(k)",
		EmployerMatchPct:    dec("0.05"),
		MonthlyContribution: dec("100"),
		// No AnnualSalary.
	}
	if got := networth.FullAnnualMatch(p); !got.IsZero() {
		t.Errorf("want no match figure without a salary, got %s", got)
	}
}

// TestEmployerMatchExpiresWithThePayPeriods is the calendar bound. Once the
// year's pay periods are exhausted the headroom is gone, not late, and the
// option must disappear entirely.
func TestEmployerMatchExpiresWithThePayPeriods(t *testing.T) {
	in := baseInputs()
	in.matches = []matchInput{{
		accountID: uuid.New().String(), name: "401(k)",
		full: dec("4000"), captured: dec("1000"), unclaimed: dec("3000"),
	}}

	if _, ok := findKind(rank(in, suppression{}), KindEmployerMatch); !ok {
		t.Fatal("want a match option while pay periods remain")
	}

	in.matchPeriodsLeft = 0
	if _, ok := findKind(rank(in, suppression{}), KindEmployerMatch); ok {
		t.Error("want NO match option once the year's pay periods are exhausted")
	}
}

// --------------------------------------------------------------------------
// Debt arithmetic
// --------------------------------------------------------------------------

// TestInterestAvoidedAgainstAHandComputedFixture checks the figure against the
// payoff engine directly, and in doing so pins the APR convention: 22 means
// 22%, not 2200%. A fraction passed here would make the interest avoided
// absurdly small and the test fails loudly.
func TestInterestAvoidedAgainstAHandComputedFixture(t *testing.T) {
	in := baseInputs()
	in.slack.amount = dec("200")
	d := testDebt("Card", "5000", "22", "150")
	in.debts = []debt{d}

	base := goals.ComputePayoff(dec("5000"), dec("22"), dec("150"), nil, testNow)
	accel := goals.ComputePayoff(dec("5000"), dec("22"), dec("350"), nil, testNow)
	want := base.TotalInterest.Sub(accel.TotalInterest).Round(2)

	card, ok := findKind(rank(in, suppression{}), KindDebtPaydown)
	if !ok {
		t.Fatal("want the card listed")
	}
	if !card.Value.Equal(want) {
		t.Errorf("interest avoided = %s, want %s", card.Value, want)
	}
	if card.ValueKind != ValueInterestAvoided {
		t.Errorf("value kind = %q, want %q", card.ValueKind, ValueInterestAvoided)
	}
	// Sanity: a 22% card carrying $5,000 at $150/month really does cost
	// hundreds in avoidable interest. A fraction-vs-percentage slip would
	// collapse this to pennies.
	if want.LessThan(dec("100")) {
		t.Errorf("fixture looks wrong: only %s avoided — is the APR being read as a fraction?", want)
	}
}

// TestUnknownAPRIsListedUnrankedAndLast. A debt whose rate nobody knows is
// exactly the debt most likely to be the expensive one, so it is never dropped,
// never defaulted, and never sorted as though its rate were zero.
func TestUnknownAPRIsListedUnrankedAndLast(t *testing.T) {
	in := baseInputs()
	in.debts = []debt{
		testDebt("Store card", "1200", "", "40"), // no APR anywhere
		testDebt("Visa", "5000", "22", "150"),
	}

	got := rank(in, suppression{})

	var mystery Option
	found := false
	for _, o := range got {
		if o.Unranked {
			mystery, found = o, true
		}
	}
	if !found {
		t.Fatal("want the unknown-APR debt listed, not dropped")
	}
	if mystery.Tier != 0 {
		t.Errorf("an unranked option carries no tier, got %d", mystery.Tier)
	}
	if mystery.Note == "" || !strings.Contains(mystery.Note, "APR unknown") {
		t.Errorf("want an APR-unknown label, got %q", mystery.Note)
	}
	if got[len(got)-1].Key != mystery.Key {
		t.Errorf("want the unranked debt last, got order %v", keys(got))
	}
	// And specifically NOT sorted as a 0% debt would be — a 0% debt is
	// below the hurdle and lands in tier 7, ahead of nothing at all.
	for _, o := range got {
		if o.Unranked && o.Tradeoff {
			t.Error("an unranked debt must not be mixed into the tier-7 tradeoff")
		}
	}
}

// TestDebtThatNeverClearsSortsFirstInItsTier. "At $50/month this card is never
// paid off" is the strongest thing the app can say about a debt, and interest
// avoided is not a difference of two finite totals — so Value stays zero and the
// flag carries the meaning.
func TestDebtThatNeverClearsSortsFirstInItsTier(t *testing.T) {
	in := baseInputs()
	in.slack.amount = dec("300")
	in.debts = []debt{
		testDebt("Ordinary card", "3000", "20", "200"),
		testDebt("Stuck card", "10000", "24", "195"), // interest ≈ $200/mo
	}

	got := rank(in, suppression{})
	if len(got) < 2 {
		t.Fatalf("want both debts, got %v", keys(got))
	}
	if !got[0].Unbounded {
		t.Errorf("want the never-clearing debt first in its tier, got %q", got[0].Label)
	}
	if !got[0].Value.IsZero() {
		t.Errorf("an unbounded saving is not a number: want zero Value, got %s", got[0].Value)
	}
}

// --------------------------------------------------------------------------
// Order, suppression, and the quiet cases
// --------------------------------------------------------------------------

// TestRankingIsDeterministicAndTotal. Same inputs, same order, twice — and no
// two options compare equal, since ties break on label.
func TestRankingIsDeterministicAndTotal(t *testing.T) {
	in := baseInputs()
	// Two debts with IDENTICAL terms: same tier, same computed value. Only the
	// label can separate them, which is exactly what makes the order total.
	a := testDebt("Alpha card", "5000", "22", "150")
	b := testDebt("Beta card", "5000", "22", "150")
	in.debts = []debt{b, a} // deliberately out of order
	in.goals = []goalInput{{
		id: uuid.New(), name: "Roof",
		feas: goals.Feasibility{Remaining: dec("6000"), RequiredMonthly: dec("500"), Shortfall: dec("100")},
	}}

	first := keys(rank(in, suppression{}))
	second := keys(rank(in, suppression{}))
	if strings.Join(first, "|") != strings.Join(second, "|") {
		t.Fatalf("ranking is not stable:\n  %v\n  %v", first, second)
	}

	got := rank(in, suppression{})
	for i := 1; i < len(got); i++ {
		p, c := got[i-1], got[i]
		if p.Tier == c.Tier && p.Unbounded == c.Unbounded &&
			p.Value.Equal(c.Value) && p.Label == c.Label {
			t.Errorf("two options compare equal, so the order is not total: %q", p.Label)
		}
	}
	// Alphabetical within the tie.
	if got[0].Label != "Pay down Alpha card" {
		t.Errorf("want the tie broken on label, got %q first", got[0].Label)
	}
}

// TestSuppressionRemovesAnOptionBeforeItIsEmitted, and does so by the stable
// key — which survives the option moving between tiers.
func TestSuppressionRemovesAnOptionBeforeItIsEmitted(t *testing.T) {
	in := baseInputs()
	d := testDebt("Card", "5000", "22", "150")
	in.debts = []debt{d}

	if _, ok := findKind(rank(in, suppression{}), KindDebtPaydown); !ok {
		t.Fatal("want the card before suppression")
	}

	muted := suppression{OptionKey(KindDebtPaydown, d.id.String()): true}
	if _, ok := findKind(rank(in, muted), KindDebtPaydown); ok {
		t.Error("want the card gone once its key is suppressed")
	}
}

// TestDebtKeyIsStableAcrossTiers. "Stop suggesting this card" must keep meaning
// that when the household's assumed return rises and the card crosses the
// hurdle — so the key cannot encode the tier.
func TestDebtKeyIsStableAcrossTiers(t *testing.T) {
	d := testDebt("Car loan", "18000", "7", "350")

	low := baseInputs()
	low.debts = []debt{d}
	high := baseInputs()
	high.realReturn = dec("0.09")
	high.hurdle, high.hurdleBasis = resolveHurdle(high.realReturn)
	high.debts = []debt{d}

	above, _ := findKind(rank(low, suppression{}), KindDebtPaydown)
	below, _ := findKind(rank(high, suppression{}), KindDebtPaydown)

	if above.Tier == below.Tier {
		t.Fatal("fixture broken: the debt should change tier between these households")
	}
	if above.Key != below.Key {
		t.Errorf("key changed with the tier: %q vs %q", above.Key, below.Key)
	}
}

// TestSilenceRatherThanAnEmptyPanel: a household with no debts, no goals and no
// retirement accounts produces nothing at all.
func TestSilenceRatherThanAnEmptyPanel(t *testing.T) {
	in := baseInputs()
	in.realReturn = decimal.Zero // no assumption ⇒ no taxable-investing option
	got := rank(in, suppression{})
	if len(got) != 0 {
		t.Errorf("want silence, got %v", keys(got))
	}
}

// TestTradeoffTierCarriesBothSides. Tier 7 is a comparison, not a ranking: the
// below-hurdle debt and taxable investing both appear and both are marked.
func TestTradeoffTierCarriesBothSides(t *testing.T) {
	in := baseInputs()
	in.debts = []debt{testDebt("Mortgage", "250000", "3.5", "1400")}

	got := rank(in, suppression{})
	var tradeoffs int
	for _, o := range got {
		if o.Tradeoff {
			tradeoffs++
			if o.Tier != tierTradeoff {
				t.Errorf("a tradeoff option must be tier %d, got %d", tierTradeoff, o.Tier)
			}
		}
	}
	if tradeoffs != 2 {
		t.Errorf("want both sides of the tradeoff, got %d: %v", tradeoffs, keys(got))
	}
}

// TestFullEmergencyFundIsTier4, above headroom and goals but below a card that
// clears the hurdle.
func TestFullEmergencyFundIsTier4(t *testing.T) {
	in := baseInputs()
	in.liquid = dec("2500") // past the starter, short of the 6,000 target
	in.debts = []debt{testDebt("Card", "5000", "22", "150")}
	in.goals = []goalInput{{
		id: uuid.New(), name: "Roof",
		feas: goals.Feasibility{Remaining: dec("6000"), RequiredMonthly: dec("500"), Shortfall: dec("100")},
	}}

	got := rank(in, suppression{})
	ef, ok := findKind(got, KindEmergencyFund)
	if !ok {
		t.Fatal("want the full emergency-fund option")
	}
	if ef.Tier != tierFullEF {
		t.Errorf("want tier %d, got %d", tierFullEF, ef.Tier)
	}
	card, _ := findKind(got, KindDebtPaydown)
	goal, _ := findKind(got, KindGoalAccel)
	if !(card.Tier < ef.Tier && ef.Tier < goal.Tier) {
		t.Errorf("want card(%d) < fund(%d) < goal(%d)", card.Tier, ef.Tier, goal.Tier)
	}
}

// TestGoalAccelerationReportsMonthsNotDollars. Value is a COUNT here, and the
// value kind says so — rendering it as money is the small lie that makes a panel
// untrustworthy.
func TestGoalAccelerationReportsMonthsNotDollars(t *testing.T) {
	in := baseInputs()
	in.slack.amount = dec("500")
	in.goals = []goalInput{{
		id: uuid.New(), name: "Roof",
		feas: goals.Feasibility{Remaining: dec("6000"), RequiredMonthly: dec("500"), Shortfall: dec("200")},
	}}

	g, ok := findKind(rank(in, suppression{}), KindGoalAccel)
	if !ok {
		t.Fatal("want the goal option")
	}
	if g.ValueKind != ValueMonthsEarlier {
		t.Errorf("value kind = %q, want %q", g.ValueKind, ValueMonthsEarlier)
	}
	// 6,000 at 500/mo is 12 months; at 1,000/mo it is 6. Six months earlier.
	if !g.Value.Equal(dec("6")) {
		t.Errorf("months earlier = %s, want 6", g.Value)
	}
}

// TestMonthsLeftInYearNeverReachesZero. December must leave one month, or the
// per-month figures divide by zero.
func TestMonthsLeftInYearNeverReachesZero(t *testing.T) {
	for m := time.January; m <= time.December; m++ {
		at := time.Date(2026, m, 15, 0, 0, 0, 0, time.UTC)
		if got := monthsLeftInYear(at); got < 1 {
			t.Errorf("monthsLeftInYear(%s) = %d, want >= 1", m, got)
		}
	}
	if got := monthsLeftInYear(time.Date(2026, time.December, 31, 0, 0, 0, 0, time.UTC)); got != 1 {
		t.Errorf("December = %d, want 1", got)
	}
}

// TestMoneyUsesThousandsSeparators guards the regression moneyfmt exists to
// prevent: the advisor quotes four-figure amounts constantly and hands every one
// of them to the model verbatim.
func TestMoneyUsesThousandsSeparators(t *testing.T) {
	if got := money(dec("1234.5")); got != "$1,234.50" {
		t.Errorf("money(1234.5) = %q, want %q", got, "$1,234.50")
	}
}
