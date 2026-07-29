package db

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/merchants"
)

// merchantFixture is a household seeded with one subscription that bills under
// TWO different descriptors — the exact shape merchant canonicalisation exists
// to fix, and the shape every assertion below is written against.
type merchantFixture struct {
	pool        *pgxpool.Pool
	q           *dbgen.Queries
	householdID uuid.UUID
	userID      uuid.UUID
	entID       uuid.UUID
	foodID      uuid.UUID
}

func setupMerchantFixture(t *testing.T) (context.Context, *merchantFixture) {
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

	f := &merchantFixture{
		pool:        pool,
		q:           dbgen.New(pool),
		householdID: uuid.New(),
		userID:      uuid.New(),
		entID:       uuid.New(),
		foodID:      uuid.New(),
	}
	itemID, acctID := uuid.New(), uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Merchant Test')`, f.householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, f.householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, f.userID, f.householdID, f.userID.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemID, f.userID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Checking', 'depository')`, acctID, itemID, acctID.String())
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Entertainment', 'entertainment')`, f.entID, f.householdID)
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Food & Drink', 'food-drink')`, f.foodID, f.householdID)

	addTx := func(amount, date, name, merchant, key string, cat uuid.UUID) {
		exec(`INSERT INTO transactions
		        (account_id, amount, currency, date, name, merchant_name, merchant_key, category_id, source)
		      VALUES ($1, $2, 'USD', $3, $4, $5, $6, $7, 'plaid')`,
			acctID, amount, date, name, merchant, key, cat)
	}

	// One monthly subscription, six charges, ~30-day cadence — but the biller
	// alternates between two descriptors. Neither descriptor alone reaches the
	// detector's n >= 3 threshold with a regular gap, so before a merge this is
	// invisible; after one it is an obvious subscription. That contrast is this
	// feature's headline result.
	addTx("11.00", "2026-01-05", "ACME PLUS", "Acme Plus", "acme plus", f.entID)
	addTx("11.00", "2026-03-05", "ACME PLUS", "Acme Plus", "acme plus", f.entID)
	addTx("11.00", "2026-05-05", "ACME PLUS", "Acme Plus", "acme plus", f.entID)
	addTx("11.00", "2026-02-05", "ACME*PLUS BILL", "Acme Plus Billing", "acmeplus bill", f.entID)
	addTx("11.00", "2026-04-05", "ACME*PLUS BILL", "Acme Plus Billing", "acmeplus bill", f.entID)
	addTx("11.00", "2026-06-05", "ACME*PLUS BILL", "Acme Plus Billing", "acmeplus bill", f.entID)

	return ctx, f
}

func (f *merchantFixture) recurring(t *testing.T, ctx context.Context) []dbgen.GetRecurringMerchantsRow {
	t.Helper()
	rows, err := f.q.GetRecurringMerchants(ctx, dbgen.GetRecurringMerchantsParams{
		HouseholdID: f.householdID, UserID: f.userID, Date: mustDate(t, "2026-01-01"),
	})
	if err != nil {
		t.Fatalf("GetRecurringMerchants: %v", err)
	}
	return rows
}

func (f *merchantFixture) topMerchants(t *testing.T, ctx context.Context) []dbgen.GetTopMerchantsRow {
	t.Helper()
	rows, err := f.q.GetTopMerchants(ctx, dbgen.GetTopMerchantsParams{
		HouseholdID: f.householdID, UserID: f.userID,
		Date: mustDate(t, "2026-01-01"), Date_2: mustDate(t, "2026-12-31"), Limit: 20,
	})
	if err != nil {
		t.Fatalf("GetTopMerchants: %v", err)
	}
	return rows
}

