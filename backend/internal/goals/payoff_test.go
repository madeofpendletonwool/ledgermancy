package goals

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// The fixtures below were computed independently (exact decimal, interest
// rounded half-up to the cent each month) and are asserted exactly, not within
// a tolerance. A schedule that is "about right" is a schedule nobody can check.

func TestComputePayoffAmortizes(t *testing.T) {
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	// $5,000 at 18.99% APR, $200/month.
	f := ComputePayoff(d("5000"), d("18.99"), d("200"), nil, now)

	if f.NeverPaysOff {
		t.Fatal("expected the debt to amortize at $200/mo")
	}
	if f.Months != 33 {
		t.Errorf("months = %d, want 33", f.Months)
	}
	if !f.TotalInterest.Equal(d("1414.47")) {
		t.Errorf("total interest = %s, want 1414.47", f.TotalInterest)
	}
	if !f.MonthlyInterest.Equal(d("79.13")) {
		t.Errorf("monthly interest = %s, want 79.13", f.MonthlyInterest)
	}
	if f.PayoffDate == nil {
		t.Fatal("expected a payoff date")
	}
	if want := now.AddDate(0, 33, 0); !f.PayoffDate.Equal(want) {
		t.Errorf("payoff date = %s, want %s", f.PayoffDate, want)
	}
	if !f.OpenEnded || !f.OnTrack {
		t.Errorf("an amortizing debt with no deadline is open-ended and on track, got open=%v on=%v",
			f.OpenEnded, f.OnTrack)
	}
}

func TestComputePayoffNeverPaysOff(t *testing.T) {
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	// The same $5,000 at 18.99%, now at $75/month — below the $79.13 the balance
	// accrues. This is the single most useful thing the feature can say, so it
	// must be an explicit "never", never a large month count.
	f := ComputePayoff(d("5000"), d("18.99"), d("75"), nil, now)

	if !f.NeverPaysOff {
		t.Fatalf("expected never-pays-off at $75/mo, got %d months", f.Months)
	}
	if f.Months != 0 || !f.TotalInterest.IsZero() || f.PayoffDate != nil {
		t.Errorf("a never-paying debt has no schedule: months=%d interest=%s date=%v",
			f.Months, f.TotalInterest, f.PayoffDate)
	}
	if !f.MonthlyInterest.Equal(d("79.13")) {
		t.Errorf("monthly interest = %s, want 79.13 — the figure that explains 'never'", f.MonthlyInterest)
	}
	if f.OnTrack {
		t.Error("a debt that never pays off is not on track")
	}

	// A payment exactly equal to the interest holds the balance flat forever.
	if flat := ComputePayoff(d("5000"), d("18.99"), d("79.13"), nil, now); !flat.NeverPaysOff {
		t.Errorf("paying exactly the interest never retires the debt, got %d months", flat.Months)
	}
	// So does no payment at all.
	if none := ComputePayoff(d("5000"), d("18.99"), decimal.Zero, nil, now); !none.NeverPaysOff {
		t.Error("a zero payment never retires the debt")
	}
}

// TestComputePayoffAPRIsAPercentage is the guard against the one bug this file
// invites: liabilities.apr stores 18.99 for 18.99%, so the maths must divide by
// 100. Drop the /100 and the monthly rate becomes 1.5825 — $200 no longer even
// covers the interest, and the same debt flips from "33 months" to "never".
func TestComputePayoffAPRIsAPercentage(t *testing.T) {
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	pct := ComputePayoff(d("5000"), d("18.99"), d("200"), nil, now)
	if pct.NeverPaysOff || pct.Months != 33 {
		t.Fatalf("18.99 must be read as 18.99%%: got never=%v months=%d", pct.NeverPaysOff, pct.Months)
	}

	// Passing the same rate already expressed as a fraction is a *different*,
	// far cheaper debt — proof the input is being scaled rather than ignored.
	frac := ComputePayoff(d("5000"), d("0.1899"), d("200"), nil, now)
	if frac.Months >= pct.Months {
		t.Errorf("0.1899%% APR must be cheaper than 18.99%%: %d vs %d months", frac.Months, pct.Months)
	}
}

