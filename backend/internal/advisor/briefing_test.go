package advisor

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/goals"
)

// The debt-free date is the ONE piece of briefing arithmetic with a tempting
// wrong answer, so it gets a test of its own rather than riding along in a
// fixture check. Every case below fails on the min.

var briefingNow = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

// knownDebt builds a debt with both terms resolved, so it is projectable.
func knownDebt(name, balance, apr, payment string) debt {
	return debt{
		id:      uuid.New(),
		name:    name,
		balance: decimal.RequireFromString(balance),
		terms: goals.DebtTerms{
			APR:           decimal.RequireFromString(apr),
			APRSource:     goals.TermSourceManual,
			Payment:       decimal.RequireFromString(payment),
			PaymentSource: goals.TermSourceManual,
		},
	}
}

// THE DEBT-FREE DATE IS THE MAX, NOT THE MIN.
//
// The min is the date the FIRST debt dies, which is a different and much more
// flattering number. A household is debt-free when the LAST one does. This test
// fails on the min by construction: the three debts clear at deliberately far
// apart horizons.
func TestDebtFreeDateIsTheMax(t *testing.T) {
	debts := []debt{
		// Interest-free, so the months are exactly balance/payment and the
		// arithmetic is checkable by hand: 800/100 = 8, 3000/100 = 30,
		// 6100/100 = 61.
		knownDebt("Store card", "800.00", "0", "100.00"),
		knownDebt("Car loan", "3000.00", "0", "100.00"),
		knownDebt("Student loan", "6100.00", "0", "100.00"),
	}

	got := debtFreeDate(debts, briefingNow)

	if got.Never {
		t.Fatalf("no debt here never pays off, but the household was reported never debt-free")
	}
	if got.Date == nil {
		t.Fatal("expected a debt-free date")
	}
	if got.Projected != 3 || got.Excluded != 0 {
		t.Errorf("projected = %d, excluded = %d; want 3 and 0", got.Projected, got.Excluded)
	}

	// 61 months out, which is the LAST debt. The min would be 8 and the middle
	// 30; both are wrong and both are the mistake this asserts against.
	want := briefingNow.AddDate(0, 61, 0)
	if got.Date.Year() != want.Year() || got.Date.Month() != want.Month() {
		t.Errorf("debt-free date = %s, want %s (the LAST debt to clear, not the first)",
			got.Date.Format("2006-01"), want.Format("2006-01"))
	}

	if total := got.TotalBalance.String(); total != "9900" {
		t.Errorf("total balance = %s, want 9900", total)
	}
}

// A debt that never pays off makes the WHOLE household's debt-free date
// "never" — not the max of the finite ones. "Never" is contagious, and a date
// computed over the debts that do clear would be a date the household never
// actually reaches.
func TestOneUnpayableDebtMakesTheWholeDateNever(t *testing.T) {
	debts := []debt{
		knownDebt("Store card", "800.00", "0", "100.00"),
		knownDebt("Car loan", "3000.00", "0", "100.00"),
		// $61/month interest at 24.4% on $3,000; a $50 payment never touches
		// the principal.
		knownDebt("Maxed card", "3000.00", "24.40", "50.00"),
	}

	got := debtFreeDate(debts, briefingNow)

	if !got.Never {
		t.Fatal("a debt that never pays off must make the household's date never")
	}
	if got.Date != nil {
		t.Errorf("date = %s; a never-debt-free household has no date at all",
			got.Date.Format(time.DateOnly))
	}
	if got.NeverAccount != "Maxed card" {
		t.Errorf("never_account = %q, want the debt that caused it", got.NeverAccount)
	}
}

// "Never" survives a later finite debt. Order must not decide the answer: the
// unpayable debt is first here, and the two that clear afterwards must not
// overwrite the verdict with a date.
func TestNeverSurvivesLaterFiniteDebts(t *testing.T) {
	debts := []debt{
		knownDebt("Maxed card", "3000.00", "24.40", "50.00"),
		knownDebt("Store card", "800.00", "0", "100.00"),
		knownDebt("Car loan", "3000.00", "0", "100.00"),
	}

	got := debtFreeDate(debts, briefingNow)
	if !got.Never || got.Date != nil {
		t.Fatalf("never = %v, date = %v; a later finite debt must not clear the verdict",
			got.Never, got.Date)
	}
}

// A debt with an unresolvable APR CANNOT be projected, so it is reported as
// excluded with a name — never defaulted to 0% and quietly maxed over.
//
// The direction matters: a zero default projects the debt as interest-free,
// which flatters exactly the debt most likely to be the expensive one.
func TestUnknownTermsAreExcludedAndCounted(t *testing.T) {
	noRate := debt{
		id: uuid.New(), name: "Mystery card",
		balance: decimal.RequireFromString("2000.00"),
		terms: goals.DebtTerms{
			APRSource:     goals.TermSourceNone,
			Payment:       decimal.RequireFromString("100.00"),
			PaymentSource: goals.TermSourceManual,
		},
	}
	noPayment := debt{
		id: uuid.New(), name: "Silent loan",
		balance: decimal.RequireFromString("500.00"),
		terms: goals.DebtTerms{
			APR:           decimal.RequireFromString("5.00"),
			APRSource:     goals.TermSourceManual,
			PaymentSource: goals.TermSourceNone,
		},
	}

	got := debtFreeDate([]debt{
		knownDebt("Store card", "800.00", "0", "100.00"),
		noRate, noPayment,
	}, briefingNow)

	if got.Excluded != 2 {
		t.Errorf("excluded = %d, want 2", got.Excluded)
	}
	if got.Projected != 1 {
		t.Errorf("projected = %d, want 1", got.Projected)
	}
	if len(got.ExcludedNames) != 2 {
		t.Fatalf("excluded_names = %v, want both names", got.ExcludedNames)
	}
	// The excluded balances still count toward what is owed. A household told
	// it owes $800 when it owes $3,300 is the failure this guards.
	if total := got.TotalBalance.String(); total != "3300" {
		t.Errorf("total balance = %s, want 3300 (excluded debts still count)", total)
	}
	// And the one projectable debt still produces a date, so the briefing says
	// "clear by March, excluding 2 we could not project" rather than nothing.
	if got.Date == nil {
		t.Error("the projectable debt should still yield a date")
	}
}

// A household with no debts has no date and no "never" — the two must not be
// confused, since one means "you are already debt-free" and the other means
// "you never will be".
func TestNoDebtsIsNotNever(t *testing.T) {
	got := debtFreeDate(nil, briefingNow)
	if got.Never {
		t.Error("a household with no debts is not 'never debt-free'")
	}
	if got.Date != nil {
		t.Error("a household with no debts has no debt-free date to report")
	}
	if !got.TotalBalance.IsZero() {
		t.Errorf("total balance = %s, want zero", got.TotalBalance)
	}
}
