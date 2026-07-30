package obligations

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// promoteNow is the clock every promotion test runs on, so the detector's
// look-back and "gone quiet" windows are fixed rather than drifting with the
// real date.
var promoteNow = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

// seedMonthlyMerchant lays down five monthly charges at one merchant — enough
// for GetRecurringMerchants to call it recurring (three or more charges, a
// regular 30-31 day gap, and a span well past its 45-day minimum) with the last
// one recent enough to still count as live.
func seedMonthlyMerchant(t *testing.T, f *testFixture, key, name, amount string, categoryID uuid.UUID) {
	t.Helper()
	for _, date := range []string{"2026-03-10", "2026-04-10", "2026-05-10", "2026-06-10", "2026-07-10"} {
		f.exec(`INSERT INTO transactions
		        (account_id, amount, currency, date, name, merchant_name, merchant_key, category_id, source)
		        VALUES ($1, $2, 'USD', $3, $4, $4, $5, $6, 'plaid')`,
			f.accountID, amount, date, name, key, categoryID)
	}
}

func (f *testFixture) detectedRows(t *testing.T) []dbgen.RecurringObligation {
	t.Helper()
	rows, err := f.q.ListObligations(context.Background(), dbgen.ListObligationsParams{
		HouseholdID: f.householdID, UserID: &f.userID,
	})
	if err != nil {
		t.Fatalf("ListObligations: %v", err)
	}
	out := make([]dbgen.RecurringObligation, 0, len(rows))
	for _, r := range rows {
		if r.Source == "detected" {
			out = append(out, r)
		}
	}
	return out
}

// TestPromoteIsIdempotent covers the three promises promotion makes: it maps a
// detected gap to a real cadence, running it twice leaves one row, and a row the
// user has corrected is never touched again.
func TestPromoteIsIdempotent(t *testing.T) {
	f := newFixture(t, "Promotion")
	ctx := context.Background()

	catID := uuid.New()
	f.exec(`INSERT INTO categories (id, household_id, name, slug, is_fixed)
	        VALUES ($1, $2, 'Utilities', 'utilities', TRUE)`, catID, f.householdID)
	seedMonthlyMerchant(t, f, "acme-power", "ACME Power", "180.00", catID)

	n, err := Promote(ctx, f.q, f.householdID, promoteNow)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if n != 1 {
		t.Fatalf("first pass promoted %d merchants, want 1", n)
	}

	rows := f.detectedRows(t)
	if len(rows) != 1 {
		t.Fatalf("after one pass there are %d detected rows, want 1", len(rows))
	}
	got := rows[0]

	// A 30-31 day gap is monthly, not "every 30.5 days" — a raw gap would drift
	// the bill off its day of the month within a year.
	if got.IntervalCount != 1 || got.IntervalUnit != UnitMonth {
		t.Errorf("cadence = every %d %s, want every 1 month", got.IntervalCount, got.IntervalUnit)
	}
	// The anchor is the last observed charge, so the first derived occurrence is
	// the NEXT one due rather than a re-run of one already paid.
	if anchor := got.AnchorDate.Format(time.DateOnly); anchor != "2026-07-10" {
		t.Errorf("anchor_date = %s, want 2026-07-10 (the last charge)", anchor)
	}
	if !got.Amount.Equal(decimal.RequireFromString("180.00")) {
		t.Errorf("amount = %s, want 180.00", got.Amount)
	}
	// The category is what lets safe-to-spend tell a covered fixed cost from an
	// uncovered one, so promotion must carry it across.
	if got.CategoryID == nil || *got.CategoryID != catID {
		t.Errorf("category_id = %v, want the merchant's usual category %s", got.CategoryID, catID)
	}
	if got.UserID != nil {
		t.Errorf("a detected row is household-owned; user_id = %v", got.UserID)
	}

	// Re-running must refresh in place, not add a second row.
	if _, err := Promote(ctx, f.q, f.householdID, promoteNow); err != nil {
		t.Fatalf("second Promote: %v", err)
	}
	if rows := f.detectedRows(t); len(rows) != 1 {
		t.Fatalf("after two passes there are %d detected rows, want 1", len(rows))
	}

	// A correction the user makes must survive every future pass. Without the
	// guard on the upsert, this is where it would be silently reverted.
	if _, err := f.q.UpdateObligation(ctx, dbgen.UpdateObligationParams{
		ID: got.ID, HouseholdID: f.householdID, UserID: &f.userID,
		Label: "Power bill", Amount: decimal.RequireFromString("205.00"),
		CategoryID: &catID, AccountID: nil,
		IntervalCount: 3, IntervalUnit: UnitMonth,
		AnchorDate: mustDate(t, "2026-08-01"), EndDate: nil,
		IsShared: true, IsActive: true,
	}); err != nil {
		t.Fatalf("UpdateObligation: %v", err)
	}

	if _, err := Promote(ctx, f.q, f.householdID, promoteNow); err != nil {
		t.Fatalf("Promote after edit: %v", err)
	}
	rows = f.detectedRows(t)
	if len(rows) != 1 {
		t.Fatalf("after the edited pass there are %d detected rows, want 1", len(rows))
	}
	edited := rows[0]
	if !edited.UserEdited {
		t.Error("user_edited was not stamped by the edit")
	}
	if edited.Label != "Power bill" {
		t.Errorf("label = %q, want the user's %q", edited.Label, "Power bill")
	}
	if !edited.Amount.Equal(decimal.RequireFromString("205.00")) {
		t.Errorf("amount = %s, want the user's 205.00", edited.Amount)
	}
	if edited.IntervalCount != 3 {
		t.Errorf("interval_count = %d, want the user's 3", edited.IntervalCount)
	}
	if edited.AnchorDate.Format(time.DateOnly) != "2026-08-01" {
		t.Errorf("anchor_date = %s, want the user's 2026-08-01",
			edited.AnchorDate.Format(time.DateOnly))
	}
}

