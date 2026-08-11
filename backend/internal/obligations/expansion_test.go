package obligations

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// testFixture is one household with a member, an institution and a checking
// account — the minimum an obligation needs to exist and be projected.
type testFixture struct {
	pool        *pgxpool.Pool
	q           *dbgen.Queries
	householdID uuid.UUID
	userID      uuid.UUID
	accountID   uuid.UUID
	exec        func(sql string, args ...any)
}

// newFixture connects to the throwaway Postgres, migrates, and seeds a
// household. Skips when TEST_DATABASE_URL is unset:
//
//	docker run -d --name lmtest-pg -e POSTGRES_PASSWORD=test \
//	  -e POSTGRES_DB=lmtest -p 55432:5432 postgres:17-alpine
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' \
//	  go test -p 1 ./internal/obligations/
func newFixture(t *testing.T, name string) *testFixture {
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

	f := &testFixture{
		pool:        pool,
		q:           dbgen.New(pool),
		householdID: uuid.New(),
		userID:      uuid.New(),
		accountID:   uuid.New(),
	}
	f.exec = func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}

	itemID := uuid.New()
	f.exec(`INSERT INTO households (id, name) VALUES ($1, $2)`, f.householdID, name)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM households WHERE id = $1`, f.householdID)
	})
	f.exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	        VALUES ($1, $2, $3, 'x', 'Tester')`,
		f.userID, f.householdID, f.userID.String()+"@example.test")
	f.exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	        VALUES ($1, $2, $3, '\x00', '{transactions}', 'active')`,
		itemID, f.userID, itemID.String())
	f.exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, current_balance)
	        VALUES ($1, $2, $3, 'Checking', 'depository', '2000.00')`,
		f.accountID, itemID, f.accountID.String())
	return f
}

// addObligation inserts one manual obligation and returns its id.
func (f *testFixture) addObligation(t *testing.T, label, amount string, count int, unit, anchor string, end *string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO recurring_obligations
	        (id, household_id, user_id, label, amount, interval_count, interval_unit, anchor_date, end_date, account_id)
	        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		id, f.householdID, f.userID, label, amount, count, unit, anchor, end, f.accountID)
	return id
}

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatalf("parse date %q: %v", s, err)
	}
	return d
}

