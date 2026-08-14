package insights

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// The bill_out_of_range producer (MAD-120), and — just as important — the
// partition it forms with overdue_bill. A bill with a stated range has exactly
// three possible outcomes per cycle, and these tests pin all three plus the
// boundaries between them:
//
//	nothing plausibly paid it ....... overdue_bill only
//	paid inside [min, max] .......... neither (silence)
//	paid outside [min, max] ......... bill_out_of_range only
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' \
//	    go test ./internal/insights/ -run TestBillOutOfRange

// billFixture is one household with a checking account, a category, and a
// monthly bill stated at $40–$60 whose most recent occurrence fell 10 days ago —
// inside both the overdue and the range lookback windows.
type billFixture struct {
	pool         *pgxpool.Pool
	q            *dbgen.Queries
	householdID  uuid.UUID
	accountID    uuid.UUID
	categoryID   uuid.UUID
	obligationID uuid.UUID
	dueDate      time.Time
}

func newBillFixture(t *testing.T, name string, shared bool) *billFixture {
	t.Helper()

	url := testdb.URL(t)
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	f := &billFixture{
		pool:        pool,
		q:           dbgen.New(pool),
		householdID: uuid.New(),
		accountID:   uuid.New(),
		categoryID:  uuid.New(),
	}
	userID, itemID := uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO households (id, name) VALUES ($1, $2)`, f.householdID, name)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM households WHERE id = $1`, f.householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`,
		userID, f.householdID, userID.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`,
		itemID, userID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Checking', 'depository')`,
		f.accountID, itemID, f.accountID.String())
	exec(`INSERT INTO categories (id, household_id, name, slug)
	      VALUES ($1, $2, 'Phone', 'phone')`, f.categoryID, f.householdID)

	// A monthly bill whose occurrence lands 10 days ago: far enough past that
	// overdue_bill is eligible, recent enough to be inside both lookbacks.
	today := truncateDay(time.Now().UTC())
	f.dueDate = today.AddDate(0, 0, -10)
	f.obligationID = uuid.New()
	exec(`INSERT INTO recurring_obligations
	      (id, household_id, user_id, is_shared, label, amount, amount_min, amount_max,
	       category_id, account_id, interval_count, interval_unit, anchor_date)
	      VALUES ($1, $2, $3, $4, 'Phone bill', '50.00', '40.00', '60.00',
	              $5, $6, 1, 'month', $7)`,
		f.obligationID, f.householdID, userID, shared,
		f.categoryID, f.accountID, f.dueDate.AddDate(0, -1, 0))
	return f
}

