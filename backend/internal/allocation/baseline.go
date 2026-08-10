// Package allocation is the multi-bucket allocator: split a lump sum and a
// monthly surplus across Roth / 529 / brokerage / debt / emergency fund, and
// report what each one does.
//
// THE WHOLE ENGINE IS DETERMINISTIC. Cap pooling and enforcement, eligibility,
// per-bucket projection, goal mapping, the college drawdown, cash drag and the
// asset-location disclosure are exact decimal arithmetic. The AI layer receives
// finished figures and writes prose over them: it never invents a bucket, never
// reallocates, never projects.
//
// Two structural rules run through the package.
//
// First, NOTHING HERE WRITES. A plan operates on a copy of the baseline, and a
// POST that runs a plan must leave `accounts`, `holdings`, `goals`,
// `account_contributions` and `projection_assumptions` byte-identical. This is
// the most dangerous bug available in a package like this — the same class as a
// scenario engine mutating the scenario it was built from — so the shape
// enforces it: AssembleBaseline does every read, Run is a pure function of the
// baseline plus the request, and the only *dbgen.Queries in the package is the
// one AssembleBaseline holds for the length of one call.
//
// Second, EVERY ENGINE IS BORROWED. Investment buckets delegate to
// networth.ProjectRetirement and read RetirementPoint.ByAccount; debt buckets
// delegate to goals.ComputePayoff; caps come from networth.AnnualLimitFor
// pooled by networth.LimitGroup. A second implementation of any of them would
// be a household told two different things about one number.
package allocation

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/goals"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/reporting"
)

// BucketKind is which arithmetic a bucket needs. The allocator's buckets are
// Roth / 529 / brokerage / debt / emergency fund, and only the first three are
// the thing a compound projection does — routing all five through one function
// would show a credit card a compound-growth formula, which is worse than
// showing nothing.
type BucketKind string

const (
	// BucketInvestment compounds: networth.ProjectRetirement.
	BucketInvestment BucketKind = "investment"
	// BucketDebt amortizes: goals.ComputePayoff. The "return" is interest
	// avoided, and the payoff can be "never".
	BucketDebt BucketKind = "debt"
	// BucketCash accrues at the account's own deposit_apy, with no volatility.
	// Liquid by definition, which is why an emergency fund lives here and never
	// in a volatile bucket.
	BucketCash BucketKind = "cash"
)

// Bucket is one place money can go, with the facts its engine needs.
//
// One struct with a Kind discriminator rather than three types behind an
// interface: every consumer switches on the kind anyway (the "show the
// arithmetic" panel has to render a different formula per kind), and an
// interface would hide exactly the distinction the UI has to make visible.
type Bucket struct {
	AccountID   uuid.UUID
	Name        string
	Institution string
	Kind        BucketKind
	Balance     decimal.Decimal

	// Treatment is the confirmed tax treatment, investment buckets only. Empty
	// means untagged, and an untagged investment account is EXCLUDED and
	// reported rather than defaulted — the same rule ProjectRetirement applies,
	// for the same reason: its limit, its horizon and its withdrawal treatment
	// are all unknown and none of them has a defensible default.
	Treatment string
	// ExistingMonthly is what is already being paid in, before the plan adds
	// anything. The plan's contribution is ON TOP of it, and the cap sees both.
	ExistingMonthly decimal.Decimal

	// Terms are the debt's resolved APR and minimum payment, debt buckets only,
	// merged through the same helper the Net Worth, Goals and advisor surfaces
	// use. An unknown rate is reported as unknown, never as zero.
	Terms goals.DebtTerms

	// DepositAPY is the user-entered yield as a PERCENT, cash buckets only.
	// Invalid means nobody has said — never zero.
	DepositAPY decimal.NullDecimal
	Subtype    string
}

