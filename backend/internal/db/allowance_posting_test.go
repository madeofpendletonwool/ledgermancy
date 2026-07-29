package db

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Allowance auto-post idempotency.
//
// A job queue redelivers, a worker restarts mid-run, two workers can pick up
// the same tick. Paying a child twice is the kind of bug that gets noticed at
// the dinner table, so the claim-then-credit sequence is tested at the level it
// actually runs: the SQL.

// claimPeriod mimics the worker's claim step and reports whether it won.
func claimPeriod(t *testing.T, f *peopleFixture, periodStart time.Time) bool {
	t.Helper()
	n, err := f.q.MarkAllowancePosted(f.ctx, dbgen.MarkAllowancePostedParams{
		PersonID: f.kidPerson, PeriodStart: periodStart,
	})
	if err != nil {
		t.Fatalf("mark allowance posted: %v", err)
	}
	return n > 0
}

func TestAllowancePostingIsIdempotentWithinAPeriod(t *testing.T) {
	f := newPeopleFixture(t)

	if _, err := f.q.UpsertAllowance(f.ctx, dbgen.UpsertAllowanceParams{
		PersonID:    f.kidPerson,
		HouseholdID: f.householdID,
		Amount:      decimal.NewNullDecimal(decimal.RequireFromString("5.00")),
		Cadence:     strptr("weekly"),
		AutoPost:    true,
	}); err != nil {
		t.Fatalf("upsert allowance: %v", err)
	}

	week1 := time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC)

	// First run wins the period.
	if !claimPeriod(t, f, week1) {
		t.Fatal("first claim did not win")
	}
	// Every subsequent run in the same period loses. This is what stops a
	// redelivered job from paying again.
	for i := range 3 {
		if claimPeriod(t, f, week1) {
			t.Fatalf("claim %d won a period that was already paid", i+2)
		}
	}

	// The next period is claimable again — the guard must not be a permanent
	// lock, or the child stops being paid entirely.
	week2 := week1.AddDate(0, 0, 7)
	if !claimPeriod(t, f, week2) {
		t.Error("the following week could not be claimed")
	}
}

// TestAutoPostListingSkipsAlreadyPaid: the worker's first step must not even
// return an allowance already paid for the current period.
func TestAutoPostListingSkipsAlreadyPaid(t *testing.T) {
	f := newPeopleFixture(t)

	if _, err := f.q.UpsertAllowance(f.ctx, dbgen.UpsertAllowanceParams{
		PersonID:    f.kidPerson,
		HouseholdID: f.householdID,
		Amount:      decimal.NewNullDecimal(decimal.RequireFromString("5.00")),
		Cadence:     strptr("weekly"),
		AutoPost:    true,
	}); err != nil {
		t.Fatalf("upsert allowance: %v", err)
	}

	week := time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC)

	has := func() bool {
		t.Helper()
		rows, err := f.q.ListAutoPostAllowances(f.ctx, week)
		if err != nil {
			t.Fatalf("list auto-post: %v", err)
		}
		for _, r := range rows {
			if r.PersonID == f.kidPerson {
				return true
			}
		}
		return false
	}

	if !has() {
		t.Fatal("an unpaid allowance was not listed as due")
	}
	claimPeriod(t, f, week)
	if has() {
		t.Error("an allowance already paid for this period was still listed as due")
	}
}

// TestAutoPostIgnoresManualAllowances: auto_post defaults off, and an allowance
// a parent pays by hand must never be posted by the job.
func TestAutoPostIgnoresManualAllowances(t *testing.T) {
	f := newPeopleFixture(t)

	if _, err := f.q.UpsertAllowance(f.ctx, dbgen.UpsertAllowanceParams{
		PersonID:    f.kidPerson,
		HouseholdID: f.householdID,
		Amount:      decimal.NewNullDecimal(decimal.RequireFromString("5.00")),
		Cadence:     strptr("weekly"),
		AutoPost:    false,
	}); err != nil {
		t.Fatalf("upsert allowance: %v", err)
	}

	rows, err := f.q.ListAutoPostAllowances(f.ctx,
		time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("list auto-post: %v", err)
	}
	for _, r := range rows {
		if r.PersonID == f.kidPerson {
			t.Error("a manual allowance was listed for automatic posting")
		}
	}
}

// TestAllowanceBalanceNetsMixedEntries: the balance is derived by summing
// signed amounts, and the sign convention is the opposite of transactions.
func TestAllowanceBalanceNetsMixedEntries(t *testing.T) {
	f := newPeopleFixture(t)

	for _, e := range []struct{ kind, amount string }{
		{"allowance", "5.00"},
		{"allowance", "5.00"},
		{"chore", "3.50"},
		{"gift", "25.00"},
		{"spend", "-12.99"},
		{"spend", "-0.01"},
		{"correction", "-1.50"}, // undoing a mistaken credit
	} {
		if _, err := f.q.CreateAllowanceEntry(f.ctx, dbgen.CreateAllowanceEntryParams{
			PersonID: f.kidPerson, HouseholdID: f.householdID,
			Kind: e.kind, Amount: decimal.RequireFromString(e.amount),
			OccurredOn: time.Now(),
		}); err != nil {
			t.Fatalf("create %s: %v", e.kind, err)
		}
	}

	// 5 + 5 + 3.50 + 25 - 12.99 - 0.01 - 1.50
	balance, err := f.q.GetAllowanceBalance(f.ctx, dbgen.GetAllowanceBalanceParams{
		PersonID: f.kidPerson, HouseholdID: f.householdID,
	})
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if got, want := balance.StringFixed(2), "24.00"; got != want {
		t.Errorf("balance = %s, want %s", got, want)
	}

	// Spending is reported as a positive magnitude against the monthly limit.
	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	spent, err := f.q.GetAllowanceSpentInMonth(f.ctx, dbgen.GetAllowanceSpentInMonthParams{
		PersonID: f.kidPerson, HouseholdID: f.householdID,
		MonthStart: monthStart, MonthEnd: monthStart.AddDate(0, 1, 0),
	})
	if err != nil {
		t.Fatalf("spent: %v", err)
	}
	// 12.99 + 0.01 + 1.50 — the negative correction counts as an outflow too.
	if got, want := spent.StringFixed(2), "14.50"; got != want {
		t.Errorf("spent this month = %s, want %s", got, want)
	}
}
