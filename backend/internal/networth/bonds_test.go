package networth

import (
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// Bond valuation is tested against figures produced by Treasury's OWN public
// savings bond calculator (https://www.treasurydirect.gov/BC/SBCPrice), quoted
// in each case below. That is the point of the exercise: a bond's value is not
// an estimate this app is entitled to be approximately right about, so the
// fixtures are external truth rather than a recording of what the code happened
// to output.
//
// The rate table under test is the one the migration actually seeds, parsed out
// of the SQL. A typo in a seeded rate is a wrong number in a load-bearing line,
// and it should fail here rather than in somebody's net worth.

var seedRowRe = regexp.MustCompile(
	`\('(i_savings|ee_savings)',\s*'(\d{4})-(\d{2})-\d{2}',\s*(-?[\d.]+),\s*(NULL|-?[\d.]+),\s*'([^']*)'\)`)

// seededRates parses savings_bond_rates out of the migration that seeds it.
func seededRates(t *testing.T) ([]BondRate, *BondRates) {
	t.Helper()

	raw, err := os.ReadFile("../db/migrations/00051_asset_revaluation.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	matches := seedRowRe.FindAllStringSubmatch(string(raw), -1)
	if len(matches) == 0 {
		t.Fatal("no savings_bond_rates rows found in the migration")
	}

	rows := make([]BondRate, 0, len(matches))
	for _, m := range matches {
		year, month := atoi(t, m[2]), atoi(t, m[3])
		row := BondRate{
			Series:    m[1],
			Start:     time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC),
			Fixed:     decimal.RequireFromString(m[4]),
			SourceURL: m[6],
		}
		if m[5] != "NULL" {
			row.Inflation = decimal.NullDecimal{
				Decimal: decimal.RequireFromString(m[5]), Valid: true,
			}
		}
		rows = append(rows, row)
	}
	return rows, NewBondRates(rows)
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return int(d.IntPart())
}

func month(y int, m time.Month) time.Time {
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}

func money(t *testing.T, label string, got decimal.Decimal, want string) {
	t.Helper()
	if got.StringFixed(2) != want {
		t.Errorf("%s = %s, want %s", label, got.StringFixed(2), want)
	}
}

// --------------------------------------------------------------------------
// The seed itself
// --------------------------------------------------------------------------

// Provenance is the whole justification for shipping this table at all: doc 15
// refused to bundle a historical return series because it would be "a
// transcribed table of numbers nobody can verify". A row without its source URL
// is exactly that.
func TestSeededRatesCarryProvenance(t *testing.T) {
	rows, _ := seededRates(t)

	var i, ee int
	for _, r := range rows {
		if r.SourceURL == "" {
			t.Errorf("%s %s has no source_url", r.Series, r.Start.Format("2006-01"))
		}
		switch r.Series {
		case SeriesISavings:
			i++
			if !r.Inflation.Valid {
				t.Errorf("I bond period %s has no inflation rate", r.Start.Format("2006-01"))
			}
		case SeriesEESavings:
			ee++
			if r.Inflation.Valid {
				t.Errorf("EE period %s carries an inflation rate; EE bonds have none",
					r.Start.Format("2006-01"))
			}
		}
	}
	if i == 0 || ee == 0 {
		t.Fatalf("expected both series seeded, got %d I and %d EE rows", i, ee)
	}
}

// A bond's semiannual periods run from its own issue anniversary, so lookup has
// to be "the greatest period_start <= this date" rather than arithmetic on the
// May/November grid. An August bond takes the May announcement.
func TestRateLookupIsGreatestPeriodStartAtOrBefore(t *testing.T) {
	_, rates := seededRates(t)

	for _, tc := range []struct {
		on   time.Time
		want time.Time
	}{
		{month(2021, time.August), month(2021, time.May)},
		{month(2021, time.October), month(2021, time.May)},
		{month(2021, time.November), month(2021, time.November)},
		{month(2026, time.April), month(2025, time.November)},
		// I bonds launched off-grid, in September 1998.
		{month(1998, time.October), month(1998, time.September)},
	} {
		got, ok := rates.At(SeriesISavings, tc.on)
		if !ok {
			t.Fatalf("no rate for %s", tc.on.Format("2006-01"))
		}
		if !got.Start.Equal(tc.want) {
			t.Errorf("rate for %s came from %s, want %s",
				tc.on.Format("2006-01"), got.Start.Format("2006-01"), tc.want.Format("2006-01"))
		}
	}
}

// --------------------------------------------------------------------------
// Series I
// --------------------------------------------------------------------------

// A $1,000 Series I bond issued October 2021, valued August 2026.
//
// Treasury's calculator reports: interest rate 3.12%, interest earned $232.00,
// total value $1,232.00, flagged "P5 — includes 3 month interest penalty".
//
// Two things are being pinned here at once. The redemption figure is what
// reaches net worth, and it is three months of interest BELOW the accrued
// value because the bond is not yet five years old. And the 3.12% confirms the
// rate plumbing end to end: the fixed rate comes from the issue period (May
// 2021, 0.00%) while the inflation rate comes from the period the bond is
// currently in (April 2026, which takes the November 2025 announcement of
// 1.56%) — 0.00 + 2×1.56 = 3.12.
func TestIBondUnderFiveYearsForfeitsThreeMonths(t *testing.T) {
	_, rates := seededRates(t)

	v := ValueBond(Bond{
		Series:        SeriesISavings,
		IssueDate:     month(2021, time.October),
		PurchasePrice: decimal.NewFromInt(1000),
		FaceValue:     decimal.NewFromInt(1000),
	}, rates, month(2026, time.August))

	if !v.OK {
		t.Fatalf("refused: %s", v.Reason)
	}
	if !v.PenaltyApplied {
		t.Error("a bond held under five years should carry the three-month penalty")
	}
	money(t, "redemption", v.Redemption, "1232.00")
	money(t, "accrued", v.Accrued, "1241.60")

	if v.Redemption.GreaterThanOrEqual(v.Accrued) {
		t.Error("redemption value must sit below accrued value inside the first five years")
	}

	last := v.Rates[len(v.Rates)-1]
	money(t, "current composite rate", last.Composite, "3.12")
}

// Past five years the penalty stops applying and the two figures converge.
// Same bond, valued after its fifth anniversary (October 2026).
func TestIBondPastFiveYearsHasNoPenalty(t *testing.T) {
	_, rates := seededRates(t)

	v := ValueBond(Bond{
		Series:        SeriesISavings,
		IssueDate:     month(2021, time.October),
		PurchasePrice: decimal.NewFromInt(1000),
		FaceValue:     decimal.NewFromInt(1000),
	}, rates, month(2026, time.October))

	if !v.OK {
		t.Fatalf("refused: %s", v.Reason)
	}
	if v.PenaltyApplied {
		t.Error("a bond held five years should not carry the penalty")
	}
	if !v.Redemption.Equal(v.Accrued) {
		t.Errorf("redemption %s and accrued %s should be equal past five years",
			v.Redemption, v.Accrued)
	}
}

// The deflation floor is a property of the instrument, not a safety check: an I
// bond never loses nominal value.
//
// May 2009 was announced at −2.78% semiannual inflation. A bond issued November
// 2008 (fixed 0.70%) has a composite of 0.70 + 2×(−2.78) + (0.70×−2.78/100) =
// −4.88% for that period, which floors to zero. The bond must not go backwards.
func TestIBondFloorsAtZeroThroughDeflation(t *testing.T) {
	_, rates := seededRates(t)

	b := Bond{
		Series:        SeriesISavings,
		IssueDate:     month(2008, time.November),
		PurchasePrice: decimal.NewFromInt(1000),
		FaceValue:     decimal.NewFromInt(1000),
	}

	before := ValueBond(b, rates, month(2009, time.May))
	after := ValueBond(b, rates, month(2009, time.November))
	if !before.OK || !after.OK {
		t.Fatalf("refused: %s / %s", before.Reason, after.Reason)
	}

	if after.Accrued.LessThan(before.Accrued) {
		t.Errorf("accrued value fell from %s to %s across a deflationary period",
			before.Accrued, after.Accrued)
	}

	var floored bool
	for _, r := range after.Rates {
		if r.PeriodStart.Equal(month(2009, time.May)) {
			floored = r.Composite.IsZero()
			if r.Composite.IsNegative() {
				t.Errorf("composite rate for May 2009 is %s; it must floor at zero", r.Composite)
			}
		}
	}
	if !floored {
		t.Error("expected the May 2009 period to floor at a zero composite rate")
	}

	// And the redemption value never drops below what was paid.
	if after.Redemption.LessThan(decimal.NewFromInt(1000)) {
		t.Errorf("redemption %s is below the purchase price", after.Redemption)
	}
}

// --------------------------------------------------------------------------
// Series EE and the doubling guarantee
// --------------------------------------------------------------------------

// A $1,000 paper Series EE bond issued May 2005 — bought for $500, because
// paper EE bonds were sold at half face.
//
// Treasury's calculator reports issue price $500.00 in both cases:
//   - April 2025 (one month short of 20 years): total value $997.20
//   - May 2025 (the 20th anniversary):          total value $1,000.00
//
// This is the pair the guarantee lives or dies on. A model that misses it is
// visibly wrong on the most common EE bond people actually hold.
func TestEEBondAtTheTwentyYearDoubling(t *testing.T) {
	_, rates := seededRates(t)

	// Half face: what a paper EE actually cost.
	b := Bond{
		Series:        SeriesEESavings,
		IssueDate:     month(2005, time.May),
		PurchasePrice: decimal.NewFromInt(500),
		FaceValue:     decimal.NewFromInt(1000),
	}

	just := ValueBond(b, rates, month(2025, time.April))
	if !just.OK {
		t.Fatalf("refused: %s", just.Reason)
	}
	money(t, "one month before the 20th anniversary", just.Redemption, "997.20")

	on := ValueBond(b, rates, month(2025, time.May))
	if !on.OK {
		t.Fatalf("refused: %s", on.Reason)
	}
	money(t, "on the 20th anniversary", on.Redemption, "1000.00")

	// The bond must be worth at least twice what was paid for it.
	if on.Redemption.LessThan(decimal.NewFromInt(1000)) {
		t.Errorf("value %s is below twice the $500 issue price", on.Redemption)
	}
}

// Entering a paper EE at its FACE value rather than what was paid is the single
// most common way a savings bond ends up recorded at twice its cost. The two
// entries must produce visibly different valuations, so the UI's warning has
// something real behind it.
func TestPaperEEEnteredAtFaceValueIsWorthDouble(t *testing.T) {
	_, rates := seededRates(t)

	correct := ValueBond(Bond{
		Series:        SeriesEESavings,
		IssueDate:     month(2005, time.May),
		PurchasePrice: decimal.NewFromInt(500),
		FaceValue:     decimal.NewFromInt(1000),
	}, rates, month(2025, time.April))

	mistaken := ValueBond(Bond{
		Series:        SeriesEESavings,
		IssueDate:     month(2005, time.May),
		PurchasePrice: decimal.NewFromInt(1000),
		FaceValue:     decimal.NewFromInt(1000),
	}, rates, month(2025, time.April))

	if !correct.OK || !mistaken.OK {
		t.Fatalf("refused: %s / %s", correct.Reason, mistaken.Reason)
	}
	money(t, "entered at what it cost", correct.Redemption, "997.20")
	// Almost exactly double, but not to the cent: the per-period rounding
	// happens on the unit, and a $25 unit loses less to rounding than a $12.50
	// one over forty periods. That it is not a clean 2× is the arithmetic
	// working, not a discrepancy.
	money(t, "entered at face", mistaken.Redemption, "1997.20")
}

// The guarantee is a cliff, and on a low-rate bond it is a very tall one. A
// 0.10% EE bond compounds to barely more than it cost over twenty years, then
// nearly doubles overnight. Pure compounding underprices it by a factor of two.
func TestEEDoublingIsACliffNotACurve(t *testing.T) {
	_, rates := seededRates(t)

	// November 2015: fixed rate 0.10%.
	b := Bond{
		Series:        SeriesEESavings,
		IssueDate:     month(2015, time.November),
		PurchasePrice: decimal.NewFromInt(500),
		FaceValue:     decimal.NewFromInt(1000),
	}

	before := ValueBond(b, rates, month(2035, time.October))
	on := ValueBond(b, rates, month(2035, time.November))
	if !before.OK || !on.OK {
		t.Fatalf("refused: %s / %s", before.Reason, on.Reason)
	}

	if before.DoublingApplied {
		t.Error("the doubling must not apply a month early")
	}
	if !on.DoublingApplied {
		t.Error("the doubling must apply on the 20th anniversary")
	}
	money(t, "one month before", before.Redemption, "516.00")
	money(t, "on the anniversary", on.Redemption, "1000.00")
}

// After the doubling the bond keeps earning, and it compounds FROM the doubled
// value. Growing for thirty years and doubling at the end would understate it.
func TestEEKeepsEarningFromTheDoubledValue(t *testing.T) {
	_, rates := seededRates(t)

	b := Bond{
		Series:        SeriesEESavings,
		IssueDate:     month(2015, time.November),
		PurchasePrice: decimal.NewFromInt(500),
		FaceValue:     decimal.NewFromInt(1000),
	}

	on := ValueBond(b, rates, month(2035, time.November))
	later := ValueBond(b, rates, month(2040, time.November))
	if !on.OK || !later.OK {
		t.Fatalf("refused: %s / %s", on.Reason, later.Reason)
	}
	if !later.Redemption.GreaterThan(on.Redemption) {
		t.Errorf("value did not grow past the doubling: %s then %s",
			on.Redemption, later.Redemption)
	}
}

// EE bonds issued before May 2005 earned variable market-based rates under
// guarantee periods that changed several times. Nothing in the table describes
// them, so they are refused rather than run through a fixed-rate model.
func TestPreMay2005EEBondIsRefused(t *testing.T) {
	_, rates := seededRates(t)

	v := ValueBond(Bond{
		Series:        SeriesEESavings,
		IssueDate:     month(2004, time.June),
		PurchasePrice: decimal.NewFromInt(500),
		FaceValue:     decimal.NewFromInt(1000),
	}, rates, month(2026, time.August))

	if v.OK {
		t.Fatal("a pre-May-2005 EE bond must not be valued from the fixed-rate table")
	}
	if v.Reason == "" {
		t.Error("a refusal must say why")
	}
}

// --------------------------------------------------------------------------
// Refusals
// --------------------------------------------------------------------------

// Carrying the last known rate forward silently invents a return. A bond
// spanning a period the table does not cover must refuse, and say how far it
// GOT — a partial answer with its limits stated is useful; a confident wrong
// one is not.
func TestMissingRatePeriodRefusesRatherThanExtrapolating(t *testing.T) {
	rows, _ := seededRates(t)

	// A table that stops at the end of 2023, against a bond that outlives it.
	var truncated []BondRate
	for _, r := range rows {
		if r.Start.Before(month(2024, time.January)) {
			truncated = append(truncated, r)
		}
	}
	rates := NewBondRates(truncated)

	v := ValueBond(Bond{
		Series:        SeriesISavings,
		IssueDate:     month(2021, time.October),
		PurchasePrice: decimal.NewFromInt(1000),
		FaceValue:     decimal.NewFromInt(1000),
	}, rates, month(2026, time.August))

	if v.OK {
		t.Fatal("a bond spanning an unseeded period must not report a complete valuation")
	}
	if v.Reason == "" {
		t.Error("a refusal must say why")
	}
	if !v.ValuedThrough.Before(v.AsOf) {
		t.Errorf("valued through %s should be earlier than the requested %s",
			v.ValuedThrough.Format("2006-01"), v.AsOf.Format("2006-01"))
	}

	// The same bond against the full table values fine, so the refusal is about
	// the missing rate rather than the bond.
	_, full := seededRates(t)
	if complete := ValueBond(Bond{
		Series:        SeriesISavings,
		IssueDate:     month(2021, time.October),
		PurchasePrice: decimal.NewFromInt(1000),
		FaceValue:     decimal.NewFromInt(1000),
	}, full, month(2026, time.August)); !complete.OK {
		t.Fatalf("full table should value this bond: %s", complete.Reason)
	}
}

func TestBondRefusalsAreExplicit(t *testing.T) {
	_, rates := seededRates(t)
	now := month(2026, time.August)

	for _, tc := range []struct {
		name string
		bond Bond
	}{
		{"no issue date", Bond{Series: SeriesISavings, PurchasePrice: decimal.NewFromInt(1000)}},
		{"no purchase price", Bond{Series: SeriesISavings, IssueDate: month(2020, time.January)}},
		{"issued in the future", Bond{
			Series: SeriesISavings, IssueDate: month(2030, time.January),
			PurchasePrice: decimal.NewFromInt(1000),
		}},
		{"unvaluable series", Bond{
			Series: SeriesOther, IssueDate: month(2020, time.January),
			PurchasePrice: decimal.NewFromInt(1000),
		}},
		{"treasury with no maturity", Bond{
			Series: SeriesTreasury, IssueDate: month(2020, time.January),
			PurchasePrice: decimal.NewFromInt(1000),
		}},
	} {
		v := ValueBond(tc.bond, rates, now)
		if v.OK {
			t.Errorf("%s: expected a refusal", tc.name)
		}
		if v.Reason == "" {
			t.Errorf("%s: refusal carries no reason", tc.name)
		}
	}
}

// --------------------------------------------------------------------------
// Marketable Treasuries
// --------------------------------------------------------------------------

// Held at par plus accrued coupon, and the basis says so rather than implying a
// market value.
func TestMarketableTreasuryIsHeldAtPar(t *testing.T) {
	_, rates := seededRates(t)

	maturity := month(2029, time.February)
	v := ValueBond(Bond{
		Series:        SeriesTreasury,
		IssueDate:     month(2024, time.February),
		PurchasePrice: decimal.NewFromInt(9800),
		FaceValue:     decimal.NewFromInt(10000),
		CouponRate:    decimal.NullDecimal{Decimal: decimal.RequireFromString("4.25"), Valid: true},
		MaturityDate:  &maturity,
	}, rates, month(2026, time.April))

	if !v.OK {
		t.Fatalf("refused: %s", v.Reason)
	}
	// Coupons run back in six-month steps from maturity: Feb and Aug. As of
	// April 2026 the last payment was February 2026, so two months have
	// accrued: 10000 × 4.25% × 2/12 = 70.83.
	money(t, "par plus accrued coupon", v.Redemption, "10070.83")

	if v.Basis == "" {
		t.Fatal("a Treasury valuation must state its basis")
	}
	// The user has to be able to tell this is not a market price.
	for _, want := range []string{"par", "market"} {
		if !contains(v.Basis, want) {
			t.Errorf("basis should mention %q; got %q", want, v.Basis)
		}
	}
}

func TestMaturedTreasuryStopsEarning(t *testing.T) {
	_, rates := seededRates(t)

	maturity := month(2025, time.February)
	v := ValueBond(Bond{
		Series:        SeriesTreasury,
		IssueDate:     month(2020, time.February),
		PurchasePrice: decimal.NewFromInt(9800),
		FaceValue:     decimal.NewFromInt(10000),
		CouponRate:    decimal.NullDecimal{Decimal: decimal.RequireFromString("4.25"), Valid: true},
		MaturityDate:  &maturity,
	}, rates, month(2026, time.August))

	if !v.OK {
		t.Fatalf("refused: %s", v.Reason)
	}
	if !v.Matured {
		t.Error("a Treasury past its maturity date should report as matured")
	}
	money(t, "matured Treasury", v.Redemption, "10000.00")
}

// --------------------------------------------------------------------------
// Properties that hold for every bond
// --------------------------------------------------------------------------

// A savings bond stops earning at 30 years. It does not keep compounding in a
// drawer, and the app must not report that it does.
func TestSavingsBondStopsAtFinalMaturity(t *testing.T) {
	_, rates := seededRates(t)

	// A Series EE bond, because it is the one that can be valued this far out:
	// its rate is fixed at issue, so no future announcement is needed. No I
	// bond has reached 30 years yet — the series launched in September 1998 —
	// and valuing one to its maturity would need rates nobody has published.
	b := Bond{
		Series:        SeriesEESavings,
		IssueDate:     month(2005, time.May),
		PurchasePrice: decimal.NewFromInt(500),
		FaceValue:     decimal.NewFromInt(1000),
	}

	at30 := ValueBond(b, rates, month(2035, time.May))
	later := ValueBond(b, rates, month(2042, time.January))
	if !at30.OK || !later.OK {
		t.Fatalf("refused: %s / %s", at30.Reason, later.Reason)
	}
	if !later.Matured {
		t.Error("a bond past 30 years should report as matured")
	}
	if !later.Redemption.Equal(at30.Redemption) {
		t.Errorf("value kept growing past final maturity: %s then %s",
			at30.Redemption, later.Redemption)
	}
}

// Determinism is what licenses writing a bond revaluation automatically, with
// no confirmation prompt. The same bond and the same `now` must give a
// byte-identical answer every time — and `now` is always a parameter, so
// nothing here reads the clock.
func TestBondValuationIsDeterministic(t *testing.T) {
	_, rates := seededRates(t)

	b := Bond{
		Series:        SeriesISavings,
		IssueDate:     month(2019, time.June),
		PurchasePrice: decimal.RequireFromString("5000"),
		FaceValue:     decimal.RequireFromString("5000"),
	}
	now := month(2026, time.August)

	first := ValueBond(b, rates, now)
	if !first.OK {
		t.Fatalf("refused: %s", first.Reason)
	}
	for i := 0; i < 25; i++ {
		again := ValueBond(b, rates, now)
		if again.Redemption.String() != first.Redemption.String() ||
			again.Accrued.String() != first.Accrued.String() {
			t.Fatalf("run %d differed: %s/%s vs %s/%s",
				i, again.Redemption, again.Accrued, first.Redemption, first.Accrued)
		}
	}

	// The value only changes on an accrual date, so any day within a month
	// gives the same answer.
	mid := time.Date(2026, time.August, 27, 13, 45, 0, 0, time.UTC)
	if v := ValueBond(b, rates, mid); v.Redemption.String() != first.Redemption.String() {
		t.Errorf("value changed within the month: %s vs %s", v.Redemption, first.Redemption)
	}
}

// Values scale with the holding: a $1,000 bond is worth ten times a $100 one,
// because the unit is $25 and rounding happens there rather than on the total.
func TestValueScalesWithDenomination(t *testing.T) {
	_, rates := seededRates(t)
	now := month(2026, time.August)

	small := ValueBond(Bond{
		Series: SeriesISavings, IssueDate: month(2021, time.October),
		PurchasePrice: decimal.NewFromInt(100), FaceValue: decimal.NewFromInt(100),
	}, rates, now)
	large := ValueBond(Bond{
		Series: SeriesISavings, IssueDate: month(2021, time.October),
		PurchasePrice: decimal.NewFromInt(1000), FaceValue: decimal.NewFromInt(1000),
	}, rates, now)

	if !small.OK || !large.OK {
		t.Fatalf("refused: %s / %s", small.Reason, large.Reason)
	}
	want := small.Redemption.Mul(decimal.NewFromInt(10))
	if !large.Redemption.Equal(want) {
		t.Errorf("$1,000 bond = %s, want ten times the $100 bond (%s)", large.Redemption, want)
	}
}

// Every valuation has to be walkable back to treasurydirect.gov by hand, which
// means saying which published periods produced it.
func TestValuationReportsTheRatePeriodsItUsed(t *testing.T) {
	_, rates := seededRates(t)

	v := ValueBond(Bond{
		Series: SeriesISavings, IssueDate: month(2021, time.October),
		PurchasePrice: decimal.NewFromInt(1000), FaceValue: decimal.NewFromInt(1000),
	}, rates, month(2026, time.August))

	if !v.OK {
		t.Fatalf("refused: %s", v.Reason)
	}
	// October 2021 to August 2026 is 58 months: nine full six-month periods and
	// a four-month tail.
	if len(v.Rates) != 10 {
		t.Fatalf("expected 10 rate periods, got %d", len(v.Rates))
	}
	if got := v.Rates[len(v.Rates)-1].Months; got != 4 {
		t.Errorf("final period counted %d months, want 4", got)
	}
	for _, r := range v.Rates[:len(v.Rates)-1] {
		if r.Months != 6 {
			t.Errorf("period starting %s counted %d months, want 6",
				r.PeriodStart.Format("2006-01"), r.Months)
		}
	}
	if v.Basis == "" {
		t.Error("a valuation must state its basis")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
