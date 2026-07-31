package insights

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// These tests need a database (TEST_DATABASE_URL, skipped without one) because
// every figure the two anomaly producers act on is computed in SQL. The Go-side
// threshold maths is covered separately and without a database in
// anomaly_math_test.go.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/insights/

// anomalyFixture is a seeded household with a spending category and TWO
// accounts. The second account exists for one specific test: the same
// institution linked twice reports one charge on two cards, and the duplicate
// detector must not read that as a double charge.
type anomalyFixture struct {
	q           *dbgen.Queries
	householdID uuid.UUID
	spendCat    uuid.UUID
	acctA       uuid.UUID
	acctB       uuid.UUID
	exec        func(sql string, args ...any)
}

func newAnomalyFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) anomalyFixture {
	t.Helper()
	householdID := uuid.New()
	userID := uuid.New()
	itemA, itemB := uuid.New(), uuid.New()
	acctA, acctB := uuid.New(), uuid.New()
	spendCat := uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Anomaly Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	for _, p := range []struct {
		item, acct uuid.UUID
		name       string
	}{{itemA, acctA, "Card A"}, {itemB, acctB, "Card B"}} {
		exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
		      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, p.item, userID, p.item.String())
		exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
		      VALUES ($1, $2, $3, $4, 'depository')`, p.acct, p.item, p.acct.String(), p.name)
	}
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'General', 'general')`,
		spendCat, householdID)

	return anomalyFixture{
		q: dbgen.New(pool), householdID: householdID,
		spendCat: spendCat, acctA: acctA, acctB: acctB, exec: exec,
	}
}

// spend records one outflow at a merchant, with an explicit merchant_key so the
// baselines group the way production does.
func (f anomalyFixture) spend(amount, date, merchant string) uuid.UUID {
	return f.spendOn(f.acctA, amount, date, merchant)
}

func (f anomalyFixture) spendOn(acct uuid.UUID, amount, date, merchant string) uuid.UUID {
	id := uuid.New()
	f.exec(`INSERT INTO transactions
	          (id, account_id, plaid_transaction_id, amount, currency, date, name,
	           merchant_name, merchant_key, category_id, source)
	        VALUES ($1, $2, $3, $4, 'USD', $5, $6, $6, $7, $8, 'plaid')`,
		id, acct, id.String(), amount, date, merchant, merchantKeyOf(merchant), f.spendCat)
	return id
}

// merchantKeyOf mimics plaid.MerchantKey closely enough for seeding: a stable
// normalised token per merchant name.
func merchantKeyOf(merchant string) string {
	out := make([]rune, 0, len(merchant))
	for _, r := range merchant {
		switch {
		case r >= 'A' && r <= 'Z':
			out = append(out, r+32)
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			out = append(out, r)
		}
	}
	return string(out)
}

func (f anomalyFixture) suppress(merchantKey, label, scope string) {
	if err := f.q.SuppressAnomalyMerchant(context.Background(), dbgen.SuppressAnomalyMerchantParams{
		HouseholdID: f.householdID, MerchantKey: merchantKey, MerchantLabel: label, Scope: scope,
	}); err != nil {
		panic(err)
	}
}

func (f anomalyFixture) setSensitivity(t *testing.T, name string) {
	t.Helper()
	hh := f.householdID
	if _, err := f.q.UpsertHouseholdPreference(context.Background(), dbgen.UpsertHouseholdPreferenceParams{
		HouseholdID: &hh, Key: anomalySensitivityKey, Value: []byte(`"` + name + `"`),
	}); err != nil {
		t.Fatalf("set sensitivity: %v", err)
	}
}

func anomalyNow() time.Time { return time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC) }

// twelveFortyDollarCharges seeds a merchant with a tight $40 history, spread far
// enough back that none of them lands inside the recent window.
func (f anomalyFixture) twelveFortyDollarCharges(merchant string) {
	for _, d := range []string{
		"2025-09-04", "2025-10-04", "2025-11-04", "2025-12-04",
		"2026-01-04", "2026-02-04", "2026-03-04", "2026-04-04",
		"2026-05-04", "2026-06-04", "2026-06-14", "2026-06-24",
	} {
		f.spend("40.00", d, merchant)
	}
}

