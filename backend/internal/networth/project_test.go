package networth

import (
	"testing"

	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestProjectCompoundsAndContributes(t *testing.T) {
	start := Result{
		AssetsTotal:      dec("100000"),
		LiabilitiesTotal: dec("20000"),
		NetWorth:         dec("80000"),
		Breakdown:        Breakdown{Investments: dec("60000"), Cash: dec("40000")},
	}

	points := Project(start, Assumptions{
		MonthlySurplus:    dec("1000"),
		AnnualReturnRate:  dec("0.06"),
		AnnualDebtPaydown: dec("6000"),
		Months:            12,
	})

	if len(points) != 12 {
		t.Fatalf("expected 12 points, got %d", len(points))
	}

	// Month 1: 60000 * (0.06/12) = 300 growth, then +1000 contributed.
	first := points[0]
	if !first.Growth.Equal(dec("300")) {
		t.Errorf("month 1 growth = %s, want 300", first.Growth)
	}
	if !first.Contributed.Equal(dec("1000")) {
		t.Errorf("month 1 contributed = %s, want 1000", first.Contributed)
	}
	// Assets: 40000 cash + (60000 + 300 + 1000) = 101300
	if !first.Assets.Equal(dec("101300")) {
		t.Errorf("month 1 assets = %s, want 101300", first.Assets)
	}
	// Liabilities: 20000 - 500 = 19500
	if !first.Liabilities.Equal(dec("19500")) {
		t.Errorf("month 1 liabilities = %s, want 19500", first.Liabilities)
	}

	last := points[11]
	if !last.Contributed.Equal(dec("12000")) {
		t.Errorf("year contributed = %s, want 12000", last.Contributed)
	}
	// Debt fully retired after 12 * 500 = 6000 against 20000 -> 14000.
	if !last.Liabilities.Equal(dec("14000")) {
		t.Errorf("year-end liabilities = %s, want 14000", last.Liabilities)
	}
	if !last.NetWorth.Equal(last.Assets.Sub(last.Liabilities)) {
		t.Error("net worth must equal assets minus liabilities")
	}
}

// Debt paydown must stop at zero rather than becoming a negative liability,
// which would silently inflate net worth.
func TestProjectDebtFloorsAtZero(t *testing.T) {
	start := Result{
		AssetsTotal:      dec("50000"),
		LiabilitiesTotal: dec("1000"),
		Breakdown:        Breakdown{Investments: dec("50000")},
	}

	points := Project(start, Assumptions{
		AnnualDebtPaydown: dec("12000"), // 1000/month against a 1000 balance
		Months:            6,
	})

	for i, p := range points {
		if p.Liabilities.IsNegative() {
			t.Fatalf("month %d has negative liabilities: %s", i+1, p.Liabilities)
		}
	}
	if !points[5].Liabilities.Equal(decimal.Zero) {
		t.Errorf("liabilities should settle at 0, got %s", points[5].Liabilities)
	}
}

// A household spending more than it earns must project downward, not be
// quietly treated as saving nothing.
func TestProjectHandlesNegativeSurplus(t *testing.T) {
	start := Result{
		AssetsTotal:      dec("10000"),
		LiabilitiesTotal: decimal.Zero,
		Breakdown:        Breakdown{Investments: decimal.Zero, Cash: dec("10000")},
	}

	points := Project(start, Assumptions{MonthlySurplus: dec("-500"), Months: 3})

	if !points[2].Assets.Equal(dec("8500")) {
		t.Errorf("assets after 3 months of -500 = %s, want 8500", points[2].Assets)
	}
	if !points[2].NetWorth.LessThan(start.AssetsTotal) {
		t.Error("expected net worth to fall")
	}
}

func TestProjectZeroMonths(t *testing.T) {
	if got := Project(Result{}, Assumptions{Months: 0}); got != nil {
		t.Errorf("expected nil for zero months, got %d points", len(got))
	}
}
