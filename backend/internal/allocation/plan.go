package allocation

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/goals"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
)

// Bounds on a request. These are not opinions about a good plan; they reject
// inputs that would make the engine produce nonsense (a 400-year horizon, a
// negative lump, a split that allocates 900% of the money).
const (
	MinHorizonYears = 1
	MaxHorizonYears = 60
)

var (
	hundred = decimal.NewFromInt(100)
	twelve  = decimal.NewFromInt(12)
	// pctTolerance is the slack allowed on a percentage total. The UI normalises
	// sliders to 100 and floating sliders produce 99.99999; rejecting that would
	// be rejecting the UI's own arithmetic rather than the user's plan.
	pctTolerance = decimal.RequireFromString("0.01")
)

// Split is one line of the allocation: how much of the lump and how much of the
// monthly surplus goes to one account.
//
// Lump and monthly carry SEPARATE percentages because the headline case needs
// them to. "Put the $30k across Roth/529/brokerage, and let the $1,800/month
// keep the card declining" is two different splits of two different pots, and a
// single slider per bucket cannot express it.
type Split struct {
	AccountID uuid.UUID
	// Percentages, 0–100. Not fractions: these are what the UI's sliders show,
	// and one conversion at the boundary beats two conventions in the engine.
	LumpPct    decimal.Decimal
	MonthlyPct decimal.Decimal
	// RealReturnRate is this bucket's assumed real return as a FRACTION.
	// Invalid means "use the household rate", which is the same convention
	// networth.AccountPlan.RealReturnRate uses and for the same reason.
	// Investment buckets only.
	RealReturnRate decimal.NullDecimal
}

// Request is a proposed plan. It is the user's decision, not the engine's: the
// allocator computes outcomes for a split somebody chose and never picks one.
type Request struct {
	Lump         decimal.Decimal
	Monthly      decimal.Decimal
	HorizonYears int
	// TargetNestEgg is optional. When set, the result says whether the plan's
	// projected assets reach it — it does not change the projection.
	TargetNestEgg decimal.NullDecimal
	Splits        []Split
	// FamilyHSA selects the family HSA cap over the self-only default, matching
	// the retirement endpoint. The default understates headroom, which is the
	// conservative direction.
	FamilyHSA bool
}

// BucketResult is one bucket's outcome, carrying WHICH ENGINE produced it so
// the "show the arithmetic" panel renders the right formula. A debt bucket
// showing a compound-growth formula would be worse than showing nothing.
type BucketResult struct {
	AccountID   uuid.UUID  `json:"account_id"`
	Name        string     `json:"name"`
	Institution string     `json:"institution,omitempty"`
	Kind        BucketKind `json:"kind"`
	Treatment   string     `json:"treatment,omitempty"`

	// Allocated is what the split asked for; Applied is what actually went in
	// after eligibility. The pair is the answer to "why is my Roth line $0" —
	// one number could not say.
	AllocatedLump    decimal.Decimal `json:"allocated_lump"`
	AllocatedMonthly decimal.Decimal `json:"allocated_monthly"`
	AppliedLump      decimal.Decimal `json:"applied_lump"`
	AppliedMonthly   decimal.Decimal `json:"applied_monthly"`

	// Engine names the arithmetic: "compound", "amortization" or "accrual".
	Engine string `json:"engine"`
	// Formula is that arithmetic spelled out with this bucket's own numbers.
	// The credibility proposition of the whole surface: every projected figure
	// expands into the compounding, the cap and the assumption behind it.
	Formula string `json:"formula"`

	StartBalance decimal.Decimal `json:"start_balance"`

	// --- investment / cash ------------------------------------------------
	// ProjectedValue is the balance at the horizon. NOT a P50: this is the value
	// at the assumed return, and compounding at the mean is a different
	// statistic from the median of a simulation. Doc 33's fan chart renders
	// beside this figure, and calling both "P50" is how a user finds a
	// discrepancy nobody can explain.
	ProjectedValue decimal.Decimal `json:"projected_value"`
	Contributed    decimal.Decimal `json:"contributed"`
	Growth         decimal.Decimal `json:"growth"`
	// ReturnRate is the real annual rate actually used, as a fraction, and
	// RateIsHousehold says whether it came from the household assumption or from
	// this bucket's own override.
	ReturnRate      decimal.Decimal `json:"return_rate"`
	RateIsHousehold bool            `json:"rate_is_household"`
	// DepositAPY is the cash bucket's nominal yield as a percent, null when
	// nobody has entered one.
	DepositAPY *decimal.Decimal `json:"deposit_apy,omitempty"`

	// --- caps and eligibility ---------------------------------------------
	// CapGroup is the shared-limit group this account pools under
	// (networth.LimitGroup): two IRAs report the same group and share one cap.
	CapGroup string `json:"cap_group,omitempty"`
	// AnnualLimit is the IRS cap for the group, null where none applies (a
	// taxable brokerage, a 529) or where the year is unconfigured.
	AnnualLimit *decimal.Decimal `json:"annual_limit,omitempty"`
	// Eligibility is separate from the cap ON PURPOSE. See eligibility.go.
	Eligibility     string `json:"eligibility,omitempty"`
	EligibilityNote string `json:"eligibility_note,omitempty"`
	// EligibilitySpill is money the eligibility check refused, as distinct from
	// money the IRS cap refused (which lands in CapNotes). Both have to go
	// somewhere else, and a user needs to know which rule stopped it.
	EligibilitySpill decimal.Decimal `json:"eligibility_spill"`

	// --- debt --------------------------------------------------------------
	// Payoff at the CURRENT payment and WITH the plan's money, so the difference
	// is visible rather than asserted.
	PayoffBase *PayoffSummary `json:"payoff_base,omitempty"`
	PayoffPlan *PayoffSummary `json:"payoff_plan,omitempty"`
	// InterestAvoided is base total interest minus plan total interest — the
	// debt bucket's "return", and the only honest way to compare it against a
	// compounding bucket. Zero when either side does not amortize.
	InterestAvoided decimal.Decimal `json:"interest_avoided"`
	// MonthsSaved is the same comparison in time.
	MonthsSaved int `json:"months_saved"`

	// Notes are the caveats attached to THIS bucket. Rendered verbatim.
	Notes []string `json:"notes"`
}