func detectOutliers(t *testing.T, ctx context.Context, f anomalyFixture) []Candidate {
	t.Helper()
	got, err := merchantOutlierProducer{}.Detect(ctx, f.q, f.householdID, anomalyNow())
	if err != nil {
		t.Fatalf("merchant_outlier detect: %v", err)
	}
	return got
}

func detectDuplicates(t *testing.T, ctx context.Context, f anomalyFixture) []Candidate {
	t.Helper()
	got, err := duplicateChargeProducer{}.Detect(ctx, f.q, f.householdID, anomalyNow())
	if err != nil {
		t.Fatalf("duplicate_charge detect: %v", err)
	}
	return got
}

// --------------------------------------------------------------------------
// merchant_outlier
// --------------------------------------------------------------------------

func TestMerchantOutlierHappyPath(t *testing.T) {
	ctx, pool := expansionPool(t)
	f := newAnomalyFixture(t, ctx, pool)

	f.twelveFortyDollarCharges("Corner Store")
	f.spend("300.00", "2026-07-15", "Corner Store")

	got := only(t, detectOutliers(t, ctx, f), nil, "merchant_outlier")
	if got.Data["merchant"] != "Corner Store" {
		t.Errorf("merchant = %v, want Corner Store", got.Data["merchant"])
	}
	if got.Data["amount"] != "300.00" {
		t.Errorf("amount = %v, want 300.00", got.Data["amount"])
	}
	// The baseline is LEAVE-ONE-OUT: twelve prior charges, not thirteen.
	if got.Data["sample_count"] != int64(12) {
		t.Errorf("sample_count = %v, want 12", got.Data["sample_count"])
	}
	if got.Data["typical"] != "40.00" {
		t.Errorf("typical = %v, want 40.00", got.Data["typical"])
	}
	if got.Data["times_typical"] != "7.5" {
		t.Errorf("times_typical = %v, want 7.5", got.Data["times_typical"])
	}
}

// The doc's explicit item: two data points do not have a distribution worth
// acting on, and a fresh install must not flag everything in its first week.
func TestMerchantOutlierRequiresMinimumSampleCount(t *testing.T) {
	ctx, pool := expansionPool(t)
	f := newAnomalyFixture(t, ctx, pool)

	// Three prior charges, then an enormous one.
	f.spend("40.00", "2026-05-04", "Sparse Merchant")
	f.spend("40.00", "2026-05-20", "Sparse Merchant")
	f.spend("40.00", "2026-06-04", "Sparse Merchant")
	f.spend("900.00", "2026-07-15", "Sparse Merchant")

	if got := detectOutliers(t, ctx, f); len(got) != 0 {
		t.Fatalf("expected no insight on a 3-charge history, got %d: %+v", len(got), got)
	}

	// A fourth prior charge is still one short of balanced's minimum of five.
	f.spend("40.00", "2026-06-20", "Sparse Merchant")
	if got := detectOutliers(t, ctx, f); len(got) != 0 {
		t.Fatalf("expected no insight on a 4-charge history, got %d: %+v", len(got), got)
	}

	// The fifth crosses it.
	f.spend("40.00", "2026-06-25", "Sparse Merchant")
	only(t, detectOutliers(t, ctx, f), nil, "merchant_outlier")
}

// The doc's explicit item: a $4 coffee against a $1.30 baseline is a
// statistical outlier and practical noise. Both gates, always.
func TestMerchantOutlierRespectsDollarFloor(t *testing.T) {
	ctx, pool := expansionPool(t)
	f := newAnomalyFixture(t, ctx, pool)

	for _, d := range []string{
		"2026-01-05", "2026-02-05", "2026-03-05", "2026-04-05", "2026-05-05",
		"2026-05-15", "2026-06-05", "2026-06-15", "2026-06-25", "2026-07-01",
	} {
		f.spend("1.30", d, "Vending Machine")
	}
	// 3× the $1.30 median clears the multiple easily, and is still noise.
	f.spend("4.00", "2026-07-15", "Vending Machine")

	if got := detectOutliers(t, ctx, f); len(got) != 0 {
		t.Fatalf("expected the dollar floor to suppress a $4 charge, got %d: %+v", len(got), got)
	}
}

