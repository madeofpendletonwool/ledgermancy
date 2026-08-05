package payroll

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

// simpleStub is a stub with one deferral line, for the roll-up arithmetic.
func simpleStub(payDate, gross, net, deferral string, ytdDeferral *string) DatedStub {
	line := Line{Category: Cat401kPreTax, Label: "401(k)", Amount: money(deferral), PreTax: true}
	if ytdDeferral != nil {
		line.YTDAmount = nullMoney(*ytdDeferral)
	}
	return DatedStub{
		Stub:    Stub{Gross: money(gross), Net: money(net), Lines: []Line{line}},
		PayDate: day(payDate),
	}
}

// TestMidYearEmployerChangePoolsOneLimit is the case doc 23 singles out as
// "real and commonly mishandled": two employers in one year, two separate sets
// of YTD figures that each restart at zero, and ONE shared elective deferral
// limit across both.
//
// Applying the cap per employer would report roughly twice the headroom that
// exists and invite an excess deferral, which has to be withdrawn before the
// filing deadline or it is taxed twice.
func TestMidYearEmployerChangePoolsOneLimit(t *testing.T) {
	ytdOld := "9000.00"
	ytdNew := "6000.00"

	year := Year{
		TaxYear: 2026,
		Employers: []EmployerYear{
			{
				Key: "acme", Name: "Acme", Frequency: Biweekly,
				Stubs: []DatedStub{simpleStub("2026-06-12", "5000.00", "3500.00", "500.00", &ytdOld)},
			},
			{
				Key: "globex", Name: "Globex", Frequency: Biweekly,
				Stubs: []DatedStub{simpleStub("2026-11-20", "6000.00", "4200.00", "600.00", &ytdNew)},
			},
		},
	}

	// 9,000 at the old job + 6,000 at the new one.
	if want := money("15000.00"); !year.GroupTotal("401k").Equal(want) {
		t.Fatalf("pooled 401(k) total = %s, want %s", year.GroupTotal("401k"), want)
	}

	headroom, ok := year.ContributionHeadroom(35, false)
	if !ok {
		t.Fatal("2026 limits should be configured")
	}

	var got *Headroom
	for i := range headroom {
		if headroom[i].Group == "401k" {
			got = &headroom[i]
		}
	}
	if got == nil {
		t.Fatal("no 401(k) headroom returned")
	}
	// 2026 elective deferral limit is 24,500; no catch-up at 35.
	if want := money("24500.00"); !got.Limit.Equal(want) {
		t.Errorf("limit = %s, want %s", got.Limit, want)
	}
	if want := money("9500.00"); !got.Remaining.Equal(want) {
		t.Errorf("remaining = %s, want %s (the limit is shared across both employers)", got.Remaining, want)
	}
	if !got.OverBy.IsZero() {
		t.Errorf("over by = %s, want 0", got.OverBy)
	}
}

// TestOverContributionIsSurfacedNotClamped: an excess deferral must be visible,
// because it has to be withdrawn before the filing deadline or it is taxed
// twice. Clamping it to "0 remaining" would hide the single most actionable
// thing this calculation can say.
func TestOverContributionIsSurfacedNotClamped(t *testing.T) {
	ytd := "26000.00"
	year := Year{
		TaxYear: 2026,
		Employers: []EmployerYear{{
			Key: "acme", Name: "Acme", Frequency: Biweekly,
			Stubs: []DatedStub{simpleStub("2026-12-18", "5000.00", "3500.00", "1000.00", &ytd)},
		}},
	}

	headroom, ok := year.ContributionHeadroom(35, false)
	if !ok {
		t.Fatal("2026 limits should be configured")
	}
	for _, h := range headroom {
		if h.Group != "401k" {
			continue
		}
		if !h.Remaining.IsZero() {
			t.Errorf("remaining = %s, want 0", h.Remaining)
		}
		if want := money("1500.00"); !h.OverBy.Equal(want) {
			t.Errorf("over by = %s, want %s", h.OverBy, want)
		}
		return
	}
	t.Fatal("no 401(k) headroom returned")
}