// GoalRow is one goal with its progress already read, so Run can map outcomes
// onto goals without touching the database.
type GoalRow struct {
	ID         uuid.UUID
	Name       string
	Kind       string
	AccountID  *uuid.UUID
	Target     decimal.Decimal
	Current    decimal.Decimal
	TargetDate *time.Time
}

// CollegeGoal is a college goal with its beneficiary horizon resolved.
type CollegeGoal struct {
	ID   uuid.UUID
	Name string
	// AnnualCost is target_amount: ONE year's cost in today's dollars. The
	// engine inflates each year separately — see college.go.
	AnnualCost decimal.Decimal
	Years      int
	AccountID  *uuid.UUID
	// YearsToEnrollment is resolved from the beneficiary's birthdate where there
	// is one, and from the stored integer only when there is not. HorizonKnown
	// is false when neither resolves, in which case the goal is reported as
	// unprojectable rather than projected against a guessed year.
	YearsToEnrollment int
	HorizonKnown      bool
}

// Baseline is the household's position as it actually stands: every read this
// package makes, gathered once.
//
// Run takes this by value and copies anything it mutates. A plan that changed a
// field here would be a plan that changed the household.
type Baseline struct {
	Now time.Time

	// Assumptions is the household's stored retirement input, with Months left
	// at zero — Run sets it from the request's horizon.
	Assumptions networth.RetirementAssumptions
	// InflationRate is the household's assumed general inflation, as a FRACTION.
	// It is what converts a nominal deposit yield into the real terms every
	// other figure in this app is expressed in. See cash.go.
	InflationRate decimal.Decimal
	// CollegeInflation is a PERCENT (5.50 = 5.5%/yr), stored per household
	// because college costs have run well above general CPI and a household that
	// disagrees with 5.5% should be able to say so.
	CollegeInflation decimal.Decimal

	// Age is the contributor's age for cap purposes; AgeKnown is false when
	// neither a birthdate nor a stored age resolves, in which case
	// AnnualLimitFor returns the base limit without catch-up. That UNDERSTATES
	// headroom, which is the conservative direction.
	Age      int
	AgeKnown bool

	// FilingStatus and MAGI are the eligibility inputs (doc 31's column and doc
	// 32's). Either one absent means eligibility is `unknown` and the plan says
	// so — it is never assumed to be `eligible`.
	FilingStatus string
	MAGI         decimal.NullDecimal
	// RiskFloor is households.risk_drawdown_floor as a PERCENT, doc 31's column.
	// Used by the horizon-aware assignment check.
	RiskFloor decimal.NullDecimal

	// Buckets is every account money can be allocated to, in one list.
	Buckets []Bucket
	// Plans is EVERY projectable account, including ones the plan does not touch.
	// The whole set travels into ProjectRetirement so cap pooling is correct: two
	// IRAs share one limit whether or not the plan funds both of them.
	Plans []networth.AccountPlan
	// UntaggedAccounts are visible investment accounts with no confirmed tax
	// treatment. Listed, never dropped silently.
	UntaggedAccounts []string

	// MonthlyFixedCosts is the trailing typical month of fixed outgoings, from
	// the same BuildSafeToSpend the Budgets page uses. It is the operating-float
	// exclusion for cash drag on a transaction account.
	MonthlyFixedCosts decimal.Decimal

	Goals        []GoalRow
	CollegeGoals []CollegeGoal
}