// The candidate must never appear in the baseline it is judged against. Without
// the leave-one-out, a merchant whose only history IS the candidate would have a
// p95 equal to the candidate and clear its own gate.
func TestMerchantOutlierBaselineExcludesTheCandidate(t *testing.T) {
	ctx, pool := expansionPool(t)
	f := newAnomalyFixture(t, ctx, pool)

	f.spend("900.00", "2026-07-15", "Only Charge Ever")

	if got := detectOutliers(t, ctx, f); len(got) != 0 {
		t.Fatalf("a merchant's sole charge must not be an outlier against itself, got %+v", got)
	}
}

func TestMerchantOutlierRespectsSuppression(t *testing.T) {
	ctx, pool := expansionPool(t)

	for _, scope := range []string{"outlier", "all"} {
		t.Run("scope="+scope, func(t *testing.T) {
			f := newAnomalyFixture(t, ctx, pool)
			f.twelveFortyDollarCharges("Corner Store")
			f.spend("300.00", "2026-07-15", "Corner Store")

			only(t, detectOutliers(t, ctx, f), nil, "merchant_outlier")

			f.suppress(merchantKeyOf("Corner Store"), "Corner Store", scope)
			if got := detectOutliers(t, ctx, f); len(got) != 0 {
				t.Fatalf("scope %q should suppress, got %d: %+v", scope, len(got), got)
			}
		})
	}

	// A 'duplicate'-scoped suppression must NOT silence the outlier detector —
	// "this merchant bills twice" and "this merchant charges oddly" are
	// different claims about the same merchant.
	t.Run("scope=duplicate does not suppress outliers", func(t *testing.T) {
		f := newAnomalyFixture(t, ctx, pool)
		f.twelveFortyDollarCharges("Corner Store")
		f.spend("300.00", "2026-07-15", "Corner Store")

		f.suppress(merchantKeyOf("Corner Store"), "Corner Store", "duplicate")
		only(t, detectOutliers(t, ctx, f), nil, "merchant_outlier")
	})
}

// Suppression is resolved on BOTH sides, so an override written against one raw
// descriptor still silences the merchant after a later merge folds that
// descriptor into an entity. Nothing else in the suite covers this path.
func TestMerchantOutlierSuppressionSurvivesAMerge(t *testing.T) {
	ctx, pool := expansionPool(t)
	f := newAnomalyFixture(t, ctx, pool)

	// History under one descriptor, the outlier under a second.
	f.twelveFortyDollarCharges("AMZ Mktp")
	f.spend("300.00", "2026-07-15", "AMZ Mktp")

	// Suppress against the RAW descriptor, before any merge exists.
	f.suppress(merchantKeyOf("AMZ Mktp"), "AMZ Mktp", "outlier")

	// Now merge that descriptor into a canonical entity.
	entityID := uuid.New()
	f.exec(`INSERT INTO merchant_entities (id, household_id, canonical_name)
	        VALUES ($1, $2, 'Amazon')`, entityID, f.householdID)
	f.exec(`INSERT INTO merchant_aliases (household_id, merchant_key, entity_id, source)
	        VALUES ($1, $2, $3, 'manual')`, f.householdID, merchantKeyOf("AMZ Mktp"), entityID)

	if got := detectOutliers(t, ctx, f); len(got) != 0 {
		t.Fatalf("a pre-merge suppression must still silence the merged merchant, got %+v", got)
	}
}

// Two descriptors merged into one entity share ONE pooled baseline. Before
// canonicalization each had a thin separate history and the threshold was wrong
// for both.
func TestMerchantOutlierPoolsMergedDescriptors(t *testing.T) {
	ctx, pool := expansionPool(t)
	f := newAnomalyFixture(t, ctx, pool)

	for _, d := range []string{"2026-01-06", "2026-02-06", "2026-03-06", "2026-04-06", "2026-05-06", "2026-06-06"} {
		f.spend("40.00", d, "AMZ Mktp")
	}
	for _, d := range []string{"2026-01-16", "2026-02-16", "2026-03-16", "2026-04-16", "2026-05-16", "2026-06-16"} {
		f.spend("40.00", d, "AMAZON COM")
	}
	f.spend("300.00", "2026-07-15", "AMAZON COM")

	entityID := uuid.New()
	f.exec(`INSERT INTO merchant_entities (id, household_id, canonical_name)
	        VALUES ($1, $2, 'Amazon')`, entityID, f.householdID)
	for _, m := range []string{"AMZ Mktp", "AMAZON COM"} {
		f.exec(`INSERT INTO merchant_aliases (household_id, merchant_key, entity_id, source)
		        VALUES ($1, $2, $3, 'manual')`, f.householdID, merchantKeyOf(m), entityID)
	}

	got := only(t, detectOutliers(t, ctx, f), nil, "merchant_outlier")
	if got.Data["sample_count"] != int64(12) {
		t.Errorf("sample_count = %v, want 12 (both descriptors pooled)", got.Data["sample_count"])
	}
	if got.Data["merchant"] != "Amazon" {
		t.Errorf("merchant = %v, want the canonical name Amazon", got.Data["merchant"])
	}
}

