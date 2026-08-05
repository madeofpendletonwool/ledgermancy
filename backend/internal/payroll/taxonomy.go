// Package payroll owns the pre-tax side of the ledger: what an employer paid,
// what came out before the deposit landed, and every figure derived from the
// difference.
//
// Two rules run through the whole package and are worth stating once here.
//
// Nothing in this package computes an obligation. Every figure it produces is a
// SUMMARY OF WHAT A STUB SAYS — an effective rate is a division of two numbers
// the employer already printed, and a W-2 mapping is a re-labelling of stored
// lines. The package never predicts withholding, never applies a bracket, and
// never tells anybody what they owe. docs/plans/23-paystub-income.md puts the
// same rule as "the data-gathering is already done", not "we file for you", and
// the exported summary says so in the file itself because it will be printed
// and read out of context.
//
// And nothing here reaches a network. The PDF importer reads the text layer on
// this machine (see pdftext.go); an unreadable stub is typed in by hand. A
// paystub is more sensitive than the tax documents the vault already refuses to
// send to an AI provider, so it is not sent to one either.
package payroll

// Category is a paystub line's kind. The values match the CHECK constraint on
// paystub_lines.category exactly — this list and that constraint are one fact
// stored twice, so a value added to either must be added to both.
type Category string

const (
	CatFederalIncomeTax Category = "federal_income_tax"
	CatStateIncomeTax   Category = "state_income_tax"
	CatLocalIncomeTax   Category = "local_income_tax"
	CatSocialSecurity   Category = "fica_social_security"
	CatMedicare         Category = "fica_medicare"
	CatMedicareSurtax   Category = "medicare_surcharge"

	Cat401kPreTax Category = "401k_pre_tax"
	Cat401kRoth   Category = "401k_roth"
	Cat401kMatch  Category = "401k_employer_match"
	CatIRAPreTax  Category = "ira_pre_tax"
	CatIRARoth    Category = "ira_roth"
	CatHSA        Category = "hsa"
	CatFSA        Category = "fsa"

	CatHealthPremium Category = "health_premium"
	CatDental        Category = "dental"
	CatVision        Category = "vision"
	CatLifeInsurance Category = "life_insurance"
	CatDisability    Category = "disability"

	CatGarnishment       Category = "garnishment"
	CatCommuter          Category = "commuter"
	CatDependentCare     Category = "dependent_care"
	CatTuitionAssistance Category = "tuition_assistance"
	CatOther             Category = "other"
)

// WageBasis says which taxable wage base a PRE-TAX line reduces.
//
// This is the distinction that makes a W-2 summary either right or subtly,
// expensively wrong, and it is not a detail the category name gives away:
//
//   - A 401(k) deferral comes out before federal income tax but NOT before
//     Social Security and Medicare. That is why box 1 and boxes 3/5 on a real
//     W-2 differ for anybody contributing to a 401(k), and it is the single
//     most common thing a naive "pre-tax means untaxed" model gets wrong.
//   - A Section 125 cafeteria item — medical, dental, vision, FSA, HSA,
//     commuter, dependent care — comes out before BOTH.
//
// The line's own pre_tax flag decides whether a line reduces anything at all;
// this decides what it reduces once it does. Both are needed: the same benefit
// is pre-tax under a cafeteria plan and post-tax without one, and only the stub
// knows which.
type WageBasis int

const (
	// ReducesNothing is a line that never lowers a wage base: taxes themselves,
	// Roth deferrals, garnishments, post-tax insurance.
	ReducesNothing WageBasis = iota
	// ReducesFederalOnly is a retirement deferral — federal taxable wages fall,
	// FICA wages do not.
	ReducesFederalOnly
	// ReducesFederalAndFICA is a Section 125 cafeteria benefit.
	ReducesFederalAndFICA
)

// spec is everything the rest of the package needs to know about a category.
type spec struct {
	// Label is the wording shown when the employer's own label is not used —
	// on the manual-entry form, and in the "where your paycheck went" chart.
	Label string
	// Group buckets a line for display and for the paycheck breakdown. One of
	// "tax", "retirement", "health", "insurance", "other".
	Group string
	// IsTax marks the lines that make up the effective tax rate numerator.
	// Only genuine taxes: a health premium is money gone, but calling it tax
	// would inflate a number people compare against published rates.
	IsTax bool
	// Basis is the wage base this category reduces WHEN the line is pre-tax.
	Basis WageBasis
	// PreTaxByDefault seeds the manual-entry form and the PDF importer. It is
	// only ever a default — the stored line is what counts.
	PreTaxByDefault bool
	// EmployerOnly marks money the employer adds on top of gross rather than
	// takes out of it. Excluded from the balance equation, and the basis of the
	// total-compensation figure.
	EmployerOnly bool
	// LimitGroup ties the category to an IRS annual cap, using the same group
	// names networth's limitGroup uses ("401k", "ira", "hsa"). Empty means no
	// annual cap applies. Shared with the retirement engine on purpose: the
	// limits table lives in internal/networth and this package reads it rather
	// than keeping a second copy that can drift a year out of date.
	LimitGroup string
	// W2Box12 is the code this category reports under in box 12 of a W-2, where
	// one applies. Informational: the summary labels the box, it does not file
	// anything.
	W2Box12 string
}