// PayoffSummary is the part of goals.PayoffFeasibility worth carrying out of
// the allocator: the full schedule is thousands of points and the allocator's
// question is "when, and what does it cost".
type PayoffSummary struct {
	MonthlyPayment decimal.Decimal `json:"monthly_payment"`
	NeverPaysOff   bool            `json:"never_pays_off"`
	Months         int             `json:"months"`
	TotalInterest  decimal.Decimal `json:"total_interest"`
	PayoffDate     *time.Time      `json:"payoff_date,omitempty"`
	// MonthlyInterest is what makes "never" make sense to a human: "at $50/month
	// this card is never paid off — the interest alone is $61/month".
	MonthlyInterest decimal.Decimal `json:"monthly_interest"`
}

// GoalMapping is what the plan does to one goal.
type GoalMapping struct {
	GoalID uuid.UUID `json:"goal_id"`
	Name   string    `json:"name"`
	Kind   string    `json:"kind"`
	// PlanMonthly is the plan's monthly contribution to this goal's linked
	// account. A goal with no linked account gets zero and says so — the
	// allocator allocates to accounts, and a goal it cannot reach is not funded
	// by guessing which bucket the user meant.
	PlanMonthly     decimal.Decimal  `json:"plan_monthly"`
	Linked          bool             `json:"linked"`
	Target          decimal.Decimal  `json:"target"`
	Current         decimal.Decimal  `json:"current"`
	Remaining       decimal.Decimal  `json:"remaining"`
	RequiredMonthly decimal.Decimal  `json:"required_monthly"`
	Shortfall       decimal.Decimal  `json:"shortfall"`
	OnTrack         bool             `json:"on_track"`
	OpenEnded       bool             `json:"open_ended"`
	Achieved        bool             `json:"achieved"`
	MonthsLeft      int              `json:"months_left"`
	CompletionDate  *decimal.Decimal `json:"-"`
}

// HorizonFlag is the horizon-aware assignment warning: short-horizon money in a
// volatile bucket.
type HorizonFlag struct {
	GoalID     uuid.UUID `json:"goal_id"`
	GoalName   string    `json:"goal_name"`
	BucketName string    `json:"bucket_name"`
	MonthsLeft int       `json:"months_left"`
	Message    string    `json:"message"`
}

