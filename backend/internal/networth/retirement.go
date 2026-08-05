package networth

import (
	"time"

	"github.com/shopspring/decimal"
)

// Account-aware retirement projection.
//
// This sits BESIDE Project, which it does not replace and must not modify. The
// Net Worth page's straight-line model is honest about being an illustration of
// "if things continue like this", and its simplicity is the reason a user can
// check it by hand. What it cannot do is answer the retirement question: it
// compounds one pooled investment balance at one rate, has no idea where a
// contribution lands, and has no withdrawal phase at all.
//
// Everything below is exact decimal arithmetic. No figure here is produced by,
// checked by, or passed through a model — somebody will decide when to stop
// working based on this output, and a language model has no business anywhere
// near that number. The only AI touching this feature is the narration layer,
// which is handed finished strings.
//
// Every rate is REAL (inflation-adjusted), so every dollar figure is in today's
// dollars throughout. Mixing a nominal return with today's spending is the most
// common way a retirement projection lies, and it always lies in the flattering
// direction.

// AccountPlan is one account's starting position and contribution plan.
type AccountPlan struct {
	ID   string
	Name string
	// Treatment is the confirmed tax treatment. An empty string means untagged;
	// such an account is excluded from the projection and reported as a gap,
	// never defaulted.
	Treatment string
	Balance   decimal.Decimal

	MonthlyContribution decimal.Decimal

	// EmployerMatchPct is a fraction OF SALARY (0.05 = 5% of salary), not of the
	// employee's contribution. AnnualSalary is what it applies to; a match with
	// no salary behind it is not counted, because the alternative is guessing.
	EmployerMatchPct   decimal.Decimal
	AnnualSalary       decimal.Decimal
	EmployerMatchLimit decimal.Decimal // annual cap on the match; zero = uncapped

	// A custodial account runs to its beneficiary's horizon, not to retirement.
	// Contributions and compounding stop there and the balance is excluded from
	// the retirement nest egg entirely — counting a college fund as retirement
	// assets overstates the household by its whole balance.
	//
	// BeneficiaryCurrentAge is resolved by the caller, preferring the linked
	// person's birthdate aged to `now` and falling back to the stored integer
	// only when there is no person or no birthdate. See ResolveAge — a stored
	// age is correct the day it is typed and wrong every year after.
	BeneficiaryCurrentAge int
	BeneficiaryTargetAge  int
}

// custodialTreatments are the tax treatments whose balance is NOT the
// household's retirement money.
//
// A 529 was already here: it funds college, not retirement. The rest joined it
// for a stronger reason — a UTMA/UGMA is irrevocably the child's property from
// the moment it is funded, a Coverdell is education money, a custodial Roth is
// the child's own retirement, and a Trump account is locked to the child until
// they are 18. Counting any of them as the household's nest egg overstates the
// household's position by the whole balance, and it is the kind of error nobody
// catches because it moves the number in the flattering direction.
var custodialTreatments = map[string]bool{
	"529":            true,
	"utma_ugma":      true,
	"coverdell":      true,
	"custodial_roth": true,
	"trump":          true,
}

// IsCustodial reports whether a tax treatment holds money that belongs to a
// dependent rather than to the household's retirement.
func IsCustodial(treatment string) bool { return custodialTreatments[treatment] }

// TaxTreatments is every value accounts.tax_treatment may take, in the order
// the UI offers them: the household's own money first, then the custodial
// treatments defined above.
//
// This is the ONE place the vocabulary lives on the Go side. It was in three
// places before, and they had already drifted: the DB CHECK (00034) and the
// frontend picker both carried 13 values while the API's own validation carried
// 9, so classifying an account as a UTMA got a 400 from an endpoint whose
// database would have accepted it happily. That was invisible while only Plaid
// could create accounts — SuggestTaxTreatment never proposed the missing four —
// and manual account creation surfaces it immediately, because a custodial
// account is exactly the kind an institution tends not to link.
//
// Keep in step with accounts_tax_treatment_check (00034). Callers validate
// through ValidTaxTreatment rather than re-listing these.
var TaxTreatments = []string{
	"taxable", "trad_401k", "roth_401k", "trad_ira", "roth_ira",
	"hsa", "trust", "other",
	"529", "utma_ugma", "coverdell", "custodial_roth", "trump",
}

