package advisor

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/reporting"
)

// The database-backed half of the advisor's verification. Everything here is
// about AGREEMENT with the rest of the app rather than about the ranking itself,
// which rank_test.go covers purely.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/advisor/

// advisorFixture is a household with six clean prior months: $5,000 income,
// $1,000 of rent, a $400 dining budget. Deliberately the same shape as
// reporting's own safe-to-spend fixture, so the two can be compared directly.
type advisorFixture struct {
	pool        *pgxpool.Pool
	q           *dbgen.Queries
	householdID uuid.UUID
	userID      uuid.UUID
	itemID      uuid.UUID
	checking    uuid.UUID
	fixedCat    uuid.UUID
	now         time.Time
}

func newFixture(t *testing.T) *advisorFixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	f := &advisorFixture{
		pool: pool, q: dbgen.New(pool),
		householdID: uuid.New(), userID: uuid.New(), itemID: uuid.New(),
		checking: uuid.New(), fixedCat: uuid.New(),
		now: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),
	}

	incomeCat, discCat := uuid.New(), uuid.New()
	f.exec(t, `INSERT INTO households (id, name) VALUES ($1, 'Advisor Test')`, f.householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, f.householdID)
	})
	f.exec(t, `INSERT INTO users (id, household_id, email, password_hash, display_name)
	           VALUES ($1, $2, $3, 'x', 'Tester')`, f.userID, f.householdID, f.userID.String()+"@example.test")
	f.exec(t, `INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	           VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, f.itemID, f.userID, f.itemID.String())
	f.exec(t, `INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, current_balance)
	           VALUES ($1, $2, $3, 'Checking', 'depository', '9000.00')`, f.checking, f.itemID, f.checking.String())
	f.exec(t, `INSERT INTO categories (id, household_id, name, slug, is_income) VALUES ($1, $2, 'Paycheck', 'paycheck', TRUE)`, incomeCat, f.householdID)
	f.exec(t, `INSERT INTO categories (id, household_id, name, slug, is_fixed) VALUES ($1, $2, 'Rent', 'rent', TRUE)`, f.fixedCat, f.householdID)
	f.exec(t, `INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Dining', 'dining')`, discCat, f.householdID)

	for _, d := range []string{"2026-01", "2026-02", "2026-03", "2026-04", "2026-05", "2026-06"} {
		f.tx(t, f.checking, "-5000.00", d+"-15", incomeCat) // income stored negative
		f.tx(t, f.checking, "1000.00", d+"-10", f.fixedCat)
	}

	if _, err := f.q.UpsertBudget(context.Background(), dbgen.UpsertBudgetParams{
		HouseholdID: f.householdID, CategoryID: discCat,
		Amount: decimal.RequireFromString("400.00"), Period: "monthly",
	}); err != nil {
		t.Fatalf("UpsertBudget: %v", err)
	}

	// Projection assumptions: the hurdle is derived from these, so a household
	// without them is a different test.
	f.exec(t, `INSERT INTO projection_assumptions (household_id, real_return_rate, inflation_rate, withdrawal_rate)
	           VALUES ($1, '0.05', '0.03', '0.04')`, f.householdID)

	return f
}

func (f *advisorFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("seed: %v\n%s", err, sql)
	}
}

func (f *advisorFixture) tx(t *testing.T, acct uuid.UUID, amount, date string, cat uuid.UUID) {
	t.Helper()
	f.exec(t, `INSERT INTO transactions (account_id, amount, currency, date, name, category_id, source)
	           VALUES ($1, $2, 'USD', $3, 'x', $4, 'plaid')`, acct, amount, date, cat)
}