// AssembleBaseline gathers every deterministic input, once.
//
// One failure sinks the call. A PARTIAL baseline is worse than an error here for
// the same reason it is in the advisor's ranker: a bucket missing because its
// query failed is indistinguishable from a bucket the household does not have,
// and the plan built on top would be arithmetic over a position that is not the
// household's.
//
// userID scopes visibility the same way every other reporting read does:
// household membership plus `user_id = $2 OR is_shared`. Pass the CALLER's id —
// a plan is built for a person, and a spouse's private account must not appear
// in one.
func AssembleBaseline(
	ctx context.Context, q *dbgen.Queries, householdID, userID uuid.UUID, now time.Time,
) (Baseline, error) {
	b := Baseline{
		Now:              now,
		Buckets:          []Bucket{},
		Plans:            []networth.AccountPlan{},
		UntaggedAccounts: []string{},
		Goals:            []GoalRow{},
		CollegeGoals:     []CollegeGoal{},
	}

	// A household that has never opened the Retirement page has no assumptions
	// row, and that is a normal state rather than an error — the allocator is
	// exactly the kind of surface somebody reaches before they have configured
	// anything. Falling back to the shared defaults is what every other
	// projecting surface does; returning the error here made the whole planner
	// a 500 for a brand-new household.
	assumptions, err := q.GetProjectionAssumptions(ctx, householdID)
	if errors.Is(err, pgx.ErrNoRows) {
		d := networth.DefaultAssumptions()
		assumptions = dbgen.ProjectionAssumption{
			HouseholdID:          householdID,
			RealReturnRate:       d.RealReturnRate,
			InflationRate:        d.InflationRate,
			WithdrawalRate:       d.WithdrawalRate,
			CollegeInflationRate: d.CollegeInflationRate,
		}
	} else if err != nil {
		return Baseline{}, err
	}
	b.InflationRate = assumptions.InflationRate
	b.CollegeInflation = assumptions.CollegeInflationRate
	b.Assumptions = networth.RetirementAssumptions{
		RealReturnRate: assumptions.RealReturnRate,
		WithdrawalRate: assumptions.WithdrawalRate,
	}
	if assumptions.TargetAnnualSpending.Valid {
		b.Assumptions.TargetAnnualSpending = assumptions.TargetAnnualSpending.Decimal
	}

	household, err := q.GetHousehold(ctx, householdID)
	if err != nil {
		return Baseline{}, err
	}
	if household.FilingStatus != nil {
		b.FilingStatus = *household.FilingStatus
	}
	b.RiskFloor = household.RiskDrawdownFloor
	// A MAGI is a statement about ONE tax year. Carrying an old one forward
	// would be the silent staleness limits.go refuses for IRS limits, so a
	// figure whose year is not the year being planned is treated as absent and
	// eligibility comes back `unknown`.
	if household.Magi.Valid && household.MagiTaxYear != nil && int(*household.MagiTaxYear) == now.Year() {
		b.MAGI = household.Magi
	}

	// The contributor's age. The caller's own birthdate wins where there is one;
	// the stored integer is the fallback, and neither is ok=false rather than a
	// zero that would make a 40-year-old eligible for nothing.
	var birthdate *time.Time
	if person, perr := q.GetPersonByUserID(ctx, &userID); perr == nil {
		birthdate = person.Birthdate
	}
	stored := 0
	if assumptions.CurrentAge != nil {
		stored = int(*assumptions.CurrentAge)
	}
	if age, ok := networth.ResolveAge(birthdate, stored, now); ok {
		b.Age, b.AgeKnown = age, true
		b.Assumptions.CurrentAge = age
	}
	if assumptions.TargetRetirementAge != nil {
		b.Assumptions.TargetRetirementAge = int(*assumptions.TargetRetirementAge)
	}
	if assumptions.SsStartAge != nil {
		b.Assumptions.SSStartAge = int(*assumptions.SsStartAge)
	}
	if assumptions.AnnualSsIncome.Valid {
		b.Assumptions.AnnualSSIncome = assumptions.AnnualSsIncome.Decimal
	}

	if err := b.loadInvestments(ctx, q, householdID, userID, now); err != nil {
		return Baseline{}, err
	}
	if err := b.loadDebts(ctx, q, householdID, userID); err != nil {
		return Baseline{}, err
	}
	if err := b.loadCash(ctx, q, householdID, userID); err != nil {
		return Baseline{}, err
	}
	if err := b.loadGoals(ctx, q, householdID, now); err != nil {
		return Baseline{}, err
	}

	// Fixed costs, for the cash-drag operating-float exclusion. The same
	// BuildSafeToSpend the Budgets page and the advisor call, so "a typical
	// month of fixed costs" means one thing across the app.
	sts, err := reporting.BuildSafeToSpend(ctx, q, householdID, now)
	if err != nil {
		return Baseline{}, err
	}
	b.MonthlyFixedCosts = sts.FixedCosts

	return b, nil
}

