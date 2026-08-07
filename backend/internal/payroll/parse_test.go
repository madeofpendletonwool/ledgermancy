package payroll

import (
	"strings"
	"testing"
)

// realisticStubLines is what ExtractPDFText hands the parser for a mainstream
// biweekly stub: label, current period, year to date.
func realisticStubLines() []string {
	return []string{
		"ACME MANUFACTURING INC",
		"Earnings Statement",
		"Employee: Jane Doe SSN: 123-45-6789",
		"Pay Date: 06/12/2026",
		"Period Beginning: 05/30/2026 Period Ending: 06/12/2026",
		"Gross Pay 3,000.00 39,000.00",
		"Federal Income Tax 330.00 4,290.00",
		"Social Security 179.80 2,337.40",
		"Medicare 42.05 546.65",
		"State Income Tax 120.00 1,560.00",
		"401(k) Pre-Tax 300.00 3,900.00",
		"Roth 401(k) 50.00 650.00",
		"401(k) Employer Match 150.00 1,950.00",
		"Medical PPO 100.00 1,300.00",
		"Dental 12.00 156.00",
		"Union Dues 25.00 325.00",
		"Hours 80.00 Rate 37.50",
		"Net Pay 1,841.15 23,935.00",
	}
}

func TestParseProposalReadsHeadersAndPeriod(t *testing.T) {
	p := ParseProposal(realisticStubLines())

	if p.EmployerName != "ACME MANUFACTURING INC" {
		t.Errorf("employer hint = %q, want the company name from the top of the page", p.EmployerName)
	}
	if p.PayDate == nil || p.PayDate.Format("2006-01-02") != "2026-06-12" {
		t.Errorf("pay date = %v, want 2026-06-12", p.PayDate)
	}
	if p.PeriodStart == nil || p.PeriodStart.Format("2006-01-02") != "2026-05-30" {
		t.Errorf("period start = %v, want 2026-05-30", p.PeriodStart)
	}
	if p.PeriodEnd == nil || p.PeriodEnd.Format("2006-01-02") != "2026-06-12" {
		t.Errorf("period end = %v, want 2026-06-12", p.PeriodEnd)
	}

	if !p.Gross.Valid || p.Gross.Decimal.String() != "3000" {
		t.Errorf("gross = %v, want 3000", p.Gross)
	}
	if !p.YTDGross.Valid || p.YTDGross.Decimal.String() != "39000" {
		t.Errorf("ytd gross = %v, want 39000", p.YTDGross)
	}
	if !p.Net.Valid || p.Net.Decimal.String() != "1841.15" {
		t.Errorf("net = %v, want 1841.15", p.Net)
	}
}

// TestParseProposalRuleOrder pins the ordering that lineRules depends on.
// "Roth 401(k)" contains "401(k)", and "401(k) Employer Match" contains both —
// so the specific rules must be tested before the general ones or a Roth
// deferral is filed as pre-tax and a match is filed as the employee's own.
func TestParseProposalRuleOrder(t *testing.T) {
	p := ParseProposal(realisticStubLines())

	byCategory := map[Category]Line{}
	for _, l := range p.Lines {
		if _, seen := byCategory[l.Category]; seen {
			t.Errorf("category %q matched twice", l.Category)
		}
		byCategory[l.Category] = l
	}

	for _, want := range []struct {
		category   Category
		amount     string
		preTax     bool
		isEmployer bool
	}{
		{CatFederalIncomeTax, "330", false, false},
		{CatSocialSecurity, "179.8", false, false},
		{CatMedicare, "42.05", false, false},
		{CatStateIncomeTax, "120", false, false},
		{Cat401kPreTax, "300", true, false},
		{Cat401kRoth, "50", false, false},
		{Cat401kMatch, "150", false, true},
		{CatHealthPremium, "100", true, false},
		{CatDental, "12", true, false},
	} {
		got, ok := byCategory[want.category]
		if !ok {
			t.Errorf("category %q was not matched", want.category)
			continue
		}
		if got.Amount.String() != want.amount {
			t.Errorf("%s amount = %s, want %s", want.category, got.Amount, want.amount)
		}
		if got.PreTax != want.preTax {
			t.Errorf("%s pre-tax = %v, want %v", want.category, got.PreTax, want.preTax)
		}
		if got.IsEmployer != want.isEmployer {
			t.Errorf("%s is-employer = %v, want %v", want.category, got.IsEmployer, want.isEmployer)
		}
	}
}