// TestPromoteHonoursSuppression covers both directions of the suppression rule:
// a merchant marked "not recurring" before promotion is never promoted, and one
// marked after promotion is retired rather than left on next week's calendar.
func TestPromoteHonoursSuppression(t *testing.T) {
	f := newFixture(t, "Promotion suppression")
	ctx := context.Background()

	// Deliberately NOT a food-and-drink or groceries category: those are excluded
	// from detection outright, which would make this test pass for the wrong
	// reason. See TestRecurringSkipsDiscretionaryCategories.
	catID := uuid.New()
	f.exec(`INSERT INTO categories (id, household_id, name, slug)
	        VALUES ($1, $2, 'General Services', 'general-services')`, catID, f.householdID)
	seedMonthlyMerchant(t, f, "never-me", "Coincidence Mart", "60.00", catID)

	// Suppressed before the first pass: it must never reach the calendar at all.
	f.exec(`INSERT INTO recurring_overrides (household_id, merchant_key, merchant_label)
	        VALUES ($1, 'never-me', 'Coincidence Mart')`, f.householdID)

	if _, err := Promote(ctx, f.q, f.householdID, promoteNow); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if rows := f.detectedRows(t); len(rows) != 0 {
		t.Fatalf("a suppressed merchant was promoted: %d rows", len(rows))
	}

	// Now the other direction: promote a merchant, then suppress it.
	seedMonthlyMerchant(t, f, "later-me", "Regret Media", "12.00", catID)
	if _, err := Promote(ctx, f.q, f.householdID, promoteNow); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	rows := f.detectedRows(t)
	if len(rows) != 1 || !rows[0].IsActive {
		t.Fatalf("expected one active detected row, got %+v", rows)
	}

	f.exec(`INSERT INTO recurring_overrides (household_id, merchant_key, merchant_label)
	        VALUES ($1, 'later-me', 'Regret Media')`, f.householdID)
	if _, err := Promote(ctx, f.q, f.householdID, promoteNow); err != nil {
		t.Fatalf("Promote after suppression: %v", err)
	}
	rows = f.detectedRows(t)
	if len(rows) != 1 {
		t.Fatalf("expected the row to be retired, not removed; got %d rows", len(rows))
	}
	if rows[0].IsActive {
		t.Error("a suppressed merchant is still an active obligation")
	}

	// And a retired obligation must not appear in any expansion.
	occ, err := ListUpcoming(ctx, f.q, f.householdID, f.userID,
		promoteNow, promoteNow.AddDate(0, 3, 0))
	if err != nil {
		t.Fatalf("ListUpcoming: %v", err)
	}
	if len(occ) != 0 {
		t.Errorf("a suppressed merchant still expands to %d occurrences", len(occ))
	}
}