func TestMerchantOutlierHonoursSensitivity(t *testing.T) {
	ctx, pool := expansionPool(t)
	f := newAnomalyFixture(t, ctx, pool)

	// Six $40 charges, then a $90 one. 90 is 2.25× the median: over sensitive's
	// 2× and under balanced's 3×. Six samples clears sensitive/balanced minimums
	// but not conservative's eight.
	//
	// The gaps are deliberately irregular (4, 42, 9, 75, 18 days). A merchant on
	// a regular cadence is a RECURRING merchant, and the producer cedes anything
	// under 3× the median on those to price creep — which would make this fixture
	// silent at every sensitivity and prove nothing.
	for _, d := range []string{"2026-01-05", "2026-01-09", "2026-02-20", "2026-03-01", "2026-05-15", "2026-06-02"} {
		f.spend("40.00", d, "Corner Store")
	}
	f.spend("90.00", "2026-07-15", "Corner Store")

	f.setSensitivity(t, "balanced")
	if got := detectOutliers(t, ctx, f); len(got) != 0 {
		t.Fatalf("balanced should stay quiet at 2.25× the median, got %+v", got)
	}

	f.setSensitivity(t, "sensitive")
	only(t, detectOutliers(t, ctx, f), nil, "merchant_outlier")

	f.setSensitivity(t, "conservative")
	if got := detectOutliers(t, ctx, f); len(got) != 0 {
		t.Fatalf("conservative should stay quiet (min 8 samples), got %+v", got)
	}
}

// --------------------------------------------------------------------------
// The hand-off with large_transaction
// --------------------------------------------------------------------------

// Where a baseline exists, merchant_outlier owns the charge and
// large_transaction stands down — exactly one insight, and it is the one that
// can name what the merchant normally costs.
func TestLargeTransactionYieldsToMerchantOutlier(t *testing.T) {
	ctx, pool := expansionPool(t)
	f := newAnomalyFixture(t, ctx, pool)

	f.twelveFortyDollarCharges("Corner Store")
	f.spend("900.00", "2026-07-15", "Corner Store")

	large, err := largeTransactionProducer{}.Detect(ctx, f.q, f.householdID, anomalyNow())
	if err != nil {
		t.Fatalf("large_transaction detect: %v", err)
	}
	if len(large) != 0 {
		t.Fatalf("large_transaction should yield on a merchant with a baseline, got %+v", large)
	}
	got := only(t, detectOutliers(t, ctx, f), nil, "merchant_outlier")
	if got.Data["typical"] != "40.00" {
		t.Errorf("typical = %v, want 40.00", got.Data["typical"])
	}
}

// Where no baseline exists — a first-ever charge at a new merchant, where most
// genuine fraud lands — large_transaction is the only signal and must still fire.
func TestLargeTransactionStillFiresWithoutABaseline(t *testing.T) {
	ctx, pool := expansionPool(t)
	f := newAnomalyFixture(t, ctx, pool)

	// A household-wide history at OTHER merchants, so the household average is
	// small and the $900 clears the 4× gate.
	for i, d := range []string{"2026-03-05", "2026-03-18", "2026-04-02", "2026-04-20", "2026-05-06", "2026-05-19", "2026-06-03", "2026-06-21"} {
		f.spend("30.00", d, []string{"A", "B", "C", "D", "E", "F", "G", "H"}[i]+" Shop")
	}
	f.spend("900.00", "2026-07-15", "Big Appliance Co")

	large, err := largeTransactionProducer{}.Detect(ctx, f.q, f.householdID, anomalyNow())
	if err != nil {
		t.Fatalf("large_transaction detect: %v", err)
	}
	got := only(t, large, nil, "large_transaction")
	if got.Data["merchant"] != "Big Appliance Co" {
		t.Errorf("merchant = %v, want Big Appliance Co", got.Data["merchant"])
	}
	if outliers := detectOutliers(t, ctx, f); len(outliers) != 0 {
		t.Fatalf("merchant_outlier must stay quiet without a baseline, got %+v", outliers)
	}
}