var validTreatments = func() map[string]bool {
	m := make(map[string]bool, len(TaxTreatments))
	for _, t := range TaxTreatments {
		m[t] = true
	}
	return m
}()

// ValidTaxTreatment reports whether a value will satisfy the DB constraint, so
// a bad one returns a readable 400 rather than a 500 from a check violation.
func ValidTaxTreatment(treatment string) bool { return validTreatments[treatment] }

// isEducation reports whether this account funds a dependent rather than the
// household's retirement. Named for the original 529 case; the set is now
// every custodial treatment.
func (p AccountPlan) isEducation() bool { return IsCustodial(p.Treatment) }

// RetirementAssumptions are the household-level inputs. Every one is
// user-visible and user-editable, and every response carries them back so a
// client cannot render a curve without the inputs that produced it.
type RetirementAssumptions struct {
	RealReturnRate decimal.Decimal
	WithdrawalRate decimal.Decimal
	CurrentAge     int
	// TargetRetirementAge may be zero, meaning "not decided". The projection
	// still runs; only the required-savings-rate solve needs it.
	TargetRetirementAge int
	// TargetAnnualSpending is what retirement needs to support, in today's
	// dollars. Defaulted by the caller from the household's actual trailing
	// spend — the app knows this figure and must not make somebody guess it.
	TargetAnnualSpending decimal.Decimal
	// AnnualSSIncome counts toward supported spending from SSStartAge onward,
	// and not one year sooner.
	AnnualSSIncome decimal.Decimal
	SSStartAge     int
	// FamilyHSA selects the family HSA cap over the self-only default. The
	// default is the conservative direction: it understates headroom rather
	// than overstating an allowed contribution.
	FamilyHSA bool
	// Months to project. The caller bounds this.
	Months int
}

// AccountPoint is one account's state at one month.
//
// Contributed is the EMPLOYEE's money only, with the match in Employer beside
// it, matching the split the household totals use. Balance is the sum of the
// opening balance and all three. Rolling the match into Contributed would let
// somebody read their own saving as larger than it is.
type AccountPoint struct {
	Balance     decimal.Decimal `json:"balance"`
	Contributed decimal.Decimal `json:"contributed"`
	Employer    decimal.Decimal `json:"employer"`
	Growth      decimal.Decimal `json:"growth"`
}

// RetirementPoint is one month of the account-aware projection.
//
// Contributed and Growth are kept apart for the same reason ProjectionPoint
// does it: it must be obvious how much of the number is money saved versus
// money assumed. Retirement is where that distinction matters most — a
// thirty-year curve at 5% is mostly assumption by the end.
type RetirementPoint struct {
	Month string `json:"month"`
	Age   int    `json:"age"`
	// Retirement is the nest egg: every projected account EXCEPT education
	// accounts. Education is tracked separately and never added in.
	Retirement  decimal.Decimal `json:"retirement"`
	Education   decimal.Decimal `json:"education"`
	Contributed decimal.Decimal `json:"contributed"`
	Growth      decimal.Decimal `json:"growth"`
	// EmployerContributed is the match, split out so a user can see how much of
	// their balance somebody else paid for.
	EmployerContributed decimal.Decimal `json:"employer_contributed"`
	// SupportedSpending is what the nest egg supports at this month if drawn at
	// the withdrawal rate, plus Social Security once it has started. This is the
	// series the FI-age scan walks.
	SupportedSpending decimal.Decimal `json:"supported_spending"`
	// ByAccount is keyed by AccountPlan.ID.
	ByAccount map[string]AccountPoint `json:"by_account"`
}

// CapNote records a contribution the projection reduced, so the UI can say what
// it did rather than quietly producing a smaller number than the user's inputs
// imply.
type CapNote struct {
	Group   string          `json:"group"`
	Planned decimal.Decimal `json:"planned_annual"`
	Allowed decimal.Decimal `json:"allowed_annual"`
}