// Result is the whole answer.
type Result struct {
	HorizonYears int    `json:"horizon_years"`
	AsOf         string `json:"as_of"`
	HorizonDate  string `json:"horizon_date"`

	Lump    decimal.Decimal `json:"lump"`
	Monthly decimal.Decimal `json:"monthly"`
	// Unallocated is what the split did not place. Not an error — a household
	// may deliberately hold some back — but it is never silently absorbed into
	// another bucket.
	UnallocatedLump    decimal.Decimal `json:"unallocated_lump"`
	UnallocatedMonthly decimal.Decimal `json:"unallocated_monthly"`

	Buckets []BucketResult `json:"buckets"`

	// ProjectedAssets is investment + cash at the horizon. Debt is NOT added in:
	// a retired balance is not a portfolio, and summing them would produce a
	// number that means nothing.
	ProjectedAssets decimal.Decimal `json:"projected_assets"`
	// BaselineAssets is the same figure with the plan's money removed — what the
	// household reaches by doing nothing. Delta is the difference, which is the
	// plan's actual contribution to the answer.
	BaselineAssets decimal.Decimal `json:"baseline_assets"`
	Delta          decimal.Decimal `json:"delta"`
	// TotalInterestAvoided is the debt side of the same question.
	TotalInterestAvoided decimal.Decimal `json:"total_interest_avoided"`

	// TargetNestEgg and TargetMet are present only when the request set a target.
	TargetNestEgg *decimal.Decimal `json:"target_nest_egg,omitempty"`
	TargetMet     *bool            `json:"target_met,omitempty"`

	// CapNotes are the groups the IRS limits actually bound, from the projection
	// engine's own capping — not recomputed here.
	CapNotes         []networth.CapNote `json:"cap_notes"`
	LimitsYear       int                `json:"limits_year"`
	LimitsConfigured bool               `json:"limits_configured"`
	CapBasis         string             `json:"cap_basis"`

	Goals        []GoalMapping   `json:"goals"`
	College      []CollegeResult `json:"college"`
	HorizonFlags []HorizonFlag   `json:"horizon_flags"`

	// ExcludedAccounts are visible investment accounts with no confirmed tax
	// treatment: excluded from every projection and listed, never defaulted.
	ExcludedAccounts []string `json:"excluded_accounts"`

	// Notes are the plan-level caveats. Rendered verbatim; they are the
	// difference between a projection and a claim.
	Notes []string `json:"notes"`
	// Estimate is always true, in the payload so a client cannot render these
	// figures without the caveat travelling with them.
	Estimate bool   `json:"estimate"`
	Basis    string `json:"basis"`
}

const planBasis = "Every figure is in today's dollars and the return rates are REAL — already net of inflation. " +
	"This is the projected value at the assumed return, NOT a median simulated outcome, and not a forecast."

