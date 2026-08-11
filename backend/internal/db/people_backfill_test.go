package db

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// The 00034 backfill, verified by replaying its two statements against a
// household seeded to look like a pre-migration one.
//
// The backfill runs once and cannot be re-run against an already-migrated
// database, so this replays the exact statements from the migration inside a
// transaction that is rolled back. If either statement changes, change it here
// too — the point is that these two are the ones that decide whether an
// upgraded install comes up in a sane state.
const (
	backfillPeople = `
		INSERT INTO household_people (household_id, user_id, display_name)
		SELECT household_id, id, display_name FROM users WHERE household_id = $1`

	backfillOwner = `
		UPDATE users u SET role = 'owner'
		WHERE u.household_id = $1 AND u.id = (
			SELECT id FROM users
			WHERE household_id = u.household_id
			ORDER BY created_at ASC, id ASC
			LIMIT 1
		)`
)

func TestMigrationBackfill(t *testing.T) {
	url := testdb.URL(t)

	ctx := context.Background()
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cases := []struct {
		name string
		// users, in insertion order, as (displayName, createdAt).
		users []struct {
			name      string
			createdAt string
		}
		// wantOwner is the display name expected to come out as owner.
		wantOwner string
	}{
		{
			name: "single-user household",
			users: []struct {
				name      string
				createdAt string
			}{{"Solo", "2024-01-01T00:00:00Z"}},
			wantOwner: "Solo",
		},
		{
			name: "the earliest user wins",
			users: []struct {
				name      string
				createdAt string
			}{
				{"Later", "2025-01-01T00:00:00Z"},
				{"Earliest", "2023-01-01T00:00:00Z"},
				{"Middle", "2024-01-01T00:00:00Z"},
			},
			wantOwner: "Earliest",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			householdID := uuid.New()

			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()

			if _, err := tx.Exec(ctx,
				`INSERT INTO households (id, name) VALUES ($1, 'Backfill Test')`,
				householdID); err != nil {
				t.Fatalf("seed household: %v", err)
			}
			for _, u := range tc.users {
				id := uuid.New()
				if _, err := tx.Exec(ctx,
					`INSERT INTO users (id, household_id, email, password_hash, display_name, created_at)
					 VALUES ($1,$2,$3,'h',$4,$5)`,
					id, householdID, id.String()+"@x", u.name, u.createdAt); err != nil {
					t.Fatalf("seed user %s: %v", u.name, err)
				}
			}

			if _, err := tx.Exec(ctx, backfillPeople, householdID); err != nil {
				t.Fatalf("backfill people: %v", err)
			}
			if _, err := tx.Exec(ctx, backfillOwner, householdID); err != nil {
				t.Fatalf("backfill owner: %v", err)
			}

			// One person per user, name carried, birthdate left NULL.
			var people, nullBirthdates int
			if err := tx.QueryRow(ctx, `
				SELECT count(*), count(*) FILTER (WHERE birthdate IS NULL)
				FROM household_people WHERE household_id = $1`,
				householdID).Scan(&people, &nullBirthdates); err != nil {
				t.Fatalf("count people: %v", err)
			}
			if people != len(tc.users) {
				t.Errorf("got %d people, want %d (one per user)", people, len(tc.users))
			}
			// The non-regression guarantee: a fabricated birthdate here would
			// change every projection on the day of the upgrade.
			if nullBirthdates != people {
				t.Errorf("%d of %d people got a birthdate; the backfill must invent none",
					people-nullBirthdates, people)
			}

			var linked int
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM household_people p
				JOIN users u ON u.id = p.user_id
				WHERE p.household_id = $1 AND u.display_name = p.display_name`,
				householdID).Scan(&linked); err != nil {
				t.Fatalf("count linked: %v", err)
			}
			if linked != len(tc.users) {
				t.Errorf("%d people linked to their user with a matching name, want %d",
					linked, len(tc.users))
			}

			// Exactly one owner, and it is the right one.
			var owners int
			var ownerName string
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM users WHERE household_id = $1 AND role = 'owner'`,
				householdID).Scan(&owners); err != nil {
				t.Fatalf("count owners: %v", err)
			}
			if owners != 1 {
				t.Fatalf("got %d owners, want exactly 1", owners)
			}
			if err := tx.QueryRow(ctx, `
				SELECT display_name FROM users WHERE household_id = $1 AND role = 'owner'`,
				householdID).Scan(&ownerName); err != nil {
				t.Fatalf("owner name: %v", err)
			}
			if ownerName != tc.wantOwner {
				t.Errorf("owner = %s, want %s", ownerName, tc.wantOwner)
			}
		})
	}
}

// TestBackfillWithIdenticalCreatedAt is the tiebreaker case the doc calls out.
// Without the `id` term in the ORDER BY, two users sharing a created_at make
// the choice non-deterministic — and a household could migrate with two owners
// or, worse, a different owner on every replica.
func TestBackfillWithIdenticalCreatedAt(t *testing.T) {
	url := testdb.URL(t)

	ctx := context.Background()
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Run the same scenario repeatedly: a stable result across runs is the
	// property being asserted.
	var firstOwner string
	for run := range 5 {
		householdID := uuid.New()
		// Fixed ids so the id tiebreaker has a known answer: "aaaa..." sorts
		// before "bbbb...".
		idA := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
		idB := uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002")

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO households (id, name) VALUES ($1, 'Tie Test')`, householdID); err != nil {
			t.Fatalf("seed household: %v", err)
		}
		// Inserted B first, so insertion order does not accidentally agree
		// with the expected answer.
		for _, u := range []struct {
			id   uuid.UUID
			name string
		}{{idB, "Bee"}, {idA, "Ay"}} {
			if _, err := tx.Exec(ctx,
				`INSERT INTO users (id, household_id, email, password_hash, display_name, created_at)
				 VALUES ($1,$2,$3,'h',$4,'2024-01-01T00:00:00Z')`,
				u.id, householdID, u.id.String()+"@x", u.name); err != nil {
				t.Fatalf("seed user: %v", err)
			}
		}

		if _, err := tx.Exec(ctx, backfillOwner, householdID); err != nil {
			t.Fatalf("backfill owner: %v", err)
		}

		var owners int
		var ownerName string
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM users WHERE household_id = $1 AND role = 'owner'`,
			householdID).Scan(&owners); err != nil {
			t.Fatalf("count owners: %v", err)
		}
		if owners != 1 {
			t.Fatalf("run %d: got %d owners, want exactly 1", run, owners)
		}
		if err := tx.QueryRow(ctx,
			`SELECT display_name FROM users WHERE household_id = $1 AND role = 'owner'`,
			householdID).Scan(&ownerName); err != nil {
			t.Fatalf("owner name: %v", err)
		}
		if ownerName != "Ay" {
			t.Errorf("run %d: owner = %s, want Ay (the lower id)", run, ownerName)
		}
		if run == 0 {
			firstOwner = ownerName
		} else if ownerName != firstOwner {
			t.Fatalf("run %d picked %s but run 0 picked %s; the backfill is not deterministic",
				run, ownerName, firstOwner)
		}

		_ = tx.Rollback(ctx)
	}
}
