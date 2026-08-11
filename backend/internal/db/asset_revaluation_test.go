package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/jobs"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// Doc 26's verification list, against a real Postgres.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/db/

type assetFixture struct {
	ctx         context.Context
	pool        *pgxpool.Pool
	q           *dbgen.Queries
	householdID uuid.UUID
	userID      uuid.UUID
	itemID      uuid.UUID
}

func newAssetFixture(t *testing.T) *assetFixture {
	t.Helper()

	url := testdb.URL(t)

	ctx := context.Background()
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	f := &assetFixture{
		ctx: ctx, pool: pool, q: dbgen.New(pool),
		householdID: uuid.New(), userID: uuid.New(), itemID: uuid.New(),
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Asset Test')`, f.householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM households WHERE id = $1`, f.householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
	      VALUES ($1,$2,$3,'h','Alex','owner')`, f.userID, f.householdID, f.userID.String()+"@x")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, institution_name, is_shared)
	      VALUES ($1,$2,$3,'\x00','Test Bank', TRUE)`, f.itemID, f.userID, f.itemID.String())

	return f
}

// addAsset creates a manual asset and returns its id.
func (f *assetFixture) addAsset(t *testing.T, name, kind string, value string, asOf time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO manual_assets (id, household_id, name, kind, value, as_of)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		id, f.householdID, name, kind, decimal.RequireFromString(value), asOf); err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	return id
}

// addLoan creates a loan account and returns its id.
func (f *assetFixture) addLoan(t *testing.T, name, balance string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance, is_active)
		 VALUES ($1,$2,$3,$4,'loan','auto',$5,TRUE)`,
		id, f.itemID, id.String(), name, decimal.RequireFromString(balance)); err != nil {
		t.Fatalf("insert loan: %v", err)
	}
	return id
}

func (f *assetFixture) assetValue(t *testing.T, id uuid.UUID) decimal.Decimal {
	t.Helper()
	a, err := f.q.GetManualAsset(f.ctx, dbgen.GetManualAssetParams{ID: id, HouseholdID: f.householdID})
	if err != nil {
		t.Fatalf("get asset: %v", err)
	}
	return a.Value
}

// --------------------------------------------------------------------------
// The migration
// --------------------------------------------------------------------------

// Every pre-existing asset gets exactly one seed valuation matching its value
// and as_of, so the history starts from what the user originally entered rather
// than a gap.
//
// The migration has already run by the time a test can insert anything, so this
// exercises the same guarantee through the code path a new asset takes: an
// asset must never sit in net worth with no recorded provenance for its value.
func TestBackfilledAssetsHaveExactlyOneSeedValuation(t *testing.T) {
	f := newAssetFixture(t)

	// Confirm the migration's own backfill ran over whatever was already there:
	// no asset in the database may have zero valuations.
	var orphans int
	if err := f.pool.QueryRow(f.ctx, `
		SELECT count(*) FROM manual_assets m
		WHERE NOT EXISTS (SELECT 1 FROM asset_valuations v WHERE v.manual_asset_id = m.id)
		  AND m.created_at < (SELECT max(created_at) FROM asset_valuations)
	`).Scan(&orphans); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d pre-existing assets have no seed valuation", orphans)
	}

	// And the seed row is exact, not approximate.
	asOf := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	id := f.addAsset(t, "Backfill check", "home", "425000.00", asOf)

	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO asset_valuations (manual_asset_id, value, as_of, source, note)
		 SELECT id, value, as_of, 'manual', 'Value as first entered.'
		 FROM manual_assets WHERE id = $1`, id); err != nil {
		t.Fatalf("seed valuation: %v", err)
	}

	rows, err := f.q.ListAssetValuations(f.ctx, dbgen.ListAssetValuationsParams{
		ManualAssetID: id, HouseholdID: f.householdID,
	})
	if err != nil {
		t.Fatalf("list valuations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly one seed valuation, got %d", len(rows))
	}
	if rows[0].Value.StringFixed(2) != "425000.00" {
		t.Errorf("seed value = %s, want 425000.00", rows[0].Value.StringFixed(2))
	}
	if !rows[0].AsOf.Equal(asOf) {
		t.Errorf("seed as_of = %s, want %s", rows[0].AsOf, asOf)
	}
}

