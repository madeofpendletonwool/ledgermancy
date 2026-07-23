// Package goals holds the deterministic feasibility maths behind savings goals:
// how much must be set aside each month to hit a target by a date, and whether
// the household's trailing surplus is keeping up. Every figure is shopspring/
// decimal — the model that phrases coaching never divides a target by months.
// This mirrors networth/project.go's ethos: the arithmetic is a few lines a
// human can check.
package goals

import (
	"time"

	"github.com/shopspring/decimal"
)

// Feasibility is the computed standing of one goal at a moment in time. Money is
// exact decimal; the API renders each as StringFixed(2).
type Feasibility struct {
	// Remaining is target minus current progress, floored at zero.
	Remaining decimal.Decimal
	// MonthsLeft is whole months from now to the target date, floored at 1. Zero
	// means the goal is open-ended (no target date) — RequiredMonthly is then
	// zero and OnTrack is true, since there is no deadline to miss.
	MonthsLeft int
	// OpenEnded is true when the goal has no target date.
	OpenEnded bool
	// RequiredMonthly is Remaining / MonthsLeft (zero when achieved or open-ended).
	RequiredMonthly decimal.Decimal
	// OnTrack reports whether the trailing monthly surplus covers RequiredMonthly.
	OnTrack bool
	// Shortfall is max(0, RequiredMonthly - monthlySurplus): how much more per
	// month is needed to get back on track.
	Shortfall decimal.Decimal
	// Achieved is true once current progress has reached the target.
	Achieved bool
}

// Compute derives a goal's feasibility. targetDate nil → open-ended.
//
//   - remaining        = max(0, target - current)
//   - monthsLeft       = whole months now → targetDate, floored at 1
//   - requiredMonthly  = remaining / monthsLeft
//   - onTrack          = monthlySurplus >= requiredMonthly
//   - shortfall        = max(0, requiredMonthly - monthlySurplus)
func Compute(target, current, monthlySurplus decimal.Decimal, targetDate *time.Time, now time.Time) Feasibility {
	f := Feasibility{}

	remaining := target.Sub(current)
	if remaining.IsNegative() {
		remaining = decimal.Zero
	}
	f.Remaining = remaining
	f.Achieved = !current.LessThan(target)

	if targetDate == nil {
		// Open-ended: no deadline, so nothing is "required" and it can't be behind.
		f.OpenEnded = true
		f.OnTrack = true
		return f
	}

	f.MonthsLeft = wholeMonthsBetween(now, *targetDate)

	if f.Achieved {
		f.OnTrack = true
		return f
	}

	f.RequiredMonthly = remaining.Div(decimal.NewFromInt(int64(f.MonthsLeft))).Round(2)
	f.OnTrack = monthlySurplus.GreaterThanOrEqual(f.RequiredMonthly)

	shortfall := f.RequiredMonthly.Sub(monthlySurplus)
	if shortfall.IsNegative() {
		shortfall = decimal.Zero
	}
	f.Shortfall = shortfall.Round(2)
	return f
}

// wholeMonthsBetween counts whole calendar months from now to target, floored at
// 1 so a past-or-imminent date never divides by zero (and never inflates the
// required contribution by pretending there is no time at all).
func wholeMonthsBetween(now, target time.Time) int {
	months := (target.Year()-now.Year())*12 + int(target.Month()) - int(now.Month())
	// If the target day-of-month hasn't been reached yet this month, the final
	// partial month still counts as time available, so only trim when the target
	// day is strictly earlier than today's day.
	if target.Day() < now.Day() {
		months--
	}
	if months < 1 {
		return 1
	}
	return months
}