// RetirementProjection is the whole result: the series, the derived answers,
// and — always — the caveats attached to them.
type RetirementProjection struct {
	Points []RetirementPoint `json:"points"`

	// LimitsYear is the tax year whose limits were applied.
	// LimitsConfigured is false when that year is missing from the table, in
	// which case contributions were NOT capped and the UI must say so.
	LimitsYear       int  `json:"limits_year"`
	LimitsConfigured bool `json:"limits_configured"`
	// CapNotes lists every contribution group the limits actually bound.
	CapNotes []CapNote `json:"cap_notes"`

	// NestEggAtTarget and SupportedAtTarget are the figures at
	// TargetRetirementAge, present only when a target age was set and lies
	// within the projected horizon.
	NestEggAtTarget   *decimal.Decimal `json:"nest_egg_at_target"`
	SupportedAtTarget *decimal.Decimal `json:"supported_at_target"`

	// FIAge is the first age at which supported spending covers target
	// spending. Nil means it is not reached within the horizon — which is an
	// answer, and a more useful one than an extrapolated guess.
	FIAge *int `json:"fi_age"`
	// FIMonth is the month FIAge was reached, for the chart marker.
	FIMonth *string `json:"fi_month"`
	// AlreadyFI is true when the household clears its target spending today.
	// FIAge is then the current age, not zero and not a negative.
	AlreadyFI bool `json:"already_fi"`

	// ExcludedAccounts are visible investment accounts with no confirmed tax
	// treatment. They are listed, not silently dropped: omitting an account
	// without saying so produces a confidently wrong number.
	ExcludedAccounts []string `json:"excluded_accounts"`
	// ExcludedValue is what those accounts hold, so the size of the gap is
	// visible and not just its count.
	ExcludedValue decimal.Decimal `json:"excluded_value"`
}

// monthsPerYear as a decimal, used often enough to be worth naming.
var twelve = decimal.NewFromInt(12)