// This is doc 17's headline claim, tested directly: a subscription split across
// two descriptors is undetected before the merge and detected after it.
func TestMergeMakesSplitSubscriptionDetectable(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	for _, row := range f.recurring(t, ctx) {
		if row.Merchant == "Acme Plus" || row.MerchantKey == "acme plus" {
			t.Fatalf("split subscription was detected before the merge: %+v", row)
		}
	}

	entityID, outcome, err := merchants.Merge(ctx, f.q, f.householdID,
		[]string{"acme plus", "acmeplus bill"}, "Acme Plus", nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if outcome.Conflict {
		t.Fatalf("unexpected category conflict")
	}

	rows := f.recurring(t, ctx)
	var found *dbgen.GetRecurringMerchantsRow
	for i := range rows {
		if rows[i].MerchantKey == entityID.String() {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatalf("merged subscription not detected; got %+v", rows)
	}
	if found.Merchant != "Acme Plus" {
		t.Errorf("merchant name = %q, want the canonical name %q", found.Merchant, "Acme Plus")
	}
	if found.Occurrences != 6 {
		t.Errorf("occurrences = %d, want 6 (both descriptors)", found.Occurrences)
	}
}

// The contract the whole design rests on: a suggested alias is inert. Reports
// must be byte-identical before and after one exists, or unreviewed model
// guesses silently reshape the user's spending history.
func TestSuggestedAliasesChangeNoReport(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	beforeRecurring := f.recurring(t, ctx)
	beforeTop := f.topMerchants(t, ctx)

	// Write the suggestion exactly as the job would: a fresh entity whose every
	// alias is 'suggested'.
	entity, err := f.q.CreateMerchantEntity(ctx, dbgen.CreateMerchantEntityParams{
		HouseholdID: f.householdID, CanonicalName: "Acme Plus",
	})
	if err != nil {
		t.Fatalf("CreateMerchantEntity: %v", err)
	}
	for _, key := range []string{"acme plus", "acmeplus bill"} {
		if _, err := f.q.UpsertMerchantAlias(ctx, dbgen.UpsertMerchantAliasParams{
			HouseholdID: f.householdID, EntityID: entity.ID, MerchantKey: key,
			Source:     "suggested",
			Confidence: decimal.NullDecimal{Decimal: decimal.NewFromFloat(0.8), Valid: true},
		}); err != nil {
			t.Fatalf("UpsertMerchantAlias: %v", err)
		}
	}

	afterRecurring := f.recurring(t, ctx)
	if len(afterRecurring) != len(beforeRecurring) {
		t.Fatalf("suggested alias changed recurring detection: %d rows before, %d after",
			len(beforeRecurring), len(afterRecurring))
	}
	for i := range beforeRecurring {
		if beforeRecurring[i].MerchantKey != afterRecurring[i].MerchantKey ||
			!beforeRecurring[i].AverageAmount.Equal(afterRecurring[i].AverageAmount) {
			t.Errorf("suggested alias changed a recurring row:\n before %+v\n after  %+v",
				beforeRecurring[i], afterRecurring[i])
		}
	}

	afterTop := f.topMerchants(t, ctx)
	if len(afterTop) != len(beforeTop) {
		t.Fatalf("suggested alias changed top merchants: %d rows before, %d after",
			len(beforeTop), len(afterTop))
	}
	for i := range beforeTop {
		if beforeTop[i].Merchant != afterTop[i].Merchant || !beforeTop[i].Total.Equal(afterTop[i].Total) {
			t.Errorf("suggested alias changed a top-merchant row:\n before %+v\n after  %+v",
				beforeTop[i], afterTop[i])
		}
	}

	// And confirming it does change things — otherwise the assertion above would
	// pass just as well against a feature that does nothing at all.
	if _, _, err := merchants.Merge(ctx, f.q, f.householdID,
		[]string{"acme plus", "acmeplus bill"}, "Acme Plus", &entity.ID); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if confirmed := f.topMerchants(t, ctx); len(confirmed) >= len(beforeTop) {
		t.Errorf("confirming the merge did not collapse two descriptors into one: %+v", confirmed)
	}
}

// A manual category is sticky — the README's rule outranks any merge. When one
// fragment was filed by hand and another by the model, the manual choice must
// survive and spread, never be overwritten.
func TestMergeNeverOverwritesManualCategory(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	// The user filed one descriptor by hand as Food; the model guessed
	// Entertainment for the other.
	if err := f.q.UpsertMerchantCategory(ctx, dbgen.UpsertMerchantCategoryParams{
		HouseholdID: f.householdID, MerchantKey: "acme plus",
		CategoryID: f.foodID, Source: "manual",
	}); err != nil {
		t.Fatalf("seed manual mapping: %v", err)
	}
	if err := f.q.UpsertMerchantCategory(ctx, dbgen.UpsertMerchantCategoryParams{
		HouseholdID: f.householdID, MerchantKey: "acmeplus bill",
		CategoryID: f.entID, Source: "llm",
	}); err != nil {
		t.Fatalf("seed llm mapping: %v", err)
	}

	_, outcome, err := merchants.Merge(ctx, f.q, f.householdID,
		[]string{"acme plus", "acmeplus bill"}, "Acme Plus", nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if outcome.Conflict {
		t.Fatalf("one manual mapping is not a conflict")
	}
	if !outcome.FromManual {
		t.Errorf("outcome should record that the surviving category was manual")
	}
	if outcome.Applied == nil || *outcome.Applied != f.foodID {
		t.Fatalf("winning category = %v, want the manual choice %v", outcome.Applied, f.foodID)
	}

	// The manual mapping is untouched, and the model's guess now agrees with it.
	for _, key := range []string{"acme plus", "acmeplus bill"} {
		got, err := f.q.LookupMerchantCategory(ctx, dbgen.LookupMerchantCategoryParams{
			HouseholdID: f.householdID, MerchantKey: key,
		})
		if err != nil {
			t.Fatalf("LookupMerchantCategory(%q): %v", key, err)
		}
		if got != f.foodID {
			t.Errorf("%q resolves to %v, want the manual category %v", key, got, f.foodID)
		}
	}
}

// Two DIFFERENT manual categories are the user's own conflicting choices.
// Neither this code nor a model gets to pick a winner, so nothing changes.
func TestMergeLeavesConflictingManualCategoriesAlone(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	if err := f.q.UpsertMerchantCategory(ctx, dbgen.UpsertMerchantCategoryParams{
		HouseholdID: f.householdID, MerchantKey: "acme plus",
		CategoryID: f.foodID, Source: "manual",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := f.q.UpsertMerchantCategory(ctx, dbgen.UpsertMerchantCategoryParams{
		HouseholdID: f.householdID, MerchantKey: "acmeplus bill",
		CategoryID: f.entID, Source: "manual",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, outcome, err := merchants.Merge(ctx, f.q, f.householdID,
		[]string{"acme plus", "acmeplus bill"}, "Acme Plus", nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !outcome.Conflict {
		t.Fatalf("two different manual categories must be reported as a conflict")
	}

	want := map[string]uuid.UUID{"acme plus": f.foodID, "acmeplus bill": f.entID}
	for key, expected := range want {
		got, err := f.q.LookupMerchantCategory(ctx, dbgen.LookupMerchantCategoryParams{
			HouseholdID: f.householdID, MerchantKey: key,
		})
		if err != nil {
			t.Fatalf("LookupMerchantCategory(%q): %v", key, err)
		}
		if got != expected {
			t.Errorf("%q was changed to %v; a conflict must change nothing (want %v)", key, got, expected)
		}
	}
}

// Suppressing a merged merchant must silence the whole merchant, and splitting
// it must record the refusal so the suggester does not re-propose the merge.
func TestSuppressionAndSplitFollowTheEntity(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	entityID, _, err := merchants.Merge(ctx, f.q, f.householdID,
		[]string{"acme plus", "acmeplus bill"}, "Acme Plus", nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// Suppress using a RAW descriptor, not the resolved key: a user clicking
	// "not recurring" somewhere else in the app has only the raw key to hand,
	// and the suppression still has to reach the merged merchant.
	if err := f.q.SuppressRecurringMerchant(ctx, dbgen.SuppressRecurringMerchantParams{
		HouseholdID: f.householdID, MerchantKey: "acme plus", MerchantLabel: "Acme Plus",
	}); err != nil {
		t.Fatalf("SuppressRecurringMerchant: %v", err)
	}
	for _, row := range f.recurring(t, ctx) {
		if row.MerchantKey == entityID.String() {
			t.Fatalf("suppressing a raw descriptor did not silence the merged merchant")
		}
	}

	if err := f.q.UnsuppressRecurringMerchant(ctx, dbgen.UnsuppressRecurringMerchantParams{
		HouseholdID: f.householdID, MerchantKey: entityID.String(),
	}); err != nil {
		t.Fatalf("UnsuppressRecurringMerchant: %v", err)
	}
	restored := false
	for _, row := range f.recurring(t, ctx) {
		if row.MerchantKey == entityID.String() {
			restored = true
		}
	}
	if !restored {
		t.Fatalf("unsuppressing by resolved key did not restore the merchant")
	}

	// Splitting undoes the merge and remembers why.
	if err := merchants.Split(ctx, f.q, f.householdID, "acmeplus bill"); err != nil {
		t.Fatalf("Split: %v", err)
	}
	rejections, err := f.q.ListMergeRejections(ctx, f.householdID)
	if err != nil {
		t.Fatalf("ListMergeRejections: %v", err)
	}
	if len(rejections) != 1 || rejections[0].KeyA != "acme plus" || rejections[0].KeyB != "acmeplus bill" {
		t.Fatalf("split did not record the pair as rejected: %+v", rejections)
	}

	// With one member left, the entity is retired — it canonicalises nothing.
	entities, err := f.q.ListMerchantAliasStats(ctx, dbgen.ListMerchantAliasStatsParams{
		HouseholdID: f.householdID, UserID: f.userID,
	})
	if err != nil {
		t.Fatalf("ListMerchantAliasStats: %v", err)
	}
	if len(entities) != 0 {
		t.Errorf("entity survived a split down to one member: %+v", entities)
	}

	// And the suggester honours the rejection rather than proposing it again.
	n, err := merchants.SuggestHousehold(ctx, f.q, nil, f.householdID, f.userID)
	if err != nil {
		t.Fatalf("SuggestHousehold: %v", err)
	}
	if n != 0 {
		t.Errorf("suggester re-proposed a rejected merge (%d suggestions)", n)
	}
}
