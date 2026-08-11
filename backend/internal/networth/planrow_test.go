package networth

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// PlanFromRow is the one definition of the row -> plan mapping, and it exists
// because there were three. When account_contributions.assumed_real_return was
// added, only the allocator's copy learned to read it: a household that entered
// 6% on the Retirement page saw its college drawdown change while that page's
// own chart, its FI age and the advisor's briefing all kept compounding the
// account at the household default. These tests pin the two fields the copies
// disagreed about.

func rowFor(rate decimal.NullDecimal, birthdate *time.Time, targetAge *int32) dbgen.ListProjectableAccountsRow {
	treatment := "529"
	return dbgen.ListProjectableAccountsRow{
		ID:                   uuid.New(),
		Name:                 "Hazel 529",
		TaxTreatment:         &treatment,
		CurrentBalance:       decimal.NewNullDecimal(decimal.RequireFromString("20000")),
		MonthlyContribution:  decimal.NewNullDecimal(decimal.RequireFromString("200")),
		AssumedRealReturn:    rate,
		BeneficiaryBirthdate: birthdate,
		BeneficiaryTargetAge: targetAge,
	}
}

// THE REGRESSION: the account's own assumed real return must survive the
// mapping. Every projection in the app reads AccountPlan; a field dropped here
// is a rate silently replaced by the household default.
func TestPlanFromRowCarriesAssumedRealReturn(t *testing.T) {
	rate := decimal.NewNullDecimal(decimal.RequireFromString("0.06"))
	p := PlanFromRow(rowFor(rate, nil, nil), time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC))

	if want := decimal.RequireFromString("0.06"); !p.RealReturnRate.Equal(want) {
		t.Errorf("RealReturnRate = %s, want %s", p.RealReturnRate, want)
	}
}

// NULL is not zero here, but it maps to the same zero VALUE on purpose: the
// engine's documented convention is that a zero RealReturnRate means "use the
// household rate". An account nobody has an opinion about must project exactly
// as it did before the column existed.
func TestPlanFromRowNullReturnMeansHouseholdRate(t *testing.T) {
	p := PlanFromRow(rowFor(decimal.NullDecimal{}, nil, nil), time.Now().UTC())
	if !p.RealReturnRate.IsZero() {
		t.Errorf("RealReturnRate = %s, want zero (the household-rate sentinel)", p.RealReturnRate)
	}
}

// The beneficiary horizon travels too. The advisor's copy of this mapping
// dropped it on the grounds that a custodial account contributes no match and no
// headroom — true of the caller that reasoning was written for, and false of
// fillRetirement, which maps rows through the same function and then runs
// ProjectRetirement. A 529 with no horizon never stops compounding and is
// counted as retirement money it will never be.
func TestPlanFromRowResolvesBeneficiaryHorizon(t *testing.T) {
	birthdate := time.Date(2025, 5, 29, 0, 0, 0, 0, time.UTC)
	target := int32(18)
	now := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)

	p := PlanFromRow(rowFor(decimal.NullDecimal{}, &birthdate, &target), now)

	if p.BeneficiaryCurrentAge != 1 {
		t.Errorf("BeneficiaryCurrentAge = %d, want 1 (born 2025-05-29, as of 2026-08-11)", p.BeneficiaryCurrentAge)
	}
	if p.BeneficiaryTargetAge != 18 {
		t.Errorf("BeneficiaryTargetAge = %d, want 18", p.BeneficiaryTargetAge)
	}

	// And the horizon actually reaches the schedule, which is the point of
	// carrying it: the account stops at the beneficiary's target age.
	s := BuildSchedule([]AccountPlan{p}, RetirementAssumptions{
		RealReturnRate: decimal.RequireFromString("0.03"), Months: 300, CurrentAge: 29,
	}, now)
	if len(s.Accounts) != 1 {
		t.Fatalf("projectable accounts = %d, want 1", len(s.Accounts))
	}
	if !s.Accounts[0].HasHorizon {
		t.Fatal("the 529 has no horizon in the schedule — it will compound past enrollment")
	}
	if want := 17 * 12; s.Accounts[0].StopMonth != want {
		t.Errorf("StopMonth = %d, want %d", s.Accounts[0].StopMonth, want)
	}
}

// The account's own rate must reach the schedule, not just the plan struct.
// This is the assertion that would have failed for the Retirement endpoint and
// the advisor before the mapping was shared.
func TestScheduleUsesTheAccountsOwnRate(t *testing.T) {
	now := time.Now().UTC()
	household := RetirementAssumptions{
		RealReturnRate: decimal.RequireFromString("0.03"), Months: 120, CurrentAge: 40,
	}

	own := PlanFromRow(rowFor(decimal.NewNullDecimal(decimal.RequireFromString("0.06")), nil, nil), now)
	unset := PlanFromRow(rowFor(decimal.NullDecimal{}, nil, nil), now)

	ownRate := BuildSchedule([]AccountPlan{own}, household, now).Accounts[0].Rate
	if want := decimal.RequireFromString("0.06"); !ownRate.Equal(want) {
		t.Errorf("rate = %s, want the account's own %s", ownRate, want)
	}
	unsetRate := BuildSchedule([]AccountPlan{unset}, household, now).Accounts[0].Rate
	if want := decimal.RequireFromString("0.03"); !unsetRate.Equal(want) {
		t.Errorf("rate = %s, want the household's %s", unsetRate, want)
	}
}
