package payroll

import (
	"regexp"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Turning extracted PDF lines into a PROPOSED paystub.
//
// Nothing in this file writes anything. It produces fields for a form a human
// confirms, exactly as the receipt extractor does — and for the same reason,
// stated more sharply by doc 23: a misread YTD figure that flows into a tax
// summary is expensive, so the reading is always a suggestion and the human is
// always the one who commits it.
//
// The matching is deliberately conservative. A line whose label is not
// recognised is returned as unmatched text for the user to classify, never
// guessed into the nearest category — an unrecognised deduction filed as
// "other" is invisible, whereas one sitting in a review list gets fixed.

// Proposal is a parsed stub awaiting review. Every money field is optional
// because a partial read is normal and useful: three of five fields filled in
// still saves most of the typing.
type Proposal struct {
	// EmployerName is a hint taken from the top of the page, not an identity.
	// The user picks or creates the employer; this only pre-fills the box.
	EmployerName string
	PayDate      *time.Time
	PeriodStart  *time.Time
	PeriodEnd    *time.Time

	Gross    decimal.NullDecimal
	Net      decimal.NullDecimal
	YTDGross decimal.NullDecimal
	YTDNet   decimal.NullDecimal

	Lines []Line

	// Unmatched are lines that carried money but no label this parser knows.
	// Surfaced rather than dropped: a deduction the parser cannot name is
	// exactly the one the user needs to see, because it is the one that will
	// make the stub fail to balance.
	Unmatched []string

	// Warnings are things worth saying out loud on the review screen. They are
	// never fatal — the user can still correct and confirm.
	Warnings []string
}

// Balanced reports whether the proposal's own figures reconcile, so the review
// screen can say "this adds up" before anything is written. A proposal that
// does not balance is still returned; refusing to show a partial read would
// mean refusing to help with the stub that most needs it.
func (p Proposal) Balanced() bool {
	if !p.Gross.Valid || !p.Net.Valid {
		return false
	}
	return p.Stub().Balances()
}

// Stub is the proposal as the arithmetic sees it, with an unread gross or net
// treated as zero.
//
// Shared so the review screen's "X is unaccounted for" and this file's own
// balance warning are the same computation. An unread figure being zero is only
// safe because Balanced() refuses a proposal missing either one — Residual() on
// its own would otherwise report the whole of gross as a shortfall.
func (p Proposal) Stub() Stub {
	s := Stub{Lines: p.Lines}
	if p.Gross.Valid {
		s.Gross = p.Gross.Decimal
	}
	if p.Net.Valid {
		s.Net = p.Net.Decimal
	}
	return s
}

// amountPattern matches a money figure as a payroll provider prints it: an
// optional currency symbol and sign, thousands separators, and always exactly
// two decimal places.
//
// The mandatory decimals are what make this safe to run over a whole page. A
// paystub is full of bare integers — hours, rates, employee numbers, dates —
// and a looser pattern would pick up "40" from an hours column and propose it
// as a deduction.
var amountPattern = regexp.MustCompile(`-?\$?\s?-?\d{1,3}(?:,\d{3})*\.\d{2}\b|-?\$?\s?-?\d+\.\d{2}\b`)

// datePattern matches the date forms US payroll providers print.
var datePattern = regexp.MustCompile(`\b(\d{1,2}/\d{1,2}/\d{2,4}|\d{4}-\d{2}-\d{2}|[A-Z][a-z]{2,8}\s+\d{1,2},?\s+\d{4})\b`)

// dateLayouts are tried in order against a matched date string.
var dateLayouts = []string{
	"01/02/2006", "1/2/2006", "01/02/06", "1/2/06",
	"2006-01-02",
	"January 2, 2006", "Jan 2, 2006", "January 2 2006", "Jan 2 2006",
}

// rule maps a label fragment onto a category. Order is everything: the list is
// scanned top to bottom and the first hit wins, so "Roth 401(k)" must be tested
// before "401(k)" and "Dependent Care FSA" before "FSA". Reordering this slice
// changes what stubs parse to.
type rule struct {
	// needles are lowercase fragments; any one of them matching the label is a
	// hit for this rule.
	needles  []string
	category Category
	// employer marks a rule that always produces an employer-paid line.
	employer bool
}

var lineRules = []rule{
	// Employer contributions first: "401(k) Employer Match" contains "401(k)"
	// and would otherwise be filed as the employee's own deferral, which both
	// breaks the balance check and eats the user's contribution headroom with
	// money that does not count against it.
	{needles: []string{"employer match", "er match", "company match", "match 401", "401k match", "401(k) match", "employer contribution"}, category: Cat401kMatch, employer: true},
	{needles: []string{"tuition"}, category: CatTuitionAssistance, employer: true},

	// Taxes.
	{needles: []string{"additional medicare", "addl medicare", "medicare surtax", "medicare surcharge"}, category: CatMedicareSurtax},
	{needles: []string{"medicare", "fica-med", "fica med"}, category: CatMedicare},
	{needles: []string{"social security", "soc sec", "socsec", "oasdi", "fica-ss", "fica ss"}, category: CatSocialSecurity},
	{needles: []string{"federal income tax", "fed income tax", "federal withholding", "fed withholding", "federal tax", "fed tax", "fed w/h", "fed w4", "fitw"}, category: CatFederalIncomeTax},
	{needles: []string{"local income tax", "local tax", "city tax", "county tax", "school district"}, category: CatLocalIncomeTax},
	{needles: []string{"state income tax", "state tax", "state withholding", "state w/h", "sdi", "state disability"}, category: CatStateIncomeTax},

	// Retirement. Roth before pre-tax; the plan-type synonyms (403(b), 457)
	// share the 401(k) elective limit and so share its category.
	{needles: []string{"roth ira"}, category: CatIRARoth},
	{needles: []string{"roth 401", "401k roth", "401(k) roth", "roth 403", "roth deferral", "roth contribution"}, category: Cat401kRoth},
	{needles: []string{"401k", "401(k)", "403b", "403(b)", "457", "pre-tax deferral", "pretax deferral", "retirement savings"}, category: Cat401kPreTax},
	{needles: []string{"ira"}, category: CatIRAPreTax},

	// Health. Dependent care before FSA, HSA before the generic health rules.
	{needles: []string{"dependent care", "dep care", "dcfsa"}, category: CatDependentCare},
	{needles: []string{"hsa", "health savings"}, category: CatHSA},
	{needles: []string{"fsa", "flex spend", "flexible spending"}, category: CatFSA},
	{needles: []string{"dental"}, category: CatDental},
	{needles: []string{"vision"}, category: CatVision},
	{needles: []string{"medical", "health", "hlth", "ppo", "hmo", "hdhp"}, category: CatHealthPremium},

	// Insurance and everything else. Employer-paid basic life is tested before
	// employee-paid supplemental life, because "Group Term Life" and "Basic
	// Life" are coverage the employer provides and lists under a Taxable
	// Benefits section rather than a deductions one. Filing them as employee
	// deductions makes the stub fail to balance by exactly the premium — the
	// same failure a missed line produces, which is a loud but misleading
	// warning. Supplemental/voluntary life is what the employee elected and
	// pays for out of pay.
	{needles: []string{"group term life", "basic life", "gtl", "basic life ins", "basic life insurance"}, category: CatLifeInsurance, employer: true},
	{needles: []string{"supp life", "supplemental life", "voluntary life", "life ins", "life insurance"}, category: CatLifeInsurance},
	{needles: []string{"disability", "std ins", "ltd ins", "short term dis", "long term dis"}, category: CatDisability},
	{needles: []string{"garnish", "child support", "tax levy", "wage levy"}, category: CatGarnishment},
	{needles: []string{"commuter", "transit", "parking"}, category: CatCommuter},
}

// headerRules pick the stub's own totals out of the page. Separate from
// lineRules because these are not deductions and must never end up as one — a
// "Net Pay" line filed as a deduction would make every stub fail to balance by
// exactly the take-home amount.
var headerRules = []struct {
	needles []string
	field   string
}{
	{[]string{"net pay", "net check", "net amount", "check amount", "take home", "net earnings"}, "net"},
	{[]string{"gross pay", "gross earnings", "gross wages", "total gross", "total earnings", "gross income"}, "gross"},
}

// ParseProposal reads a paystub's extracted lines into a reviewable proposal.
func ParseProposal(lines []string) Proposal {
	var p Proposal

	p.EmployerName = guessEmployer(lines)
	p.PayDate, p.PeriodStart, p.PeriodEnd = parseDates(lines)

	for _, raw := range lines {
		line := RedactIdentifiers(raw)
		amounts := findAmounts(line)
		if len(amounts) == 0 {
			continue
		}
		label := strings.TrimSpace(labelPart(line))
		if label == "" {
			continue
		}
		lower := strings.ToLower(label)

		if field, ok := matchHeader(lower); ok {
			switch field {
			case "gross":
				p.Gross = firstAmount(amounts)
				p.YTDGross = secondAmount(amounts)
			case "net":
				p.Net = firstAmount(amounts)
				p.YTDNet = secondAmount(amounts)
			}
			continue
		}

		cat, employer, ok := matchLine(lower)
		if !ok {
			// Only worth showing the user if it plausibly IS a deduction. A
			// page full of hours-and-rates rows would otherwise drown the
			// review screen in noise.
			if looksLikeDeductionRow(lower) {
				p.Unmatched = append(p.Unmatched, collapseSpaces(line))
			}
			continue
		}

		amount := firstAmount(amounts)
		if !amount.Valid || amount.Decimal.IsZero() {
			continue
		}
		spec := SpecFor(cat)
		p.Lines = append(p.Lines, Line{
			Category: cat,
			// The employer's own wording, capped and redacted. Kept so a stub
			// can be checked against the paper it came from — "Med Pre AFTC"
			// means something to the person who receives it and nothing to the
			// taxonomy.
			Label:      clipLabel(label),
			Amount:     amount.Decimal.Abs(),
			YTDAmount:  secondAmount(amounts),
			PreTax:     spec.Basis != ReducesNothing && spec.PreTaxByDefault,
			IsEmployer: employer || spec.EmployerOnly,
		})
	}

	p.Warnings = warnings(p)
	return p
}

// matchHeader tests the gross/net rules against a label.
func matchHeader(lower string) (string, bool) {
	for _, h := range headerRules {
		for _, n := range h.needles {
			if strings.Contains(lower, n) {
				return h.field, true
			}
		}
	}
	return "", false
}

// matchLine tests the deduction rules against a label, first hit wins.
func matchLine(lower string) (Category, bool, bool) {
	for _, r := range lineRules {
		for _, n := range r.needles {
			if strings.Contains(lower, n) {
				return r.category, r.employer, true
			}
		}
	}
	return "", false, false
}

// looksLikeDeductionRow is the filter on what reaches the unmatched list. A
// deduction row names something; an hours-and-rates row is mostly digits.
func looksLikeDeductionRow(lower string) bool {
	letters := 0
	for _, r := range lower {
		if r >= 'a' && r <= 'z' {
			letters++
		}
	}
	if letters < 4 {
		return false
	}
	for _, n := range nonDeductionNeedles {
		if strings.Contains(lower, n) {
			return false
		}
	}
	return true
}

// nonDeductionNeedles are rows that carry money but are not deductions, and so
// must not reach the "we could not classify this" list.
//
// Only rows that matched no rule in lineRules get this far, so nothing here can
// steal a real deduction — "401(k) Employer Match" contains "employer" but was
// claimed by lineRules several steps earlier.
//
// A letter count alone is not enough: "Hours 80.00 Rate 37.50" is nine letters
// of pure payroll furniture, and a review list padded with earnings detail is
// one nobody reads, which defeats the point of surfacing the genuinely
// unrecognised line sitting in it.
//
// "total" is the sharpest entry and the reason this is worth doing carefully. A
// "Total Deductions 1,071.85" row offered as an unclassified line invites the
// user to add it — which double-counts every deduction on the stub and produces
// a paystub that fails to balance by the whole deduction total.
//
// "deduction" and "taxes" extend the same guard to section SUBTOTAL rows a
// provider prints without the word "total": "Pre-Tax Deductions 934.71" and
// "Taxes 894.84" are the sums of the lines beneath them, and offering either as
// an unclassified line double-counts every line in that section. They are safe
// because a real deduction line ("FSA Deduction", "Local Taxes") is claimed by
// lineRules before it ever reaches this filter.
var nonDeductionNeedles = []string{
	// The earnings side. These make gross up; they are not taken out of it.
	"hours", "hrs", "rate", "regular", "overtime", "salary", "earnings",
	"bonus", "commission", "holiday", "vacation", "pto", "sick", "shift",
	"retro", "reimburse", "units",

	// Summary and administrative furniture.
	"total", "gross", "net pay", "ytd", "year to date", "balance", "accrued",
	"employee", "check no", "check #", "advice", "period", "deposit",
	"taxable", "memo", "statement",
	// Section subtotal headers. See the note above for why these must not reach
	// the unmatched list.
	"deduction", "taxes",
}

// findAmounts returns the money figures on one line, in printed order.
func findAmounts(line string) []decimal.Decimal {
	var out []decimal.Decimal
	for _, m := range amountPattern.FindAllString(line, -1) {
		cleaned := strings.NewReplacer("$", "", ",", "", " ", "").Replace(m)
		d, err := decimal.NewFromString(cleaned)
		if err != nil {
			continue
		}
		out = append(out, d)
	}
	return out
}

// labelPart is everything before the first money figure on a line.
func labelPart(line string) string {
	loc := amountPattern.FindStringIndex(line)
	if loc == nil {
		return line
	}
	return line[:loc[0]]
}

func firstAmount(a []decimal.Decimal) decimal.NullDecimal {
	if len(a) == 0 {
		return decimal.NullDecimal{}
	}
	return decimal.NullDecimal{Decimal: a[0], Valid: true}
}

// secondAmount is the year-to-date column.
//
// The convention assumed is current-period first, YTD second, which is what
// every major provider prints. A stub that reverses them produces a YTD smaller
// than the period amount, which is impossible — so it is detected in warnings()
// and told to the user rather than silently stored the wrong way round.
func secondAmount(a []decimal.Decimal) decimal.NullDecimal {
	if len(a) < 2 {
		return decimal.NullDecimal{}
	}
	return decimal.NullDecimal{Decimal: a[1].Abs(), Valid: true}
}

func clipLabel(s string) string {
	s = collapseSpaces(s)
	if len(s) > 80 {
		return s[:80]
	}
	return s
}

// warnings collects everything the review screen should say out loud.
func warnings(p Proposal) []string {
	var out []string

	if !p.Gross.Valid {
		out = append(out, "No gross pay figure was found — check the amount before saving.")
	}
	if !p.Net.Valid {
		out = append(out, "No net pay figure was found — check the amount before saving.")
	}
	if p.PayDate == nil {
		out = append(out, "No pay date was found on this stub.")
	}

	for _, l := range p.Lines {
		if l.YTDAmount.Valid && l.YTDAmount.Decimal.LessThan(l.Amount) {
			out = append(out,
				"The year-to-date column on \""+l.Label+"\" is smaller than the period amount. "+
					"This stub may print year-to-date first — check both figures.")
			break
		}
	}

	if p.Gross.Valid && p.Net.Valid && !p.Balanced() {
		residual := Stub{Gross: p.Gross.Decimal, Net: p.Net.Decimal, Lines: p.Lines}.Residual()
		out = append(out,
			"Gross minus the deductions read does not equal net — "+residual.Abs().StringFixed(2)+
				" is unaccounted for. A deduction was probably missed; add it before saving.")
	}
	if len(p.Unmatched) > 0 {
		out = append(out, "Some lines could not be classified and are listed for you to assign.")
	}
	return out
}

// guessEmployer takes the employer name from the top of the page.
//
// A hint, and labelled as one everywhere it surfaces. Payroll providers put the
// company name in the first text block, but they also put "EARNINGS STATEMENT"
// and their own branding there, so this skips the obvious boilerplate and
// stops. Getting it wrong costs the user one dropdown selection.
func guessEmployer(lines []string) string {
	boilerplate := []string{
		"earnings statement", "pay statement", "payroll", "advice",
		"direct deposit", "employee", "statement of earnings", "check date",
		"pay stub", "paystub", "confidential",
	}
	for i, l := range lines {
		if i > 6 {
			break
		}
		candidate := collapseSpaces(RedactIdentifiers(l))
		if len(candidate) < 3 || len(candidate) > 60 {
			continue
		}
		if len(findAmounts(candidate)) > 0 || datePattern.MatchString(candidate) {
			continue
		}
		lower := strings.ToLower(candidate)
		skip := false
		for _, b := range boilerplate {
			if strings.Contains(lower, b) {
				skip = true
				break
			}
		}
		if !skip {
			return candidate
		}
	}
	return ""
}

// parseDates pulls the pay date and the period out of the page.
//
// Anchored on the labels rather than on position, because the dates on a stub
// are printed in three different places by three different providers. A label
// naming a range takes the first two dates on its line; a pay-date label takes
// one.
func parseDates(lines []string) (payDate, periodStart, periodEnd *time.Time) {
	for _, raw := range lines {
		line := collapseSpaces(raw)
		lower := strings.ToLower(line)
		found := parseDateList(line)
		if len(found) == 0 {
			continue
		}

		switch {
		case containsAny(lower, "pay date", "check date", "date of pay", "payment date"):
			if payDate == nil {
				payDate = &found[0]
			}
		case containsAny(lower, "period beginning", "period start", "pay period", "period ending", "period end", "payroll period"):
			if periodStart == nil && len(found) >= 2 {
				periodStart, periodEnd = &found[0], &found[1]
			} else if periodEnd == nil && len(found) == 1 && containsAny(lower, "period ending", "period end") {
				periodEnd = &found[0]
			} else if periodStart == nil && len(found) == 1 && containsAny(lower, "period beginning", "period start") {
				periodStart = &found[0]
			}
		}
	}
	return payDate, periodStart, periodEnd
}

func parseDateList(line string) []time.Time {
	var out []time.Time
	for _, m := range datePattern.FindAllString(line, -1) {
		if d, ok := parseDate(m); ok {
			out = append(out, d)
		}
	}
	return out
}

func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			// A stub dated decades away is a misread, not a paystub. Rejecting
			// it leaves the field empty for the user to fill, which is a much
			// better outcome than a plausible-looking wrong date.
			year := t.Year()
			if year < 1980 || year > time.Now().UTC().Year()+1 {
				return time.Time{}, false
			}
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// --------------------------------------------------------------------------
// Redaction
// --------------------------------------------------------------------------

// ssnPattern matches a Social Security number as a stub prints it, masked or
// not, plus the bare nine-digit form.
var ssnPattern = regexp.MustCompile(`\b(?:[\dXx*]{3}[- ][\dXx*]{2}[- ]\d{4}|\d{9})\b`)

// bankAccountPattern matches the masked account number printed beside the
// direct-deposit line.
var bankAccountPattern = regexp.MustCompile(`\b[Xx*]{3,}\d{2,6}\b`)

// RedactIdentifiers strips personal identifiers out of text taken off a stub,
// BEFORE it is stored rather than merely before it is displayed.
//
// This is doc 23's point about lowering the sensitivity ceiling of the whole
// database, and it costs nothing: no figure this package computes needs an SSN
// or a bank account number, so there is no reason for either to ever reach a
// row, a log line, or a backup. A vault holding a database that has never
// contained an SSN is a materially different thing to restore, to export, and
// to lose.
//
// Applied to every label and every unmatched line the parser returns, which is
// every path by which text off a paystub can reach storage.
func RedactIdentifiers(s string) string {
	s = ssnPattern.ReplaceAllString(s, "[redacted]")
	return bankAccountPattern.ReplaceAllString(s, "[redacted]")
}