// Run computes a plan. It is a pure function of the baseline and the request:
// no I/O, no clock, and — the property that matters most — no mutation of the
// baseline it was handed.
//
// The order of operations is load-bearing:
//
//  1. Split the money into per-bucket amounts.
//  2. Apply ELIGIBILITY, which can zero or reduce a contribution. This happens
//     in the allocator rather than in the projection engine because it is not a
//     cap: it keys on filing status and MAGI, which limits.go knows nothing
//     about and has no business learning.
//  3. Hand the whole account set to ProjectRetirement, which applies the IRS
//     CAPS by pooling shared-limit groups. That rule stays where it lives.
//
// Doing 2 after 3 would let a plan be capped at $7,500 and then zeroed, losing
// the cap note; doing the caps here would put the pooling rule in two places.
func Run(b Baseline, req Request) (Result, error) {
	if req.HorizonYears < MinHorizonYears || req.HorizonYears > MaxHorizonYears {
		return Result{}, fmt.Errorf("horizon_years must be between %d and %d", MinHorizonYears, MaxHorizonYears)
	}
	if req.Lump.IsNegative() || req.Monthly.IsNegative() {
		return Result{}, errors.New("lump and monthly cannot be negative")
	}

	months := req.HorizonYears * 12
	horizonDate := b.Now.AddDate(req.HorizonYears, 0, 0)
	limits, limitsConfigured := networth.Limits(b.Now.Year())

	out := Result{
		HorizonYears:     req.HorizonYears,
		AsOf:             b.Now.Format(time.DateOnly),
		HorizonDate:      horizonDate.Format(time.DateOnly),
		Lump:             req.Lump.Round(2),
		Monthly:          req.Monthly.Round(2),
		Buckets:          []BucketResult{},
		CapNotes:         []networth.CapNote{},
		Goals:            []GoalMapping{},
		College:          []CollegeResult{},
		HorizonFlags:     []HorizonFlag{},
		ExcludedAccounts: b.UntaggedAccounts,
		Notes:            []string{},
		LimitsYear:       b.Now.Year(),
		LimitsConfigured: limitsConfigured,
		Estimate:         true,
		Basis:            planBasis,
	}
	if out.ExcludedAccounts == nil {
		out.ExcludedAccounts = []string{}
	}

	// ---- 1. split the money ------------------------------------------------
	lumpPctTotal, monthlyPctTotal := decimal.Zero, decimal.Zero
	type alloc struct {
		split   Split
		bucket  Bucket
		lump    decimal.Decimal
		monthly decimal.Decimal
	}
	allocs := make([]alloc, 0, len(req.Splits))
	seen := map[uuid.UUID]bool{}

	for _, s := range req.Splits {
		if s.LumpPct.IsNegative() || s.MonthlyPct.IsNegative() {
			return Result{}, errors.New("allocation percentages cannot be negative")
		}
		if seen[s.AccountID] {
			return Result{}, errors.New("each account may appear at most once in a split")
		}
		seen[s.AccountID] = true

		bucket, ok := b.BucketByID(s.AccountID)
		if !ok {
			// Either the account is not visible to this caller or it is an
			// untagged investment account. Both are refusals rather than silent
			// omissions: a line of the split that vanished would leave the
			// percentages summing to 100 while the money went nowhere.
			return Result{}, fmt.Errorf("account %s is not an allocatable bucket", s.AccountID)
		}
		lumpPctTotal = lumpPctTotal.Add(s.LumpPct)
		monthlyPctTotal = monthlyPctTotal.Add(s.MonthlyPct)
		allocs = append(allocs, alloc{
			split:   s,
			bucket:  bucket,
			lump:    req.Lump.Mul(s.LumpPct).Div(hundred).Round(2),
			monthly: req.Monthly.Mul(s.MonthlyPct).Div(hundred).Round(2),
		})
	}
	if lumpPctTotal.Sub(hundred).GreaterThan(pctTolerance) {
		return Result{}, errors.New("lump allocation percentages add up to more than 100%")
	}
	if monthlyPctTotal.Sub(hundred).GreaterThan(pctTolerance) {
		return Result{}, errors.New("monthly allocation percentages add up to more than 100%")
	}

	placedLump, placedMonthly := decimal.Zero, decimal.Zero
	for _, a := range allocs {
		placedLump = placedLump.Add(a.lump)
		placedMonthly = placedMonthly.Add(a.monthly)
	}
	out.UnallocatedLump = req.Lump.Sub(placedLump).Round(2)
	out.UnallocatedMonthly = req.Monthly.Sub(placedMonthly).Round(2)

	// ---- 2. eligibility, then build the projection input --------------------
	//
	// plans is a COPY. b.Plans is the household's real position and must come
	// out of this function exactly as it went in.
	plans := make([]networth.AccountPlan, len(b.Plans))
	copy(plans, b.Plans)
	planIndex := map[string]int{}
	for i, p := range plans {
		planIndex[p.ID] = i
	}

	results := make([]BucketResult, 0, len(allocs))
	eligibilityYearOK := true

	for _, a := range allocs {
		r := BucketResult{
			AccountID:        a.bucket.AccountID,
			Name:             a.bucket.Name,
			Institution:      a.bucket.Institution,
			Kind:             a.bucket.Kind,
			Treatment:        a.bucket.Treatment,
			AllocatedLump:    a.lump,
			AllocatedMonthly: a.monthly,
			AppliedLump:      a.lump,
			AppliedMonthly:   a.monthly,
			StartBalance:     a.bucket.Balance,
			Notes:            []string{},
		}

		if a.bucket.Kind == BucketInvestment {
			fullLimit, capped := networth.AnnualLimitFor(a.bucket.Treatment, b.Age, limits, req.FamilyHSA)
			if capped && limitsConfigured {
				l := fullLimit
				r.AnnualLimit = &l
				r.CapGroup = networth.LimitGroup(a.bucket.Treatment)
			}

			elig, yearOK := networth.EligibilityFor(
				a.bucket.Treatment, b.Now.Year(), b.FilingStatus, b.MAGI, fullLimit)
			if !yearOK {
				eligibilityYearOK = false
			}
			r.Eligibility, r.EligibilityNote = elig.Status, elig.Note

			// A cap is not permission. Where the household may not contribute,
			// the projected contribution is $0 — not the cap — and the refused
			// money is reported as spill so it can be placed somewhere legal.
			if capped && limitsConfigured {
				applied, spill := applyEligibility(
					elig, a.bucket.ExistingMonthly, r.AppliedLump, r.AppliedMonthly)
				r.AppliedLump, r.AppliedMonthly, r.EligibilitySpill = applied.lump, applied.monthly, spill
			}

			idx, ok := planIndex[a.bucket.AccountID.String()]
			if !ok {
				// Cannot happen: an investment bucket is built from a row that
				// also produced a plan. Refusing loudly beats projecting a
				// bucket the engine never saw.
				return Result{}, fmt.Errorf("internal: no projection plan for account %s", a.bucket.AccountID)
			}
			plans[idx].FirstYearContribution = plans[idx].FirstYearContribution.Add(r.AppliedLump)
			plans[idx].MonthlyContribution = plans[idx].MonthlyContribution.Add(r.AppliedMonthly)
			if a.split.RealReturnRate.Valid && a.split.RealReturnRate.Decimal.IsPositive() {
				plans[idx].RealReturnRate = a.split.RealReturnRate.Decimal
				r.ReturnRate = a.split.RealReturnRate.Decimal
			} else {
				r.ReturnRate = b.Assumptions.RealReturnRate
				r.RateIsHousehold = true
			}
		}

		results = append(results, r)
	}

	// ---- 3. project ---------------------------------------------------------
	assumptions := b.Assumptions
	assumptions.Months = months
	assumptions.FamilyHSA = req.FamilyHSA

	planProjection := networth.ProjectRetirement(plans, assumptions, b.Now)
	baseProjection := networth.ProjectRetirement(b.Plans, assumptions, b.Now)
	out.CapNotes = planProjection.CapNotes
	out.CapBasis = capBasis(b.Now.Year(), limitsConfigured)

	planFinal := lastByAccount(planProjection)
	baseFinal := lastByAccount(baseProjection)

	// ---- 4. per-bucket results ---------------------------------------------
	for i := range results {
		r := &results[i]
		switch r.Kind {
		case BucketInvestment:
			fillInvestment(r, planFinal, b, req.HorizonYears)
		case BucketDebt:
			bucket, _ := b.BucketByID(r.AccountID)
			fillDebt(r, bucket, months, b.Now)
			out.TotalInterestAvoided = out.TotalInterestAvoided.Add(r.InterestAvoided)
		case BucketCash:
			bucket, _ := b.BucketByID(r.AccountID)
			fillCash(r, bucket, months, b.InflationRate)
		}
		if r.Kind == BucketInvestment || r.Kind == BucketCash {
			out.ProjectedAssets = out.ProjectedAssets.Add(r.ProjectedValue)
		}
	}
	out.Buckets = results

	// The do-nothing comparison. Cash and debt buckets are unchanged in the
	// baseline by construction (their plan money is the only thing that moves
	// them), so the baseline side is the investment projection plus today's cash.
	for i := range results {
		r := results[i]
		switch r.Kind {
		case BucketInvestment:
			if pt, ok := baseFinal[r.AccountID.String()]; ok {
				out.BaselineAssets = out.BaselineAssets.Add(pt.Balance)
			}
		case BucketCash:
			out.BaselineAssets = out.BaselineAssets.Add(r.StartBalance)
		}
	}
	out.ProjectedAssets = out.ProjectedAssets.Round(2)
	out.BaselineAssets = out.BaselineAssets.Round(2)
	out.Delta = out.ProjectedAssets.Sub(out.BaselineAssets).Round(2)
	out.TotalInterestAvoided = out.TotalInterestAvoided.Round(2)

	if req.TargetNestEgg.Valid && req.TargetNestEgg.Decimal.IsPositive() {
		target := req.TargetNestEgg.Decimal.Round(2)
		met := out.ProjectedAssets.GreaterThanOrEqual(target)
		out.TargetNestEgg, out.TargetMet = &target, &met
	}

	// ---- 5. goals, college, horizon ----------------------------------------
	out.Goals = mapGoals(b, results)
	out.College = projectCollege(b, results, planProjection)
	out.HorizonFlags = flagShortHorizons(b, results, out.Goals)

	// ---- 6. plan-level caveats ---------------------------------------------
	if !limitsConfigured {
		out.Notes = append(out.Notes, fmt.Sprintf(
			"IRS limits for %d aren't configured (we have them through %d), so contributions were NOT capped. "+
				"Treat the headroom here as unavailable, not as unlimited.",
			b.Now.Year(), networth.LatestConfiguredYear()))
	}
	if !eligibilityYearOK {
		out.Notes = append(out.Notes, fmt.Sprintf(
			"Roth income limits for %d aren't configured (we have them through %d), so eligibility could not be checked.",
			b.Now.Year(), networth.LatestEligibilityYear()))
	}
	if !b.AgeKnown {
		out.Notes = append(out.Notes,
			"No age on file, so catch-up contributions are not applied. That understates headroom for anyone over 50 — "+
				"set a birthdate or a current age to fix it.")
	}
	if len(out.ExcludedAccounts) > 0 {
		out.Notes = append(out.Notes,
			"Some investment accounts have no confirmed tax treatment and are excluded from every projection here. "+
				"Tag them on the Investments page to include them.")
	}
	if out.UnallocatedLump.IsPositive() || out.UnallocatedMonthly.IsPositive() {
		out.Notes = append(out.Notes, fmt.Sprintf(
			"$%s of the lump and $%s a month are not allocated to any bucket. They are held, not invested — "+
				"nothing here assumes a home for them.",
			out.UnallocatedLump.StringFixed(2), out.UnallocatedMonthly.StringFixed(2)))
	}
	out.Notes = append(out.Notes,
		"This is the projected value at the assumed return. It is not a P50 — compounding at the mean and taking "+
			"the median of a simulation are different statistics, and they disagree over long horizons.")

	return out, nil
}

