package db

import (
	"context"
	"os"
	"strings"
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

// addTx seeds one extra transaction, for tests that need descriptors beyond the
// base subscription fixture.
func (f *merchantFixture) addTx(t *testing.T, ctx context.Context, date, name, key string) {
	t.Helper()
	var acctID uuid.UUID
	if err := f.pool.QueryRow(ctx, `
		SELECT a.id FROM accounts a
		JOIN plaid_items i ON i.id = a.plaid_item_id
		WHERE i.user_id = $1 LIMIT 1`, f.userID).Scan(&acctID); err != nil {
		t.Fatalf("find account: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO transactions
		  (account_id, amount, currency, date, name, merchant_name, merchant_key, category_id, source)
		VALUES ($1, '25.00', 'USD', $2, $3, $3, $4, $5, 'plaid')`,
		acctID, date, name, key, f.foodID); err != nil {
		t.Fatalf("seed tx: %v", err)
	}
}

// suggestedGroups reads the pending queue as sets of descriptor keys, one per
// proposed merchant.
func (f *merchantFixture) suggestedGroups(t *testing.T, ctx context.Context) map[string][]string {
	t.Helper()
	rows, err := f.pool.Query(ctx, `
		SELECT e.canonical_name, a.merchant_key
		FROM merchant_aliases a
		JOIN merchant_entities e ON e.id = a.entity_id
		WHERE a.household_id = $1 AND a.source = 'suggested'
		ORDER BY e.canonical_name, a.merchant_key`, f.householdID)
	if err != nil {
		t.Fatalf("read suggestions: %v", err)
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var name, key string
		if err := rows.Scan(&name, &key); err != nil {
			t.Fatalf("scan suggestion: %v", err)
		}
		out[name] = append(out[name], key)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read suggestions: %v", err)
	}
	return out
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

	// One member left, and the entity SURVIVES holding it.
	//
	// This used to retire at fewer than two aliases, on the reasoning that a
	// one-alias entity canonicalises nothing. Normalising descriptors at import
	// broke that reasoning: a business whose every descriptor collapses to one key
	// legitimately has one alias, and retiring it would throw away the name the
	// user gave it and redisplay the raw descriptor. A name is worth more than the
	// tidiness, so only a genuinely empty entity is retired now.
	entities, err := f.q.ListMerchantAliasStats(ctx, dbgen.ListMerchantAliasStatsParams{
		HouseholdID: f.householdID, UserID: f.userID,
	})
	if err != nil {
		t.Fatalf("ListMerchantAliasStats: %v", err)
	}
	if len(entities) != 1 || entities[0].CanonicalName != "Acme Plus" {
		t.Fatalf("want the named entity kept with its last member, got %+v", entities)
	}

	// Splitting that last member is how the merge is fully undone, and it does
	// retire the entity — so nothing is a one-way door.
	if err := merchants.Split(ctx, f.q, f.householdID, "acme plus"); err != nil {
		t.Fatalf("Split last member: %v", err)
	}
	entities, err = f.q.ListMerchantAliasStats(ctx, dbgen.ListMerchantAliasStatsParams{
		HouseholdID: f.householdID, UserID: f.userID,
	})
	if err != nil {
		t.Fatalf("ListMerchantAliasStats: %v", err)
	}
	if len(entities) != 0 {
		t.Errorf("entity survived losing its last alias: %+v", entities)
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

// The Home Depot / HomeGoods case, end to end through the real suggestion pass.
//
// These five descriptors are taken from live card data, where the engine proposed
// all five as ONE merchant: "home" truncates "homegoods", so a hardware store and
// a homeware store were chained together and the only way to accept the correct
// half was to accept the incorrect half with it.
func TestSuggestSeparatesHomeDepotFromHomeGoods(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	f.addTx(t, ctx, "2026-01-10", "HOME DEPOT", "home depot")
	f.addTx(t, ctx, "2026-02-10", "THE HOME DEPOT #4905", "the home depot")
	f.addTx(t, ctx, "2026-03-10", "HOMEGOODS", "homegoods")
	f.addTx(t, ctx, "2026-04-10", "HOMEGOODS #0265", "homegoods 0265")

	if _, err := merchants.SuggestHousehold(ctx, f.q, nil, f.householdID, f.userID); err != nil {
		t.Fatalf("SuggestHousehold: %v", err)
	}

	groups := f.suggestedGroups(t, ctx)
	for name, keys := range groups {
		hasDepot, hasGoods := false, false
		for _, k := range keys {
			if strings.Contains(k, "depot") {
				hasDepot = true
			}
			if strings.Contains(k, "homegoods") {
				hasGoods = true
			}
		}
		if hasDepot && hasGoods {
			t.Fatalf("%q groups the hardware store with the homeware store: %v", name, keys)
		}
	}

	// Both halves still have to be proposed — separating them must not cost the
	// two correct merges.
	var depot, goods []string
	for _, keys := range groups {
		if len(keys) > 0 && strings.Contains(keys[0], "depot") {
			depot = keys
		}
		if len(keys) > 0 && strings.Contains(keys[0], "homegoods") {
			goods = keys
		}
	}
	if len(depot) != 2 {
		t.Errorf("want both Home Depot descriptors proposed together, got %v", depot)
	}
	if len(goods) != 2 {
		t.Errorf("want both HomeGoods descriptors proposed together, got %v", goods)
	}
}

// A suggestion pass REPLACES the pending queue rather than adding to it, so a
// proposal the current rules would not make does not outlive them.
func TestSuggestReplacesStalePendingQueue(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	// A stale proposal of exactly the shape the old rules produced: two businesses
	// that today's rules keep apart.
	stale, err := f.q.CreateMerchantEntity(ctx, dbgen.CreateMerchantEntityParams{
		HouseholdID: f.householdID, CanonicalName: "Home Depot #4905",
	})
	if err != nil {
		t.Fatalf("CreateMerchantEntity: %v", err)
	}
	for _, key := range []string{"home depot", "homegoods"} {
		f.addTx(t, ctx, "2026-01-10", strings.ToUpper(key), key)
		if _, err := f.q.UpsertMerchantAlias(ctx, dbgen.UpsertMerchantAliasParams{
			HouseholdID: f.householdID, EntityID: stale.ID, MerchantKey: key,
			Source: "suggested", Confidence: decimal.NullDecimal{},
		}); err != nil {
			t.Fatalf("UpsertMerchantAlias: %v", err)
		}
	}

	if _, err := merchants.SuggestHousehold(ctx, f.q, nil, f.householdID, f.userID); err != nil {
		t.Fatalf("SuggestHousehold: %v", err)
	}

	for name, keys := range f.suggestedGroups(t, ctx) {
		if len(keys) < 2 {
			continue
		}
		hasDepot, hasGoods := false, false
		for _, k := range keys {
			if k == "home depot" {
				hasDepot = true
			}
			if k == "homegoods" {
				hasGoods = true
			}
		}
		if hasDepot && hasGoods {
			t.Fatalf("stale proposal %q survived the pass that should have replaced it: %v", name, keys)
		}
	}
}

// Confirming PART of a proposal, which is the whole point of per-descriptor
// selection — and the follow-on question it raises: what happens next time?
//
// Starting state is the real one the old rules produced: a single proposal holding
// three Home Depot descriptors and two HomeGoods descriptors. The user confirms
// the three and unticks the two, and three things have to be true afterwards.
// The merge lands. The refusal is recorded pairwise, and ONLY across the split —
// the two HomeGoods descriptors were never claimed to differ from each other. And
// the next pass proposes HomeGoods on its own rather than re-proposing the
// grouping that was just declined.
func TestPartialConfirmSplitsProposalAndReproposesTheRest(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	depot := []string{"home depot", "the home depot", "the home depot 4905"}
	goods := []string{"homegoods", "homegoods 0265"}

	entity, err := f.q.CreateMerchantEntity(ctx, dbgen.CreateMerchantEntityParams{
		HouseholdID: f.householdID, CanonicalName: "Home Depot",
	})
	if err != nil {
		t.Fatalf("CreateMerchantEntity: %v", err)
	}
	for _, key := range append(append([]string{}, depot...), goods...) {
		f.addTx(t, ctx, "2026-01-10", strings.ToUpper(key), key)
		if _, err := f.q.UpsertMerchantAlias(ctx, dbgen.UpsertMerchantAliasParams{
			HouseholdID: f.householdID, EntityID: entity.ID, MerchantKey: key,
			Source: "suggested", Confidence: decimal.NullDecimal{},
		}); err != nil {
			t.Fatalf("UpsertMerchantAlias: %v", err)
		}
	}

	// The partial confirm, in the order the handler runs it: refuse first, while
	// the entity still holds every proposed descriptor, then merge.
	if err := merchants.Reject(ctx, f.q, f.householdID, entity.ID, goods); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if _, _, err := merchants.Merge(ctx, f.q, f.householdID, depot, "Home Depot", &entity.ID); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// The three Home Depots are one merchant, and the HomeGoods keys are not on it.
	aliases, err := f.q.ListMerchantAliases(ctx, dbgen.ListMerchantAliasesParams{
		HouseholdID: f.householdID, EntityID: entity.ID,
	})
	if err != nil {
		t.Fatalf("ListMerchantAliases: %v", err)
	}
	if len(aliases) != len(depot) {
		t.Fatalf("want %d confirmed descriptors, got %d: %+v", len(depot), len(aliases), aliases)
	}
	for _, a := range aliases {
		if a.Source != "manual" {
			t.Errorf("%q confirmed with source %q, want manual", a.MerchantKey, a.Source)
		}
		if strings.Contains(a.MerchantKey, "homegoods") {
			t.Errorf("%q was merged despite being unticked", a.MerchantKey)
		}
	}

	// Rejections span the split and nothing else: 3 x 2 across, none within.
	rejections, err := f.q.ListMergeRejections(ctx, f.householdID)
	if err != nil {
		t.Fatalf("ListMergeRejections: %v", err)
	}
	if len(rejections) != len(depot)*len(goods) {
		t.Errorf("want %d cross rejections, got %d: %+v", len(depot)*len(goods), len(rejections), rejections)
	}
	for _, r := range rejections {
		if strings.Contains(r.KeyA, "homegoods") && strings.Contains(r.KeyB, "homegoods") {
			t.Errorf("recorded a refusal between two descriptors unticked together: %s / %s", r.KeyA, r.KeyB)
		}
	}

	// And the next pass offers HomeGoods on its own.
	if _, err := merchants.SuggestHousehold(ctx, f.q, nil, f.householdID, f.userID); err != nil {
		t.Fatalf("SuggestHousehold: %v", err)
	}
	var proposed []string
	for name, keys := range f.suggestedGroups(t, ctx) {
		for _, k := range keys {
			if strings.Contains(k, "depot") {
				t.Errorf("%q re-proposed a Home Depot descriptor that is already confirmed: %v", name, keys)
			}
		}
		if len(keys) == 2 && strings.Contains(keys[0], "homegoods") {
			proposed = keys
		}
	}
	if len(proposed) != 2 {
		t.Errorf("want HomeGoods proposed as its own pair, got %v", proposed)
	}
}

// The merchant detail queries have to agree with the report they are reached
// from. A drill-down that shows a different total than the row you clicked is
// worse than no drill-down: it makes both numbers untrustworthy.
//
// Checked on a MERGED merchant, where the two could most easily diverge — the
// report groups by canonical name while the detail addresses an entity UUID, and
// both have to resolve the same two descriptors to the same money.
func TestMerchantDetailReconcilesWithTopMerchants(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	entityID, _, err := merchants.Merge(ctx, f.q, f.householdID,
		[]string{"acme plus", "acmeplus bill"}, "Acme Plus", nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	var want dbgen.GetTopMerchantsRow
	for _, row := range f.topMerchants(t, ctx) {
		if row.Merchant == "Acme Plus" {
			want = row
		}
	}
	if want.Merchant == "" {
		t.Fatal("merged merchant missing from top merchants")
	}
	// The report has to hand out the resolved key, or nothing can link to detail.
	if want.MerchantKey != entityID.String() {
		t.Fatalf("top merchants key = %q, want the entity id %q", want.MerchantKey, entityID)
	}

	from, to := mustDate(t, "2026-01-01"), mustDate(t, "2026-12-31")
	got, err := f.q.GetMerchantSummary(ctx, dbgen.GetMerchantSummaryParams{
		HouseholdID: f.householdID, UserID: f.userID,
		Date: from, Date_2: to, ResolvedKey: entityID.String(),
	})
	if err != nil {
		t.Fatalf("GetMerchantSummary: %v", err)
	}
	if !got.Total.Equal(want.Total) {
		t.Errorf("detail total = %s, top merchants said %s", got.Total, want.Total)
	}
	if got.TransactionCount != want.TransactionCount {
		t.Errorf("detail count = %d, top merchants said %d", got.TransactionCount, want.TransactionCount)
	}
	if got.TransactionCount != 6 {
		t.Errorf("count = %d, want 6 across both descriptors", got.TransactionCount)
	}

	// Identity resolves the display name and both descriptors behind it.
	ident, err := f.q.GetMerchantIdentity(ctx, dbgen.GetMerchantIdentityParams{
		HouseholdID: f.householdID, UserID: f.userID, ResolvedKey: entityID.String(),
	})
	if err != nil {
		t.Fatalf("GetMerchantIdentity: %v", err)
	}
	if ident.Merchant != "Acme Plus" || !ident.IsGrouped || len(ident.Descriptors) != 2 {
		t.Errorf("identity = %+v, want the canonical name, grouped, two descriptors", ident)
	}

	// The month series has to add up to the same money as the summary.
	months, err := f.q.GetMerchantMonthlySpend(ctx, dbgen.GetMerchantMonthlySpendParams{
		HouseholdID: f.householdID, UserID: f.userID,
		Date: from, Date_2: to, ResolvedKey: entityID.String(),
	})
	if err != nil {
		t.Fatalf("GetMerchantMonthlySpend: %v", err)
	}
	sum := decimal.Zero
	for _, m := range months {
		sum = sum.Add(m.Total)
	}
	if !sum.Equal(got.Total) {
		t.Errorf("monthly series sums to %s, summary says %s", sum, got.Total)
	}

	// An ungrouped descriptor is addressable by its raw key, which is the case
	// most of a household's spending falls into.
	f.addTx(t, ctx, "2026-03-01", "LONE SHOP", "lone shop")
	lone, err := f.q.GetMerchantIdentity(ctx, dbgen.GetMerchantIdentityParams{
		HouseholdID: f.householdID, UserID: f.userID, ResolvedKey: "lone shop",
	})
	if err != nil {
		t.Fatalf("GetMerchantIdentity for a raw key: %v", err)
	}
	if lone.IsGrouped || len(lone.Descriptors) != 1 {
		t.Errorf("ungrouped identity = %+v, want not grouped with one descriptor", lone)
	}
}

// The same reconciliation for an UNGROUPED merchant whose bank text varies.
//
// This is the case that actually broke: two charges normalise to one merchant_key
// but arrive with different descriptor text, so a report grouped by display name
// emitted two rows carrying the SAME resolved key — two different totals, both
// linking to one detail page that agreed with neither.
func TestTopMerchantsGroupsByResolvedKeyNotDisplayName(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	// One key, two descriptor spellings, as plaid.MerchantKey would leave them.
	f.addTx(t, ctx, "2026-02-11", "THE HOME DEPOT #4905", "the home depot")
	f.addTx(t, ctx, "2026-06-11", "THE HOME DEPOT 4905", "the home depot")

	var rows []dbgen.GetTopMerchantsRow
	for _, row := range f.topMerchants(t, ctx) {
		if row.MerchantKey == "the home depot" {
			rows = append(rows, row)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("want one row per resolved key, got %d: %+v", len(rows), rows)
	}
	if rows[0].TransactionCount != 2 {
		t.Errorf("count = %d, want both charges on the one row", rows[0].TransactionCount)
	}
	// The most recent descriptor names the row.
	if rows[0].Merchant != "THE HOME DEPOT 4905" {
		t.Errorf("merchant = %q, want the most recent descriptor", rows[0].Merchant)
	}

	// And the detail page agrees with the row.
	summary, err := f.q.GetMerchantSummary(ctx, dbgen.GetMerchantSummaryParams{
		HouseholdID: f.householdID, UserID: f.userID,
		Date: mustDate(t, "2026-01-01"), Date_2: mustDate(t, "2026-12-31"),
		ResolvedKey: "the home depot",
	})
	if err != nil {
		t.Fatalf("GetMerchantSummary: %v", err)
	}
	if !summary.Total.Equal(rows[0].Total) {
		t.Errorf("detail total %s != top-merchants total %s", summary.Total, rows[0].Total)
	}
}