// TestAdvisorSlackAgreesWithSafeToSpend IS THE SINGLE MOST IMPORTANT TEST HERE.
//
// The Budgets page and the advisor print the same figure with opposite
// instructions attached. If they ever differ by a cent, a household that notices
// will trust neither surface again — so this asserts the EXACT decimal, against
// both of BuildSafeToSpend's figures, and asserts the advisor picked the
// bill-aware one exactly when ObligationCoverage > 0.
func TestAdvisorSlackAgreesWithSafeToSpend(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	sts, err := reporting.BuildSafeToSpend(ctx, f.q, f.householdID, f.now)
	if err != nil {
		t.Fatalf("BuildSafeToSpend: %v", err)
	}
	adv, err := Build(ctx, f.q, f.householdID, f.now)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// No obligations in this fixture, so the ordinary figure is the one.
	if sts.ObligationCoverage != 0 {
		t.Fatalf("fixture drifted: obligation coverage = %d, want 0", sts.ObligationCoverage)
	}
	if adv.SlackBasis != SlackBasisTypicalMonth {
		t.Errorf("slack basis = %q, want %q with no obligation coverage", adv.SlackBasis, SlackBasisTypicalMonth)
	}
	if !adv.Slack.Equal(sts.Amount) {
		t.Errorf("advisor slack = %s, safe-to-spend = %s — these must be identical", adv.Slack, sts.Amount)
	}

	// And the breakdown behind it is carried through, not re-derived.
	if !adv.SlackParts.ExpectedIncome.Equal(sts.ExpectedIncome) {
		t.Errorf("expected income = %s, want %s", adv.SlackParts.ExpectedIncome, sts.ExpectedIncome)
	}
	if !adv.SlackParts.FixedCosts.Equal(sts.FixedCosts) {
		t.Errorf("fixed costs = %s, want %s", adv.SlackParts.FixedCosts, sts.FixedCosts)
	}

	// Now give the household a known bill so the bill-aware figure becomes the
	// better answer, and assert the advisor switches to it — and to nothing else.
	// The obligation's occurrence must fall inside the upcoming window
	// [today, month-end] for it to count; anchor it after `now` like the
	// reporting safe-to-spend fixture does. A manual obligation needs an owner,
	// so user_id is set — is_shared defaults TRUE, which is what makes it a
	// household bill.
	f.exec(t, `INSERT INTO recurring_obligations
	             (household_id, user_id, label, amount, category_id, interval_count, interval_unit, anchor_date)
	           VALUES ($1, $2, 'Rent', '1200.00', $3, 1, 'month', '2026-07-25')`,
		f.householdID, f.userID, f.fixedCat)

	sts2, err := reporting.BuildSafeToSpend(ctx, f.q, f.householdID, f.now)
	if err != nil {
		t.Fatalf("BuildSafeToSpend (with bill): %v", err)
	}
	if sts2.ObligationCoverage == 0 {
		t.Fatalf("fixture drifted: want obligation coverage > 0 after adding a bill")
	}

	adv2, err := Build(ctx, f.q, f.householdID, f.now)
	if err != nil {
		t.Fatalf("Build (with bill): %v", err)
	}
	if adv2.SlackBasis != SlackBasisAfterBills {
		t.Errorf("slack basis = %q, want %q once coverage > 0", adv2.SlackBasis, SlackBasisAfterBills)
	}
	if !adv2.Slack.Equal(sts2.AmountAfterBills) {
		t.Errorf("advisor slack = %s, safe-to-spend after bills = %s — these must be identical",
			adv2.Slack, sts2.AmountAfterBills)
	}
	if adv2.ObligationCoverage != sts2.ObligationCoverage {
		t.Errorf("obligation coverage = %d, want %d", adv2.ObligationCoverage, sts2.ObligationCoverage)
	}
}

// TestAdvisorSlackIsAMedianNotAMean. A one-off bonus month must not raise the
// advisor's idea of a typical month. This is the property the doc's ORIGINAL
// formula — "income so far plus projected income" — would have broken, and it
// holds here only because the advisor calls BuildSafeToSpend rather than
// reimplementing it.
func TestAdvisorSlackIsAMedianNotAMean(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	before, err := Build(ctx, f.q, f.householdID, f.now)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// A $20,000 bonus in one of the six trailing months. A mean would move the
	// figure by more than $3,000; a median ignores it outright.
	var incomeCat uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM categories WHERE household_id = $1 AND slug = 'paycheck'`,
		f.householdID).Scan(&incomeCat); err != nil {
		t.Fatalf("find income category: %v", err)
	}
	f.tx(t, f.checking, "-20000.00", "2026-04-20", incomeCat)

	after, err := Build(ctx, f.q, f.householdID, f.now)
	if err != nil {
		t.Fatalf("Build (with bonus): %v", err)
	}
	if !after.Slack.Equal(before.Slack) {
		t.Errorf("a one-off bonus moved slack from %s to %s — the median is not holding",
			before.Slack, after.Slack)
	}
}

// TestPrivateAccountsStayInvisible. The advisor is a household-shared surface:
// a member's private account must change neither the slack figure nor any
// option. Anything else leaks one member's money to the rest of the household.
func TestPrivateAccountsStayInvisible(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	before, err := Build(ctx, f.q, f.householdID, f.now)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// A private manual debt owned by one member, with an APR that would
	// otherwise sail past the hurdle and top the list.
	private := uuid.New()
	f.exec(t, `INSERT INTO accounts
	             (id, plaid_item_id, plaid_account_id, name, type, current_balance,
	              source, user_id, is_shared, household_id)
	           VALUES ($1, NULL, NULL, 'Secret card', 'credit', '8000.00',
	                   'manual', $2, FALSE, $3)`, private, f.userID, f.householdID)
	f.exec(t, `INSERT INTO account_terms (account_id, apr, minimum_payment, updated_by)
	           VALUES ($1, '27.99', '200.00', $2)`, private, f.userID)

	after, err := Build(ctx, f.q, f.householdID, f.now)
	if err != nil {
		t.Fatalf("Build (with private account): %v", err)
	}

	if !after.Slack.Equal(before.Slack) {
		t.Errorf("a private account moved slack from %s to %s", before.Slack, after.Slack)
	}
	for _, o := range after.Options {
		if o.SubjectID == private.String() {
			t.Errorf("a private account produced a household-visible option: %q", o.Label)
		}
	}
	if len(after.Options) != len(before.Options) {
		t.Errorf("option count changed from %d to %d because of a private account",
			len(before.Options), len(after.Options))
	}
}

// TestZeroSlackProducesNothing. Never a "you have -$300 to allocate" suggestion.
func TestZeroSlackProducesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Budget the household well past its income so slack goes negative.
	var discCat uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM categories WHERE household_id = $1 AND slug = 'dining'`,
		f.householdID).Scan(&discCat); err != nil {
		t.Fatalf("find dining category: %v", err)
	}
	if _, err := f.q.UpsertBudget(ctx, dbgen.UpsertBudgetParams{
		HouseholdID: f.householdID, CategoryID: discCat,
		Amount: decimal.RequireFromString("9000.00"), Period: "monthly",
	}); err != nil {
		t.Fatalf("UpsertBudget: %v", err)
	}

	adv, err := Build(ctx, f.q, f.householdID, f.now)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if adv.Slack.IsPositive() {
		t.Fatalf("fixture drifted: want non-positive slack, got %s", adv.Slack)
	}
	if adv.Significant {
		t.Error("negative slack must not be significant")
	}
	if len(adv.Options) != 0 {
		t.Errorf("want no options at negative slack, got %d", len(adv.Options))
	}
}

