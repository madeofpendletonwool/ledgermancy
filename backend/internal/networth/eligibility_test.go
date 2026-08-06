package networth

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// A CAP IS NOT PERMISSION, and these tests are the assertion of it.
//
// The failure this file exists to prevent is not a crash. It is a household well
// over the Roth MAGI phase-out being shown $7,500 of headroom — a plan that is
// not merely optimistic but not legal to execute, presented with exactly the
// same confidence as every figure that came out of an exact decimal engine.

// nd is a NullDecimal from a string; dec lives in project_test.go.
func nd(s string) decimal.NullDecimal { return decimal.NewNullDecimal(dec(s)) }

// Below the phase-out the full cap applies and nothing is qualified.
func TestEligibilityBelowPhaseOut(t *testing.T) {
	limit := dec("7500")
	got, ok := EligibilityFor("roth_ira", 2026, FilingSingle, nd("120000"), limit)
	if !ok {
		t.Fatal("2026 should be a configured year")
	}
	if got.Status != EligibilityEligible {
		t.Errorf("status = %q, want %q", got.Status, EligibilityEligible)
	}
	if !got.Limit.Equal(limit) {
		t.Errorf("limit = %s, want %s", got.Limit, limit)
	}
}

// Above the range the answer is $0 of headroom — NOT the cap. And the wording
// matters as much as the number: a backdoor Roth is legal and common above this
// line, so the claim is that a DIRECT contribution is disallowed.
func TestEligibilityAbovePhaseOutIsZeroNotTheCap(t *testing.T) {
	got, ok := EligibilityFor("roth_ira", 2026, FilingSingle, nd("250000"), dec("7500"))
	if !ok {
		t.Fatal("2026 should be a configured year")
	}
	if got.Status != EligibilityIneligible {
		t.Errorf("status = %q, want %q", got.Status, EligibilityIneligible)
	}
	if !got.Limit.IsZero() {
		t.Errorf("limit = %s, want 0 — a cap is not permission", got.Limit)
	}
	if !strings.Contains(strings.ToLower(got.Note), "backdoor") {
		t.Errorf("note does not mention the backdoor route, which is the true statement: %q", got.Note)
	}
}

// Inside the band the limit falls linearly and is rounded the IRS's way: up to
// the next $10. A cliff here would be wrong in both directions depending on
// which side of the middle the household sits.
func TestEligibilityInsidePhaseOutIsReduced(t *testing.T) {
	// 2026 single: $153,000–$168,000, a $15,000 span. At $160,500 exactly half
	// the range remains, so half of $7,500 is $3,750 — already a $10 multiple.
	got, ok := EligibilityFor("roth_ira", 2026, FilingSingle, nd("160500"), dec("7500"))
	if !ok {
		t.Fatal("2026 should be a configured year")
	}
	if got.Status != EligibilityPhasedOut {
		t.Fatalf("status = %q, want %q", got.Status, EligibilityPhasedOut)
	}
	if want := dec("3750"); !got.Limit.Equal(want) {
		t.Errorf("reduced limit = %s, want %s", got.Limit, want)
	}
	if got.Limit.GreaterThan(dec("7500")) {
		t.Error("a reduced limit must never exceed the full cap")
	}
}

// The $200 floor. Just under the top of the band the linear figure is a few
// dollars; the IRS rounds any non-zero result up to $200, and dropping that rule
// produces a number a household cannot actually contribute.
func TestEligibilityPhaseOutFloorIsTwoHundred(t *testing.T) {
	got, _ := EligibilityFor("roth_ira", 2026, FilingSingle, nd("167900"), dec("7500"))
	if got.Status != EligibilityPhasedOut {
		t.Fatalf("status = %q, want phased_out", got.Status)
	}
	if want := dec("200"); !got.Limit.Equal(want) {
		t.Errorf("limit = %s, want the $200 floor", got.Limit)
	}
}

// THE MOST IMPORTANT CASE IN THE FILE. No MAGI on file is `unknown`, never
// `eligible`. The flattering default is the one that ships by accident.
func TestEligibilityWithoutMAGIIsUnknownNeverEligible(t *testing.T) {
	got, ok := EligibilityFor("roth_ira", 2026, FilingSingle, decimal.NullDecimal{}, dec("7500"))
	if !ok {
		t.Fatal("a missing MAGI is not an unconfigured year")
	}
	if got.Status != EligibilityUnknown {
		t.Fatalf("status = %q, want %q", got.Status, EligibilityUnknown)
	}
	if got.Note == "" {
		t.Error("an unknown status must carry a note saying what was assumed")
	}
}

// A missing filing status is the same story from the other side.
func TestEligibilityWithoutFilingStatusIsUnknown(t *testing.T) {
	got, _ := EligibilityFor("roth_ira", 2026, "", nd("120000"), dec("7500"))
	if got.Status != EligibilityUnknown {
		t.Errorf("status = %q, want %q", got.Status, EligibilityUnknown)
	}
}

// An unconfigured year returns ok=false and is surfaced — never silently served
// from an adjacent year's thresholds. Same rule limits.go applies to caps.
func TestEligibilityUnconfiguredYear(t *testing.T) {
	got, ok := EligibilityFor("roth_ira", 2099, FilingSingle, nd("120000"), dec("7500"))
	if ok {
		t.Fatal("2099 should not be configured — did somebody add it without a test?")
	}
	if got.Status != EligibilityUnknown {
		t.Errorf("status = %q, want %q", got.Status, EligibilityUnknown)
	}
}

// Married-separate is its own band and a brutal one: $0–$10,000, never indexed.
// Folding it into "single" would tell almost every household filing separately
// that it may make a full Roth contribution.
func TestMarriedSeparateHasItsOwnBand(t *testing.T) {
	got, _ := EligibilityFor("roth_ira", 2026, FilingMarriedSeparate, nd("60000"), dec("7500"))
	if got.Status != EligibilityIneligible {
		t.Errorf("status = %q at $60k married-separate, want ineligible", got.Status)
	}
	joint, _ := EligibilityFor("roth_ira", 2026, FilingMarriedJoint, nd("60000"), dec("7500"))
	if joint.Status != EligibilityEligible {
		t.Errorf("the same income filing jointly = %q, want eligible", joint.Status)
	}
}

// An HSA is permanently unknown: the app has no insurance data, so it cannot
// know whether the household had HDHP coverage. Saying "eligible" because
// nothing contradicted it would be a claim with nothing behind it.
func TestHSAEligibilityIsAlwaysUnknown(t *testing.T) {
	got, _ := EligibilityFor("hsa", 2026, FilingSingle, nd("50000"), dec("4400"))
	if got.Status != EligibilityUnknown {
		t.Errorf("status = %q, want unknown", got.Status)
	}
	if !strings.Contains(got.Note, "HDHP") {
		t.Errorf("note should name the coverage requirement: %q", got.Note)
	}
}

// A traditional IRA's phase-out is on DEDUCTIBILITY, not on the contribution.
// Reporting it as an eligibility limit would be wrong in the unflattering
// direction, which is still wrong.
func TestTraditionalIRAHasNoContributionPhaseOut(t *testing.T) {
	got, _ := EligibilityFor("trad_ira", 2026, FilingSingle, nd("400000"), dec("7500"))
	if got.Status != EligibilityEligible {
		t.Errorf("status = %q at $400k into a traditional IRA, want eligible", got.Status)
	}
}
