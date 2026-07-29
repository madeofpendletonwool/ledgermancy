package networth

import (
	"testing"
	"time"
)

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func TestAgeOnBirthdayBoundary(t *testing.T) {
	born := date(1990, time.June, 15)

	cases := []struct {
		name string
		now  time.Time
		want int
	}{
		{"day before the birthday", date(2026, time.June, 14), 35},
		{"the birthday itself", date(2026, time.June, 15), 36},
		{"day after the birthday", date(2026, time.June, 16), 36},
		{"earlier month", date(2026, time.January, 1), 35},
		{"later month", date(2026, time.December, 31), 36},
		{"the day they were born", date(1990, time.June, 15), 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AgeOn(born, tc.now); got != tc.want {
				t.Errorf("AgeOn(%s) = %d, want %d", tc.now.Format(time.DateOnly), got, tc.want)
			}
		})
	}
}

// TestAgeOnLeapDay: someone born on 29 February. Comparing month/day rather
// than year-day is what makes this come out right.
func TestAgeOnLeapDay(t *testing.T) {
	born := date(2000, time.February, 29)

	cases := []struct {
		now  time.Time
		want int
	}{
		{date(2025, time.February, 28), 24}, // not yet, in a non-leap year
		{date(2025, time.March, 1), 25},     // counted from 1 March
		{date(2024, time.February, 29), 24}, // the actual birthday, in a leap year
	}

	for _, tc := range cases {
		if got := AgeOn(born, tc.now); got != tc.want {
			t.Errorf("AgeOn(%s) = %d, want %d", tc.now.Format(time.DateOnly), got, tc.want)
		}
	}
}

// TestResolveAgeOrder is the rule the whole birthdate change rests on.
func TestResolveAgeOrder(t *testing.T) {
	now := date(2026, time.July, 1)
	born := date(1990, time.June, 15)

	t.Run("birthdate wins over a stored age", func(t *testing.T) {
		// The stored 30 is what somebody typed six years ago. The birthdate is
		// the truth, and this is the drift the feature exists to stop.
		got, ok := ResolveAge(&born, 30, now)
		if !ok {
			t.Fatal("expected ok")
		}
		if got != 36 {
			t.Errorf("age = %d, want 36 (from the birthdate, not the stored 30)", got)
		}
	})

	t.Run("stored age is used when there is no birthdate", func(t *testing.T) {
		got, ok := ResolveAge(nil, 42, now)
		if !ok {
			t.Fatal("expected ok")
		}
		if got != 42 {
			t.Errorf("age = %d, want the stored 42", got)
		}
	})

	t.Run("neither reports not-known rather than zero", func(t *testing.T) {
		// The important half. Returning (0, true) would silently make a
		// 40-year-old eligible for no catch-up and a 65-year-old eligible for
		// everything, with no signal that the input was missing.
		got, ok := ResolveAge(nil, 0, now)
		if ok {
			t.Errorf("expected ok=false with no birthdate and no stored age, got age %d", got)
		}
	})

	t.Run("a future birthdate falls back rather than going negative", func(t *testing.T) {
		future := date(2030, time.January, 1)
		got, ok := ResolveAge(&future, 50, now)
		if !ok || got != 50 {
			t.Errorf("ResolveAge(future, 50) = (%d, %v), want (50, true)", got, ok)
		}
	})
}

// TestResolveAgeIsUpgradeSafe is the guarantee for existing installs.
//
// The migration backfills every current user with a person row whose birthdate
// is NULL. If that fell through to anything other than the stored age, every
// upgraded instance would see its projections move on the day it upgraded —
// with no user action and no explanation.
func TestResolveAgeIsUpgradeSafe(t *testing.T) {
	now := date(2026, time.July, 1)

	for _, stored := range []int{1, 30, 45, 67, 120} {
		got, ok := ResolveAge(nil, stored, now)
		if !ok || got != stored {
			t.Errorf("upgraded instance with stored age %d resolved to (%d, %v); "+
				"a NULL birthdate must change nothing", stored, got, ok)
		}
	}
}

// TestCustodialTreatmentsExcludedFromNestEgg: money held for a dependent is not
// the household's retirement savings.
func TestCustodialTreatmentsExcludedFromNestEgg(t *testing.T) {
	custodial := []string{"529", "utma_ugma", "coverdell", "custodial_roth", "trump"}
	retirement := []string{"taxable", "trad_401k", "roth_401k", "trad_ira", "roth_ira", "hsa", "trust"}

	for _, treatment := range custodial {
		if !IsCustodial(treatment) {
			t.Errorf("%s should be custodial: counting it as retirement money "+
				"overstates the household by the whole balance", treatment)
		}
	}
	for _, treatment := range retirement {
		if IsCustodial(treatment) {
			t.Errorf("%s should NOT be custodial: excluding it understates "+
				"retirement savings", treatment)
		}
	}
}

// TestStartingRetirementBalanceExcludesCustodial asserts the exclusion through
// the engine rather than only through the predicate.
func TestStartingRetirementBalanceExcludesCustodial(t *testing.T) {
	plans := []AccountPlan{
		{ID: "a", Name: "401k", Treatment: "trad_401k", Balance: dec("100000")},
		{ID: "b", Name: "College", Treatment: "529", Balance: dec("40000")},
		{ID: "c", Name: "Ellie UTMA", Treatment: "utma_ugma", Balance: dec("15000")},
		{ID: "d", Name: "Robin Trump acct", Treatment: "trump", Balance: dec("5000")},
		{ID: "e", Name: "Brokerage", Treatment: "taxable", Balance: dec("25000")},
	}

	got := startingRetirementBalance(plans)
	if want := dec("125000"); !got.Equal(want) {
		t.Errorf("nest egg = %s, want %s (401k + taxable only)",
			got.StringFixed(2), want.StringFixed(2))
	}
}