// ProjectRetirement rolls each account forward separately and derives the FI
// answers from the resulting series.
//
// Untagged accounts (empty Treatment) are excluded and reported. `now` is
// injected so tests are not at the mercy of the calendar.
func ProjectRetirement(plans []AccountPlan, a RetirementAssumptions, now time.Time) RetirementProjection {
	out := RetirementProjection{
		LimitsYear:       now.Year(),
		ExcludedAccounts: []string{},
		CapNotes:         []CapNote{},
		Points:           []RetirementPoint{},
	}

	limits, configured := Limits(now.Year())
	out.LimitsConfigured = configured

	// Split the tagged from the untagged before anything is computed. An
	// untagged account has an unknown limit, an unknown horizon, and an unknown
	// withdrawal treatment; there is no defensible default for any of them.
	projectable := make([]AccountPlan, 0, len(plans))
	for _, p := range plans {
		if p.Treatment == "" {
			out.ExcludedAccounts = append(out.ExcludedAccounts, p.Name)
			out.ExcludedValue = out.ExcludedValue.Add(p.Balance)
			continue
		}
		projectable = append(projectable, p)
	}

	if a.Months <= 0 || len(projectable) == 0 {
		// Even with nothing to project, the already-FI question is answerable
		// from Social Security alone, so fall through rather than returning
		// early — but there is no series to build.
		out.AlreadyFI = alreadyFI(decimal.Zero, a)
		if out.AlreadyFI && a.CurrentAge > 0 {
			age := a.CurrentAge
			out.FIAge = &age
		}
		return out
	}

	// Apply the annual limits by POOLING accounts that share one. The 401(k)
	// elective limit is a single number across every 401(k) a person holds, and
	// the IRA limit is shared between traditional and Roth — applying the full
	// cap per account would overstate headroom for anyone holding two of a kind.
	// When the year is unconfigured nothing is capped, and LimitsConfigured
	// carries that fact to the UI.
	monthly := make([]decimal.Decimal, len(projectable))
	for i, p := range projectable {
		monthly[i] = p.MonthlyContribution
	}
	if configured {
		monthly, out.CapNotes = capContributions(projectable, monthly, a, limits)
	}

	// Per-account running state.
	balances := make([]decimal.Decimal, len(projectable))
	contribs := make([]decimal.Decimal, len(projectable))
	employers := make([]decimal.Decimal, len(projectable))
	growths := make([]decimal.Decimal, len(projectable))
	for i, p := range projectable {
		balances[i] = p.Balance
	}

	// A real annual rate divided by twelve, matching Project's choice: not a
	// compounded monthly equivalent, because the difference is immaterial at
	// these horizons and the simpler form is the one a user can check by hand.
	// The two models must agree on this or their numbers will diverge for
	// reasons no one can explain.
	monthlyRate := decimal.Zero
	if a.RealReturnRate.IsPositive() {
		monthlyRate = a.RealReturnRate.Div(twelve)
	}

	// Employer match, converted to a monthly figure and held at its annual cap.
	monthlyMatch := make([]decimal.Decimal, len(projectable))
	for i, p := range projectable {
		monthlyMatch[i] = annualMatch(p).Div(twelve)
	}

	var totalContributed, totalGrowth, totalEmployer decimal.Decimal

	for m := 1; m <= a.Months; m++ {
		month := now.AddDate(0, m, 0)
		age := a.CurrentAge
		if age > 0 {
			// Integer years elapsed, so the age ticks over on the anniversary
			// rather than being fractional — every downstream comparison
			// (Social Security start, catch-up eligibility, FI age) is against
			// a whole age.
			age = a.CurrentAge + (m-1)/12
		}

		point := RetirementPoint{
			Month:     month.Format("2006-01"),
			Age:       age,
			ByAccount: make(map[string]AccountPoint, len(projectable)),
		}

		for i, p := range projectable {
			// An education account stops at its horizon: no more growth, no
			// more contributions. Its balance is spent on the thing it was for.
			if p.isEducation() && educationHorizonPassed(p, m) {
				point.ByAccount[p.ID] = AccountPoint{
					Balance: balances[i].Round(2), Contributed: contribs[i].Round(2),
					Employer: employers[i].Round(2), Growth: growths[i].Round(2),
				}
				point.Education = point.Education.Add(balances[i])
				continue
			}

			// Growth applies to the balance BEFORE this month's contribution,
			// so a deposit is not credited with a full month of return it did
			// not earn. Same rule as Project.
			g := balances[i].Mul(monthlyRate)
			balances[i] = balances[i].Add(g)
			growths[i] = growths[i].Add(g)
			totalGrowth = totalGrowth.Add(g)

			if monthly[i].IsPositive() {
				balances[i] = balances[i].Add(monthly[i])
				contribs[i] = contribs[i].Add(monthly[i])
				totalContributed = totalContributed.Add(monthly[i])
			}
			if monthlyMatch[i].IsPositive() {
				balances[i] = balances[i].Add(monthlyMatch[i])
				employers[i] = employers[i].Add(monthlyMatch[i])
				totalEmployer = totalEmployer.Add(monthlyMatch[i])
			}

			point.ByAccount[p.ID] = AccountPoint{
				Balance: balances[i].Round(2), Contributed: contribs[i].Round(2),
				Employer: employers[i].Round(2), Growth: growths[i].Round(2),
			}
			if p.isEducation() {
				point.Education = point.Education.Add(balances[i])
			} else {
				point.Retirement = point.Retirement.Add(balances[i])
			}
		}

		point.Contributed = totalContributed.Round(2)
		point.Growth = totalGrowth.Round(2)
		point.EmployerContributed = totalEmployer.Round(2)
		point.SupportedSpending = supportedSpending(point.Retirement, age, a).Round(2)
		point.Retirement = point.Retirement.Round(2)
		point.Education = point.Education.Round(2)

		out.Points = append(out.Points, point)
	}

	out.AlreadyFI = alreadyFI(startingRetirementBalance(projectable), a)
	fillTargets(&out, a)
	fillFIAge(&out, a)

	return out
}

// startingRetirementBalance is today's nest egg: the tagged, non-education
// accounts as they stand before any projection.
func startingRetirementBalance(plans []AccountPlan) decimal.Decimal {
	total := decimal.Zero
	for _, p := range plans {
		if !p.isEducation() {
			total = total.Add(p.Balance)
		}
	}
	return total
}

// educationHorizonPassed reports whether a 529's beneficiary has reached the
// age the money is for. A plan with no beneficiary ages set has no horizon and
// simply keeps compounding — but it is still excluded from the nest egg, which
// is the part that matters.
func educationHorizonPassed(p AccountPlan, month int) bool {
	if p.BeneficiaryTargetAge <= 0 || p.BeneficiaryCurrentAge <= 0 {
		return false
	}
	yearsToGo := p.BeneficiaryTargetAge - p.BeneficiaryCurrentAge
	if yearsToGo <= 0 {
		return true
	}
	return month > yearsToGo*12
}