// TestPromoteSkipsGoneQuietMerchants checks the live-merchant gate. A cancelled
// subscription stops charging; without this it would sit on the calendar
// forever, predicting money that will never leave the account.
func TestPromoteSkipsGoneQuietMerchants(t *testing.T) {
	f := newFixture(t, "Promotion quiet")
	ctx := context.Background()

	catID := uuid.New()
	f.exec(`INSERT INTO categories (id, household_id, name, slug)
	        VALUES ($1, $2, 'Entertainment', 'entertainment')`, catID, f.householdID)

	// Regular monthly charges that stopped five months before `now`.
	for _, date := range []string{"2025-11-05", "2025-12-05", "2026-01-05", "2026-02-05"} {
		f.exec(`INSERT INTO transactions
		        (account_id, amount, currency, date, name, merchant_name, merchant_key, category_id, source)
		        VALUES ($1, '9.99', 'USD', $2, 'Old Stream', 'Old Stream', 'old-stream', $3, 'plaid')`,
			f.accountID, date, catID)
	}

	if _, err := Promote(ctx, f.q, f.householdID, promoteNow); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if rows := f.detectedRows(t); len(rows) != 0 {
		t.Errorf("a cancelled subscription was promoted to the calendar: %+v", rows)
	}
}

// TestPromoteRetiresUndetectedMerchants is the fix for bills that were counted
// twice. Promotion is an upsert keyed on the RESOLVED merchant key, and nothing
// used to clear a row, so a merchant that stopped being detected sat on the
// calendar forever. The expensive version of that is a descriptor merge: the
// resolved key changes, promotion writes a fresh row under the entity, and the
// row under the raw descriptor keeps billing alongside it.
func TestPromoteRetiresUndetectedMerchants(t *testing.T) {
	f := newFixture(t, "Promotion retirement")
	ctx := context.Background()

	catID := uuid.New()
	f.exec(`INSERT INTO categories (id, household_id, name, slug)
	        VALUES ($1, $2, 'General Services', 'general-services')`, catID, f.householdID)
	seedMonthlyMerchant(t, f, "gone-soon", "Ephemeral Media", "8.00", catID)

	if _, err := Promote(ctx, f.q, f.householdID, promoteNow); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	rows := f.detectedRows(t)
	if len(rows) != 1 || !rows[0].IsActive {
		t.Fatalf("expected one active detected row, got %+v", rows)
	}

	// Take the merchant out of the detector's reach the way a real change would:
	// its charges stop existing under that key. A merge does exactly this, by
	// moving them under an entity id instead.
	f.exec(`DELETE FROM transactions WHERE merchant_key = 'gone-soon'`)

	if _, err := Promote(ctx, f.q, f.householdID, promoteNow); err != nil {
		t.Fatalf("Promote after the merchant vanished: %v", err)
	}
	rows = f.detectedRows(t)
	if len(rows) != 1 {
		t.Fatalf("expected the row to be retired, not deleted; got %d rows", len(rows))
	}
	if rows[0].IsActive {
		t.Error("an undetected merchant is still an active obligation")
	}

	// A retired obligation must reach no expansion — the calendar, the projection
	// and safe-to-spend all read through the same one.
	occ, err := ListUpcoming(ctx, f.q, f.householdID, f.userID,
		promoteNow, promoteNow.AddDate(0, 3, 0))
	if err != nil {
		t.Fatalf("ListUpcoming: %v", err)
	}
	if len(occ) != 0 {
		t.Errorf("a retired merchant still expands to %d occurrences", len(occ))
	}
}