// TestListUpcomingCadences is the heart of the bill calendar. Every case here is
// a date-arithmetic trap that a naive implementation gets wrong:
//
//   - Monthly from the 31st must CLAMP to the shortest month and then return to
//     the 31st, not drift a day earlier every cycle. Both a leap February and a
//     common one are covered, because 29 vs. 28 is its own off-by-one.
//   - Every-2-weeks must stay on the same weekday across a DST boundary. The
//     expansion is in DATE arithmetic precisely so a 23-hour civil day cannot
//     shift an occurrence.
//   - Every 2 years must land on the same calendar day two years out.
//   - end_date must cut the series mid-window rather than at a period boundary.
//   - A window entirely before anchor_date must yield nothing, not the anchor.
func TestListUpcomingCadences(t *testing.T) {
	f := newFixture(t, "Cadence expansion")
	ctx := context.Background()

	cases := []struct {
		name      string
		amount    string
		count     int
		unit      string
		anchor    string
		endDate   *string
		from, to  string
		wantDates []string
	}{
		{
			name:   "monthly from the 31st clamps and recovers (leap year)",
			amount: "100.00", count: 1, unit: "month", anchor: "2024-01-31",
			from: "2024-01-01", to: "2024-05-31",
			// February 2024 has 29 days, and March is back on the 31st.
			wantDates: []string{"2024-01-31", "2024-02-29", "2024-03-31", "2024-04-30", "2024-05-31"},
		},
		{
			name:   "monthly from the 31st clamps and recovers (common year)",
			amount: "100.00", count: 1, unit: "month", anchor: "2025-01-31",
			from: "2025-01-01", to: "2025-05-31",
			wantDates: []string{"2025-01-31", "2025-02-28", "2025-03-31", "2025-04-30", "2025-05-31"},
		},
		{
			name:   "monthly from the 30th skips no month in February",
			amount: "100.00", count: 1, unit: "month", anchor: "2025-01-30",
			from: "2025-02-01", to: "2025-03-31",
			wantDates: []string{"2025-02-28", "2025-03-30"},
		},
		{
			name:   "every 2 weeks holds its weekday across the spring DST boundary",
			amount: "50.00", count: 2, unit: "week", anchor: "2025-02-28",
			// US DST starts 2025-03-09; every date below is a Friday.
			from: "2025-02-01", to: "2025-04-30",
			wantDates: []string{"2025-02-28", "2025-03-14", "2025-03-28", "2025-04-11", "2025-04-25"},
		},
		{
			name:   "every 2 weeks holds its weekday across the autumn DST boundary",
			amount: "50.00", count: 2, unit: "week", anchor: "2025-10-24",
			// US DST ends 2025-11-02; every date below is a Friday.
			from: "2025-10-01", to: "2025-12-15",
			wantDates: []string{"2025-10-24", "2025-11-07", "2025-11-21", "2025-12-05"},
		},
		{
			name:   "every 2 years",
			amount: "900.00", count: 2, unit: "year", anchor: "2020-06-15",
			from: "2021-01-01", to: "2027-01-01",
			wantDates: []string{"2022-06-15", "2024-06-15", "2026-06-15"},
		},
		{
			name:   "yearly on 29 February clamps in common years",
			amount: "400.00", count: 1, unit: "year", anchor: "2024-02-29",
			from: "2024-01-01", to: "2028-12-31",
			wantDates: []string{"2024-02-29", "2025-02-28", "2026-02-28", "2027-02-28", "2028-02-29"},
		},
		{
			name:   "end_date cuts the series mid-window",
			amount: "75.00", count: 1, unit: "month", anchor: "2026-01-15",
			endDate: ptr("2026-03-20"),
			from:    "2026-01-01", to: "2026-06-30",
			wantDates: []string{"2026-01-15", "2026-02-15", "2026-03-15"},
		},
		{
			name:   "a window entirely before the anchor yields nothing",
			amount: "75.00", count: 1, unit: "month", anchor: "2026-06-01",
			from: "2026-01-01", to: "2026-05-31",
			wantDates: nil,
		},
		{
			name:   "a window starting mid-cadence skips already-past occurrences",
			amount: "20.00", count: 10, unit: "day", anchor: "2026-01-01",
			from: "2026-02-01", to: "2026-02-28",
			wantDates: []string{"2026-02-10", "2026-02-20"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := f.addObligation(t, tc.name, tc.amount, tc.count, tc.unit, tc.anchor, tc.endDate)
			t.Cleanup(func() {
				_, _ = f.pool.Exec(context.Background(),
					`DELETE FROM recurring_obligations WHERE id = $1`, id)
			})

			got, err := ListUpcoming(ctx, f.q, f.householdID, f.userID,
				mustDate(t, tc.from), mustDate(t, tc.to))
			if err != nil {
				t.Fatalf("ListUpcoming: %v", err)
			}

			dates := make([]string, 0, len(got))
			for _, o := range got {
				if o.ObligationID != id {
					continue // another case's row, cleaned up but concurrent-safe
				}
				dates = append(dates, o.DueDate.Format(time.DateOnly))
			}
			if len(dates) != len(tc.wantDates) {
				t.Fatalf("got %v, want %v", dates, tc.wantDates)
			}
			for i := range dates {
				if dates[i] != tc.wantDates[i] {
					t.Fatalf("got %v, want %v", dates, tc.wantDates)
				}
			}
		})
	}
}