// AnnualMatch is annualMatch for callers outside this package: the match the
// employer actually pays at the plan's CURRENT deferral. Doc 24's advisor
// subtracts it from the match available at a full deferral to get the unclaimed
// headroom, and doc 24's own verification requires the two agree to the cent —
// which only holds while there is one implementation.
func AnnualMatch(p AccountPlan) decimal.Decimal { return annualMatch(p) }

// FullAnnualMatch is the match the employer would pay if the employee deferred
// enough to capture all of it: the percentage-of-salary figure held at its cap,
// WITHOUT annualMatch's "nobody matches a contribution that was never made"
// clamp.
//
// The pair is the whole employer-match option. annualMatch answers "what is
// being paid"; this answers "what is on the table", and the gap between them is
// money the household is leaving there. Computing the second by re-deriving
// pct × salary somewhere else is how the two would drift.
func FullAnnualMatch(p AccountPlan) decimal.Decimal {
	if !p.EmployerMatchPct.IsPositive() || !p.AnnualSalary.IsPositive() {
		return decimal.Zero
	}
	match := p.EmployerMatchPct.Mul(p.AnnualSalary)
	if p.EmployerMatchLimit.IsPositive() && match.GreaterThan(p.EmployerMatchLimit) {
		match = p.EmployerMatchLimit
	}
	return match
}

// annualMatch is the employer's yearly contribution: a percentage of salary,
// held at its annual cap.
//
// Both the percentage and the salary must be present. A match percentage with
// no salary behind it is not an amount, and inventing a salary to multiply it
// by would be exactly the kind of quiet guess this feature exists to avoid.
func annualMatch(p AccountPlan) decimal.Decimal {
	if !p.EmployerMatchPct.IsPositive() || !p.AnnualSalary.IsPositive() {
		return decimal.Zero
	}
	match := p.EmployerMatchPct.Mul(p.AnnualSalary)
	// A match is also bounded by what the employee actually defers: nobody
	// matches a contribution that was never made.
	deferred := p.MonthlyContribution.Mul(twelve)
	if match.GreaterThan(deferred) {
		match = deferred
	}
	if p.EmployerMatchLimit.IsPositive() && match.GreaterThan(p.EmployerMatchLimit) {
		match = p.EmployerMatchLimit
	}
	return match
}

// capContributions holds each shared-limit group's total annual contribution at
// its IRS cap, scaling the group's accounts down proportionally when it binds.
//
// Capping is the honest direction. An uncapped projection assumes a
// contribution the IRS will not allow, and like every other error mode here it
// errs in the flattering direction.
func capContributions(
	plans []AccountPlan,
	monthly []decimal.Decimal,
	a RetirementAssumptions,
	limits TaxYearLimits,
) ([]decimal.Decimal, []CapNote) {
	notes := []CapNote{}

	// Group index lists by shared limit.
	groups := map[string][]int{}
	for i, p := range plans {
		g := limitGroup(p.Treatment)
		groups[g] = append(groups[g], i)
	}

	for g, idx := range groups {
		limit, capped := AnnualLimitFor(plans[idx[0]].Treatment, a.CurrentAge, limits, a.FamilyHSA)
		if !capped {
			continue // taxable, 529, trust, UTMA: no federal annual deferral cap
		}

		planned := decimal.Zero
		for _, i := range idx {
			planned = planned.Add(monthly[i].Mul(twelve))
		}
		if !planned.GreaterThan(limit) {
			continue
		}

		// Scale proportionally rather than filling accounts in some arbitrary
		// order: the split between a traditional and a Roth is the user's
		// decision, and this projection has no business re-allocating it.
		scale := limit.Div(planned)
		for _, i := range idx {
			monthly[i] = monthly[i].Mul(scale)
		}
		notes = append(notes, CapNote{
			Group: g, Planned: planned.Round(2), Allowed: limit.Round(2),
		})
	}

	return monthly, notes
}

// supportedSpending is what a nest egg supports at a given age: the withdrawal
// rate applied to the balance, plus Social Security once it has actually
// started.
//
// Social Security is counted from SSStartAge onward and not one month sooner.
// Crediting it early is a mistake that moves FI age years in the wrong
// direction for someone planning around it.
func supportedSpending(nestEgg decimal.Decimal, age int, a RetirementAssumptions) decimal.Decimal {
	supported := nestEgg.Mul(a.WithdrawalRate)
	if a.AnnualSSIncome.IsPositive() && a.SSStartAge > 0 && age >= a.SSStartAge {
		supported = supported.Add(a.AnnualSSIncome)
	}
	return supported
}