// applied is the pair applyEligibility returns.
type applied struct{ lump, monthly decimal.Decimal }

// applyEligibility reduces a contribution to what the household is actually
// ALLOWED to make, and returns the money it refused.
//
// The three statuses do three different things:
//
//   - ineligible — nothing goes in. The whole allocation spills.
//   - phased_out — the account's TOTAL planned annual contribution (what is
//     already being paid in, plus the plan's monthly, plus the lump) is held at
//     the reduced limit, scaled proportionally for the same reason
//     capContributions scales proportionally: the split is the user's decision.
//   - eligible / unknown — nothing changes. `unknown` deliberately projects the
//     full amount, WITH the caveat the caller renders, because refusing to
//     project would make the planner useless for every household that has not
//     filled in an optional field.
func applyEligibility(
	elig networth.Eligibility, existingMonthly, lump, monthly decimal.Decimal,
) (applied, decimal.Decimal) {
	switch elig.Status {
	case networth.EligibilityIneligible:
		return applied{decimal.Zero, decimal.Zero}, lump.Add(monthly.Mul(twelve)).Round(2)

	case networth.EligibilityPhasedOut:
		planned := existingMonthly.Add(monthly).Mul(twelve).Add(lump)
		if !planned.GreaterThan(elig.Limit) {
			return applied{lump, monthly}, decimal.Zero
		}
		// What is left for the PLAN after the existing contributions have taken
		// their share of the reduced limit. Existing contributions are not the
		// allocator's to reduce — they are already happening.
		room := elig.Limit.Sub(existingMonthly.Mul(twelve))
		if !room.IsPositive() {
			return applied{decimal.Zero, decimal.Zero}, lump.Add(monthly.Mul(twelve)).Round(2)
		}
		planPortion := lump.Add(monthly.Mul(twelve))
		scale := room.Div(planPortion)
		return applied{
				lump:    lump.Mul(scale).Round(2),
				monthly: monthly.Mul(scale).Round(2),
			},
			planPortion.Sub(room).Round(2)

	default:
		return applied{lump, monthly}, decimal.Zero
	}
}