// --------------------------------------------------------------------------
// Price creep — the doc's explicit "assert exactly one insight" item
// --------------------------------------------------------------------------

func TestPriceCreepIsNotDuplicatedByOutlier(t *testing.T) {
	ctx, pool := expansionPool(t)
	f := newAnomalyFixture(t, ctx, pool)

	// A monthly subscription creeping from $15.99 to $19.99.
	for _, c := range []struct{ amount, date string }{
		{"15.99", "2025-12-10"}, {"15.99", "2026-01-10"}, {"15.99", "2026-02-10"},
		{"15.99", "2026-03-10"}, {"19.99", "2026-04-10"}, {"19.99", "2026-05-10"},
		{"19.99", "2026-06-10"}, {"19.99", "2026-07-10"},
	} {
		f.spend(c.amount, c.date, "Streamflix")
	}

	subs, err := subscriptionProducer{}.Detect(ctx, f.q, f.householdID, anomalyNow())
	if err != nil {
		t.Fatalf("subscription detect: %v", err)
	}
	outliers := detectOutliers(t, ctx, f)

	if total := len(subs) + len(outliers); total != 1 {
		t.Fatalf("expected exactly 1 insight for a creeping subscription, got %d (subs=%+v outliers=%+v)",
			total, subs, outliers)
	}
	if len(subs) != 1 || subs[0].Data["flavor"] != "price_creep" {
		t.Fatalf("the one insight should be the price_creep flavor, got %+v", subs)
	}
}

// --------------------------------------------------------------------------
// duplicate_charge — the false-positive suite
// --------------------------------------------------------------------------

func TestDuplicateChargeGenuineDoubleCharge(t *testing.T) {
	ctx, pool := expansionPool(t)
	f := newAnomalyFixture(t, ctx, pool)

	f.spend("449.00", "2026-07-15", "Furniture Barn")
	f.spend("449.00", "2026-07-16", "Furniture Barn")

	got := only(t, detectDuplicates(t, ctx, f), nil, "duplicate_charge")
	if got.Data["charge_count"] != int64(2) {
		t.Errorf("charge_count = %v, want 2", got.Data["charge_count"])
	}
	if got.Data["total"] != "898.00" {
		t.Errorf("total = %v, want 898.00", got.Data["total"])
	}
	if got.Data["amount"] != "449.00" {
		t.Errorf("amount = %v, want 449.00", got.Data["amount"])
	}
	if got.Priority < insightPushMinPriorityForTest {
		t.Errorf("priority = %d, want >= %d (an $898 duplicate should push)",
			got.Priority, insightPushMinPriorityForTest)
	}
}

// A third identical charge refreshes the SAME insight rather than raising a
// second one — the dedupe key is anchored on the group's earliest charge.
func TestDuplicateChargeThirdChargeRefreshesTheSameInsight(t *testing.T) {
	ctx, pool := expansionPool(t)
	f := newAnomalyFixture(t, ctx, pool)

	f.spend("449.00", "2026-07-15", "Furniture Barn")
	f.spend("449.00", "2026-07-16", "Furniture Barn")
	first := only(t, detectDuplicates(t, ctx, f), nil, "duplicate_charge")

	f.spend("449.00", "2026-07-16", "Furniture Barn")
	second := only(t, detectDuplicates(t, ctx, f), nil, "duplicate_charge")

	if first.DedupeKey != second.DedupeKey {
		t.Errorf("dedupe key changed: %q then %q — the insight would be raised twice",
			first.DedupeKey, second.DedupeKey)
	}
	if second.Data["charge_count"] != int64(3) {
		t.Errorf("charge_count = %v, want 3", second.Data["charge_count"])
	}
}