// TestListUpcomingVisibility checks the scoping every household read shares: a
// member's private obligation must not reach the household view, and a
// deactivated one must not reach any view.
func TestListUpcomingVisibility(t *testing.T) {
	f := newFixture(t, "Obligation visibility")
	ctx := context.Background()

	// A second member with a private obligation.
	otherID := uuid.New()
	f.exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	        VALUES ($1, $2, $3, 'x', 'Other')`,
		otherID, f.householdID, otherID.String()+"@example.test")
	f.exec(`INSERT INTO recurring_obligations
	        (household_id, user_id, is_shared, label, amount, interval_count, interval_unit, anchor_date)
	        VALUES ($1, $2, FALSE, 'Private gym', '40.00', 1, 'month', '2026-07-05')`,
		f.householdID, otherID)
	f.addObligation(t, "Shared rent", "1200.00", 1, "month", "2026-07-01", nil)
	f.exec(`INSERT INTO recurring_obligations
	        (household_id, user_id, label, amount, interval_count, interval_unit, anchor_date, is_active)
	        VALUES ($1, $2, 'Cancelled', '15.00', 1, 'month', '2026-07-03', FALSE)`,
		f.householdID, f.userID)

	from, to := mustDate(t, "2026-07-01"), mustDate(t, "2026-07-31")

	labels := func(userID uuid.UUID) []string {
		t.Helper()
		got, err := ListUpcoming(ctx, f.q, f.householdID, userID, from, to)
		if err != nil {
			t.Fatalf("ListUpcoming: %v", err)
		}
		out := make([]string, 0, len(got))
		for _, o := range got {
			out = append(out, o.Label)
		}
		return out
	}

	// The household-shared view (uuid.Nil) is what safe-to-spend and the insight
	// feed use, so a private obligation leaking here would leak into both.
	shared := labels(uuid.Nil)
	if len(shared) != 1 || shared[0] != "Shared rent" {
		t.Errorf("household-shared view = %v, want just [Shared rent]", shared)
	}

	// The owner sees their own private row on top of the shared one.
	own := labels(otherID)
	if len(own) != 2 {
		t.Errorf("owner view = %v, want the shared and the private obligation", own)
	}

	// The deactivated row appears in nobody's expansion.
	for _, l := range append(shared, own...) {
		if l == "Cancelled" {
			t.Error("a deactivated obligation was expanded")
		}
	}
}

// TestNextDue checks the derived next-due date, including the case the search
// window exists for: a cadence far longer than the default horizon.
func TestNextDue(t *testing.T) {
	f := newFixture(t, "Next due")
	ctx := context.Background()

	monthly := f.addObligation(t, "Rent", "1200.00", 1, "month", "2026-01-31", nil)
	biennial := f.addObligation(t, "Passport", "165.00", 2, "year", "2025-04-02", nil)
	// Anchored well past any period-length window: only the anchor extension in
	// NextDue finds this one.
	future := f.addObligation(t, "Future dues", "80.00", 1, "year", "2030-09-09", nil)
	ended := f.addObligation(t, "Ended", "10.00", 1, "month", "2025-01-01", ptr("2025-06-01"))

	rows, err := f.q.ListObligations(ctx, dbgen.ListObligationsParams{
		HouseholdID: f.householdID, UserID: &f.userID,
	})
	if err != nil {
		t.Fatalf("ListObligations: %v", err)
	}

	now := mustDate(t, "2026-07-15")
	next, err := NextDue(ctx, f.q, f.householdID, f.userID, rows, now)
	if err != nil {
		t.Fatalf("NextDue: %v", err)
	}

	want := map[uuid.UUID]string{
		monthly:  "2026-07-31",
		biennial: "2027-04-02",
		future:   "2030-09-09",
	}
	for id, wantDate := range want {
		got, ok := next[id]
		if !ok {
			t.Errorf("no next due for %s, want %s", id, wantDate)
			continue
		}
		if got.Format(time.DateOnly) != wantDate {
			t.Errorf("next due = %s, want %s", got.Format(time.DateOnly), wantDate)
		}
	}
	// An obligation whose end_date has passed has no next occurrence at all.
	if d, ok := next[ended]; ok {
		t.Errorf("an ended obligation reported a next due of %s", d.Format(time.DateOnly))
	}
}

func ptr(s string) *string { return &s }