// specs is the whole taxonomy. Every category in the migration's CHECK
// constraint has an entry; SpecFor panics for anything else, because a category
// that reached the database without a spec would silently contribute to no
// total at all — a stub that balances on screen and under-reports every derived
// figure is worse than a crash on the line that introduced it.
var specs = map[Category]spec{
	CatFederalIncomeTax: {Label: "Federal income tax", Group: "tax", IsTax: true},
	CatStateIncomeTax:   {Label: "State income tax", Group: "tax", IsTax: true},
	CatLocalIncomeTax:   {Label: "Local income tax", Group: "tax", IsTax: true},
	CatSocialSecurity:   {Label: "Social Security", Group: "tax", IsTax: true},
	CatMedicare:         {Label: "Medicare", Group: "tax", IsTax: true},
	CatMedicareSurtax:   {Label: "Additional Medicare", Group: "tax", IsTax: true},

	Cat401kPreTax: {
		Label: "401(k) pre-tax", Group: "retirement",
		Basis: ReducesFederalOnly, PreTaxByDefault: true,
		LimitGroup: "401k", W2Box12: "D",
	},
	Cat401kRoth: {
		Label: "401(k) Roth", Group: "retirement",
		// A Roth deferral is made from already-taxed pay, so it reduces no wage
		// base — but it DOES count against the same elective-deferral limit as
		// the pre-tax half, which is why it carries the same LimitGroup.
		Basis: ReducesNothing, LimitGroup: "401k", W2Box12: "AA",
	},
	Cat401kMatch: {
		Label: "401(k) employer match", Group: "retirement",
		EmployerOnly: true,
		// Deliberately no LimitGroup. The employer match does not count against
		// the employee's elective deferral limit — it falls under the much
		// higher section 415(c) total-additions cap, which this app does not
		// model. Putting it in the "401k" group would eat the user's headroom
		// with somebody else's money.
	},
	CatIRAPreTax: {
		Label: "Traditional IRA", Group: "retirement",
		// An IRA contribution routed through payroll is a post-tax deduction:
		// the deductibility is settled on the 1040, not by the employer, so the
		// stub's own taxable wages are unaffected.
		Basis: ReducesNothing, LimitGroup: "ira",
	},
	CatIRARoth: {
		Label: "Roth IRA", Group: "retirement",
		Basis: ReducesNothing, LimitGroup: "ira",
	},
	CatHSA: {
		Label: "HSA", Group: "health",
		Basis: ReducesFederalAndFICA, PreTaxByDefault: true,
		LimitGroup: "hsa", W2Box12: "W",
	},
	CatFSA: {
		Label: "FSA", Group: "health",
		Basis: ReducesFederalAndFICA, PreTaxByDefault: true,
	},

	CatHealthPremium: {
		Label: "Health insurance", Group: "health",
		Basis: ReducesFederalAndFICA, PreTaxByDefault: true,
	},
	CatDental: {
		Label: "Dental", Group: "health",
		Basis: ReducesFederalAndFICA, PreTaxByDefault: true,
	},
	CatVision: {
		Label: "Vision", Group: "health",
		Basis: ReducesFederalAndFICA, PreTaxByDefault: true,
	},
	CatLifeInsurance: {Label: "Life insurance", Group: "insurance"},
	CatDisability:    {Label: "Disability insurance", Group: "insurance"},

	CatGarnishment: {Label: "Garnishment", Group: "other"},
	CatCommuter: {
		Label: "Commuter", Group: "other",
		Basis: ReducesFederalAndFICA, PreTaxByDefault: true,
	},
	CatDependentCare: {
		Label: "Dependent care", Group: "other",
		Basis: ReducesFederalAndFICA, PreTaxByDefault: true,
	},
	CatTuitionAssistance: {
		Label: "Tuition assistance", Group: "other", EmployerOnly: true,
	},
	CatOther: {Label: "Other", Group: "other"},
}