// TestUnconfiguredLimitYearIsReported mirrors networth/limits.go's rule: a year
// the app does not have limits for is a state to surface, never a nearest-year
// substitution.
func TestUnconfiguredLimitYearIsReported(t *testing.T) {
	year := Year{TaxYear: 2099}
	if _, ok := year.ContributionHeadroom(35, false); ok {
		t.Error("an unconfigured tax year must report ok=false, not fall back to another year")
	}
}

func TestCatchUpAppliesFromFifty(t *testing.T) {
	year := Year{TaxYear: 2026}

	base, _ := year.ContributionHeadroom(35, false)
	catchup, _ := year.ContributionHeadroom(52, false)

	limitFor := func(hs []Headroom, group string) decimal.Decimal {
		for _, h := range hs {
			if h.Group == group {
				return h.Limit
			}
		}
		return decimal.Zero
	}
	// 24,500 + 8,000 for 2026.
	if want := money("32500.00"); !limitFor(catchup, "401k").Equal(want) {
		t.Errorf("catch-up limit = %s, want %s", limitFor(catchup, "401k"), want)
	}
	if want := money("24500.00"); !limitFor(base, "401k").Equal(want) {
		t.Errorf("base limit = %s, want %s", limitFor(base, "401k"), want)
	}
}

// TestReconcilePrefersTheLargerFigure pins the rule in reconcile(): a partial
// history must not under-report a contribution total, because understating it
// reports headroom that is not there.
func TestReconcilePrefersTheLargerFigure(t *testing.T) {
	ytd := "4000.00"
	e := EmployerYear{
		Key: "acme", Name: "Acme", Frequency: Biweekly,
		Stubs: []DatedStub{
			// Only two stubs entered, but the latest one's YTD knows better.
			simpleStub("2026-06-12", "5000.00", "3500.00", "500.00", nil),
			simpleStub("2026-06-26", "5000.00", "3500.00", "500.00", &ytd),
		},
	}
	if want := money("4000.00"); !e.CategoryTotal(Cat401kPreTax).Equal(want) {
		t.Errorf("category total = %s, want %s (the printed YTD beats a partial sum)",
			e.CategoryTotal(Cat401kPreTax), want)
	}

	// And the other way: a sum that has overtaken a stale YTD wins.
	e.Stubs = append(e.Stubs, simpleStub("2026-07-10", "5000.00", "3500.00", "3500.00", nil))
	if want := money("4500.00"); !e.CategoryTotal(Cat401kPreTax).Equal(want) {
		t.Errorf("category total = %s, want %s (the sum beats a stale YTD)",
			e.CategoryTotal(Cat401kPreTax), want)
	}
}

func TestRemainingPayPeriods(t *testing.T) {
	cases := []struct {
		name string
		last string
		freq PayFrequency
		want int
	}{
		// 2026-07-03 to 2026-12-31 is 181 days: 25 whole weeks, 12 fortnights.
		{"weekly mid-year", "2026-07-03", Weekly, 25},
		{"biweekly mid-year", "2026-07-03", Biweekly, 12},
		// October leaves November and December.
		{"monthly", "2026-10-15", Monthly, 2},
		// Paid on the 15th of December: only the month end is left.
		{"semimonthly december", "2026-12-15", Semimonthly, 1},
		// Paid at the end of November: the 15th and the end of December.
		{"semimonthly november", "2026-11-30", Semimonthly, 2},
		// Nothing left after the last day of the year.
		{"year end", "2026-12-31", Biweekly, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := RemainingPayPeriods(day(c.last), c.freq, 2026)
			if !ok {
				t.Fatalf("frequency %q should be known", c.freq)
			}
			if got != c.want {
				t.Errorf("remaining = %d, want %d", got, c.want)
			}
		})
	}

	if _, ok := RemainingPayPeriods(day("2026-07-03"), PayFrequency("daily"), 2026); ok {
		t.Error("an unknown frequency must report ok=false")
	}
}

