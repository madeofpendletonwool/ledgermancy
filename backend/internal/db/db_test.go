package db

import (
	"context"
	"os"
	"testing"

	"github.com/shopspring/decimal"
)

// TestMigrate applies the full migration set against a real Postgres. It is
// skipped unless TEST_DATABASE_URL points at a throwaway database, since it
// mutates schema.
//
//	docker run -d --rm --name lm-test -e POSTGRES_PASSWORD=test \
//	  -e POSTGRES_DB=lmtest -p 55432:5432 postgres:17-alpine
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/db/
func TestMigrate(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Re-running must be a no-op, not an error — the api calls this on every
	// boot, including restarts against an already-migrated database.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate is not idempotent: %v", err)
	}

	// Spot-check that the tables the rest of the app depends on exist.
	for _, table := range []string{
		"households", "users", "sessions", "plaid_items", "accounts",
		"categories", "transactions", "category_rules",
		"merchant_category_map", "budgets", "alerts", "alert_events",
	} {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			                WHERE table_schema = 'public' AND table_name = $1)`,
			table).Scan(&exists)
		if err != nil {
			t.Fatalf("check %s: %v", table, err)
		}
		if !exists {
			t.Errorf("expected table %q to exist after migration", table)
		}
	}
}

// TestDecimalRoundTrip is the guard on the app's core correctness rule: money
// must survive a database round trip exactly. If the pgx decimal codec is ever
// dropped from Connect, this fails rather than silently degrading to a float.
func TestDecimalRoundTrip(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// Values chosen to break binary floating point: the classic 0.1+0.2 case,
	// a repeating-decimal third, a large balance, and a negative (income).
	cases := []string{
		"0.1", "0.2", "20.29", "0.3333", "-1234.5678",
		"99999999999999.9999", "0.0001",
	}

	for _, want := range cases {
		in, err := decimal.NewFromString(want)
		if err != nil {
			t.Fatalf("parse %s: %v", want, err)
		}

		var out decimal.Decimal
		if err := pool.QueryRow(ctx, `SELECT $1::numeric(20,4)`, in).Scan(&out); err != nil {
			t.Fatalf("round trip %s: %v", want, err)
		}
		if !in.Equal(out) {
			t.Errorf("value changed in transit: sent %s, got back %s", in, out)
		}
	}

	// Summing in the database must also stay exact.
	var sum decimal.Decimal
	err = pool.QueryRow(ctx,
		`SELECT SUM(v) FROM (VALUES (0.1::numeric), (0.2::numeric)) AS t(v)`).Scan(&sum)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if !sum.Equal(decimal.RequireFromString("0.3")) {
		t.Errorf("0.1 + 0.2 = %s, want exactly 0.3", sum)
	}
}
