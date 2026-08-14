package insights

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// retractionFixture is the shared seed for the tests below: one household with a
// checking account and one category, ready for an obligation and a payment.
type retractionFixture struct {
	pool        *pgxpool.Pool
	q           *dbgen.Queries
	householdID uuid.UUID
	userID      uuid.UUID
	acctID      uuid.UUID
	catID       uuid.UUID
	exec        func(sql string, args ...any)
}

func newRetractionFixture(t *testing.T, name string) *retractionFixture {
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

	f := &retractionFixture{
		pool: pool, q: dbgen.New(pool),
		householdID: uuid.New(), userID: uuid.New(),
		acctID: uuid.New(), catID: uuid.New(),
	}
	itemID := uuid.New()
	f.exec = func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}

	f.exec(`INSERT INTO households (id, name) VALUES ($1, $2)`, f.householdID, name)
	f.exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	        VALUES ($1, $2, $3, 'x', 'Tester')`,
		f.userID, f.householdID, f.userID.String()+"@example.test")
	f.exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	        VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemID, f.userID, itemID.String())
	f.exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	        VALUES ($1, $2, $3, 'Checking', 'depository')`, f.acctID, itemID, f.acctID.String())
	f.exec(`INSERT INTO categories (id, household_id, name, slug)
	        VALUES ($1, $2, 'Electric', 'electric')`, f.catID, f.householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, f.householdID)
	})
	return f
}

// generate runs the engine with AI off, so only the deterministic templates and
// the storage behaviour under test are exercised.
func (f *retractionFixture) generate(t *testing.T, now time.Time) {
	t.Helper()
	if _, err := Generate(context.Background(), f.q, ai.New(config.AIConfig{}), f.householdID, now); err != nil {
		t.Fatalf("generate: %v", err)
	}
}

// feed returns the insights of one kind a member would actually see.
func (f *retractionFixture) feed(t *testing.T, kind string, now time.Time) []dbgen.Insight {
	t.Helper()
	return f.list(t, kind, false, now)
}

// history returns them including dismissed and retracted rows (state=all).
func (f *retractionFixture) history(t *testing.T, kind string, now time.Time) []dbgen.Insight {
	t.Helper()
	return f.list(t, kind, true, now)
}