// Categories returns every category in a stable, display-ready order: taxes
// first, then retirement, then health, insurance and everything else. The
// manual-entry form is built from this, so the order is the form's order.
//
// Not derived from the map — Go map iteration is randomised, and a form whose
// fields move between page loads is its own bug.
func Categories() []Category {
	return []Category{
		CatFederalIncomeTax, CatStateIncomeTax, CatLocalIncomeTax,
		CatSocialSecurity, CatMedicare, CatMedicareSurtax,
		Cat401kPreTax, Cat401kRoth, Cat401kMatch,
		CatIRAPreTax, CatIRARoth,
		CatHSA, CatFSA,
		CatHealthPremium, CatDental, CatVision,
		CatLifeInsurance, CatDisability,
		CatGarnishment, CatCommuter, CatDependentCare,
		CatTuitionAssistance, CatOther,
	}
}

// Valid reports whether a string is a known category. The API validates request
// input through this rather than letting Postgres' CHECK constraint be the
// first thing to notice, so a bad payload is a 400 naming the field instead of
// a 500 naming a constraint.
func Valid(c string) bool {
	_, ok := specs[Category(c)]
	return ok
}

// SpecFor returns a category's spec. It panics on an unknown category; see the
// comment on specs for why that is the right failure.
func SpecFor(c Category) spec {
	s, ok := specs[c]
	if !ok {
		panic("payroll: no spec for category " + string(c))
	}
	return s
}

// Descriptor is the wire form of a category, so the frontend's manual-entry
// form and pre-tax defaults come from this file rather than from a second list
// maintained in TypeScript.
type Descriptor struct {
	Category        string `json:"category"`
	Label           string `json:"label"`
	Group           string `json:"group"`
	IsTax           bool   `json:"is_tax"`
	PreTaxByDefault bool   `json:"pre_tax_by_default"`
	EmployerOnly    bool   `json:"employer_only"`
	// PreTaxLocked marks a category whose pre-tax status is not the user's to
	// choose: a tax is never a pre-tax deduction of itself, and a Roth deferral
	// is post-tax by definition. The form renders these without the toggle
	// rather than offering a choice that would be rejected.
	PreTaxLocked bool   `json:"pre_tax_locked"`
	LimitGroup   string `json:"limit_group"`
}

// Taxonomy returns the descriptor list, in Categories() order.
func Taxonomy() []Descriptor {
	out := make([]Descriptor, 0, len(specs))
	for _, c := range Categories() {
		s := SpecFor(c)
		out = append(out, Descriptor{
			Category:        string(c),
			Label:           s.Label,
			Group:           s.Group,
			IsTax:           s.IsTax,
			PreTaxByDefault: s.PreTaxByDefault,
			EmployerOnly:    s.EmployerOnly,
			PreTaxLocked:    s.Basis == ReducesNothing,
			LimitGroup:      s.LimitGroup,
		})
	}
	return out
}

// PayFrequency is how often an employer pays. Values match the CHECK constraint
// on employers.pay_frequency.
type PayFrequency string

const (
	Weekly      PayFrequency = "weekly"
	Biweekly    PayFrequency = "biweekly"
	Semimonthly PayFrequency = "semimonthly"
	Monthly     PayFrequency = "monthly"
)

// periodsPerYear is how many pay dates a frequency produces in a calendar year.
//
// Biweekly is 26 and not 26.0714 on purpose. A biweekly year is really 26 or 27
// pay dates depending on where the calendar falls, and the headroom figure this
// feeds says "N pay periods left" — a sentence that cannot carry a fraction.
// Rounding down is the conservative direction: it says there is less runway to
// spread a contribution over than there may turn out to be, which errs towards
// contributing sooner rather than towards missing the cap.
var periodsPerYear = map[PayFrequency]int{
	Weekly:      52,
	Biweekly:    26,
	Semimonthly: 24,
	Monthly:     12,
}

// PeriodsPerYear returns the pay dates a frequency produces in a year, and
// false for an unrecognised frequency. Callers surface the false rather than
// substituting a guess: "we cannot say how many periods are left" is honest,
// and a headroom-per-period figure computed against a made-up cadence is not.
func PeriodsPerYear(f PayFrequency) (int, bool) {
	n, ok := periodsPerYear[f]
	return n, ok
}

// ValidFrequency reports whether a string is a known pay frequency.
func ValidFrequency(f string) bool {
	_, ok := periodsPerYear[PayFrequency(f)]
	return ok
}