func TestComputePayoffZeroAPR(t *testing.T) {
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	// Interest-free: 5000/200 = 25 payments exactly, no interest.
	f := ComputePayoff(d("5000"), decimal.Zero, d("200"), nil, now)
	if f.Months != 25 || !f.TotalInterest.IsZero() {
		t.Errorf("zero APR = simple division: months=%d interest=%s, want 25 and 0", f.Months, f.TotalInterest)
	}
	// A remainder rounds up to a final partial payment: 5000/300 = 16.67 → 17.
	if part := ComputePayoff(d("5000"), decimal.Zero, d("300"), nil, now); part.Months != 17 {
		t.Errorf("months = %d, want 17", part.Months)
	}
	// A missing APR arrives as zero and must not be read as a discount.
	if neg := ComputePayoff(d("5000"), d("-3"), d("200"), nil, now); neg.Months != 25 {
		t.Errorf("a negative APR is treated as interest-free: months = %d, want 25", neg.Months)
	}
}

func TestComputePayoffRequiredPayment(t *testing.T) {
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	target := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC) // 12 whole months out

	f := ComputePayoff(d("5000"), d("18.99"), d("200"), &target, now)

	if f.MonthsLeft != 12 {
		t.Fatalf("months left = %d, want 12", f.MonthsLeft)
	}
	if !f.TargetReachable {
		t.Fatal("a 12-month deadline on a $5,000 card is reachable")
	}
	if !f.RequiredMonthly.Equal(d("460.76")) {
		t.Errorf("required monthly = %s, want 460.76", f.RequiredMonthly)
	}
	// $200/mo takes 33 months, so this goal is behind.
	if f.OnTrack {
		t.Error("$200/mo cannot clear the card in 12 months")
	}

	// The bisection result is the *smallest* payment that works: feeding it back
	// hits the deadline, and one cent less does not.
	if m, _, ok := simulatePayoff(d("5000"), monthlyRate(d("18.99")), f.RequiredMonthly); !ok || m != 12 {
		t.Errorf("required payment %s clears in %d months (ok=%v), want 12", f.RequiredMonthly, m, ok)
	}
	short := f.RequiredMonthly.Sub(d("0.01"))
	if m, _, ok := simulatePayoff(d("5000"), monthlyRate(d("18.99")), short); !ok || m <= 12 {
		t.Errorf("one cent less should miss the deadline, got %d months (ok=%v)", m, ok)
	}

	// Paying the required amount puts the goal on track.
	if on := ComputePayoff(d("5000"), d("18.99"), d("460.76"), &target, now); !on.OnTrack {
		t.Errorf("paying the required %s should be on track (%d months)", on.RequiredMonthly, on.Months)
	}
}

// A deadline already past floors to one month, which is a real answer — clear
// the whole balance plus a month's interest — rather than a division by zero.
func TestComputePayoffPastTargetDate(t *testing.T) {
	now := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	past := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	f := ComputePayoff(d("5000"), d("18.99"), d("200"), &past, now)
	if f.MonthsLeft != 1 {
		t.Errorf("months left = %d, want 1", f.MonthsLeft)
	}
	if !f.TargetReachable {
		t.Fatal("clearing the balance outright always reaches a one-month deadline")
	}
	if !f.RequiredMonthly.Equal(d("5079.13")) {
		t.Errorf("required monthly = %s, want 5079.13 (balance + one month's interest)", f.RequiredMonthly)
	}
}

func TestComputePayoffAlreadyPaid(t *testing.T) {
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	target := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)

	f := ComputePayoff(decimal.Zero, d("18.99"), d("200"), &target, now)
	if !f.Achieved || !f.OnTrack {
		t.Errorf("a cleared debt is achieved and on track, got achieved=%v on=%v", f.Achieved, f.OnTrack)
	}
	if f.NeverPaysOff || f.PayoffDate != nil || !f.RequiredMonthly.IsZero() {
		t.Errorf("a cleared debt has no schedule left: never=%v date=%v required=%s",
			f.NeverPaysOff, f.PayoffDate, f.RequiredMonthly)
	}
}

// A payment that amortizes only nominally is reported as "never" rather than as
// a month count past the 100-year horizon.
func TestComputePayoffBeyondHorizon(t *testing.T) {
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	// $500,000 at 5% accrues $2,083.33/month; a cent more than that technically
	// amortizes, in millions of months.
	f := ComputePayoff(d("500000"), d("5"), d("2083.34"), nil, now)
	if !f.NeverPaysOff {
		t.Errorf("expected never-pays-off past the horizon, got %d months", f.Months)
	}
}
