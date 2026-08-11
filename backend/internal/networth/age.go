package networth

import "time"

// Age resolution.
//
// The app used to store ages as integers: projection_assumptions.current_age
// and account_contributions.beneficiary_current_age. Both are correct on the
// day they are typed and wrong every year after, and nobody re-enters them, so
// every projection built on one drifts further from reality the longer an
// install runs. household_people.birthdate fixes that at the source.
//
// THE RESOLUTION ORDER, and it is load-bearing:
//
//  1. the linked person's birthdate, aged to the caller's `now`
//  2. the stored integer, when there is no person or no birthdate
//  3. neither — report ok=false. Surface it; never substitute a default.
//
// Step 2 is why upgrading an existing instance changes nothing. The backfill
// gives every current user a person row with a NULL birthdate, which falls
// straight through to the stored integer, so an upgraded install produces
// byte-identical projections until somebody actually enters a birthdate.
//
// Step 3 mirrors AnnualLimitFor's treatment of an unconfigured tax year: a
// missing input is a state to report, not a zero to compute with. An age of 0
// silently makes a 40-year-old eligible for nothing and a 65-year-old
// eligible for everything.

// AgeOn returns the age in whole years on a given day.
//
// Takes `now` rather than reading the clock, for the same reason
// ProjectRetirement does: an age derived from the wall clock makes every test
// that touches it calendar-dependent. The increment lands exactly ON the
// birthday.
func AgeOn(birthdate, now time.Time) int {
	years := now.Year() - birthdate.Year()
	// Month/day comparison, not YearDay: the latter is off by one across a
	// leap year for anyone born after February.
	if now.Month() < birthdate.Month() ||
		(now.Month() == birthdate.Month() && now.Day() < birthdate.Day()) {
		years--
	}
	return years
}

// ResolveAge applies the resolution order above.
//
// birthdate is the linked person's, or nil when there is no person or they have
// not given one. stored is the legacy integer column, where zero means "not
// set" — that is the existing convention in projection_assumptions, where a
// nullable column arrives as 0 when absent.
//
// A birthdate that produces a negative age is treated as absent rather than
// returned: it can only come from a future date, which the API refuses on
// write, so reaching here means data predating that check.
func ResolveAge(birthdate *time.Time, stored int, now time.Time) (int, bool) {
	if birthdate != nil {
		if age := AgeOn(*birthdate, now); age >= 0 {
			return age, true
		}
	}
	if stored > 0 {
		return stored, true
	}
	return 0, false
}

// DefaultEnrollmentAge is the age a custodial account's beneficiary is assumed
// to start spending it when the account carries no target age of its own.
//
// This deliberately does NOT follow step 3 above, and the difference is the
// whole reason it is written down. A beneficiary's age is a FACT about a
// person: there is no honest default for it, so ResolveAge reports it missing.
// An enrollment age is a CONVENTION — eighteen, which is exactly why the
// per-account field exists to override it — and declining to apply it does not
// leave the projection silent, it leaves the projection WRONG: with no horizon
// a 529 compounds past enrollment forever and its whole balance counts toward
// the household's FI number. That error runs in the flattering direction, which
// is the kind nobody catches. Being off by a couple of years on the stop date
// is the smaller mistake, and the visible one.
//
// The college goal has defaulted this way since it was written. This constant
// is here rather than private to allocation so the retirement projection reads
// the same rule from the same place: two surfaces disagreeing about one account
// is the bug both of them exist downstream of.
const DefaultEnrollmentAge = 18

// EnrollmentAge applies that rule: the account's own target age where the
// household set one, the convention otherwise. `stored` is zero when the
// nullable column is NULL, the same "zero means not set" convention ResolveAge
// takes for the legacy age integers.
func EnrollmentAge(stored int) int {
	if stored > 0 {
		return stored
	}
	return DefaultEnrollmentAge
}