// alreadyFI reports whether today's position already covers target spending.
func alreadyFI(nestEgg decimal.Decimal, a RetirementAssumptions) bool {
	if !a.TargetAnnualSpending.IsPositive() {
		return false
	}
	return supportedSpending(nestEgg, a.CurrentAge, a).
		GreaterThanOrEqual(a.TargetAnnualSpending)
}

// fillTargets records the nest egg and supported spending at the target
// retirement age, when one is set and falls inside the projected horizon.
func fillTargets(out *RetirementProjection, a RetirementAssumptions) {
	if a.TargetRetirementAge <= 0 || a.CurrentAge <= 0 {
		return
	}
	for _, p := range out.Points {
		if p.Age >= a.TargetRetirementAge {
			nest := p.Retirement
			supported := p.SupportedSpending
			out.NestEggAtTarget = &nest
			out.SupportedAtTarget = &supported
			return
		}
	}
}

// fillFIAge finds the first month whose supported spending covers target
// spending.
//
// Solved by scanning the series rather than by closed form, exactly as the doc
// specifies: the series has to exist anyway, and a scan is verifiable by eye
// against the chart the user is looking at. A closed form would also have to
// model the Social Security step, which is where a closed form would be wrong.
func fillFIAge(out *RetirementProjection, a RetirementAssumptions) {
	if !a.TargetAnnualSpending.IsPositive() {
		return
	}
	// A household already past FI is at FI now. Reporting the first month the
	// scan happens to clear the bar would be a month late, and reporting a
	// negative or zero age would be nonsense.
	if out.AlreadyFI {
		if a.CurrentAge > 0 {
			age := a.CurrentAge
			out.FIAge = &age
		}
		if len(out.Points) > 0 {
			m := out.Points[0].Month
			out.FIMonth = &m
		}
		return
	}
	for _, p := range out.Points {
		if p.SupportedSpending.GreaterThanOrEqual(a.TargetAnnualSpending) {
			age, month := p.Age, p.Month
			if age > 0 {
				out.FIAge = &age
			}
			out.FIMonth = &month
			return
		}
	}
	// Not reached within the horizon. FIAge stays nil, which the UI renders as
	// "not reached in N years" — an answer, and a truer one than extrapolating.
}

// --------------------------------------------------------------------------
// Required savings rate
// --------------------------------------------------------------------------

// SavingsRateSolve is the answer to "what would I have to save to retire at N?".
type SavingsRateSolve struct {
	// Reachable is false when no monthly contribution inside the searched
	// bounds gets there. The UI says so plainly instead of printing a number.
	Reachable bool `json:"reachable"`
	// RequiredMonthly is the additional total monthly contribution needed,
	// across all projectable accounts. Nil when not reachable.
	RequiredMonthly *decimal.Decimal `json:"required_monthly"`
	// RequiredRate is that figure as a fraction of gross annual income, when
	// income is known. Nil otherwise — a rate with no denominator is not a rate.
	RequiredRate *decimal.Decimal `json:"required_rate"`
	// Note carries the reason when Reachable is false.
	Note string `json:"note"`
}

// maxSearchedMonthly bounds the bisection.
//
// The bound is the point of the exercise. Without one, bisection on an
// unreachable target either runs away or converges on an absurdity, and "save
// 340% of your income" is not an answer — it is a bug with a decimal point. A
// million dollars a month is far past any real plan, so hitting the bound means
// the target genuinely is not reachable by saving.
var maxSearchedMonthly = decimal.NewFromInt(1000000)

// savingsRateIterations is enough bisection steps to resolve a monthly figure
// to well under a cent across the searched range (2^-40 of $1,000,000).
const savingsRateIterations = 40

