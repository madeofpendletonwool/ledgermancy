package jobs

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestDueItemsQuery pins which items a sweep picks up.
//
// The first version of this query filtered on sync staleness alone and merely
// *projected* refresh_due, which made the bank pull unreachable for any item
// that was not simultaneously due for a cache read. That is not a corner case:
// a webhook-driven sync bumps last_synced_at, so an item receiving webhooks
// stays permanently sync-fresh and never gets refreshed at all — the exact
// stall the refresh was added to break. It shipped green because nothing
// exercised the SQL.
//
// Requires a throwaway Postgres:
//
//	docker run -d --name lmtest-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=lmtest \
//	    -p 55432:5432 postgres:17-alpine
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' \
//	    go test ./internal/db/ ./internal/jobs/
func TestDueItemsQuery(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	const (
		staleAfter   = 55 * time.Minute
		refreshAfter = 4 * time.Hour
	)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// Ages are expressed relative to the cutoffs rather than as literals, so
	// the cases stay meaningful if the intervals are ever retuned.
	cases := []struct {
		name        string
		syncedAgo   time.Duration
		refreshAgo  time.Duration // negative means never refreshed
		status      string
		wantPicked  bool
		wantRefresh bool
	}{
		{
			// Production's state at the moment the first version shipped and
			// did nothing: synced half an hour ago, never refreshed.
			name: "sync fresh, never refreshed", syncedAgo: staleAfter / 2, refreshAgo: -1,
			status: "active", wantPicked: true, wantRefresh: true,
		},
		{
			// The starvation case: webhooks keep the sync perpetually fresh.
			name: "sync always fresh, refresh long overdue", syncedAgo: time.Minute, refreshAgo: refreshAfter * 2,
			status: "active", wantPicked: true, wantRefresh: true,
		},
		{
			name: "sync stale, refresh fresh", syncedAgo: staleAfter * 2, refreshAgo: refreshAfter / 2,
			status: "active", wantPicked: true, wantRefresh: false,
		},
		{
			name: "neither due", syncedAgo: staleAfter / 2, refreshAgo: refreshAfter / 2,
			status: "active", wantPicked: false,
		},
		{
			// Everything overdue, but the item needs re-authentication first;
			// syncing or refreshing it would only produce errors.
			name: "inactive is never swept", syncedAgo: staleAfter * 4, refreshAgo: -1,
			status: "login_required", wantPicked: false,
		},
	}

	householdID, userID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO households (id, name) VALUES ($1, 'test')`, householdID); err != nil {
		t.Fatalf("seed household: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, household_id, email, password_hash, display_name)
		 VALUES ($1, $2, $3, 'x', 'test')`,
		userID, householdID, userID.String()+"@example.test"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		// Cascades through plaid_items, so the table is clean for a re-run.
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})

	ids := make(map[uuid.UUID]int, len(cases))
	for i, tc := range cases {
		id := uuid.New()
		ids[id] = i

		var refreshedAt *time.Time
		if tc.refreshAgo >= 0 {
			at := time.Now().Add(-tc.refreshAgo)
			refreshedAt = &at
		}

		if _, err := pool.Exec(ctx,
			`INSERT INTO plaid_items
			   (id, user_id, plaid_item_id, access_token_encrypted, institution_name,
			    products, status, last_synced_at, last_refresh_at)
			 VALUES ($1, $2, $3, '\x00', $4, '{transactions}', $5, $6, $7)`,
			id, userID, id.String(), tc.name, tc.status,
			time.Now().Add(-tc.syncedAgo), refreshedAt); err != nil {
			t.Fatalf("seed %q: %v", tc.name, err)
		}
	}

	now := time.Now()
	rows, err := pool.Query(ctx, dueItemsQuery, now.Add(-staleAfter), now.Add(-refreshAfter))
	if err != nil {
		t.Fatalf("run due-items query: %v", err)
	}
	defer rows.Close()

	picked := make(map[int]bool)
	refresh := make(map[int]bool)
	for rows.Next() {
		var id uuid.UUID
		var refreshDue bool
		if err := rows.Scan(&id, &refreshDue); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if i, ours := ids[id]; ours {
			picked[i] = true
			refresh[i] = refreshDue
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	for i, tc := range cases {
		if picked[i] != tc.wantPicked {
			t.Errorf("%s: swept = %v, want %v", tc.name, picked[i], tc.wantPicked)
			continue
		}
		if tc.wantPicked && refresh[i] != tc.wantRefresh {
			t.Errorf("%s: refresh_due = %v, want %v", tc.name, refresh[i], tc.wantRefresh)
		}
	}
}