func TestDuplicateChargeFalsePositives(t *testing.T) {
	ctx, pool := expansionPool(t)

	cases := []struct {
		name string
		seed func(f anomalyFixture)
	}{
		{
			// Two same-day coffees at the same price ARE exactly equal, which is
			// why amount equality alone cannot be the rule. The dollar floor is.
			"two identical coffees, below the floor",
			func(f anomalyFixture) {
				f.spend("5.75", "2026-07-15", "Cafe")
				f.spend("5.75", "2026-07-15", "Cafe")
			},
		},
		{
			"two same-day charges at different amounts",
			func(f anomalyFixture) {
				f.spend("449.00", "2026-07-15", "Furniture Barn")
				f.spend("312.00", "2026-07-15", "Furniture Barn")
			},
		},
		{
			// The habitual repeater: this merchant on this card has doubled up
			// before, so doubling up is simply what it does.
			"habitual repeater with prior same-amount pairs",
			func(f anomalyFixture) {
				for _, d := range []string{"2026-02-03", "2026-03-03", "2026-04-03", "2026-05-03", "2026-06-03"} {
					f.spend("30.00", d, "Parking Garage")
					f.spend("30.00", d, "Parking Garage")
				}
				f.spend("30.00", "2026-07-15", "Parking Garage")
				f.spend("30.00", "2026-07-15", "Parking Garage")
			},
		},
		{
			// One institution linked twice: the same charge reported on two
			// accounts. A genuine double charge always hits the same card.
			"same charge on two accounts",
			func(f anomalyFixture) {
				f.spendOn(f.acctA, "449.00", "2026-07-15", "Furniture Barn")
				f.spendOn(f.acctB, "449.00", "2026-07-15", "Furniture Barn")
			},
		},
		{
			// A refund carries the opposite sign and never enters the universe.
			"refund of the same amount the next day",
			func(f anomalyFixture) {
				f.spend("449.00", "2026-07-15", "Furniture Barn")
				f.spend("-449.00", "2026-07-16", "Furniture Barn")
			},
		},
		{
			// A monthly subscription cannot bill twice a day apart, so cadence
			// never produces a pair at this window.
			"monthly subscription on cadence",
			func(f anomalyFixture) {
				for _, d := range []string{"2026-03-15", "2026-04-15", "2026-05-15", "2026-06-15", "2026-07-15"} {
					f.spend("99.00", d, "Streamflix")
				}
			},
		},
		{
			"a charge more than a day apart",
			func(f anomalyFixture) {
				f.spend("449.00", "2026-07-13", "Furniture Barn")
				f.spend("449.00", "2026-07-16", "Furniture Barn")
			},
		},
		{
			"$6 double charge is below the floor",
			func(f anomalyFixture) {
				f.spend("6.00", "2026-07-15", "Cafe")
				f.spend("6.00", "2026-07-15", "Cafe")
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newAnomalyFixture(t, ctx, pool)
			c.seed(f)
			if got := detectDuplicates(t, ctx, f); len(got) != 0 {
				t.Fatalf("expected no duplicate insight, got %d: %+v", len(got), got)
			}
		})
	}
}

// A pending row and its posted counterpart are one charge. The universe drops
// pending rows, and the plaid_transaction_id / pending_transaction_id
// cross-check covers the case where the pending row was never deleted.
func TestDuplicateChargeIgnoresPendingPostedPairs(t *testing.T) {
	ctx, pool := expansionPool(t)

	t.Run("pending row still present and flagged pending", func(t *testing.T) {
		f := newAnomalyFixture(t, ctx, pool)
		pendingID := uuid.New()
		f.exec(`INSERT INTO transactions
		          (id, account_id, plaid_transaction_id, amount, currency, date, name,
		           merchant_name, merchant_key, category_id, source, pending)
		        VALUES ($1, $2, $3, '449.00', 'USD', '2026-07-15', 'Furniture Barn',
		                'Furniture Barn', $4, $5, 'plaid', TRUE)`,
			pendingID, f.acctA, pendingID.String(), merchantKeyOf("Furniture Barn"), f.spendCat)

		postedID := uuid.New()
		f.exec(`INSERT INTO transactions
		          (id, account_id, plaid_transaction_id, pending_transaction_id, amount, currency,
		           date, name, merchant_name, merchant_key, category_id, source)
		        VALUES ($1, $2, $3, $4, '449.00', 'USD', '2026-07-16', 'Furniture Barn',
		                'Furniture Barn', $5, $6, 'plaid')`,
			postedID, f.acctA, postedID.String(), pendingID.String(),
			merchantKeyOf("Furniture Barn"), f.spendCat)

		if got := detectDuplicates(t, ctx, f); len(got) != 0 {
			t.Fatalf("a pending/posted pair is one charge, got %+v", got)
		}
	})

	// The residual case the cross-check exists for: an institution that flipped
	// the pending row to posted instead of Plaid deleting it, so both rows are
	// in the universe and linked only by pending_transaction_id.
	t.Run("orphaned pending row flipped to posted", func(t *testing.T) {
		f := newAnomalyFixture(t, ctx, pool)
		origID := uuid.New()
		f.exec(`INSERT INTO transactions
		          (id, account_id, plaid_transaction_id, amount, currency, date, name,
		           merchant_name, merchant_key, category_id, source, pending)
		        VALUES ($1, $2, $3, '449.00', 'USD', '2026-07-15', 'Furniture Barn',
		                'Furniture Barn', $4, $5, 'plaid', FALSE)`,
			origID, f.acctA, origID.String(), merchantKeyOf("Furniture Barn"), f.spendCat)

		postedID := uuid.New()
		f.exec(`INSERT INTO transactions
		          (id, account_id, plaid_transaction_id, pending_transaction_id, amount, currency,
		           date, name, merchant_name, merchant_key, category_id, source)
		        VALUES ($1, $2, $3, $4, '449.00', 'USD', '2026-07-16', 'Furniture Barn',
		                'Furniture Barn', $5, $6, 'plaid')`,
			postedID, f.acctA, postedID.String(), origID.String(),
			merchantKeyOf("Furniture Barn"), f.spendCat)

		if got := detectDuplicates(t, ctx, f); len(got) != 0 {
			t.Fatalf("a linked pending/posted pair is one charge even when both are posted, got %+v", got)
		}
	})
}