// TestW2BoxesAgainstHandComputedYear works the whole annual mapping out on
// paper first. One employer, two identical biweekly stubs.
//
//	Gross              2 × 3,000.00 = 6,000.00
//	Federal tax        2 ×   330.00 =   660.00
//	Social Security    2 ×   179.80 =   359.60
//	Medicare           2 ×    42.05 =    84.10
//	401(k) pre-tax     2 ×   300.00 =   600.00
//	Health premium     2 ×   100.00 =   200.00
//	Employer match     2 ×   150.00 =   300.00
//
//	Box 1 = 6,000 − 600 − 200 = 5,200
//	Box 3 = Box 5 = 6,000 − 200 = 5,800   (the 401(k) does not reduce FICA)
//	Box 12 D = 600
func TestW2BoxesAgainstHandComputedYear(t *testing.T) {
	stub := func(payDate string) DatedStub {
		s := typicalStub()
		return DatedStub{Stub: s, PayDate: day(payDate)}
	}
	e := EmployerYear{
		Key: "acme", Name: "Acme", Frequency: Biweekly,
		Stubs: []DatedStub{stub("2026-01-16"), stub("2026-01-30")},
	}

	summary := e.W2(2026)
	boxes := map[string]decimal.Decimal{}
	for _, b := range summary.Boxes {
		key := b.Box
		if b.Code != "" {
			key = b.Box + "-" + b.Code
		}
		boxes[key] = b.Amount
	}

	want := map[string]string{
		"1":    "5200.00",
		"2":    "660.00",
		"3":    "5800.00",
		"4":    "359.60",
		"5":    "5800.00",
		"6":    "84.10",
		"12-D": "600.00",
		"17":   "240.00",
	}
	for box, expected := range want {
		got, ok := boxes[box]
		if !ok {
			t.Errorf("box %s missing from the summary", box)
			continue
		}
		if !got.Equal(money(expected)) {
			t.Errorf("box %s = %s, want %s", box, got, expected)
		}
	}

	// Box 19 has nothing in it and should not be printed; box 1 always is.
	if _, ok := boxes["19"]; ok {
		t.Error("box 19 is zero and should have been dropped")
	}
	if summary.Disclaimer == "" {
		t.Error("the disclaimer must travel with the data, not only with the UI")
	}
}

// TestW2MedicareSurtaxJoinsBox6 — a real W-2 reports the additional Medicare
// surtax under box 6 with ordinary Medicare rather than in a box of its own.
func TestW2MedicareSurtaxJoinsBox6(t *testing.T) {
	e := EmployerYear{
		Key: "acme", Name: "Acme", Frequency: Biweekly,
		Stubs: []DatedStub{{
			PayDate: day("2026-06-12"),
			Stub: Stub{
				Gross: money("10000.00"), Net: money("9000.00"),
				Lines: []Line{
					{Category: CatMedicare, Amount: money("145.00")},
					{Category: CatMedicareSurtax, Amount: money("90.00")},
					{Category: CatFederalIncomeTax, Amount: money("765.00")},
				},
			},
		}},
	}

	for _, b := range e.W2(2026).Boxes {
		if b.Box == "6" && b.Code == "" {
			if want := money("235.00"); !b.Amount.Equal(want) {
				t.Errorf("box 6 = %s, want %s", b.Amount, want)
			}
			return
		}
	}
	t.Fatal("box 6 missing")
}

func TestYearTotalsAcrossEmployers(t *testing.T) {
	stub := func(payDate string) DatedStub {
		return DatedStub{Stub: typicalStub(), PayDate: day(payDate)}
	}
	year := Year{
		TaxYear: 2026,
		Employers: []EmployerYear{
			{Key: "acme", Name: "Acme", Frequency: Biweekly, Stubs: []DatedStub{stub("2026-02-13")}},
			{Key: "globex", Name: "Globex", Frequency: Monthly, Stubs: []DatedStub{stub("2026-09-30")}},
		},
	}

	if want := money("6000.00"); !year.Gross().Equal(want) {
		t.Errorf("gross = %s, want %s", year.Gross(), want)
	}
	if want := money("1343.70"); !year.TaxTotal().Equal(want) {
		t.Errorf("tax total = %s, want %s", year.TaxTotal(), want)
	}
	if want := money("300.00"); !year.EmployerTotal().Equal(want) {
		t.Errorf("employer total = %s, want %s", year.EmployerTotal(), want)
	}
	if want := money("6300.00"); !year.TotalCompensation().Equal(want) {
		t.Errorf("total compensation = %s, want %s", year.TotalCompensation(), want)
	}
}
