package audit

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// These tests pin the pure diff logic without a database: an unchanged value
// produces no change, a changed one produces exactly one, and the JSON-friendly
// normalisation (decimal/date/uuid as canonical text) is what the History panel
// will render. The same-transaction and visibility guarantees live with the
// handlers and are covered by object_history_test.go in package api.

func mustDec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return d
}

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return d
}

func TestDiffHelpersUnchangedYieldsNothing(t *testing.T) {
	cat := uuid.New()
	cases := []struct {
		name string
		ch   *Change
	}{
		{"string", changeString("name", "same", "same")},
		{"decimal", changeDecimal("amount", mustDec(t, "1.20"), mustDec(t, "1.2"))}, // equal by value
		{"date", changeDate("date", mustDate(t, "2026-01-01"), mustDate(t, "2026-01-01"))},
		{"nil uuid ptrs", changeUUIDPtr("category_id", nil, nil)},
		{"equal uuid ptrs", changeUUIDPtr("category_id", &cat, &cat)},
		{"equal string ptrs", changeStringPtr("notes", ptr("n"), ptr("n"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ch != nil {
				t.Fatalf("expected no change for an equal value, got %+v", *tc.ch)
			}
		})
	}
}

func TestDiffHelpersChangedYieldsCanonicalText(t *testing.T) {
	old, new := uuid.New(), uuid.New()
	ch := changeUUIDPtr("category_id", &old, &new)
	if ch == nil {
		t.Fatal("expected a change")
	}
	if ch.Old != old.String() || ch.New != new.String() {
		t.Errorf("uuids should be canonical text, got old=%v new=%v", ch.Old, ch.New)
	}

	ch = changeDecimal("amount", mustDec(t, "10.00"), mustDec(t, "12.50"))
	if ch.Old != "10" || ch.New != "12.5" {
		t.Errorf("decimal should be the canonical string, got old=%v new=%v", ch.Old, ch.New)
	}

	ch = changeDate("date", mustDate(t, "2026-01-01"), mustDate(t, "2026-02-03"))
	if ch.Old != "2026-01-01" || ch.New != "2026-02-03" {
		t.Errorf("date should be YYYY-MM-DD, got old=%v new=%v", ch.Old, ch.New)
	}

	// Setting a field (nil → value) reads as NULL → value, the create/clear shape
	// the History panel relies on to tell "set" apart from "changed from empty".
	ch = changeUUIDPtr("category_id", nil, &new)
	if ch.Old != nil || ch.New != new.String() {
		t.Errorf("nil old should stay nil, got old=%v new=%v", ch.Old, ch.New)
	}
	ch = changeUUIDPtr("category_id", &old, nil)
	if ch.Old != old.String() || ch.New != nil {
		t.Errorf("nil new should stay nil, got old=%v new=%v", ch.Old, ch.New)
	}
}

func TestTransactionDiffOnlyReportsChangedFields(t *testing.T) {
	oldCat := uuid.New()
	newCat := uuid.New()
	before := dbgen.Transaction{
		Amount: mustDec(t, "11.86"), Date: mustDate(t, "2026-06-15"),
		Name: "Coffee", CategoryID: &oldCat,
	}
	after := dbgen.Transaction{
		// Amount and date unchanged; name and category changed.
		Amount: mustDec(t, "11.86"), Date: mustDate(t, "2026-06-15"),
		Name: "Tea", CategoryID: &newCat,
	}
	changes := TransactionDiff(before, after)
	if len(changes) != 2 {
		t.Fatalf("expected 2 changes (name, category_id), got %d: %+v", len(changes), changes)
	}
	fields := map[string]bool{}
	for _, c := range changes {
		fields[c.Field] = true
	}
	if !fields["name"] || !fields["category_id"] {
		t.Errorf("expected name and category_id, got %v", fields)
	}
}

func TestBudgetDiffDetectsAmountAndPeriodNotIdentity(t *testing.T) {
	before := dbgen.Budget{Amount: mustDec(t, "100.00"), Period: "monthly", Rollover: false}
	after := dbgen.Budget{Amount: mustDec(t, "120.00"), Period: "monthly", Rollover: true}
	changes := BudgetDiff(before, after)
	if len(changes) != 2 {
		t.Fatalf("expected amount + rollover, got %d: %+v", len(changes), changes)
	}
}

func ptr(s string) *string { return &s }
