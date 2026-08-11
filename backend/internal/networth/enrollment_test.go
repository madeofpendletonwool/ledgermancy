package networth

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// THE REGRESSION these cover: account_contributions.beneficiary_target_age is
// nullable and most households never fill it in. The college goal has always
// read a missing one as eighteen; the retirement projection used to read it as
// "no horizon", so the same 529 stopped at enrollment on one surface and
// compounded past it forever on the other — counted as retirement money it will
// never be, which overstates the household in the flattering direction.
//
// EnrollmentAge is now the one definition and both surfaces call it.

func TestEnrollmentAge(t *testing.T) {
	cases := []struct {
		name   string
		stored int
		want   int
	}{
		{"the household set a target age", 22, 22},
		{"an early one is honoured too", 17, 17},
		{"a NULL column arrives as zero", 0, DefaultEnrollmentAge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EnrollmentAge(c.stored); got != c.want {
				t.Errorf("EnrollmentAge(%d) = %d, want %d", c.stored, got, c.want)
			}
		})
	}
	// Pinned rather than derived: the college goal's own tests are written
	// against eighteen, and the point of this constant is that both surfaces
	// mean the same year.
	if DefaultEnrollmentAge != 18 {
		t.Errorf("DefaultEnrollmentAge = %d, want 18", DefaultEnrollmentAge)
	}
}

// The live Pendleton case: the 529 is tagged with a child who has a birthdate,
// and beneficiary_target_age is NULL. The beneficiary's age resolves, so the
// account must stop.
func TestCustodialHorizonWithoutStoredTargetAge(t *testing.T) {
	plans := []AccountPlan{{
		ID: "529", Name: "College", Treatment: "529",
		Balance:               decimal.NewFromInt(20000),
		BeneficiaryCurrentAge: 6,
		// BeneficiaryTargetAge deliberately left at zero.
	}}

	s := BuildSchedule(plans, baseAssumptions(360), testNow).Accounts[0]

	if !s.HasHorizon {
		t.Fatal("no horizon on a 529 whose beneficiary's age is known — it compounds past enrollment and counts toward the FI number")
	}
	want := 12 * 12
	if s.StopMonth != want {
		t.Errorf("StopMonth = %d, want %d (age 6 to the default enrollment age of 18)", s.StopMonth, want)
	}
	if s.Stopped(want) || !s.Stopped(want+1) {
		t.Errorf("the account stops on the wrong side of month %d", want)
	}
}

// And the balance actually leaves the nest egg, which is the number the bug
// moved. Without the horizon the 529 would still be growing at month 360 and
// its whole balance would be counted as retirement money.
func TestCustodialWithoutStoredTargetAgeLeavesTheNestEgg(t *testing.T) {
	plans := []AccountPlan{
		{ID: "b", Name: "Brokerage", Treatment: "taxable", Balance: decimal.NewFromInt(10000)},
		{
			ID: "c", Name: "College", Treatment: "529",
			Balance:               decimal.NewFromInt(20000),
			MonthlyContribution:   decimal.NewFromInt(100),
			BeneficiaryCurrentAge: 17,
		},
	}
	a := baseAssumptions(36)
	a.RealReturnRate = decimal.Zero

	last := ProjectRetirement(plans, a, testNow).Points[35]

	mustEqual(t, "nest egg excludes the 529", last.Retirement, dec("10000"))
	// One year of $100/month, then the default horizon stops it: 20000 + 1200.
	mustEqual(t, "education balance", last.Education, dec("21200"))
}

// A stored target age still wins outright. Eighteen is a convention, and the
// per-account column exists precisely to override it.
func TestStoredTargetAgeOverridesTheDefault(t *testing.T) {
	plans := []AccountPlan{{
		ID: "529", Name: "College", Treatment: "529",
		Balance:               decimal.NewFromInt(20000),
		BeneficiaryCurrentAge: 6,
		BeneficiaryTargetAge:  22,
	}}

	s := BuildSchedule(plans, baseAssumptions(360), testNow).Accounts[0]

	if want := 16 * 12; s.StopMonth != want {
		t.Errorf("StopMonth = %d, want %d (age 6 to the stored target age of 22)", s.StopMonth, want)
	}
}

// The fallback is for the target age ONLY. A beneficiary's own age is a fact
// about a person, and there is no convention that supplies it — an account with
// nobody's age on file still runs the full term rather than being stopped at an
// invented year.
func TestNoBeneficiaryAgeStillMeansNoHorizon(t *testing.T) {
	plans := []AccountPlan{{
		ID: "529", Name: "College", Treatment: "529",
		Balance: decimal.NewFromInt(20000),
	}}

	s := BuildSchedule(plans, baseAssumptions(360), testNow).Accounts[0]

	if s.HasHorizon {
		t.Error("a horizon was invented for a beneficiary whose age is not on file")
	}
	if s.Stopped(1) {
		t.Error("the account stopped despite having no resolved horizon")
	}
}

// Only custodial accounts get an enrollment. A 401(k) carrying a stray
// beneficiary age must not acquire one — the default enrollment age answers
// "when does college money get spent", which is not a question a retirement
// account has.
func TestNonCustodialAccountNeverGetsAnEnrollmentHorizon(t *testing.T) {
	plans := []AccountPlan{{
		ID: "401k", Name: "401(k)", Treatment: "trad_401k",
		Balance:               decimal.NewFromInt(50000),
		BeneficiaryCurrentAge: 6,
	}}

	s := BuildSchedule(plans, baseAssumptions(360), testNow).Accounts[0]

	if s.HasHorizon {
		t.Error("a retirement account was given an enrollment horizon")
	}
}

// Every custodial treatment shares the rule, not just the 529 the bug was found
// on: each of them is money that belongs to a dependent and is spent on them.
func TestEveryCustodialTreatmentUsesTheDefaultEnrollmentAge(t *testing.T) {
	for treatment := range custodialTreatments {
		t.Run(treatment, func(t *testing.T) {
			plans := []AccountPlan{{
				ID: treatment, Name: treatment, Treatment: treatment,
				Balance:               decimal.NewFromInt(10000),
				BeneficiaryCurrentAge: 10,
			}}
			s := BuildSchedule(plans, baseAssumptions(360), testNow).Accounts[0]
			if !s.HasHorizon {
				t.Fatalf("%s has no horizon with a known beneficiary age", treatment)
			}
			if want := 8 * 12; s.StopMonth != want {
				t.Errorf("StopMonth = %d, want %d", s.StopMonth, want)
			}
		})
	}
}

// PlanFromRow's end of it: a row with a birthdate and a NULL target age must
// reach the schedule with a horizon. This is the path the live household takes.
func TestPlanFromRowHorizonWithoutStoredTargetAge(t *testing.T) {
	birthdate := time.Date(2020, 5, 29, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)

	p := PlanFromRow(rowFor(decimal.NullDecimal{}, &birthdate, nil), now)

	if p.BeneficiaryTargetAge != 0 {
		t.Errorf("BeneficiaryTargetAge = %d, want 0 — the plan carries the column, not the fallback", p.BeneficiaryTargetAge)
	}
	s := BuildSchedule([]AccountPlan{p}, RetirementAssumptions{
		RealReturnRate: decimal.RequireFromString("0.03"), Months: 300, CurrentAge: 29,
	}, now).Accounts[0]

	if !s.HasHorizon {
		t.Fatal("the 529 has no horizon in the schedule — it will compound past enrollment")
	}
	if want := 12 * 12; s.StopMonth != want {
		t.Errorf("StopMonth = %d, want %d (age 6 to the default enrollment age of 18)", s.StopMonth, want)
	}
}
