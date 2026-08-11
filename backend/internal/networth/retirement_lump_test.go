package networth

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// The two extensions doc 32 makes to the projection engine, and the reason each
// one is tested rather than trusted.
//
// FirstYearContribution exists so the CAP can see a lump. Fold an $8,000 Roth
// lump into Balance instead and ProjectRetirement projects the whole of it,
// uncapped, because capContributions never inspects Balance — and the household
// is shown a plan the IRS does not allow. The case that catches an
// implementation getting this half-right is not the pure-lump one; it is the
// combination that only breaches the cap when the lump and the monthly are added
// together.
//
// RealReturnRate exists so a per-bucket assumption is a lookup rather than a
// second projection loop. The test that matters most for it is the one asserting
// nothing changed: a plan with every bucket rate left at zero must produce
// output identical to the household-rate behaviour that shipped before it.

// in2026 is a fixed clock inside a configured tax year, so nothing here depends
// on when it is run.
var in2026 = time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)

func capAssumptions(months int) RetirementAssumptions {
	return RetirementAssumptions{
		RealReturnRate: dec("0.05"),
		WithdrawalRate: dec("0.04"),
		CurrentAge:     40, // under 50: no catch-up, so the 2026 IRA cap is $7,500
		Months:         months,
	}
}

func noteFor(p RetirementProjection, group string) (CapNote, bool) {
	for _, n := range p.CapNotes {
		if n.Group == group {
			return n, true
		}
	}
	return CapNote{}, false
}

// THE DOC'S HEADLINE CASE. An $8,000 lump into one Roth is held at the 2026 IRA
// limit of $7,500, and the $500 spill is REPORTED — not projected as a
// contribution and not quietly folded into the opening balance.
func TestLumpIsCappedAtTheIRALimit(t *testing.T) {
	plans := []AccountPlan{{
		ID: "roth", Name: "Roth IRA", Treatment: "roth_ira",
		Balance:               decimal.Zero,
		FirstYearContribution: dec("8000"),
	}}

	got := ProjectRetirement(plans, capAssumptions(12), in2026)

	if !got.LimitsConfigured {
		t.Fatal("2026 limits should be configured")
	}
	note, ok := noteFor(got, "ira")
	if !ok {
		t.Fatal("no cap note for the IRA group — the lump was not capped at all")
	}
	if want := dec("8000"); !note.Planned.Equal(want) {
		t.Errorf("planned = %s, want %s (the lump must be in the group total)", note.Planned, want)
	}
	if want := dec("7500"); !note.Allowed.Equal(want) {
		t.Errorf("allowed = %s, want %s — the IRA limit, NOT the elective deferral", note.Allowed, want)
	}
	if want := dec("500"); !note.Spill.Equal(want) {
		t.Errorf("spill = %s, want %s", note.Spill, want)
	}

	// And the projection must actually reflect it: $7,500 contributed, not
	// $8,000, and not $500 hiding in the balance.
	last := got.Points[len(got.Points)-1].ByAccount["roth"]
	if want := dec("7500.00"); !last.Contributed.Equal(want) {
		t.Errorf("contributed = %s, want %s", last.Contributed, want)
	}
}

// The same assertion from the monthly side, which is the path that already
// worked — kept so a change to the pooling cannot fix the lump by breaking this.
func TestMonthlySumIsCappedAtTheIRALimit(t *testing.T) {
	plans := []AccountPlan{{
		ID: "roth", Name: "Roth IRA", Treatment: "roth_ira",
		MonthlyContribution: dec("8000").Div(decimal.NewFromInt(12)),
	}}
	got := ProjectRetirement(plans, capAssumptions(12), in2026)

	note, ok := noteFor(got, "ira")
	if !ok {
		t.Fatal("no cap note for the IRA group")
	}
	if want := dec("500"); !note.Spill.Round(2).Equal(want) {
		t.Errorf("spill = %s, want %s", note.Spill, want)
	}
}