// loadInvestments builds both the investment buckets and the FULL plan list.
//
// The two are deliberately different lengths. Buckets are what a user may
// allocate to; Plans is every projectable account, because cap pooling is a
// household-level fact — a plan funding one IRA still shares its limit with the
// other IRA it does not touch, and leaving that one out of the projection would
// hand the household headroom it does not have.
func (b *Baseline) loadInvestments(
	ctx context.Context, q *dbgen.Queries, householdID, userID uuid.UUID, now time.Time,
) error {
	rows, err := q.ListProjectableAccounts(ctx, dbgen.ListProjectableAccountsParams{
		HouseholdID: householdID, UserID: userID,
	})
	if err != nil {
		return err
	}
	for _, r := range rows {
		p := networth.AccountPlan{ID: r.ID.String(), Name: r.Name}
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
		storedAge := 0
		if r.BeneficiaryCurrentAge != nil {
			storedAge = int(*r.BeneficiaryCurrentAge)
		}
		if age, ok := networth.ResolveAge(r.BeneficiaryBirthdate, storedAge, now); ok {
			p.BeneficiaryCurrentAge = age
		}
		if r.BeneficiaryTargetAge != nil {
			p.BeneficiaryTargetAge = int(*r.BeneficiaryTargetAge)
		}
		// The account's own real return, where the household has set one. Zero
		// (the zero value, and what an unset/null column scans as) means "use the
		// household rate" — exactly the convention ProjectRetirement already
		// applies, so this line is the whole wire from the column to the
		// projection. Without it every account compounds at the household rate,
		// which understates a 529 and overstates a cash bucket.
		if r.AssumedRealReturn.Valid {
			p.RealReturnRate = r.AssumedRealReturn.Decimal
		}
		b.Plans = append(b.Plans, p)

		if p.Treatment == "" {
			b.UntaggedAccounts = append(b.UntaggedAccounts, r.Name)
			continue // not offerable as a bucket: nothing about it is known
		}
		bucket := Bucket{
			AccountID:       r.ID,
			Name:            r.Name,
			Kind:            BucketInvestment,
			Balance:         p.Balance,
			Treatment:       p.Treatment,
			ExistingMonthly: p.MonthlyContribution,
		}
		if r.InstitutionName != nil {
			bucket.Institution = *r.InstitutionName
		}
		b.Buckets = append(b.Buckets, bucket)
	}
	return nil
}

// loadDebts builds the debt buckets: every visible liability with a positive
// balance, terms merged through the shared helper.
func (b *Baseline) loadDebts(ctx context.Context, q *dbgen.Queries, householdID, userID uuid.UUID) error {
	rows, err := q.ListVisibleLiabilities(ctx, dbgen.ListVisibleLiabilitiesParams{
		HouseholdID: householdID, UserID: userID,
	})
	if err != nil {
		return err
	}
	for _, r := range rows {
		balance := decimal.Zero
		if r.CurrentBalance.Valid {
			balance = r.CurrentBalance.Decimal
		}
		// A cleared debt is not a bucket. Offering to pay down a zero balance is
		// noise, and the payoff arithmetic has nothing to say about it.
		if !balance.IsPositive() {
			continue
		}
		bucket := Bucket{
			AccountID: r.AccountID,
			Name:      r.AccountName,
			Kind:      BucketDebt,
			Balance:   balance,
			Terms: goals.MergeDebtTerms(
				r.ManualApr, r.Apr, r.InterestRatePercentage,
				r.ObligationAmount, r.ManualMinimumPayment, r.MinimumPayment,
			),
		}
		if r.InstitutionName != nil {
			bucket.Institution = *r.InstitutionName
		}
		if r.Subtype != nil {
			bucket.Subtype = *r.Subtype
		}
		b.Buckets = append(b.Buckets, bucket)
	}
	return nil
}