func (f *retractionFixture) list(t *testing.T, kind string, all bool, now time.Time) []dbgen.Insight {
	t.Helper()
	rows, err := f.q.ListInsights(context.Background(), dbgen.ListInsightsParams{
		HouseholdID: f.householdID, IncludeDismissed: all, AsOf: now,
	})
	if err != nil {
		t.Fatalf("list insights: %v", err)
	}
	var out []dbgen.Insight
	for _, r := range rows {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// overdueBill seeds a monthly bill anchored 40 days back, so occurrences fall at
// today-40 (outside the 21-day lookback) and today-10 (inside it, and unpaid).
func (f *retractionFixture) overdueBill(t *testing.T) (uuid.UUID, time.Time) {
	t.Helper()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	anchor := today.AddDate(0, 0, -40)
	ob, err := f.q.CreateObligation(context.Background(), dbgen.CreateObligationParams{
		HouseholdID: f.householdID, UserID: &f.userID, IsShared: true,
		Label: "Electric bill", Amount: decimal.RequireFromString("120.00"),
		CategoryID: &f.catID, IntervalCount: 1, IntervalUnit: "month",
		AnchorDate: anchor,
	})
	if err != nil {
		t.Fatalf("create obligation: %v", err)
	}
	return ob.ID, today.AddDate(0, 0, -10)
}

// TestOverdueBillRetractedOncePaid is the regression this whole mechanism exists
// for. Before retraction the engine only upserted, so an overdue_bill raised
// before the payment synced sat in the feed afterwards — "we can't find a
// payment for it" displayed next to the transaction that paid it, until somebody
// dismissed it by hand.
//
// Deliberately does NOT clear the insights table between the two passes: leaving
// the raised row in place is the entire point.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' \
//	    go test ./internal/insights/ -run TestOverdueBillRetractedOncePaid
func TestOverdueBillRetractedOncePaid(t *testing.T) {
	f := newRetractionFixture(t, "Retraction Test")
	_, due := f.overdueBill(t)

	now := time.Now()
	f.generate(t, now)
	if got := f.feed(t, "overdue_bill", now); len(got) != 1 {
		t.Fatalf("expected 1 overdue_bill before payment, got %d", len(got))
	}

	// The payment lands: same category, within the window, at the expected
	// amount. Detection goes quiet, and the standing row must go with it.
	f.exec(`INSERT INTO transactions (account_id, amount, currency, date, name, category_id, source)
	        VALUES ($1, '120.00', 'USD', $2, 'Electric Co', $3, 'plaid')`,
		f.acctID, due.Format(time.DateOnly), f.catID)

	now = time.Now()
	f.generate(t, now)

	if got := f.feed(t, "overdue_bill", now); len(got) != 0 {
		t.Errorf("overdue_bill should be retracted once the payment posts; feed still shows %d: %q",
			len(got), got[0].Title)
	}

	// Soft, not deleted: history keeps the row, and it is marked as the app's
	// withdrawal rather than a member's dismissal.
	hist := f.history(t, "overdue_bill", now)
	if len(hist) != 1 {
		t.Fatalf("expected the retracted row to survive in history, got %d", len(hist))
	}
	if hist[0].RetractedAt == nil {
		t.Errorf("retracted_at should be set on the withdrawn insight")
	}
	if hist[0].DismissedAt != nil {
		t.Errorf("dismissed_at must stay NULL: no member dismissed this, the app withdrew it")
	}
}

// TestRetractedInsightReRaises pins the asymmetry that made retracted_at a
// separate column instead of a reuse of dismissed_at. A retraction is the app's
// own claim being withdrawn, so the fact coming back has to bring the insight
// back. Folding it into dismissed_at — which UpsertInsight preserves on purpose
// — would have buried the row permanently the first time it was ever retracted.
func TestRetractedInsightReRaises(t *testing.T) {
	f := newRetractionFixture(t, "Re-raise Test")
	_, due := f.overdueBill(t)

	now := time.Now()
	f.generate(t, now)
	f.exec(`INSERT INTO transactions (account_id, amount, currency, date, name, category_id, source)
	        VALUES ($1, '120.00', 'USD', $2, 'Electric Co', $3, 'plaid')`,
		f.acctID, due.Format(time.DateOnly), f.catID)
	now = time.Now()
	f.generate(t, now)
	if got := f.feed(t, "overdue_bill", now); len(got) != 0 {
		t.Fatalf("setup: expected the insight retracted, feed shows %d", len(got))
	}

	// The payment turns out not to be one — a miscategorised transaction the
	// member fixes, or a Plaid removal. The bill is unpaid again.
	f.exec(`DELETE FROM transactions WHERE account_id = $1`, f.acctID)

	now = time.Now()
	f.generate(t, now)

	got := f.feed(t, "overdue_bill", now)
	if len(got) != 1 {
		t.Fatalf("expected the overdue_bill back in the feed once the payment vanished, got %d", len(got))
	}
	if got[0].RetractedAt != nil {
		t.Errorf("retracted_at must be cleared on re-raise, got %v", got[0].RetractedAt)
	}
}

// TestDismissalOutlivesRetraction is the other half of the asymmetry: a member's
// dismissal is a judgement about what they want to see, and neither continued
// detection nor a later retraction may rewrite it as the app's doing. If this
// ever inverted, the history view would start attributing members' decisions to
// the detector.
func TestDismissalOutlivesRetraction(t *testing.T) {
	f := newRetractionFixture(t, "Dismissal Test")
	_, due := f.overdueBill(t)

	now := time.Now()
	f.generate(t, now)
	raised := f.feed(t, "overdue_bill", now)
	if len(raised) != 1 {
		t.Fatalf("setup: expected 1 overdue_bill, got %d", len(raised))
	}
	if err := f.q.DismissInsight(context.Background(), dbgen.DismissInsightParams{
		ID: raised[0].ID, HouseholdID: f.householdID,
	}); err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	// Still detected: the retraction pass must skip an already-dismissed row.
	now = time.Now()
	f.generate(t, now)
	hist := f.history(t, "overdue_bill", now)
	if len(hist) != 1 {
		t.Fatalf("expected 1 row in history, got %d", len(hist))
	}
	if hist[0].DismissedAt == nil {
		t.Errorf("dismissed_at must survive a re-run that still detects the fact")
	}
	if hist[0].RetractedAt != nil {
		t.Errorf("a dismissed row must not also be marked retracted, got %v", hist[0].RetractedAt)
	}

	// And once the fact stops being true, the row stays the member's dismissal
	// rather than being re-stamped as a withdrawal.
	f.exec(`INSERT INTO transactions (account_id, amount, currency, date, name, category_id, source)
	        VALUES ($1, '120.00', 'USD', $2, 'Electric Co', $3, 'plaid')`,
		f.acctID, due.Format(time.DateOnly), f.catID)
	now = time.Now()
	f.generate(t, now)
	hist = f.history(t, "overdue_bill", now)
	if len(hist) != 1 {
		t.Fatalf("expected 1 row in history after the payment, got %d", len(hist))
	}
	if hist[0].RetractedAt != nil {
		t.Errorf("retraction must not overwrite a member's dismissal, got %v", hist[0].RetractedAt)
	}
}

// TestOverdueMatchesMergedMerchant pins the merchant-key resolution fix.
//
// A promoted obligation stores its merchant RESOLVED, so once the household
// merges two descriptors the obligation holds an entity UUID while every
// transaction still holds the raw key. The matcher compared the two directly and
// so matched nothing — the merchant branch of guard (T) was dead for every
// merged merchant, leaving those bills to survive on the category+amount
// fallback alone.
//
// The seed is built so ONLY the merchant branch can suppress the reminder: the
// payment carries no category, which takes the category branch off the table
// entirely.
func TestOverdueMatchesMergedMerchant(t *testing.T) {
	f := newRetractionFixture(t, "Merged Merchant Test")

	today := time.Now().UTC().Truncate(24 * time.Hour)
	anchor := today.AddDate(0, 0, -40)
	due := today.AddDate(0, 0, -10)

	entityID, obID := uuid.New(), uuid.New()
	f.exec(`INSERT INTO merchant_entities (id, household_id, canonical_name)
	        VALUES ($1, $2, 'Electric Co')`, entityID, f.householdID)
	// Two raw descriptors the household merged into one business.
	f.exec(`INSERT INTO merchant_aliases (household_id, entity_id, merchant_key, source)
	        VALUES ($1, $2, 'electric co', 'manual'), ($1, $2, 'electric co ach', 'manual')`,
		f.householdID, entityID)

	// A detected obligation, storing the RESOLVED key exactly as promotion does
	// after a merge. No category, so only the merchant branch can ever match.
	f.exec(`INSERT INTO recurring_obligations
	          (id, household_id, user_id, is_shared, label, amount, interval_count,
	           interval_unit, anchor_date, source, merchant_key)
	        VALUES ($1, $2, $3, TRUE, 'Electric bill', '120.00', 1, 'month', $4, 'detected', $5)`,
		obID, f.householdID, f.userID, anchor, entityID.String())

	// Unpaid: the reminder should stand.
	now := time.Now()
	f.generate(t, now)
	if got := f.feed(t, "overdue_bill", now); len(got) != 1 {
		t.Fatalf("expected 1 overdue_bill for the unpaid merged-merchant bill, got %d", len(got))
	}

	// The payment lands under the OTHER raw descriptor of the merged pair, with
	// no category and an amount the ±25% band would have rejected anyway. Only
	// resolved-to-resolved merchant matching can suppress this.
	f.exec(`INSERT INTO transactions (account_id, amount, currency, date, name, merchant_key, source)
	        VALUES ($1, '211.40', 'USD', $2, 'ELECTRIC CO ACH', 'electric co ach', 'plaid')`,
		f.acctID, due.Format(time.DateOnly))

	now = time.Now()
	f.generate(t, now)
	if got := f.feed(t, "overdue_bill", now); len(got) != 0 {
		t.Errorf("a payment under a merged alias of the obligation's merchant must satisfy it; "+
			"feed still shows %d: %q", len(got), got[0].Title)
	}

	// Control: the match is the ALIAS resolution doing the work, not the payment
	// merely existing. Drop the alias that maps the payment's descriptor and the
	// two merchants resolve apart again, so the bill reads as unpaid.
	f.exec(`DELETE FROM merchant_aliases WHERE household_id = $1 AND merchant_key = 'electric co ach'`,
		f.householdID)
	now = time.Now()
	f.generate(t, now)
	if got := f.feed(t, "overdue_bill", now); len(got) != 1 {
		t.Errorf("without the alias the payment resolves to a different merchant and the bill is "+
			"unpaid again; expected 1 overdue_bill, got %d", len(got))
	}
}