// lastByAccount is the per-account state at the END of a projection, keyed by
// AccountPlan.ID. RetirementPoint.ByAccount is already keyed that way — this
// only picks the final point. There is deliberately no second projection loop.
func lastByAccount(p networth.RetirementProjection) map[string]networth.AccountPoint {
	if len(p.Points) == 0 {
		return map[string]networth.AccountPoint{}
	}
	return p.Points[len(p.Points)-1].ByAccount
}

// fillInvestment reads this bucket's series out of the projection. It does not
// compute a balance — ProjectRetirement already did, and a second computation
// here is how the bucket total and the household total would drift apart.
func fillInvestment(
	r *BucketResult, final map[string]networth.AccountPoint, b Baseline, horizonYears int,
) {
	r.Engine = "compound"
	pt, ok := final[r.AccountID.String()]
	if !ok {
		// The engine excluded it — the only route to that is an untagged
		// treatment, which BucketByID already refuses, so this is defensive.
		r.Notes = append(r.Notes, "This account was excluded from the projection, so no value is shown for it.")
		return
	}
	r.ProjectedValue = pt.Balance
	r.Contributed = pt.Contributed
	r.Growth = pt.Growth

	rateSource := "your household assumption"
	if !r.RateIsHousehold {
		rateSource = "this bucket's own assumed return"
	}
	r.Formula = fmt.Sprintf(
		"$%s starting balance, plus $%s in the first month and $%s a month, compounded at %s%%/yr real "+
			"(%s) over %d years. Growth is applied to the balance BEFORE each month's deposit, so money is not "+
			"credited with return it has not earned.",
		r.StartBalance.StringFixed(2), r.AppliedLump.StringFixed(2), r.AppliedMonthly.StringFixed(2),
		r.ReturnRate.Mul(hundred).Round(2).String(), rateSource, horizonYears)

	if networth.IsCustodial(r.Treatment) {
		r.Notes = append(r.Notes,
			"This is custodial money — it belongs to the beneficiary, not to the household's retirement, "+
				"and it stops compounding at the beneficiary's horizon.")
	}
	if r.EligibilitySpill.IsPositive() {
		r.Notes = append(r.Notes, fmt.Sprintf(
			"$%s could not go here: %s", r.EligibilitySpill.StringFixed(2), r.EligibilityNote))
	}
	_ = b
}