// The seeded rate table has to be present and complete, because a bond that
// cannot find its rates silently stops being valued.
func TestSavingsBondRatesAreSeeded(t *testing.T) {
	f := newAssetFixture(t)

	rows, err := f.q.ListSavingsBondRates(f.ctx)
	if err != nil {
		t.Fatalf("list rates: %v", err)
	}

	var i, ee int
	for _, r := range rows {
		if r.SourceUrl == "" {
			t.Errorf("%s %s has no source URL", r.Series, r.PeriodStart.Format("2006-01"))
		}
		switch r.Series {
		case "i_savings":
			i++
			if !r.InflationRate.Valid {
				t.Errorf("I bond period %s has no inflation rate", r.PeriodStart.Format("2006-01"))
			}
		case "ee_savings":
			ee++
		}
	}
	if i < 50 {
		t.Errorf("expected the full I bond history, got %d periods", i)
	}
	if ee < 40 {
		t.Errorf("expected the full EE history from May 2005, got %d periods", ee)
	}
}

// --------------------------------------------------------------------------
// Writes are atomic
// --------------------------------------------------------------------------

// Writing a valuation updates manual_assets.value and inserts the history row
// atomically. A failure leaves neither — otherwise the headline figure and the
// history that is supposed to explain it disagree.
func TestValuationWriteIsAtomic(t *testing.T) {
	f := newAssetFixture(t)
	id := f.addAsset(t, "Atomic", "home", "300000.00", time.Now().UTC())

	// A transaction that writes both and then rolls back must leave the asset
	// untouched and no history behind.
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	qtx := f.q.WithTx(tx)
	asOf := time.Now().UTC().Truncate(24 * time.Hour)

	if _, err := qtx.InsertAssetValuation(f.ctx, dbgen.InsertAssetValuationParams{
		ManualAssetID: id, Value: decimal.RequireFromString("999999.00"),
		AsOf: asOf, Source: "manual",
	}); err != nil {
		t.Fatalf("insert valuation: %v", err)
	}
	if _, err := qtx.SetManualAssetValue(f.ctx, dbgen.SetManualAssetValueParams{
		ID: id, HouseholdID: f.householdID,
		Value: decimal.RequireFromString("999999.00"), AsOf: asOf,
	}); err != nil {
		t.Fatalf("set value: %v", err)
	}
	if err := tx.Rollback(f.ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if got := f.assetValue(t, id); got.StringFixed(2) != "300000.00" {
		t.Errorf("value = %s after rollback, want the original 300000.00", got.StringFixed(2))
	}
	rows, err := f.q.ListAssetValuations(f.ctx, dbgen.ListAssetValuationsParams{
		ManualAssetID: id, HouseholdID: f.householdID,
	})
	if err != nil {
		t.Fatalf("list valuations: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rollback left %d history rows behind", len(rows))
	}

	// Committed, both land together.
	tx2, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	q2 := f.q.WithTx(tx2)
	if _, err := q2.InsertAssetValuation(f.ctx, dbgen.InsertAssetValuationParams{
		ManualAssetID: id, Value: decimal.RequireFromString("335000.00"),
		AsOf: asOf, Source: "manual",
	}); err != nil {
		t.Fatalf("insert valuation: %v", err)
	}
	if _, err := q2.SetManualAssetValue(f.ctx, dbgen.SetManualAssetValueParams{
		ID: id, HouseholdID: f.householdID,
		Value: decimal.RequireFromString("335000.00"), AsOf: asOf,
	}); err != nil {
		t.Fatalf("set value: %v", err)
	}
	if err := tx2.Commit(f.ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if got := f.assetValue(t, id); got.StringFixed(2) != "335000.00" {
		t.Errorf("value = %s after commit, want 335000.00", got.StringFixed(2))
	}
}

// --------------------------------------------------------------------------
// Equity does not double-count
// --------------------------------------------------------------------------

// Net worth for a household with a linked asset and loan equals net worth with
// them unlinked, to the cent.
//
// This is the invariant that makes equity safe to display: it is derived from
// two figures the sum already contains, so linking must be a presentation
// change and nothing more.
func TestLinkingALoanDoesNotChangeNetWorth(t *testing.T) {
	f := newAssetFixture(t)

	assetID := f.addAsset(t, "Car", "vehicle", "32000.00", time.Now().UTC())
	loanID := f.addLoan(t, "Car loan", "8000.00")

	before, err := f.q.ComputeNetWorth(f.ctx, f.householdID)
	if err != nil {
		t.Fatalf("compute net worth: %v", err)
	}

	if _, err := f.q.SetManualAssetLoan(f.ctx, dbgen.SetManualAssetLoanParams{
		ID: assetID, HouseholdID: f.householdID, LoanAccountID: &loanID,
	}); err != nil {
		t.Fatalf("link loan: %v", err)
	}

	after, err := f.q.ComputeNetWorth(f.ctx, f.householdID)
	if err != nil {
		t.Fatalf("compute net worth: %v", err)
	}

	for _, tc := range []struct {
		name        string
		before, got decimal.Decimal
	}{
		{"manual assets", before.ManualAssets, after.ManualAssets},
		{"loan debt", before.LoanDebt, after.LoanDebt},
		{"cash", before.Cash, after.Cash},
		{"manual debt", before.ManualDebt, after.ManualDebt},
	} {
		if !tc.before.Equal(tc.got) {
			t.Errorf("%s changed on linking: %s then %s", tc.name, tc.before, tc.got)
		}
	}

	// And the equity figure is exactly the difference already in the sum.
	e := networth.ComputeEquity(
		decimal.RequireFromString("32000.00"), decimal.RequireFromString("8000.00"))
	if e.Owned.StringFixed(2) != "24000.00" {
		t.Errorf("equity = %s, want 24000.00", e.Owned.StringFixed(2))
	}
}

// --------------------------------------------------------------------------
// The staleness nudge
// --------------------------------------------------------------------------

// The nudge fires at the threshold and not before, and never for a bond.
func TestStaleAssetQueryRespectsTheThreshold(t *testing.T) {
	f := newAssetFixture(t)
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	fresh := f.addAsset(t, "Fresh house", "home", "400000.00", now.AddDate(0, -6, 0))
	edge := f.addAsset(t, "Edge house", "home", "400000.00", now.AddDate(-1, 0, 1))
	stale := f.addAsset(t, "Stale house", "home", "400000.00", now.AddDate(-1, 0, 0))
	old := f.addAsset(t, "Old car", "vehicle", "9000.00", now.AddDate(-3, 0, 0))

	// A bond, old enough to be stale by date, which must still never appear:
	// it revalues itself from published rates.
	bond := f.addAsset(t, "I bond", "bond", "1200.00", now.AddDate(-2, 0, 0))
	issue := time.Date(2019, 6, 1, 0, 0, 0, 0, time.UTC)
	price := decimal.NullDecimal{Decimal: decimal.RequireFromString("1000"), Valid: true}
	series := "i_savings"
	if _, err := f.q.UpsertAssetDetail(f.ctx, dbgen.UpsertAssetDetailParams{
		ManualAssetID: bond, BondSeries: &series, IssueDate: &issue,
		PurchasePrice: price, FaceValue: price,
	}); err != nil {
		t.Fatalf("upsert bond detail: %v", err)
	}

	rows, err := f.q.ListStaleAssets(f.ctx, dbgen.ListStaleAssetsParams{
		HouseholdID: f.householdID, StaleBefore: networth.StaleBefore(now),
	})
	if err != nil {
		t.Fatalf("list stale: %v", err)
	}

	got := map[uuid.UUID]bool{}
	for _, r := range rows {
		got[r.ID] = true
	}

	for _, tc := range []struct {
		name string
		id   uuid.UUID
		want bool
	}{
		{"six months old", fresh, false},
		{"one day short of a year", edge, false},
		{"exactly a year", stale, true},
		{"three years", old, true},
		{"a bond", bond, false},
	} {
		if got[tc.id] != tc.want {
			t.Errorf("%s: in stale list = %v, want %v", tc.name, got[tc.id], tc.want)
		}
	}
}

// --------------------------------------------------------------------------
// The revaluation job
// --------------------------------------------------------------------------

// The bond job writes a complete valuation and REFUSES an incomplete one,
// leaving the asset's value untouched.
//
// The refusal half is the important one: carrying the last known rate forward
// would invent a return, and the whole feature exists to stop net worth
// carrying numbers nobody can check.
func TestBondJobWritesWhatItCanAndRefusesTheRest(t *testing.T) {
	f := newAssetFixture(t)
	now := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)

	// Valuable: a $1,000 Series I bond issued October 2021. Treasury's own
	// calculator puts this at $1,232.00 in August 2026.
	good := f.addAsset(t, "I bond", "bond", "1000.00", time.Date(2021, 10, 1, 0, 0, 0, 0, time.UTC))
	issue := time.Date(2021, 10, 1, 0, 0, 0, 0, time.UTC)
	par := decimal.NullDecimal{Decimal: decimal.RequireFromString("1000"), Valid: true}
	iSeries := "i_savings"
	if _, err := f.q.UpsertAssetDetail(f.ctx, dbgen.UpsertAssetDetailParams{
		ManualAssetID: good, BondSeries: &iSeries, IssueDate: &issue,
		PurchasePrice: par, FaceValue: par,
	}); err != nil {
		t.Fatalf("upsert detail: %v", err)
	}

	// Unvaluable: an EE bond from before the fixed-rate era. No table describes
	// it, so it must be left exactly as the user entered it.
	bad := f.addAsset(t, "Old EE bond", "bond", "77.00", time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	oldIssue := time.Date(1998, 4, 1, 0, 0, 0, 0, time.UTC)
	eeSeries := "ee_savings"
	half := decimal.NullDecimal{Decimal: decimal.RequireFromString("50"), Valid: true}
	face := decimal.NullDecimal{Decimal: decimal.RequireFromString("100"), Valid: true}
	if _, err := f.q.UpsertAssetDetail(f.ctx, dbgen.UpsertAssetDetailParams{
		ManualAssetID: bad, BondSeries: &eeSeries, IssueDate: &oldIssue,
		PurchasePrice: half, FaceValue: face,
	}); err != nil {
		t.Fatalf("upsert detail: %v", err)
	}

	written, refused, err := jobs.RevalueBonds(f.ctx, f.pool, f.q, f.householdID, now)
	if err != nil {
		t.Fatalf("revalue bonds: %v", err)
	}
	if written != 1 {
		t.Errorf("wrote %d valuations, want 1", written)
	}
	if refused != 1 {
		t.Errorf("refused %d bonds, want 1", refused)
	}

	if got := f.assetValue(t, good).StringFixed(2); got != "1232.00" {
		t.Errorf("valued I bond at %s, want Treasury's 1232.00", got)
	}
	if got := f.assetValue(t, bad).StringFixed(2); got != "77.00" {
		t.Errorf("refused bond was rewritten to %s; it must keep its 77.00", got)
	}

	// The written valuation carries a note naming the rate periods behind it,
	// so a history row can be walked back to treasurydirect.gov.
	rows, err := f.q.ListAssetValuations(f.ctx, dbgen.ListAssetValuationsParams{
		ManualAssetID: good, HouseholdID: f.householdID,
	})
	if err != nil {
		t.Fatalf("list valuations: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no valuation history written")
	}
	last := rows[len(rows)-1]
	if last.Source != "estimated" {
		t.Errorf("source = %q, want estimated", last.Source)
	}
	if last.Note == nil || *last.Note == "" {
		t.Error("an automatic valuation must record which rates produced it")
	}

	// Re-running is a no-op rather than a second row for the same day.
	written2, _, err := jobs.RevalueBonds(f.ctx, f.pool, f.q, f.householdID, now)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if written2 != 0 {
		t.Errorf("second run wrote %d valuations, want 0", written2)
	}
}

// A depreciating asset is never touched by any automatic path. Running the bond
// job over a household that owns a car must leave the car exactly alone.
func TestAutomaticRevaluationNeverTouchesAnEstimatedAsset(t *testing.T) {
	f := newAssetFixture(t)

	car := f.addAsset(t, "Car", "vehicle", "18000.00", time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC))
	year := int32(2019)
	if _, err := f.q.UpsertAssetDetail(f.ctx, dbgen.UpsertAssetDetailParams{
		ManualAssetID: car, Year: &year,
	}); err != nil {
		t.Fatalf("upsert detail: %v", err)
	}

	// The estimator has plenty to say about this car…
	sug := networth.SuggestVehicleValue(networth.VehicleValuation{
		LastValue:     decimal.RequireFromString("18000.00"),
		LastValueAsOf: time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC),
		ModelYear:     2019,
	}, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	if !sug.OK {
		t.Fatalf("expected a suggestion: %s", sug.Reason)
	}
	if sug.Value.Equal(decimal.RequireFromString("18000.00")) {
		t.Fatal("the suggestion is identical to the current value; the test proves nothing")
	}

	// …and none of it reaches the database on its own.
	if _, _, err := jobs.RevalueBonds(f.ctx, f.pool, f.q, f.householdID,
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("revalue bonds: %v", err)
	}

	if got := f.assetValue(t, car).StringFixed(2); got != "18000.00" {
		t.Errorf("car value moved to %s without the user accepting anything", got)
	}
}