// charge posts a transaction on the bill's due date, in the bill's category —
// the shape the matcher's category branch looks for.
func (f *billFixture) charge(t *testing.T, amount string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO transactions (account_id, amount, currency, date, name, category_id, source)
		 VALUES ($1, $2, 'USD', $3, 'Telco Inc', $4, 'plaid')`,
		f.accountID, amount, f.dueDate.Format(time.DateOnly), f.categoryID); err != nil {
		t.Fatalf("seed transaction: %v", err)
	}
}

// generate runs the whole engine (AI off) and returns the raised insights keyed
// by kind. Going through Generate rather than calling Detect directly is
// deliberate: it proves the producer is actually registered.
func (f *billFixture) generate(t *testing.T) map[string]dbgen.Insight {
	t.Helper()
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `DELETE FROM insights WHERE household_id = $1`, f.householdID); err != nil {
		t.Fatalf("clear insights: %v", err)
	}
	now := time.Now()
	if _, err := Generate(ctx, f.q, ai.New(config.AIConfig{}), f.householdID, now); err != nil {
		t.Fatalf("generate: %v", err)
	}
	rows, err := f.q.ListInsights(ctx, dbgen.ListInsightsParams{
		HouseholdID: f.householdID, IncludeDismissed: true, AsOf: now,
	})
	if err != nil {
		t.Fatalf("list insights: %v", err)
	}
	out := make(map[string]dbgen.Insight, len(rows))
	for _, r := range rows {
		out[r.Kind] = r
	}
	return out
}

func truncateDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func insightData(t *testing.T, row dbgen.Insight) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(row.Data, &data); err != nil {
		t.Fatalf("unmarshal insight data: %v", err)
	}
	return data
}

// TestBillOutOfRangeOverCharge is the headline case from MAD-120: a phone bill
// stated at $40–$60 lands at $90.
func TestBillOutOfRangeOverCharge(t *testing.T) {
	f := newBillFixture(t, "Bill Range Over", true)
	f.charge(t, "90.00")

	raised := f.generate(t)

	row, ok := raised["bill_out_of_range"]
	if !ok {
		t.Fatal("expected a bill_out_of_range insight for the $90 charge; got none")
	}
	data := insightData(t, row)
	if data["direction"] != "over" {
		t.Errorf("direction = %v, want over", data["direction"])
	}
	// $90 against a $60 ceiling is $30 over. The number in the feed is the one
	// the SQL produced — never re-derived, and never a float.
	if data["difference"] != "30.00" {
		t.Errorf("difference = %v, want 30.00", data["difference"])
	}
	if data["charged_amount"] != "90.00" {
		t.Errorf("charged_amount = %v, want 90.00", data["charged_amount"])
	}
	if data["expected_min"] != "40.00" || data["expected_max"] != "60.00" {
		t.Errorf("expected range = %v–%v, want 40.00–60.00", data["expected_min"], data["expected_max"])
	}
	if data["obligation_id"] != f.obligationID.String() {
		t.Errorf("obligation_id = %v, want %s", data["obligation_id"], f.obligationID)
	}
	if data["due_date"] != f.dueDate.Format(time.DateOnly) {
		t.Errorf("due_date = %v, want %s", data["due_date"], f.dueDate.Format(time.DateOnly))
	}

	// The partition: a charge this producer claims must NOT also read as
	// unpaid. Reporting one $90 payment as both "we can't find a payment" and
	// "that was more than expected" is the failure mode the shared candidate
	// predicate exists to prevent.
	if _, ok := raised["overdue_bill"]; ok {
		t.Error("an out-of-range charge must suppress overdue_bill for the same occurrence")
	}
}

// TestBillOutOfRangeUnderCharge covers the other direction — a partial payment
// or proration — which is informational rather than urgent.
func TestBillOutOfRangeUnderCharge(t *testing.T) {
	f := newBillFixture(t, "Bill Range Under", true)
	f.charge(t, "25.00")

	raised := f.generate(t)

	row, ok := raised["bill_out_of_range"]
	if !ok {
		t.Fatal("expected a bill_out_of_range insight for the $25 charge; got none")
	}
	data := insightData(t, row)
	if data["direction"] != "under" {
		t.Errorf("direction = %v, want under", data["direction"])
	}
	if data["difference"] != "15.00" {
		t.Errorf("difference = %v, want 15.00", data["difference"])
	}
	// Under-charges never push: paying less than expected cannot overdraw
	// anyone, so it must stay below the jobs layer's priority-4 push threshold.
	if row.Priority >= 4 {
		t.Errorf("priority = %d, want below the push threshold of 4", row.Priority)
	}
	if _, ok := raised["overdue_bill"]; ok {
		t.Error("an under-range charge is still a payment; overdue_bill must not fire")
	}
}

// TestBillInRangeIsSilent covers the boundaries. A charge landing exactly on
// either bound is inside the range the household stated, so the correct
// behaviour is complete silence — no surprise AND no overdue nag.
func TestBillInRangeIsSilent(t *testing.T) {
	for _, amount := range []string{"40.00", "50.00", "60.00"} {
		t.Run(amount, func(t *testing.T) {
			f := newBillFixture(t, "Bill In Range "+amount, true)
			f.charge(t, amount)

			raised := f.generate(t)
			if row, ok := raised["bill_out_of_range"]; ok {
				t.Errorf("%s is inside 40.00–60.00; got %q", amount, row.Title)
			}
			if _, ok := raised["overdue_bill"]; ok {
				t.Errorf("%s paid the bill; overdue_bill must not fire", amount)
			}
		})
	}
}

// TestBillUnpaidStillGoesOverdue is the third branch of the partition: nothing
// plausibly paid the bill, so the overdue reminder owns it and the range
// producer stays quiet. Adding ranges must not create a hole where a genuinely
// missed bill is reported by neither producer.
func TestBillUnpaidStillGoesOverdue(t *testing.T) {
	f := newBillFixture(t, "Bill Range Unpaid", true)
	// No charge at all.

	raised := f.generate(t)
	if _, ok := raised["overdue_bill"]; !ok {
		t.Error("an unpaid ranged bill must still raise overdue_bill")
	}
	if _, ok := raised["bill_out_of_range"]; ok {
		t.Error("nothing was charged; there is no amount to be out of range")
	}
}

// TestBillFarOutsideRangeIsNotTheBill guards the candidate band. A charge in the
// bill's category at ten times the expected amount is some other purchase, not
// the phone bill arriving high — claiming it as the bill would put a confidently
// wrong number in the feed. The occurrence stays unpaid, which is the honest
// answer.
func TestBillFarOutsideRangeIsNotTheBill(t *testing.T) {
	f := newBillFixture(t, "Bill Range Far Out", true)
	f.charge(t, "500.00")

	raised := f.generate(t)
	if row, ok := raised["bill_out_of_range"]; ok {
		t.Errorf("a $500 category charge is not a $40–$60 phone bill; got %q", row.Title)
	}
	if _, ok := raised["overdue_bill"]; !ok {
		t.Error("nothing plausibly paid the bill, so overdue_bill should still fire")
	}
}

// TestBillOutOfRangeRespectsVisibility pins the scoping every household feed
// producer needs: a member's PERSONAL bill must not surface in the shared feed,
// where the rest of the household would see it.
func TestBillOutOfRangeRespectsVisibility(t *testing.T) {
	f := newBillFixture(t, "Bill Range Personal", false)
	f.charge(t, "90.00")

	raised := f.generate(t)
	if row, ok := raised["bill_out_of_range"]; ok {
		t.Errorf("a personal bill must not reach the household feed; got %q", row.Title)
	}
}

// TestBillOutOfRangeClearedBySatisfaction covers the escape hatch: once a member
// accepts the charge for that cycle, the surprise is settled and must not be
// raised again on the next generation pass.
func TestBillOutOfRangeClearedBySatisfaction(t *testing.T) {
	f := newBillFixture(t, "Bill Range Accepted", true)
	f.charge(t, "90.00")

	if _, ok := f.generate(t)["bill_out_of_range"]; !ok {
		t.Fatal("expected the insight before the charge is accepted")
	}

	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO obligation_satisfaction (obligation_id, due_date, source)
		 VALUES ($1, $2, 'manual')`, f.obligationID, f.dueDate); err != nil {
		t.Fatalf("mark satisfied: %v", err)
	}

	raised := f.generate(t)
	if row, ok := raised["bill_out_of_range"]; ok {
		t.Errorf("accepting the charge should settle the occurrence; still got %q", row.Title)
	}
	if _, ok := raised["overdue_bill"]; ok {
		t.Error("a satisfied occurrence must not fall through to overdue_bill either")
	}
}