func TestDuplicateChargeRespectsSuppression(t *testing.T) {
	ctx, pool := expansionPool(t)

	for _, scope := range []string{"duplicate", "all"} {
		t.Run("scope="+scope, func(t *testing.T) {
			f := newAnomalyFixture(t, ctx, pool)
			f.spend("449.00", "2026-07-15", "Furniture Barn")
			f.spend("449.00", "2026-07-16", "Furniture Barn")

			only(t, detectDuplicates(t, ctx, f), nil, "duplicate_charge")

			f.suppress(merchantKeyOf("Furniture Barn"), "Furniture Barn", scope)
			if got := detectDuplicates(t, ctx, f); len(got) != 0 {
				t.Fatalf("scope %q should suppress, got %d: %+v", scope, len(got), got)
			}
		})
	}

	t.Run("scope=outlier does not suppress duplicates", func(t *testing.T) {
		f := newAnomalyFixture(t, ctx, pool)
		f.spend("449.00", "2026-07-15", "Furniture Barn")
		f.spend("449.00", "2026-07-16", "Furniture Barn")

		f.suppress(merchantKeyOf("Furniture Barn"), "Furniture Barn", "outlier")
		only(t, detectDuplicates(t, ctx, f), nil, "duplicate_charge")
	})
}

// Unsuppressing restores the merchant to the detector.
func TestUnsuppressAnomalyMerchantRestoresDetection(t *testing.T) {
	ctx, pool := expansionPool(t)
	f := newAnomalyFixture(t, ctx, pool)

	f.spend("449.00", "2026-07-15", "Furniture Barn")
	f.spend("449.00", "2026-07-16", "Furniture Barn")
	f.suppress(merchantKeyOf("Furniture Barn"), "Furniture Barn", "duplicate")
	if got := detectDuplicates(t, ctx, f); len(got) != 0 {
		t.Fatalf("expected suppression to hold, got %+v", got)
	}

	if err := f.q.UnsuppressAnomalyMerchant(ctx, dbgen.UnsuppressAnomalyMerchantParams{
		HouseholdID: f.householdID, MerchantKey: merchantKeyOf("Furniture Barn"), Scope: "duplicate",
	}); err != nil {
		t.Fatalf("unsuppress: %v", err)
	}
	only(t, detectDuplicates(t, ctx, f), nil, "duplicate_charge")

	rows, err := f.q.ListAnomalyOverrides(ctx, f.householdID)
	if err != nil {
		t.Fatalf("list overrides: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected no overrides after unsuppress, got %+v", rows)
	}
}

// --------------------------------------------------------------------------
// Engine level: dedupe-key stability, dismissal, and merge safety
// --------------------------------------------------------------------------

