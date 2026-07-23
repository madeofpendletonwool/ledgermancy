package insights

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/apex42group/ledgermancy/backend/internal/ai"
	"github.com/apex42group/ledgermancy/backend/internal/config"
	"github.com/apex42group/ledgermancy/backend/internal/db"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// TestGenerateDetectsAndDedupes drives the whole engine against a real Postgres:
// deterministic detectors must raise rows with AI disabled, a second run must
// upsert rather than duplicate, and a user's dismissal must survive a re-run.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/insights/
func TestGenerateDetectsAndDedupes(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	q := dbgen.New(pool)

	householdID := uuid.New()
	userID := uuid.New()
	itemID := uuid.New()
	acctID := uuid.New()
	diningID := uuid.New()
	incomeID := uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Insights Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	// is_shared defaults TRUE, which is what the household-wide detectors read.
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`, itemID, userID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
	      VALUES ($1, $2, $3, 'Checking', 'depository')`, acctID, itemID, acctID.String())
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Dining', 'dining')`, diningID, householdID)
	exec(`INSERT INTO categories (id, household_id, name, slug, is_income)
	      VALUES ($1, $2, 'Paycheck', 'paycheck', TRUE)`, incomeID, householdID)

	// merchant_key NULL keeps these out of the recurring detector, so the test
	// isolates the spike and leftover producers.
	addSpend := func(amount, date string, cat uuid.UUID) {
		exec(`INSERT INTO transactions
		        (account_id, amount, currency, date, name, category_id, source)
		      VALUES ($1, $2, 'USD', $3, 'seed', $4, 'plaid')`, acctID, amount, date, cat)
	}

	// Six prior months of modest dining (~$40/mo) set the trailing baseline.
	for _, d := range []string{
		"2026-01-15", "2026-02-15", "2026-03-15",
		"2026-04-15", "2026-05-15", "2026-06-15",
	} {
		addSpend("40.00", d, diningID)
	}
	// This month spikes well past 1.5× the baseline and past the dollar floor.
	addSpend("300.00", "2026-07-05", diningID)
	// Income landed but is dwarfed by spend, so leftover goes negative.
	addSpend("-100.00", "2026-07-01", incomeID)

	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	aiOff := ai.New(config.AIConfig{}) // no key → phrasing is skipped, templates used

	// First run.
	if _, err := Generate(ctx, q, aiOff, householdID, now); err != nil {
		t.Fatalf("Generate (1st): %v", err)
	}

	byKind := func() map[string]dbgen.Insight {
		rows, err := q.ListInsights(ctx, dbgen.ListInsightsParams{HouseholdID: householdID})
		if err != nil {
			t.Fatalf("ListInsights: %v", err)
		}
		out := map[string]dbgen.Insight{}
		for _, r := range rows {
			out[r.Kind] = r
		}
		return out
	}

	first := byKind()
	spike, ok := first["spending_spike"]
	if !ok {
		t.Fatalf("expected a spending_spike insight, got kinds %v", keys(first))
	}
	// The narrative must carry the real, finished numbers — never recomputed.
	if got := factString(t, spike, "this_month"); got != "300.00" {
		t.Errorf("spike this_month = %q, want 300.00", got)
	}
	if _, ok := first["low_leftover"]; !ok {
		t.Errorf("expected a low_leftover insight, got kinds %v", keys(first))
	}
	firstCount := len(first)

	// Second run must upsert, not duplicate.
	if _, err := Generate(ctx, q, aiOff, householdID, now); err != nil {
		t.Fatalf("Generate (2nd): %v", err)
	}
	if second := byKind(); len(second) != firstCount {
		t.Errorf("insight count changed on re-run: %d → %d (upsert should dedupe)", firstCount, len(second))
	}

	// Dismissal must stick across a re-run.
	if err := q.DismissInsight(ctx, dbgen.DismissInsightParams{ID: spike.ID, HouseholdID: householdID}); err != nil {
		t.Fatalf("DismissInsight: %v", err)
	}
	if _, err := Generate(ctx, q, aiOff, householdID, now); err != nil {
		t.Fatalf("Generate (3rd): %v", err)
	}
	// Gone from the unread feed…
	if _, ok := byKind()["spending_spike"]; ok {
		t.Errorf("dismissed spending_spike reappeared in the unread feed")
	}
	// …but still present (and still dismissed) when dismissed rows are included.
	all, err := q.ListInsights(ctx, dbgen.ListInsightsParams{HouseholdID: householdID, IncludeDismissed: true})
	if err != nil {
		t.Fatalf("ListInsights (all): %v", err)
	}
	var found bool
	for _, r := range all {
		if r.Kind == "spending_spike" {
			found = true
			if r.DismissedAt == nil {
				t.Errorf("spending_spike lost its dismissal on re-run")
			}
		}
	}
	if !found {
		t.Errorf("dismissed spending_spike vanished entirely")
	}
}

func keys(m map[string]dbgen.Insight) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func factString(t *testing.T, row dbgen.Insight, key string) string {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(row.Data, &data); err != nil {
		t.Fatalf("unmarshal insight data: %v", err)
	}
	s, _ := data[key].(string)
	return s
}