// TestBillWithoutRangeRaisesNothing pins the opt-in. An obligation with no
// stated range is the ordinary, pre-MAD-120 case: the ±25% heuristic still
// decides whether it was paid, and a charge outside that is not a "surprise"
// because the household never stated an expectation to miss.
func TestBillWithoutRangeRaisesNothing(t *testing.T) {
	f := newBillFixture(t, "Bill No Range", true)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE recurring_obligations SET amount_min = NULL, amount_max = NULL WHERE id = $1`,
		f.obligationID); err != nil {
		t.Fatalf("clear range: %v", err)
	}
	f.charge(t, "90.00")

	raised := f.generate(t)
	if row, ok := raised["bill_out_of_range"]; ok {
		t.Errorf("an un-ranged bill has no range to miss; got %q", row.Title)
	}
}

// TestBillOutOfRangeDedupeKeyIsPerCycle pins the key shape the mark-paid handler
// depends on. If the key stopped carrying the due date, one surprise would
// suppress every later cycle's.
func TestBillOutOfRangeDedupeKeyIsPerCycle(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	jan := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	feb := time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)

	if got, want := BillOutOfRangeDedupeKey(id, jan),
		fmt.Sprintf("bill_out_of_range:%s:2026-01-15", id); got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
	if BillOutOfRangeDedupeKey(id, jan) == BillOutOfRangeDedupeKey(id, feb) {
		t.Error("two cycles of the same bill must be two different insights")
	}
}
