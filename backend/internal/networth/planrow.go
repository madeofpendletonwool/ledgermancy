package networth

import (
	"time"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// PlanFromRow maps a projectable-account row onto the engine's plan type.
//
// THIS IS THE ONE PLACE THAT MAPPING LIVES, and it is a single function because
// it was three and they had already drifted — the same failure TaxTreatments
// records above it. The API layer, the advisor and the allocator each carried
// their own copy; when account_contributions.assumed_real_return was added, only
// the allocator's copy learned to read it. The result was a household that
// entered 6% on the Retirement page and saw its college drawdown change while
// the Retirement page's own chart, its FI age and the advisor's briefing all
// kept compounding that account at the household default. Two surfaces
// disagreeing about one account is the bug; three copies of the mapping is how
// it got there.
//
// `now` is taken rather than read from the clock because BeneficiaryCurrentAge
// is resolved through ResolveAge, which prefers the linked person's birthdate
// aged to a caller-supplied day — see that function for why a stored integer is
// correct only on the day it is typed.
func PlanFromRow(r dbgen.ListProjectableAccountsRow, now time.Time) AccountPlan {
	p := AccountPlan{ID: r.ID.String(), Name: r.Name}
	if r.TaxTreatment != nil {
		p.Treatment = *r.TaxTreatment
	}
	if r.CurrentBalance.Valid {
		p.Balance = r.CurrentBalance.Decimal
	}
	if r.MonthlyContribution.Valid {
		p.MonthlyContribution = r.MonthlyContribution.Decimal
	}
	if r.EmployerMatchPct.Valid {
		p.EmployerMatchPct = r.EmployerMatchPct.Decimal
	}
	if r.AnnualSalary.Valid {
		p.AnnualSalary = r.AnnualSalary.Decimal
	}
	if r.EmployerMatchLimit.Valid {
		p.EmployerMatchLimit = r.EmployerMatchLimit.Decimal
	}
	stored := 0
	if r.BeneficiaryCurrentAge != nil {
		stored = int(*r.BeneficiaryCurrentAge)
	}
	if age, ok := ResolveAge(r.BeneficiaryBirthdate, stored, now); ok {
		p.BeneficiaryCurrentAge = age
	}
	if r.BeneficiaryTargetAge != nil {
		p.BeneficiaryTargetAge = int(*r.BeneficiaryTargetAge)
	}
	// The account's own real return, where the household has set one. A NULL
	// column leaves the field at its zero value, which BuildSchedule reads as
	// "use the household rate" — so an account nobody has an opinion about
	// projects exactly as it did before this column existed.
	if r.AssumedRealReturn.Valid {
		p.RealReturnRate = r.AssumedRealReturn.Decimal
	}
	return p
}

// PlansFromRows maps a whole result set. A NULL tax_treatment becomes an empty
// Treatment, which the engine excludes and reports rather than defaulting — the
// whole point of leaving that column nullable.
func PlansFromRows(rows []dbgen.ListProjectableAccountsRow, now time.Time) []AccountPlan {
	plans := make([]AccountPlan, 0, len(rows))
	for _, r := range rows {
		plans = append(plans, PlanFromRow(r, now))
	}
	return plans
}