// fillDebt amortizes the debt twice — at today's payment and with the plan's
// money on top — so the difference between them is computed rather than
// asserted. goals.ComputePayoff is the same function the Goals page calls.
func fillDebt(r *BucketResult, bucket Bucket, months int, now time.Time) {
	r.Engine = "amortization"
	r.ReturnRate = decimal.Zero

	apr := bucket.Terms.APR
	if bucket.Terms.APRSource == goals.TermSourceNone {
		// Not zero: unknown. ComputePayoff treats a zero APR as interest-free,
		// which is a real answer for a 0% promotional card and a badly wrong one
		// for a card whose rate nobody has entered — so the caveat travels.
		apr = decimal.Zero
		r.Notes = append(r.Notes,
			"No APR is known for this account, so the payoff below assumes it is interest-free. "+
				"Enter the rate on the Accounts page to get a real figure.")
	}
	if bucket.Terms.PaymentSource == goals.TermSourceNone && !r.AppliedMonthly.IsPositive() {
		r.Notes = append(r.Notes,
			"No minimum payment is known for this account and the plan adds none, so there is nothing to amortize.")
		r.Formula = "No payment known — no payoff schedule."
		return
	}

	base := goals.ComputePayoff(bucket.Balance, apr, bucket.Terms.Payment, nil, now)
	// The lump pays the balance DOWN on day one; the monthly is added to the
	// payment. Both are what a household would actually do with the money.
	planBalance := bucket.Balance.Sub(r.AppliedLump)
	if planBalance.IsNegative() {
		planBalance = decimal.Zero
	}
	plan := goals.ComputePayoff(planBalance, apr, bucket.Terms.Payment.Add(r.AppliedMonthly), nil, now)

	r.PayoffBase = summarise(base)
	r.PayoffPlan = summarise(plan)
	r.ProjectedValue = decimal.Zero // a debt is not an asset; never summed in

	if !base.NeverPaysOff && !plan.NeverPaysOff {
		r.InterestAvoided = base.TotalInterest.Sub(plan.TotalInterest).Round(2)
		r.MonthsSaved = base.Months - plan.Months
	}

	r.Formula = fmt.Sprintf(
		"$%s balance at %s%% APR. Each month: interest = balance × APR ÷ 12 (rounded to the cent, as a lender "+
			"posts it), then the payment is applied. Today's payment is $%s; the plan makes it $%s and pays "+
			"$%s off up front.",
		bucket.Balance.StringFixed(2), apr.StringFixed(2),
		bucket.Terms.Payment.StringFixed(2),
		bucket.Terms.Payment.Add(r.AppliedMonthly).StringFixed(2),
		r.AppliedLump.StringFixed(2))

	if plan.NeverPaysOff {
		r.Notes = append(r.Notes, fmt.Sprintf(
			"Even with the plan's money this debt never pays off — the interest alone is $%s a month.",
			plan.MonthlyInterest.StringFixed(2)))
	}
	_ = months
}

func summarise(f goals.PayoffFeasibility) *PayoffSummary {
	return &PayoffSummary{
		MonthlyPayment:  f.MonthlyPayment.Round(2),
		NeverPaysOff:    f.NeverPaysOff,
		Months:          f.Months,
		TotalInterest:   f.TotalInterest.Round(2),
		PayoffDate:      f.PayoffDate,
		MonthlyInterest: f.MonthlyInterest.Round(2),
	}
}

