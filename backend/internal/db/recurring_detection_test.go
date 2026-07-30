package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// detectionAsOf is the clock every case below runs against. Fixed rather than
// time.Now() because the gone-quiet rule is measured from it — a drifting clock
// would make these assertions expire.
var detectionAsOf = mustDateValue("2026-07-01")

func mustDateValue(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d
}

// detectionFixture is one household with one account, and a helper to lay down
// charges at a named merchant.
type detectionFixture struct {
	q           *dbgen.Queries
	householdID uuid.UUID
	userID      uuid.UUID
	acctID      uuid.UUID
	cats        map[string]uuid.UUID
	exec        func(sql string, args ...any)
}

func newDetectionFixture(t *testing.T) (context.Context, *detectionFixture) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	f := &detectionFixture{
		q:           dbgen.New(pool),
		householdID: uuid.New(),
		userID:      uuid.New(),
		acctID:      uuid.New(),
		cats:        map[string]uuid.UUID{},
	}
	f.exec = func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}

	itemID := uuid.New()
	f.exec(`INSERT INTO households (id, name) VALUES ($1, 'Detection Test')`, f.householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, f.householdID)
	})
	f.exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	        VALUES ($1, $2, $3, 'x', 'Tester')`,
		f.userID, f.householdID, f.userID.String()+"@example.test")
	f.exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	        VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemID, f.userID, itemID.String())
	f.exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	        VALUES ($1, $2, $3, 'Checking', 'depository')`, f.acctID, itemID, f.acctID.String())

	// Slugs match the seeded global set (migration 00002), because the detector's
	// discretionary-category exclusion matches on slug.
	for name, slug := range map[string]string{
		"services": "general-services",
		"utility":  "rent-and-utilities",
		"dining":   "food-and-drink",
		"grocery":  "groceries",
	} {
		id := uuid.New()
		f.cats[name] = id
		f.exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, $3, $4)`,
			id, f.householdID, name, slug)
	}
	return ctx, f
}

// charge lays down one transaction at a merchant on a date.
func (f *detectionFixture) charge(key, amount, date, category string) {
	f.exec(`INSERT INTO transactions
	        (account_id, amount, currency, date, name, merchant_name, merchant_key, category_id, source)
	        VALUES ($1, $2, 'USD', $3, $4, $4, $5, $6, 'plaid')`,
		f.acctID, amount, date, key, key, f.cats[category])
}

// detected reports whether the merchant key survives detection, and its row.
func (f *detectionFixture) detected(t *testing.T, ctx context.Context, key string) (dbgen.GetRecurringMerchantsRow, bool) {
	t.Helper()
	rows, err := f.q.GetRecurringMerchants(ctx, dbgen.GetRecurringMerchantsParams{
		HouseholdID: f.householdID,
		UserID:      f.userID,
		// Matches the production lookback so a long cadence has room to be seen
		// more than once.
		Date:    detectionAsOf.AddDate(0, -36, 0),
		Column4: detectionAsOf,
	})
	if err != nil {
		t.Fatalf("GetRecurringMerchants: %v", err)
	}
	for _, r := range rows {
		if r.MerchantKey == key {
			return r, true
		}
	}
	return dbgen.GetRecurringMerchantsRow{}, false
}

// TestRecurringDetectsLongCadences covers the bands added when the detector's
// gap ceiling went from 40 days to 400. Before this a quarterly city bill and an
// annual renewal were structurally invisible: no amount of history could make
// them recur "weekly to monthly".
func TestRecurringDetectsLongCadences(t *testing.T) {
	ctx, f := newDetectionFixture(t)

	// Quarterly, four charges across nine months, landing within a couple of days
	// of the quarter each time. The shape of a real utility bill.
	for _, d := range []string{"2025-10-05", "2026-01-04", "2026-04-06", "2026-06-30"} {
		f.charge("city water", "136.00", d, "utility")
	}
	// Annual, seen exactly twice at an identical amount — all the evidence a
	// yearly charge can ever offer inside a household's history.
	f.charge("domain renewal", "87.00", "2025-07-10", "services")
	f.charge("domain renewal", "87.00", "2026-07-01", "services")

	if row, ok := f.detected(t, ctx, "city water"); !ok {
		t.Error("a quarterly bill with four tight cycles was not detected")
	} else if gap := row.AvgGapDays.InexactFloat64(); gap < 80 || gap > 100 {
		t.Errorf("quarterly avg_gap_days = %.1f, want roughly 90", gap)
	}

	if row, ok := f.detected(t, ctx, "domain renewal"); !ok {
		t.Error("an annual renewal seen twice at an identical amount was not detected")
	} else if row.Occurrences != 2 {
		t.Errorf("annual occurrences = %d, want 2", row.Occurrences)
	}
}

// TestRecurringRejectsLongCadenceCoincidence is the other half of the bands
// above. Long cycles are where coincidence is easiest to mistake for a bill,
// so each rejection here has a distinct reason.
func TestRecurringRejectsLongCadenceCoincidence(t *testing.T) {
	ctx, f := newDetectionFixture(t)

	// Two visits a year apart at prices that merely resemble each other. Without
	// the amount-identity rule this is indistinguishable from a renewal.
	f.charge("gift shop", "80.00", "2025-07-10", "services")
	f.charge("gift shop", "88.00", "2026-07-01", "services")

	// Two shopping trips two months apart. The single gap makes the spread
	// trivially zero, so only the minimum span rejects it.
	f.charge("big box", "45.00", "2026-04-28", "services")
	f.charge("big box", "45.00", "2026-06-27", "services")

	// A subscription split across two ALTERNATING descriptors. Each half alone is
	// three identical charges at a flawless 60-day cadence — statistically a
	// perfect bi-monthly bill. Only the half-year span requirement keeps it out
	// until the descriptors are merged, which is what stops one subscription
	// showing up as two phantom bills.
	for _, d := range []string{"2026-02-05", "2026-04-05", "2026-06-05"} {
		f.charge("split-a", "11.00", d, "services")
	}

	// Irregular: right cadence on average, nothing like a schedule in practice.
	for _, d := range []string{"2025-09-01", "2025-11-20", "2026-01-02", "2026-06-15"} {
		f.charge("whenever", "30.00", d, "services")
	}

	for _, key := range []string{"gift shop", "big box", "split-a", "whenever"} {
		if row, ok := f.detected(t, ctx, key); ok {
			t.Errorf("%q should not be recurring: %+v", key, row)
		}
	}
}

// TestRecurringSkipsDiscretionaryCategories covers the category gate. Eating at
// the same place on a rhythm is a habit, not a bill, but nothing in the cadence
// arithmetic can tell them apart — a fast-food merchant visited monthly passes
// every statistical threshold the detector has.
func TestRecurringSkipsDiscretionaryCategories(t *testing.T) {
	ctx, f := newDetectionFixture(t)

	dates := []string{
		"2025-08-08", "2025-09-07", "2025-10-08", "2025-11-07",
		"2025-12-08", "2026-01-07", "2026-02-06", "2026-03-08",
		"2026-04-07", "2026-05-07", "2026-06-06",
	}
	for _, d := range dates {
		f.charge("burger place", "12.00", d, "dining")
		f.charge("corner market", "60.00", d, "grocery")
		// Same shape, ordinary category: the control. If this one fails to be
		// detected the test is proving nothing about categories.
		f.charge("some saas", "12.00", d, "services")
	}

	if _, ok := f.detected(t, ctx, "some saas"); !ok {
		t.Fatal("control merchant was not detected; the fixture shape is wrong")
	}
	for _, key := range []string{"burger place", "corner market"} {
		if row, ok := f.detected(t, ctx, key); ok {
			t.Errorf("%q is discretionary spend and must not be recurring: %+v", key, row)
		}
	}
}

// TestRecurringQuietWindowScalesWithCadence covers the rule that replaced six
// copies of a flat day count in Go. One number cannot serve both a weekly
// payment and an annual renewal, and when the copies disagreed (45 days in the
// report handler, 75 in obligation promotion) the Spending page and the Schedule
// page showed different merchants.
func TestRecurringQuietWindowScalesWithCadence(t *testing.T) {
	ctx, f := newDetectionFixture(t)

	// Monthly, last charged 60 days before the clock. Inside a monthly charge's
	// 75-day tolerance — and squarely inside the 46-75 day window where the two
	// pages used to disagree.
	for _, d := range []string{"2026-02-02", "2026-03-04", "2026-04-03", "2026-05-02"} {
		f.charge("still paying", "20.00", d, "services")
	}
	// Monthly, last charged 88 days before the clock: past tolerance, cancelled.
	for _, d := range []string{"2026-01-04", "2026-02-03", "2026-03-05", "2026-04-04"} {
		f.charge("cancelled", "20.00", d, "services")
	}
	// Weekly, last charged 35 days before the clock. Five missed weeks is dead
	// even though the same 35 days would leave a monthly charge comfortably live.
	for _, d := range []string{
		"2026-03-27", "2026-04-03", "2026-04-10", "2026-04-17",
		"2026-04-24", "2026-05-01", "2026-05-08", "2026-05-15",
		"2026-05-22", "2026-05-27",
	} {
		f.charge("weekly gone", "15.00", d, "services")
	}

	if _, ok := f.detected(t, ctx, "still paying"); !ok {
		t.Error("a monthly charge 60 days quiet is still live and must be detected")
	}
	if row, ok := f.detected(t, ctx, "cancelled"); ok {
		t.Errorf("a monthly charge 88 days quiet is cancelled: %+v", row)
	}
	if row, ok := f.detected(t, ctx, "weekly gone"); ok {
		t.Errorf("a weekly charge 35 days quiet is cancelled: %+v", row)
	}
}