// loadCash builds the cash buckets: depository accounts, with whatever yield
// the household has entered.
func (b *Baseline) loadCash(ctx context.Context, q *dbgen.Queries, householdID, userID uuid.UUID) error {
	rows, err := q.ListVisibleDepositAccounts(ctx, dbgen.ListVisibleDepositAccountsParams{
		HouseholdID: householdID, UserID: userID,
	})
	if err != nil {
		return err
	}
	for _, r := range rows {
		balance := decimal.Zero
		if r.CurrentBalance.Valid {
			balance = r.CurrentBalance.Decimal
		}
		bucket := Bucket{
			AccountID:  r.ID,
			Name:       r.Name,
			Kind:       BucketCash,
			Balance:    balance,
			DepositAPY: r.DepositApy,
		}
		if r.Subtype != nil {
			bucket.Subtype = *r.Subtype
		}
		if r.InstitutionName != nil {
			bucket.Institution = *r.InstitutionName
		}
		b.Buckets = append(b.Buckets, bucket)
	}
	return nil
}

// loadGoals reads the household's active goals with progress already computed,
// and resolves the college goals' enrollment horizons.
//
// Household-scoped goals only, matching every other shared surface: a personal
// goal must not surface on output the whole household reads.
func (b *Baseline) loadGoals(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) error {
	rows, err := q.ListActiveHouseholdGoals(ctx, householdID)
	if err != nil {
		return err
	}
	for _, g := range rows {
		current, err := reporting.GoalProgress(ctx, q, g, now)
		if err != nil {
			return err
		}
		b.Goals = append(b.Goals, GoalRow{
			ID: g.ID, Name: g.Name, Kind: g.Kind, AccountID: g.AccountID,
			Target: g.TargetAmount, Current: current, TargetDate: g.TargetDate,
		})
	}

	college, err := q.ListCollegeGoals(ctx, householdID)
	if err != nil {
		return err
	}
	for _, c := range college {
		goal := CollegeGoal{
			ID: c.ID, Name: c.Name, AnnualCost: c.TargetAmount,
			Years: int(c.CollegeYears), AccountID: c.AccountID,
		}
		// The enrollment horizon. Birthdate first, the stored integer second,
		// neither means the goal is reported as unprojectable — the same order
		// and the same refusal to guess that ResolveAge exists for.
		storedAge := 0
		if c.BeneficiaryCurrentAge != nil {
			storedAge = int(*c.BeneficiaryCurrentAge)
		}
		targetAge := defaultEnrollmentAge
		if c.BeneficiaryTargetAge != nil && *c.BeneficiaryTargetAge > 0 {
			targetAge = int(*c.BeneficiaryTargetAge)
		}
		if age, ok := networth.ResolveAge(c.BeneficiaryBirthdate, storedAge, now); ok {
			years := targetAge - age
			if years < 0 {
				years = 0 // already enrolled: the drawdown starts now
			}
			goal.YearsToEnrollment, goal.HorizonKnown = years, true
		}
		b.CollegeGoals = append(b.CollegeGoals, goal)
	}
	return nil
}

// defaultEnrollmentAge is used only when the linked account carries no
// beneficiary target age. Eighteen is the convention rather than a fact, which
// is exactly why the account-level field overrides it.
const defaultEnrollmentAge = 18

// BucketByID finds a bucket. Returns false rather than a zero Bucket for a
// missing one, so a request naming an account the caller cannot see is a 400
// and not a silently ignored line of the split.
func (b Baseline) BucketByID(id uuid.UUID) (Bucket, bool) {
	for _, bucket := range b.Buckets {
		if bucket.AccountID == id {
			return bucket, true
		}
	}
	return Bucket{}, false
}