// mapGoals runs each goal's feasibility against the plan's contribution to its
// LINKED ACCOUNT. goals.Compute is the same function the Goals page uses.
//
// Debt-payoff goals are skipped: Compute is accumulation arithmetic with no
// notion of interest, and running it over a payoff goal reads the debt's
// balance as progress. Debt is answered by its bucket, with amortization.
func mapGoals(b Baseline, results []BucketResult) []GoalMapping {
	byAccount := map[uuid.UUID]decimal.Decimal{}
	for _, r := range results {
		byAccount[r.AccountID] = byAccount[r.AccountID].Add(r.AppliedMonthly)
	}

	out := make([]GoalMapping, 0, len(b.Goals))
	for _, g := range b.Goals {
		if g.Kind != "savings" && g.Kind != "college" {
			continue
		}
		m := GoalMapping{
			GoalID: g.ID, Name: g.Name, Kind: g.Kind,
			Target: g.Target.Round(2), Current: g.Current.Round(2),
		}
		if g.AccountID != nil {
			m.Linked = true
			m.PlanMonthly = byAccount[*g.AccountID].Round(2)
		}
		f := goals.Compute(g.Target, g.Current, m.PlanMonthly, g.TargetDate, b.Now)
		m.Remaining = f.Remaining.Round(2)
		m.RequiredMonthly = f.RequiredMonthly
		m.Shortfall = f.Shortfall
		m.OnTrack = f.OnTrack
		m.OpenEnded = f.OpenEnded
		m.Achieved = f.Achieved
		m.MonthsLeft = f.MonthsLeft
		out = append(out, m)
	}
	return out
}

// shortHorizonMonths is where "short horizon" starts.
//
// Three years, which is the conventional line and is stated rather than hidden:
// a goal due inside it cannot ride out a drawdown, so putting its money in a
// volatile bucket is a risk the household should have chosen deliberately.
const shortHorizonMonths = 36

// flagShortHorizons is the horizon-aware assignment check: a goal due soon whose
// money the plan is routing into a volatile bucket.
//
// It FLAGS; it does not refuse. The engine computes outcomes for the split the
// user chose, and silently reallocating one would be the auto-optimisation this
// doc puts out of scope. Per-bucket volatility arrives with doc 33 — until then
// "volatile" means "an investment bucket", which is the honest coarse answer
// rather than a σ nobody computed.
func flagShortHorizons(b Baseline, results []BucketResult, mappings []GoalMapping) []HorizonFlag {
	investment := map[uuid.UUID]string{}
	for _, r := range results {
		if r.Kind == BucketInvestment && (r.AppliedLump.IsPositive() || r.AppliedMonthly.IsPositive()) {
			investment[r.AccountID] = r.Name
		}
	}

	byGoal := map[uuid.UUID]GoalMapping{}
	for _, m := range mappings {
		byGoal[m.GoalID] = m
	}

	out := []HorizonFlag{}
	for _, g := range b.Goals {
		if g.AccountID == nil || g.TargetDate == nil {
			continue
		}
		name, ok := investment[*g.AccountID]
		if !ok {
			continue
		}
		m, ok := byGoal[g.ID]
		if !ok || m.Achieved || m.MonthsLeft <= 0 || m.MonthsLeft >= shortHorizonMonths {
			continue
		}
		msg := fmt.Sprintf(
			"%q is due in %d months, and this plan puts money for it into %s — an investment bucket that can be "+
				"down when the money is needed. Short-horizon money usually belongs somewhere liquid.",
			g.Name, m.MonthsLeft, name)
		if b.RiskFloor.Valid {
			msg += fmt.Sprintf(" Your stated drawdown floor is %s%%.", b.RiskFloor.Decimal.StringFixed(2))
		}
		out = append(out, HorizonFlag{
			GoalID: g.ID, GoalName: g.Name, BucketName: name,
			MonthsLeft: m.MonthsLeft, Message: msg,
		})
	}
	return out
}

// capBasis names the modelling choice the cap figures rest on, so it is the
// user's knowledge rather than their surprise.
//
// Caps are applied at the CURRENT year's limits for the whole horizon. The IRS
// indexes these limits to inflation, and every figure in this app is in real
// (today's) dollars, so a constant real cap is closer to the truth than a
// nominal one frozen in this year — but it is a choice, and it is named.
func capBasis(year int, configured bool) string {
	if !configured {
		return fmt.Sprintf(
			"Contribution caps were NOT applied: IRS limits for %d aren't configured (we have them through %d).",
			year, networth.LatestConfiguredYear())
	}
	return fmt.Sprintf(
		"Contribution caps applied at %d limits, held constant in real terms across the whole horizon. "+
			"Accounts sharing a limit (two 401(k)s, a traditional and a Roth IRA) are pooled before the cap is applied.",
		year)
}