// SolveRequiredSavings finds the total monthly contribution needed to support
// target spending by the target retirement age.
//
// The extra contribution is distributed across the projectable accounts in
// proportion to what is already planned there (or evenly, when nothing is
// planned yet) — the projection does not get to decide which account somebody
// should be filling, only how much in total is required.
//
// annualIncome may be zero, in which case RequiredRate is nil rather than a
// division by zero dressed up as a percentage.
func SolveRequiredSavings(
	plans []AccountPlan,
	a RetirementAssumptions,
	annualIncome decimal.Decimal,
	now time.Time,
) SavingsRateSolve {
	if a.TargetRetirementAge <= 0 || a.CurrentAge <= 0 {
		return SavingsRateSolve{Note: "Set your current age and a target retirement age to solve for a savings rate."}
	}
	if a.TargetRetirementAge <= a.CurrentAge {
		return SavingsRateSolve{Note: "Your target retirement age is not in the future."}
	}
	if !a.TargetAnnualSpending.IsPositive() {
		return SavingsRateSolve{Note: "Set the annual spending retirement needs to support."}
	}

	// The horizon is fixed at the target age: the question is what it takes to
	// arrive by then, so the search must not be allowed to answer it by simply
	// running longer.
	solve := a
	solve.Months = (a.TargetRetirementAge - a.CurrentAge) * 12

	projectable := make([]AccountPlan, 0, len(plans))
	for _, p := range plans {
		// Untagged accounts have no known treatment, and custodial ones are not
		// the household's to save into — solving for a contribution rate that
		// funds retirement out of a child's UTMA would be nonsense.
		if p.Treatment != "" && !IsCustodial(p.Treatment) {
			projectable = append(projectable, p)
		}
	}
	if len(projectable) == 0 {
		return SavingsRateSolve{Note: "No tagged retirement accounts to contribute to. Tag your accounts first."}
	}

	// reaches reports whether adding `extra` per month, spread across the
	// accounts, gets supported spending to target by the target age.
	reaches := func(extra decimal.Decimal) bool {
		result := ProjectRetirement(distribute(projectable, extra), solve, now)
		if len(result.Points) == 0 {
			return false
		}
		last := result.Points[len(result.Points)-1]
		return last.SupportedSpending.GreaterThanOrEqual(solve.TargetAnnualSpending)
	}

	// Already there with the current plan.
	if reaches(decimal.Zero) {
		zero := decimal.Zero
		rate := rateOf(zero, annualIncome)
		return SavingsRateSolve{Reachable: true, RequiredMonthly: &zero, RequiredRate: rate}
	}
	// Not reachable even at the bound.
	if !reaches(maxSearchedMonthly) {
		return SavingsRateSolve{
			Note: "Not reachable by saving alone at these assumptions — the target age or spending has to move.",
		}
	}

	lo, hi := decimal.Zero, maxSearchedMonthly
	two := decimal.NewFromInt(2)
	for i := 0; i < savingsRateIterations; i++ {
		mid := lo.Add(hi).Div(two)
		if reaches(mid) {
			hi = mid
		} else {
			lo = mid
		}
	}

	// hi is the smallest tested amount that reaches the target, so it is the
	// side to report: rounding down to lo would name a figure that does not.
	required := hi.Round(2)
	return SavingsRateSolve{
		Reachable:       true,
		RequiredMonthly: &required,
		RequiredRate:    rateOf(required, annualIncome),
	}
}

// distribute returns copies of the plans with `extra` per month spread across
// them, proportionally to existing contributions or evenly when there are none.
func distribute(plans []AccountPlan, extra decimal.Decimal) []AccountPlan {
	out := make([]AccountPlan, len(plans))
	copy(out, plans)
	if !extra.IsPositive() || len(out) == 0 {
		return out
	}

	planned := decimal.Zero
	for _, p := range out {
		planned = planned.Add(p.MonthlyContribution)
	}

	if planned.IsPositive() {
		for i := range out {
			share := out[i].MonthlyContribution.Div(planned)
			out[i].MonthlyContribution = out[i].MonthlyContribution.Add(extra.Mul(share))
		}
		return out
	}

	each := extra.Div(decimal.NewFromInt(int64(len(out))))
	for i := range out {
		out[i].MonthlyContribution = out[i].MonthlyContribution.Add(each)
	}
	return out
}

// rateOf expresses a monthly contribution as a fraction of gross annual income.
// Nil when income is unknown: a savings "rate" with no denominator is not one.
func rateOf(monthly, annualIncome decimal.Decimal) *decimal.Decimal {
	if !annualIncome.IsPositive() {
		return nil
	}
	rate := monthly.Mul(twelve).Div(annualIncome).Round(4)
	return &rate
}