// TestSlackBelowThresholdIsSilent. An advisor that fires on $12 trains the user
// to ignore it, so the threshold gates the whole run.
func TestSlackBelowThresholdIsSilent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Default threshold is $100; this fixture's slack is $3,600. Raise the bar
	// past it through the same preference the settings UI writes.
	hh := f.householdID
	if _, err := f.q.UpsertHouseholdPreference(ctx, dbgen.UpsertHouseholdPreferenceParams{
		HouseholdID: &hh, Key: "advisor.slack_threshold", Value: []byte(`5000`),
	}); err != nil {
		t.Fatalf("UpsertHouseholdPreference: %v", err)
	}

	adv, err := Build(ctx, f.q, f.householdID, f.now)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if adv.Significant || len(adv.Options) != 0 {
		t.Errorf("want silence below the threshold, got %d options", len(adv.Options))
	}
	if !adv.Threshold.Equal(decimal.NewFromInt(5000)) {
		t.Errorf("threshold = %s, want 5000", adv.Threshold)
	}
}

// TestSuppressionSurvivesARun: a key muted through preferences is absent from
// the next run, and dismissing an insight is not what did it.
func TestSuppressionSurvivesARun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A shared card that would otherwise rank. A manual account always has an
	// OWNER (the accounts_manual_owner check requires user_id NOT NULL); the
	// household sees it because is_shared is TRUE, not because it has no owner.
	card := uuid.New()
	f.exec(t, `INSERT INTO accounts
	             (id, plaid_item_id, plaid_account_id, name, type, current_balance,
	              source, user_id, is_shared, household_id)
	           VALUES ($1, NULL, NULL, 'Joint card', 'credit', '5000.00',
	                   'manual', $2, TRUE, $3)`, card, f.userID, f.householdID)
	f.exec(t, `INSERT INTO account_terms (account_id, apr, minimum_payment, updated_by)
	           VALUES ($1, '22.00', '150.00', $2)`, card, f.userID)

	adv, err := Build(ctx, f.q, f.householdID, f.now)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	key := OptionKey(KindDebtPaydown, card.String())
	if !hasKey(adv.Options, key) {
		t.Fatalf("fixture drifted: want the card ranked, got %d options", len(adv.Options))
	}

	hh := f.householdID
	if _, err := f.q.UpsertHouseholdPreference(ctx, dbgen.UpsertHouseholdPreferenceParams{
		HouseholdID: &hh, Key: "advisor.suppressed_options",
		Value: []byte(`["` + key + `"]`),
	}); err != nil {
		t.Fatalf("UpsertHouseholdPreference: %v", err)
	}

	adv2, err := Build(ctx, f.q, f.householdID, f.now)
	if err != nil {
		t.Fatalf("Build (suppressed): %v", err)
	}
	if hasKey(adv2.Options, key) {
		t.Error("want the suppressed option absent from the next run")
	}
	if len(adv2.Suppressed) != 1 || adv2.Suppressed[0] != key {
		t.Errorf("suppressed list = %v, want [%s]", adv2.Suppressed, key)
	}
}

func hasKey(opts []Option, key string) bool {
	for _, o := range opts {
		if o.Key == key {
			return true
		}
	}
	return false
}