// THE CASE THAT FAILS IF THE LUMP IS NOT IN THE GROUP TOTAL. Neither side
// breaches on its own — $4,000 of lump and $400/month is $4,800 a year — but
// together they are $8,800 against a $7,500 cap.
func TestLumpPlusMonthlyBreachesOnlyWhenAddedTogether(t *testing.T) {
	plans := []AccountPlan{{
		ID: "roth", Name: "Roth IRA", Treatment: "roth_ira",
		FirstYearContribution: dec("4000"),
		MonthlyContribution:   dec("400"),
	}}
	got := ProjectRetirement(plans, capAssumptions(12), in2026)

	note, ok := noteFor(got, "ira")
	if !ok {
		t.Fatal("no cap note: the lump and the monthly were not pooled into one annual total")
	}
	if want := dec("8800"); !note.Planned.Equal(want) {
		t.Errorf("planned = %s, want %s", note.Planned, want)
	}
	if want := dec("1300"); !note.Spill.Equal(want) {
		t.Errorf("spill = %s, want %s", note.Spill, want)
	}

	// Scaled PROPORTIONALLY, not by draining one side first: the split between a
	// lump and a monthly plan is the household's decision about when money goes
	// in, and the projection has no business re-timing it.
	scale := dec("7500").Div(dec("8800"))
	last := got.Points[len(got.Points)-1].ByAccount["roth"]
	wantContributed := dec("4000").Mul(scale).
		Add(dec("400").Mul(scale).Mul(decimal.NewFromInt(12))).Round(2)
	if diff := last.Contributed.Sub(wantContributed).Abs(); diff.GreaterThan(dec("0.05")) {
		t.Errorf("contributed = %s, want ≈%s", last.Contributed, wantContributed)
	}
}

// Two IRAs share ONE cap. Applying the full limit per account would tell a
// household it may contribute twice what the IRS allows.
func TestTwoIRAsShareOneCap(t *testing.T) {
	plans := []AccountPlan{
		{ID: "roth", Name: "Roth IRA", Treatment: "roth_ira", FirstYearContribution: dec("5000")},
		{ID: "trad", Name: "Trad IRA", Treatment: "trad_ira", FirstYearContribution: dec("5000")},
	}
	got := ProjectRetirement(plans, capAssumptions(12), in2026)

	note, ok := noteFor(got, "ira")
	if !ok {
		t.Fatal("no pooled IRA cap note — the two accounts were capped separately")
	}
	if want := dec("10000"); !note.Planned.Equal(want) {
		t.Errorf("planned = %s, want %s (both accounts pooled)", note.Planned, want)
	}
	if want := dec("7500"); !note.Allowed.Equal(want) {
		t.Errorf("allowed = %s, want %s", note.Allowed, want)
	}

	final := got.Points[len(got.Points)-1].ByAccount
	total := final["roth"].Contributed.Add(final["trad"].Contributed)
	if diff := total.Sub(dec("7500")).Abs(); diff.GreaterThan(dec("0.05")) {
		t.Errorf("combined contributions = %s, want ≈7500", total)
	}
}

// A Roth is capped at the IRA limit, never at the 401(k) elective deferral. An
// implementer following prose rather than the table would cap it at $24,500.
func TestRothIsNotCappedAtTheElectiveDeferral(t *testing.T) {
	plans := []AccountPlan{{
		ID: "roth", Name: "Roth IRA", Treatment: "roth_ira",
		FirstYearContribution: dec("20000"),
	}}
	got := ProjectRetirement(plans, capAssumptions(12), in2026)

	note, _ := noteFor(got, "ira")
	if want := dec("7500"); !note.Allowed.Equal(want) {
		t.Fatalf("allowed = %s, want %s — %s is the 401(k) number", note.Allowed, want, dec("24500"))
	}
}

// An unconfigured year caps NOTHING and says so, rather than substituting an
// adjacent year's limits.
func TestUnconfiguredYearCapsNothing(t *testing.T) {
	future := time.Date(2099, 3, 15, 0, 0, 0, 0, time.UTC)
	plans := []AccountPlan{{
		ID: "roth", Name: "Roth IRA", Treatment: "roth_ira",
		FirstYearContribution: dec("8000"),
	}}
	got := ProjectRetirement(plans, capAssumptions(12), future)

	if got.LimitsConfigured {
		t.Fatal("2099 should not be configured")
	}
	if len(got.CapNotes) != 0 {
		t.Errorf("cap notes = %v, want none when the year is unconfigured", got.CapNotes)
	}
	last := got.Points[len(got.Points)-1].ByAccount["roth"]
	if want := dec("8000.00"); !last.Contributed.Equal(want) {
		t.Errorf("contributed = %s, want the full %s uncapped", last.Contributed, want)
	}
}