// TestPromoteRetirementSparesUserEditedRows guards the other side of the sweep.
// Retirement must respect the same boundary the upsert does: once someone has
// corrected a bill by hand it is theirs, and deleting a correction because the
// detector lost interest is worse than carrying a stale row.
func TestPromoteRetirementSparesUserEditedRows(t *testing.T) {
	f := newFixture(t, "Promotion retirement edited")
	ctx := context.Background()

	catID := uuid.New()
	f.exec(`INSERT INTO categories (id, household_id, name, slug)
	        VALUES ($1, $2, 'General Services', 'general-services')`, catID, f.householdID)
	seedMonthlyMerchant(t, f, "curated", "Hand Tuned", "40.00", catID)

	if _, err := Promote(ctx, f.q, f.householdID, promoteNow); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	rows := f.detectedRows(t)
	if len(rows) != 1 {
		t.Fatalf("expected one detected row, got %+v", rows)
	}

	if _, err := f.q.UpdateObligation(ctx, dbgen.UpdateObligationParams{
		ID: rows[0].ID, HouseholdID: f.householdID, UserID: &f.userID,
		Label: "Hand Tuned", Amount: decimal.RequireFromString("41.00"),
		CategoryID: &catID, AccountID: nil,
		IntervalCount: 1, IntervalUnit: UnitMonth,
		AnchorDate: mustDate(t, "2026-07-10"), EndDate: nil,
		IsShared: true, IsActive: true,
	}); err != nil {
		t.Fatalf("UpdateObligation: %v", err)
	}

	f.exec(`DELETE FROM transactions WHERE merchant_key = 'curated'`)

	if _, err := Promote(ctx, f.q, f.householdID, promoteNow); err != nil {
		t.Fatalf("Promote after the merchant vanished: %v", err)
	}
	rows = f.detectedRows(t)
	if len(rows) != 1 {
		t.Fatalf("expected the edited row to survive, got %d rows", len(rows))
	}
	if !rows[0].IsActive {
		t.Error("a user-edited obligation was retired by the detector")
	}
}

// TestProjectSubtractsOnlyKnownObligations checks the projection's arithmetic
// and its two honesty constraints: only depository accounts get a series, and
// obligations with no account still reach the combined line.
func TestProjectSubtractsOnlyKnownObligations(t *testing.T) {
	f := newFixture(t, "Projection")
	ctx := context.Background()

	// A credit card in the same household. Projecting its balance against its
	// own bills would subtract the balance from itself, so it must not appear.
	itemID := uuid.New()
	cardID := uuid.New()
	f.exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	        VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemID, f.userID, itemID.String())
	f.exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, current_balance)
	        VALUES ($1, $2, $3, 'Card', 'credit', '900.00')`, cardID, itemID, cardID.String())

	// $2,000 in checking (seeded by the fixture). Rent on the 10th is assigned to
	// it; the insurance premium names no account.
	f.addObligation(t, "Rent", "1500.00", 1, "month", "2026-07-10", nil)
	f.exec(`INSERT INTO recurring_obligations
	        (household_id, user_id, label, amount, interval_count, interval_unit, anchor_date)
	        VALUES ($1, $2, 'Insurance', '300.00', 1, 'month', '2026-07-20')`,
		f.householdID, f.userID)

	proj, err := Project(ctx, f.q, f.householdID, f.userID, mustDate(t, "2026-07-05"), 30)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}

	if len(proj.Accounts) != 1 {
		t.Fatalf("got %d account series, want 1 (depository only)", len(proj.Accounts))
	}
	if proj.Accounts[0].Name != "Checking" {
		t.Errorf("projected account = %q, want Checking", proj.Accounts[0].Name)
	}

	check := func(label string, got decimal.Decimal, want string) {
		t.Helper()
		if !got.Equal(decimal.RequireFromString(want)) {
			t.Errorf("%s = %s, want %s", label, got, want)
		}
	}

	// Checking sees only the bill assigned to it: 2000 − 1500.
	check("checking low", proj.Accounts[0].LowestBalance, "500.00")
	if proj.Accounts[0].GoesNegative {
		t.Error("checking should not be projected negative")
	}

	// The combined line sees both, including the unassigned premium:
	// 2000 − 1500 − 300.
	check("combined low", proj.Combined.LowestBalance, "200.00")
	check("unassigned", proj.UnassignedTotal, "300.00")
	check("total due", proj.TotalDue, "1800.00")

	// The low point is the day the last bill in the window lands, not the end of
	// the horizon.
	if d := proj.Combined.LowestDate.Format(time.DateOnly); d != "2026-07-20" {
		t.Errorf("combined low date = %s, want 2026-07-20", d)
	}
	// One point per day of the horizon, inclusive of both ends.
	if len(proj.Combined.Points) != 31 {
		t.Errorf("combined series has %d points, want 31", len(proj.Combined.Points))
	}
}
