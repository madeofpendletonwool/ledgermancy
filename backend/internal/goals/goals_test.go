package goals

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestCompute(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	dec := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC) // ~11-12 whole months out

	t.Run("comfortably funded is on track", func(t *testing.T) {
		// $10k target, $2k saved, surplus $2k/mo, ~11 months → required ~$727/mo.
		f := Compute(d("10000"), d("2000"), d("2000"), &dec, now)
		if !f.OnTrack {
			t.Errorf("expected on track, required=%s surplus=2000", f.RequiredMonthly)
		}
		if !f.Shortfall.IsZero() {
			t.Errorf("expected zero shortfall, got %s", f.Shortfall)
		}
		if f.RequiredMonthly.IsNegative() || f.RequiredMonthly.IsZero() {
			t.Errorf("expected a positive required monthly, got %s", f.RequiredMonthly)
		}
	})

	t.Run("aggressive goal has a positive shortfall", func(t *testing.T) {
		// $10k target, nothing saved, tiny $50/mo surplus → far behind.
		f := Compute(d("10000"), decimal.Zero, d("50"), &dec, now)
		if f.OnTrack {
			t.Errorf("expected behind, required=%s", f.RequiredMonthly)
		}
		if !f.Shortfall.IsPositive() {
			t.Errorf("expected positive shortfall, got %s", f.Shortfall)
		}
		// required = 10000/11 ≈ 909.09; shortfall = 909.09 - 50 = 859.09
		if !f.Shortfall.Equal(f.RequiredMonthly.Sub(d("50"))) {
			t.Errorf("shortfall %s != required %s - surplus 50", f.Shortfall, f.RequiredMonthly)
		}
	})

	t.Run("open-ended goal has no required monthly and never divides by zero", func(t *testing.T) {
		f := Compute(d("10000"), d("1000"), d("100"), nil, now)
		if !f.OpenEnded {
			t.Error("expected open-ended")
		}
		if !f.OnTrack {
			t.Error("open-ended goals are never 'behind'")
		}
		if !f.RequiredMonthly.IsZero() {
			t.Errorf("expected zero required monthly, got %s", f.RequiredMonthly)
		}
	})

	t.Run("achieved goal is marked and on track", func(t *testing.T) {
		f := Compute(d("10000"), d("10500"), decimal.Zero, &dec, now)
		if !f.Achieved {
			t.Error("expected achieved")
		}
		if !f.OnTrack || f.Shortfall.IsPositive() {
			t.Errorf("an achieved goal is on track with no shortfall, got onTrack=%v shortfall=%s", f.OnTrack, f.Shortfall)
		}
		if !f.Remaining.IsZero() {
			t.Errorf("remaining should floor at zero, got %s", f.Remaining)
		}
	})

	t.Run("past date floors months at 1 rather than dividing by zero", func(t *testing.T) {
		past := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
		f := Compute(d("1000"), decimal.Zero, decimal.Zero, &past, now)
		if f.MonthsLeft != 1 {
			t.Errorf("expected months floored to 1, got %d", f.MonthsLeft)
		}
		if !f.RequiredMonthly.Equal(d("1000")) {
			t.Errorf("required should be the whole remaining over 1 month, got %s", f.RequiredMonthly)
		}
	})
}