// TestParseProposalSurfacesUnknownLines: a deduction the parser cannot name is
// exactly the one that makes the stub fail to balance, so it must be listed for
// the user rather than guessed into "other" where it becomes invisible.
func TestParseProposalSurfacesUnknownLines(t *testing.T) {
	p := ParseProposal(realisticStubLines())

	found := false
	for _, u := range p.Unmatched {
		if strings.Contains(u, "Union Dues") {
			found = true
		}
		if strings.Contains(u, "Hours") {
			t.Errorf("an hours-and-rates row reached the unmatched list: %q", u)
		}
	}
	if !found {
		t.Errorf("Union Dues should be listed as unmatched, got %v", p.Unmatched)
	}

	// The stub is short by exactly the dues, so it must not claim to balance
	// and must say by how much.
	if p.Balanced() {
		t.Error("a stub missing a deduction must not report as balanced")
	}
	if !hasWarningContaining(p.Warnings, "25.00") {
		t.Errorf("the balance warning should name the unaccounted amount, got %v", p.Warnings)
	}
}

// TestParseProposalRedactsIdentifiers is the "lower the sensitivity ceiling"
// rule: an SSN never reaches a stored label, which means it never reaches a
// backup or a portable export either.
func TestParseProposalRedactsIdentifiers(t *testing.T) {
	lines := append(realisticStubLines(), "Direct Deposit XXXXXX4321 1,841.15")
	p := ParseProposal(lines)

	for _, l := range p.Lines {
		if strings.Contains(l.Label, "123-45-6789") {
			t.Errorf("an SSN survived into a line label: %q", l.Label)
		}
	}
	for _, u := range p.Unmatched {
		if strings.Contains(u, "123-45-6789") || strings.Contains(u, "XXXXXX4321") {
			t.Errorf("an identifier survived into the unmatched list: %q", u)
		}
	}

	if got := RedactIdentifiers("Employee 123-45-6789 and 987654321"); strings.ContainsAny(got, "0123456789") {
		t.Errorf("redaction left digits behind: %q", got)
	}
}

// TestParseProposalFlagsReversedYTDColumn: a YTD figure smaller than the period
// amount is impossible, so it means the columns are the other way round on this
// provider's layout. Told to the user rather than silently stored backwards.
func TestParseProposalFlagsReversedYTDColumn(t *testing.T) {
	p := ParseProposal([]string{
		"Pay Date: 06/12/2026",
		"Gross Pay 3,000.00 39,000.00",
		"Federal Income Tax 4,290.00 330.00",
		"Net Pay 2,670.00 34,710.00",
	})
	if !hasWarningContaining(p.Warnings, "year-to-date") {
		t.Errorf("expected a reversed-column warning, got %v", p.Warnings)
	}
}

func TestParseProposalWarnsOnMissingHeaders(t *testing.T) {
	p := ParseProposal([]string{"Federal Income Tax 330.00"})

	for _, want := range []string{"gross pay", "net pay", "pay date"} {
		if !hasWarningContaining(p.Warnings, want) {
			t.Errorf("expected a warning about %q, got %v", want, p.Warnings)
		}
	}
}

// TestParseProposalIgnoresBareIntegers is why amountPattern insists on two
// decimal places: a stub is full of hours, rates and employee numbers, and a
// looser pattern would propose "40" as a deduction.
func TestParseProposalIgnoresBareIntegers(t *testing.T) {
	p := ParseProposal([]string{
		"Pay Date: 06/12/2026",
		"Employee Number 40881",
		"Regular Hours 80",
		"Gross Pay 3,000.00",
		"Net Pay 3,000.00",
	})
	if len(p.Lines) != 0 {
		t.Errorf("no deduction lines expected, got %v", p.Lines)
	}
	if !p.Balanced() {
		t.Error("a stub with no deductions and equal gross/net should balance")
	}
}

func TestParseProposalRejectsImplausibleDates(t *testing.T) {
	// A misread date is dropped, leaving the field empty for the user, rather
	// than filled with something plausible-looking and wrong.
	p := ParseProposal([]string{"Pay Date: 06/12/1899", "Gross Pay 100.00", "Net Pay 100.00"})
	if p.PayDate != nil {
		t.Errorf("pay date = %v, want nil for an implausible year", p.PayDate)
	}
}

func hasWarningContaining(warnings []string, needle string) bool {
	for _, w := range warnings {
		if strings.Contains(strings.ToLower(w), strings.ToLower(needle)) {
			return true
		}
	}
	return false
}