// generate runs the whole engine (AI disabled) and returns only the anomaly
// results, so these assertions exercise the real upsert path rather than a
// producer in isolation.
func generateAnomalies(t *testing.T, ctx context.Context, f anomalyFixture) []Result {
	t.Helper()
	all, err := Generate(ctx, f.q, nil, f.householdID, anomalyNow())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var out []Result
	for _, r := range all {
		if r.Kind == "merchant_outlier" || r.Kind == "duplicate_charge" {
			out = append(out, r)
		}
	}
	return out
}

// UpsertInsight omits dismissed_at from its DO UPDATE SET, so dismissal already
// survives regeneration FOR FREE — provided the dedupe key is stable across
// passes. Stability is the thing that actually needs asserting.
func TestAnomalyInsightsAreStableAndStayDismissed(t *testing.T) {
	ctx, pool := expansionPool(t)
	f := newAnomalyFixture(t, ctx, pool)

	f.twelveFortyDollarCharges("Corner Store")
	f.spend("300.00", "2026-07-15", "Corner Store")
	f.spend("449.00", "2026-07-15", "Furniture Barn")
	f.spend("449.00", "2026-07-16", "Furniture Barn")

	first := generateAnomalies(t, ctx, f)
	if len(first) != 2 {
		t.Fatalf("expected 2 anomaly insights, got %d: %+v", len(first), first)
	}
	for _, r := range first {
		if !r.Inserted {
			t.Errorf("%s was not inserted on the first pass", r.Kind)
		}
	}

	// Second pass: same facts, so the engine must UPSERT rather than duplicate.
	second := generateAnomalies(t, ctx, f)
	if len(second) != 2 {
		t.Fatalf("second pass produced %d anomaly insights, want 2: %+v", len(second), second)
	}
	for _, r := range second {
		if r.Inserted {
			t.Errorf("%s reported Inserted on the second pass — the dedupe key is not stable "+
				"(%q), so the feed would grow a row every hour and push every time",
				r.Kind, r.DedupeKey)
		}
	}

	// Dismiss both, then regenerate: they must stay gone.
	for _, r := range second {
		if err := f.q.DismissInsight(ctx, dbgen.DismissInsightParams{ID: r.ID, HouseholdID: f.householdID}); err != nil {
			t.Fatalf("dismiss: %v", err)
		}
	}
	generateAnomalies(t, ctx, f)

	rows, err := f.q.ListInsights(ctx, dbgen.ListInsightsParams{HouseholdID: f.householdID})
	if err != nil {
		t.Fatalf("list insights: %v", err)
	}
	for _, r := range rows {
		if r.Kind == "merchant_outlier" || r.Kind == "duplicate_charge" {
			t.Fatalf("%s reappeared in the feed after being dismissed", r.Kind)
		}
	}
}

// The reason merchant_outlier keys on the transaction id rather than
// merchant:date:amount. A merge changes a merchant's resolved key and its
// display name; if the dedupe key moved with it, every dismissed anomaly at that
// merchant would silently come back.
func TestAnomalyDedupeKeySurvivesAMerchantMerge(t *testing.T) {
	ctx, pool := expansionPool(t)
	f := newAnomalyFixture(t, ctx, pool)

	f.twelveFortyDollarCharges("AMZ Mktp")
	f.spend("300.00", "2026-07-15", "AMZ Mktp")

	before := only(t, detectOutliers(t, ctx, f), nil, "merchant_outlier")

	// Merge the descriptor into a canonical entity, changing both the resolved
	// key and the displayed name.
	entityID := uuid.New()
	f.exec(`INSERT INTO merchant_entities (id, household_id, canonical_name)
	        VALUES ($1, $2, 'Amazon')`, entityID, f.householdID)
	f.exec(`INSERT INTO merchant_aliases (household_id, merchant_key, entity_id, source)
	        VALUES ($1, $2, $3, 'manual')`, f.householdID, merchantKeyOf("AMZ Mktp"), entityID)

	after := only(t, detectOutliers(t, ctx, f), nil, "merchant_outlier")

	if before.DedupeKey != after.DedupeKey {
		t.Fatalf("dedupe key changed across a merge: %q then %q — a dismissed insight would return",
			before.DedupeKey, after.DedupeKey)
	}
	if before.Data["merchant"] == after.Data["merchant"] {
		t.Fatalf("fixture is not exercising the merge: merchant name unchanged (%v)", after.Data["merchant"])
	}
}
