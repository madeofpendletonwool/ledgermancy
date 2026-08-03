package goals

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// TestPayoffScheduleMatchesTotals is the load-bearing invariant for item #10:
// the per-month schedule the chart plots must agree with the headline totals,
// because they are quoted side-by-side on the same card. The recorded interest
// series sums exactly to TotalInterest, the schedule length equals Months, and
// the final balance is zero (never a fractional-cent overshoot).
func TestPayoffScheduleMatchesTotals(t *testing.T) {
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	// $5,000 at 18.99%, $200/month — 33 payments, $1,414.47 of interest. The
	// same fixture payoff_test.go pins the totals against.
	f := ComputePayoff(d("5000"), d("18.99"), d("200"), nil, now)

	if f.NeverPaysOff {
		t.Fatal("expected the debt to amortize")
	}
	if len(f.Schedule) != f.Months {
		t.Fatalf("schedule length = %d, want Months = %d", len(f.Schedule), f.Months)
	}

	// The interest series sums to TotalInterest exactly — not approximately,
	// because both come from the same loop.
	sum := decimal.Zero
	for _, p := range f.Schedule {
		sum = sum.Add(p.Interest)
	}
	if !sum.Equal(f.TotalInterest) {
		t.Errorf("schedule interest sums to %s, want total %s", sum, f.TotalInterest)
	}

	// The schedule starts at month 1 and steps by one.
	for i, p := range f.Schedule {
		if p.Month != i+1 {
			t.Errorf("schedule[%d].month = %d, want %d", i, p.Month, i+1)
		}
	}

	// The balance strictly decreases and the final point is zero — never a
	// negative overshoot, which a lender would not report and a chart would
	// render as a dip below the axis.
	prev := f.Balance
	for _, p := range f.Schedule {
		if p.Balance.GreaterThan(prev) {
			t.Errorf("balance increased from %s to %s — amortization must strictly decrease", prev, p.Balance)
		}
		if p.Balance.IsNegative() {
			t.Errorf("balance %s is negative — the final payment should clamp to zero", p.Balance)
		}
		prev = p.Balance
	}
	if !f.Schedule[len(f.Schedule)-1].Balance.IsZero() {
		t.Errorf("final balance = %s, want exactly 0", f.Schedule[len(f.Schedule)-1].Balance)
	}
}

// TestPayoffScheduleZeroAPR checks the interest-free path: every schedule point
// carries zero interest, the balance steps down by the payment, and the series
// is one payment shorter than the rounded division would suggest (the final
// partial payment covers the remainder).
func TestPayoffScheduleZeroAPR(t *testing.T) {
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	// 5000/300 = 16.67 → 17 payments, no interest at all.
	f := ComputePayoff(d("5000"), decimal.Zero, d("300"), nil, now)
	if f.Months != 17 {
		t.Fatalf("months = %d, want 17", f.Months)
	}
	if len(f.Schedule) != 17 {
		t.Fatalf("schedule length = %d, want 17", len(f.Schedule))
	}

	for i, p := range f.Schedule {
		if !p.Interest.IsZero() {
			t.Errorf("schedule[%d].interest = %s, want 0 at zero APR", i, p.Interest)
		}
	}

	// The first 16 points step down by 300 each; the 17th is the partial
	// remainder that clamps to zero.
	for i := 0; i < 16; i++ {
		want := d("5000").Sub(d("300").Mul(d(string(intToDecimalString(i + 1)))))
		if !f.Schedule[i].Balance.Equal(want) {
			t.Errorf("schedule[%d].balance = %s, want %s", i, f.Schedule[i].Balance, want)
		}
	}
	if !f.Schedule[16].Balance.IsZero() {
		t.Errorf("final balance = %s, want 0", f.Schedule[16].Balance)
	}
}

// TestPayoffScheduleAbsentWhenNeverPaysOff: a debt that never retires carries
// no schedule, so the chart's empty state is the honest rendering rather than a
// curve that contradicts "never".
func TestPayoffScheduleAbsentWhenNeverPaysOff(t *testing.T) {
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	// $75/mo against $5,000 at 18.99% — below the $79.13 the balance accrues.
	f := ComputePayoff(d("5000"), d("18.99"), d("75"), nil, now)
	if !f.NeverPaysOff {
		t.Fatal("expected never-pays-off")
	}
	if f.Schedule != nil {
		t.Errorf("a never-paying debt carries no schedule, got %d points", len(f.Schedule))
	}
}

// TestPayoffScheduleAbsentWhenAlreadyPaid: a cleared debt has nothing to plot.
func TestPayoffScheduleAbsentWhenAlreadyPaid(t *testing.T) {
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	f := ComputePayoff(decimal.Zero, d("18.99"), d("200"), nil, now)
	if !f.Achieved {
		t.Fatal("expected achieved")
	}
	if f.Schedule != nil {
		t.Errorf("an already-cleared debt carries no schedule, got %d points", len(f.Schedule))
	}
}

// TestPayoffScheduleFirstPointInterestMatchesMonthly pins the relationship
// between the schedule's first point and the headline MonthlyInterest: both are
// the interest on the opening balance at one month's rate, rounded to the cent,
// so the chart's first segment and the "interest alone is $X/mo" sentence the
// card renders must agree.
func TestPayoffScheduleFirstPointInterestMatchesMonthly(t *testing.T) {
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	f := ComputePayoff(d("5000"), d("18.99"), d("200"), nil, now)
	if len(f.Schedule) == 0 {
		t.Fatal("expected a schedule")
	}
	if !f.Schedule[0].Interest.Equal(f.MonthlyInterest) {
		t.Errorf("first point interest = %s, MonthlyInterest = %s — must match", f.Schedule[0].Interest, f.MonthlyInterest)
	}
}

// intToDecimalString is a tiny helper so the zero-APR test can build "300 * n"
// expectations without a second decimal parse helper. Kept local because it is
// only used to construct expected values in this file.
func intToDecimalString(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	x := n
	for x > 0 {
		digits = append([]byte{byte('0' + x%10)}, digits...)
		x /= 10
	}
	return string(digits)
}