// TestParseProposalRecognisesProviderTaxLabels covers the abbreviations Paycor
// and similar providers print for the core tax lines, which a substring needle
// list alone cannot cover: "FICA EE" (Social Security on a split-FICA stub),
// "Fed MWT EE" (Medicare Wage Tax) and "WI W/H" (state-code withholding). With
// gross supplied these are the difference between a stub that balances on
// import and one the user has to classify by hand.
func TestParseProposalRecognisesProviderTaxLabels(t *testing.T) {
	p := ParseProposal([]string{
		"Pay Date: 06/12/2026",
		"Gross Pay 4,484.79",
		"Fed W/H 413.10 5,783.40",   // federal — already recognised
		"FICA EE 248.32 3,476.48",   // Social Security (Paycor)
		"Fed MWT EE 58.08 813.05",   // Medicare Wage Tax (Paycor)
		"WI W/H 175.34 2,454.76",    // Wisconsin state withholding
		"Accident 1.05 27.30",       // voluntary accident insurance
		// 4484.79 − 413.10 − 248.32 − 58.08 − 175.34 − 1.05 = 3588.90
		"Net Pay 3,588.90",
	})

	byCat := map[Category]Line{}
	for _, l := range p.Lines {
		byCat[l.Category] = l
	}
	for _, want := range []struct {
		category Category
		amount   string
	}{
		{CatFederalIncomeTax, "413.1"},
		{CatSocialSecurity, "248.32"},
		{CatMedicare, "58.08"},
		{CatStateIncomeTax, "175.34"},
		{CatOther, "1.05"},
	} {
		got, ok := byCat[want.category]
		if !ok {
			t.Errorf("%s was not matched (provider label not recognised)", want.category)
			continue
		}
		if got.Amount.String() != want.amount {
			t.Errorf("%s amount = %s, want %s", want.category, got.Amount, want.amount)
		}
	}

	if !p.Balanced() {
		t.Errorf("provider tax labels should balance once gross is supplied, residual %s",
			p.Stub().Residual().StringFixed(2))
	}
}

// TestStateWithholdingRegexSafety pins the boundaries of stateWithholdingRe.
// It must catch every state-code withholding label and must never fire on a
// federal line, a longer word that happens to start with two letters, or an
// unrelated deduction — a substring needle could not make that guarantee.
func TestStateWithholdingRegexSafety(t *testing.T) {
	for _, label := range []string{
		"wi w/h", "ca w/h", "ny tax", "il income tax", "oh withholding", "wi wh",
	} {
		if !stateWithholdingRe.MatchString(label) {
			t.Errorf("state regex should match %q", label)
		}
	}
	for _, label := range []string{
		"fed w/h", "federal withholding", "dental", "medical", "vision",
		"regular", "earnings", "fica ee", "supplemental life",
	} {
		if stateWithholdingRe.MatchString(label) {
			t.Errorf("state regex misfired on %q", label)
		}
	}
}

// TestParseProposalSplitsEmployerAndEmployeeLife pins the split between
// employer-paid basic life and employee-paid supplemental life. "Group Term
// Life" is the classic taxable benefit — the employer pays the premium and the
// stub lists its imputed value under a Taxable Benefits section, not a
// deductions one. Filing it as an employee deduction makes the stub fail to
// balance by exactly the premium. "Supplemental Life" is elected and paid by
// the employee.
func TestParseProposalSplitsEmployerAndEmployeeLife(t *testing.T) {
	p := ParseProposal([]string{
		"Pay Date: 06/12/2026",
		"Gross Pay 3,000.00",
		// Employee deductions.
		"Supplemental Life 20.00",
		"Federal Income Tax 300.00",
		// Employer-paid taxable benefit, NOT a deduction.
		"Group Term Life 6.60",
		// 3000 − 20 − 300 = 2680.
		"Net Pay 2,680.00",
	})

	var groupTerm, supp *Line
	for i := range p.Lines {
		if p.Lines[i].Category != CatLifeInsurance {
			continue
		}
		if p.Lines[i].IsEmployer {
			groupTerm = &p.Lines[i]
		} else {
			supp = &p.Lines[i]
		}
	}
	if groupTerm == nil {
		t.Fatalf("Group Term Life was not matched as an employer line: %+v", p.Lines)
	}
	if groupTerm.Amount.String() != "6.6" {
		t.Errorf("Group Term Life amount = %s, want 6.6", groupTerm.Amount)
	}
	if supp == nil {
		t.Fatalf("Supplemental Life was not matched as an employee line: %+v", p.Lines)
	}
	if supp.Amount.String() != "20" {
		t.Errorf("Supplemental Life amount = %s, want 20", supp.Amount)
	}

	// Group Term Life must not be counted against net, or the stub appears
	// $6.60 short — exactly the hand-entry bug this split prevents.
	if !p.Balanced() {
		t.Errorf("group term life as employer should balance, residual %s",
			p.Stub().Residual().StringFixed(2))
	}
}