// Per-bucket returns: two accounts with different rates compound differently in
// ONE run.
func TestPerBucketReturnsCompoundDifferently(t *testing.T) {
	plans := []AccountPlan{
		{ID: "slow", Name: "Bonds", Treatment: "taxable", Balance: dec("10000"), RealReturnRate: decimal.NewNullDecimal(dec("0.02"))},
		{ID: "fast", Name: "Equity", Treatment: "taxable", Balance: dec("10000"), RealReturnRate: decimal.NewNullDecimal(dec("0.08"))},
	}
	got := ProjectRetirement(plans, capAssumptions(120), in2026)

	final := got.Points[len(got.Points)-1].ByAccount
	if !final["fast"].Balance.GreaterThan(final["slow"].Balance) {
		t.Fatalf("8%% bucket (%s) did not outgrow the 2%% bucket (%s) — one rate was applied to both",
			final["fast"].Balance, final["slow"].Balance)
	}
	// And neither followed the household's 5%: both overrides were honoured.
	household := ProjectRetirement([]AccountPlan{
		{ID: "x", Name: "X", Treatment: "taxable", Balance: dec("10000")},
	}, capAssumptions(120), in2026)
	at5 := household.Points[len(household.Points)-1].ByAccount["x"].Balance
	if final["fast"].Balance.Equal(at5) || final["slow"].Balance.Equal(at5) {
		t.Error("a per-bucket rate fell through to the household rate")
	}
}

// THE COMPATIBILITY ASSERTION. Every existing caller leaves RealReturnRate and
// FirstYearContribution at zero, and must get byte-identical output.
func TestZeroPerBucketFieldsPreserveTheOldBehaviour(t *testing.T) {
	base := []AccountPlan{
		{ID: "a", Name: "401k", Treatment: "trad_401k", Balance: dec("50000"), MonthlyContribution: dec("500")},
		{ID: "b", Name: "Brokerage", Treatment: "taxable", Balance: dec("25000"), MonthlyContribution: dec("250")},
	}
	a := capAssumptions(240)

	got := ProjectRetirement(base, a, in2026)

	// The same series computed the way the engine did before the fields existed:
	// one household rate, no lump. Recomputed here by hand rather than compared
	// against a golden file, so the assertion says what it means.
	monthlyRate := a.RealReturnRate.Div(decimal.NewFromInt(12))
	want := map[string]decimal.Decimal{}
	for _, p := range base {
		balance := p.Balance
		for m := 0; m < a.Months; m++ {
			balance = balance.Add(balance.Mul(monthlyRate)).Add(p.MonthlyContribution)
		}
		want[p.ID] = balance.Round(2)
	}

	final := got.Points[len(got.Points)-1].ByAccount
	for id, w := range want {
		if !final[id].Balance.Equal(w) {
			t.Errorf("account %s = %s, want %s — the per-account rate slice changed the arithmetic",
				id, final[id].Balance, w)
		}
	}
}

// A 529 returns ok=false from AnnualLimitFor and is projected UNCAPPED. The
// honest answer: it is bounded by gift-tax rules and state aggregate caps, and
// inventing a federal one would be worse than not capping it.
func TestFiveTwentyNineIsUncapped(t *testing.T) {
	plans := []AccountPlan{{
		ID: "529", Name: "College", Treatment: "529",
		FirstYearContribution: dec("40000"),
		BeneficiaryCurrentAge: 5, BeneficiaryTargetAge: 18,
	}}
	got := ProjectRetirement(plans, capAssumptions(12), in2026)

	if len(got.CapNotes) != 0 {
		t.Errorf("cap notes = %v, want none: a 529 has no federal annual deferral cap", got.CapNotes)
	}
	last := got.Points[len(got.Points)-1].ByAccount["529"]
	if want := dec("40000.00"); !last.Contributed.Equal(want) {
		t.Errorf("contributed = %s, want the full %s", last.Contributed, want)
	}
}
