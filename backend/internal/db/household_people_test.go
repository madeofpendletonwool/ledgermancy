package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// Doc 21's verification list, against a real Postgres.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/db/

// peopleFixture is a household with two adults, one child with a login, and one
// child with none — the shape the whole feature exists to support.
type peopleFixture struct {
	ctx         context.Context
	q           *dbgen.Queries
	householdID uuid.UUID
	ownerID     uuid.UUID // adult login
	partnerID   uuid.UUID // adult login
	ownerPerson uuid.UUID
	partnerPers uuid.UUID
	teenPerson  uuid.UUID // has a login
	teenUserID  uuid.UUID
	kidPerson   uuid.UUID // NO login: the case that motivates the whole table
}

func newPeopleFixture(t *testing.T) *peopleFixture {
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

	f := &peopleFixture{
		ctx:         ctx,
		q:           dbgen.New(pool),
		householdID: uuid.New(),
		ownerID:     uuid.New(),
		partnerID:   uuid.New(),
		teenUserID:  uuid.New(),
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}

	exec(`INSERT INTO households (id, name) VALUES ($1, 'People Test')`, f.householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM households WHERE id = $1`, f.householdID)
	})

	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
	      VALUES ($1,$2,$3,'h','Alex','owner')`, f.ownerID, f.householdID, f.ownerID.String()+"@x")
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
	      VALUES ($1,$2,$3,'h','Sam','member')`, f.partnerID, f.householdID, f.partnerID.String()+"@x")
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name, role)
	      VALUES ($1,$2,$3,'h','Robin','child')`, f.teenUserID, f.householdID, f.teenUserID.String()+"@x")

	mk := func(name string, userID *uuid.UUID, birthdate *time.Time, dependent bool) uuid.UUID {
		t.Helper()
		p, err := f.q.CreatePerson(ctx, dbgen.CreatePersonParams{
			HouseholdID: f.householdID, UserID: userID,
			DisplayName: name, Birthdate: birthdate, IsDependent: dependent,
		})
		if err != nil {
			t.Fatalf("create person %s: %v", name, err)
		}
		return p.ID
	}

	adultDOB := time.Date(1985, 6, 15, 0, 0, 0, 0, time.UTC)
	teenDOB := time.Date(2010, 3, 1, 0, 0, 0, 0, time.UTC)
	kidDOB := time.Date(2020, 11, 20, 0, 0, 0, 0, time.UTC)

	f.ownerPerson = mk("Alex", &f.ownerID, &adultDOB, false)
	f.partnerPers = mk("Sam", &f.partnerID, nil, false)
	f.teenPerson = mk("Robin", &f.teenUserID, &teenDOB, true)
	f.kidPerson = mk("Ellie", nil, &kidDOB, true)

	return f
}

// TestPersonWithoutLoginIsFullyUsable is the doc's headline case: a child with
// no credentials must be a first-class subject of the household's money.
//
// If any of this needs a `users` row, the whole design has failed and children
// are back to being restricted logins.
func TestPersonWithoutLoginIsFullyUsable(t *testing.T) {
	f := newPeopleFixture(t)

	person, err := f.q.GetPerson(f.ctx, dbgen.GetPersonParams{
		ID: f.kidPerson, HouseholdID: f.householdID,
	})
	if err != nil {
		t.Fatalf("get person: %v", err)
	}
	if person.UserID != nil {
		t.Fatalf("expected no login, got user_id %v", person.UserID)
	}
	if person.Email != nil || person.Role != nil {
		t.Errorf("a login-less person must have no email or role, got %v / %v",
			person.Email, person.Role)
	}

	// An allowance.
	if _, err := f.q.UpsertAllowance(f.ctx, dbgen.UpsertAllowanceParams{
		PersonID:    f.kidPerson,
		HouseholdID: f.householdID,
		Amount:      decimal.NewNullDecimal(decimal.RequireFromString("5.00")),
		Cadence:     strptr("weekly"),
		AutoPost:    false,
	}); err != nil {
		t.Fatalf("upsert allowance for login-less person: %v", err)
	}

	// Ledger entries.
	for _, e := range []struct {
		kind   string
		amount string
	}{
		{"allowance", "5.00"},
		{"gift", "20.00"},
		{"spend", "-7.50"},
	} {
		if _, err := f.q.CreateAllowanceEntry(f.ctx, dbgen.CreateAllowanceEntryParams{
			PersonID:    f.kidPerson,
			HouseholdID: f.householdID,
			Kind:        e.kind,
			Amount:      decimal.RequireFromString(e.amount),
			OccurredOn:  time.Now(),
		}); err != nil {
			t.Fatalf("create %s entry: %v", e.kind, err)
		}
	}

	balance, err := f.q.GetAllowanceBalance(f.ctx, dbgen.GetAllowanceBalanceParams{
		PersonID: f.kidPerson, HouseholdID: f.householdID,
	})
	if err != nil {
		t.Fatalf("allowance balance: %v", err)
	}
	if got, want := balance.StringFixed(2), "17.50"; got != want {
		t.Errorf("balance = %s, want %s", got, want)
	}

	// A person-scoped goal.
	if _, err := f.q.CreateGoal(f.ctx, dbgen.CreateGoalParams{
		HouseholdID:  f.householdID,
		Scope:        "person",
		PersonID:     &f.kidPerson,
		Kind:         "savings",
		Name:         "New bike",
		TargetAmount: decimal.RequireFromString("120.00"),
	}); err != nil {
		t.Fatalf("create person-scoped goal: %v", err)
	}

	// A manual asset held for them — savings bonds from a grandparent.
	if _, err := f.q.CreateManualAsset(f.ctx, dbgen.CreateManualAssetParams{
		HouseholdID: f.householdID,
		Name:        "Series I bonds",
		Kind:        "other",
		Value:       decimal.RequireFromString("1000.00"),
		PersonID:    &f.kidPerson,
	}); err != nil {
		t.Fatalf("create manual asset: %v", err)
	}

	manual, err := f.q.SumManualAssetsByPerson(f.ctx, f.householdID)
	if err != nil {
		t.Fatalf("manual assets by person: %v", err)
	}
	if len(manual) != 1 || manual[0].PersonID == nil || *manual[0].PersonID != f.kidPerson {
		t.Fatalf("manual asset not attributed to the child: %+v", manual)
	}
	if got, want := manual[0].Total.StringFixed(2), "1000.00"; got != want {
		t.Errorf("manual total = %s, want %s", got, want)
	}
}

// TestGoalScopeVisibility: a child sees their own person-scoped goal and
// nobody else's; an adult sees every person-scoped goal in the household.
func TestGoalScopeVisibility(t *testing.T) {
	f := newPeopleFixture(t)

	mkGoal := func(scope string, userID, personID *uuid.UUID, name string) uuid.UUID {
		t.Helper()
		g, err := f.q.CreateGoal(f.ctx, dbgen.CreateGoalParams{
			HouseholdID: f.householdID, Scope: scope, UserID: userID, PersonID: personID,
			Kind: "savings", Name: name,
			TargetAmount: decimal.RequireFromString("100.00"),
		})
		if err != nil {
			t.Fatalf("create goal %s: %v", name, err)
		}
		return g.ID
	}

	householdGoal := mkGoal("household", nil, nil, "Vacation")
	ownerGoal := mkGoal("user", &f.ownerID, nil, "Alex private")
	teenGoal := mkGoal("person", nil, &f.teenPerson, "Robin bike")
	kidGoal := mkGoal("person", nil, &f.kidPerson, "Ellie lego")

	names := func(rows []dbgen.Goal) map[uuid.UUID]bool {
		out := map[uuid.UUID]bool{}
		for _, g := range rows {
			out[g.ID] = true
		}
		return out
	}

	// The child: their own person goal plus household goals. Not the other
	// child's, and not the adult's private one.
	childRows, err := f.q.ListGoals(f.ctx, dbgen.ListGoalsParams{
		HouseholdID: f.householdID, UserID: &f.teenUserID,
		PersonID: &f.teenPerson, AllPersonGoals: false,
	})
	if err != nil {
		t.Fatalf("list goals as child: %v", err)
	}
	seen := names(childRows)
	if !seen[teenGoal] {
		t.Error("child cannot see their own goal")
	}
	if !seen[householdGoal] {
		t.Error("child cannot see the household goal")
	}
	if seen[kidGoal] {
		t.Error("child can see a sibling's goal")
	}
	if seen[ownerGoal] {
		t.Error("child can see an adult's private goal")
	}

	// The adult: every person goal, plus their own private one.
	adultRows, err := f.q.ListGoals(f.ctx, dbgen.ListGoalsParams{
		HouseholdID: f.householdID, UserID: &f.ownerID,
		PersonID: &f.ownerPerson, AllPersonGoals: true,
	})
	if err != nil {
		t.Fatalf("list goals as adult: %v", err)
	}
	seen = names(adultRows)
	for _, want := range []uuid.UUID{householdGoal, ownerGoal, teenGoal, kidGoal} {
		if !seen[want] {
			t.Errorf("adult cannot see goal %v", want)
		}
	}

	// The other adult must still not see the first adult's private goal.
	partnerRows, err := f.q.ListGoals(f.ctx, dbgen.ListGoalsParams{
		HouseholdID: f.householdID, UserID: &f.partnerID,
		PersonID: &f.partnerPers, AllPersonGoals: true,
	})
	if err != nil {
		t.Fatalf("list goals as partner: %v", err)
	}
	if names(partnerRows)[ownerGoal] {
		t.Error("an adult can see another adult's private goal")
	}
}

// TestDeletingChildLoginKeepsTheirRecords covers the ON DELETE SET NULL choice
// on household_people.user_id. Revoking a teenager's login must not delete the
// person their money points at.
func TestDeletingChildLoginKeepsTheirRecords(t *testing.T) {
	f := newPeopleFixture(t)

	if _, err := f.q.CreateAllowanceEntry(f.ctx, dbgen.CreateAllowanceEntryParams{
		PersonID: f.teenPerson, HouseholdID: f.householdID,
		Kind: "allowance", Amount: decimal.RequireFromString("10.00"),
		OccurredOn: time.Now(),
	}); err != nil {
		t.Fatalf("seed entry: %v", err)
	}
	goal, err := f.q.CreateGoal(f.ctx, dbgen.CreateGoalParams{
		HouseholdID: f.householdID, Scope: "person", PersonID: &f.teenPerson,
		Kind: "savings", Name: "Skateboard",
		TargetAmount: decimal.RequireFromString("80.00"),
	})
	if err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	if err := f.q.DeleteUser(f.ctx, dbgen.DeleteUserParams{
		ID: f.teenUserID, HouseholdID: f.householdID,
	}); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	person, err := f.q.GetPerson(f.ctx, dbgen.GetPersonParams{
		ID: f.teenPerson, HouseholdID: f.householdID,
	})
	if err != nil {
		t.Fatalf("person did not survive login deletion: %v", err)
	}
	if person.UserID != nil {
		t.Errorf("expected user_id cleared, got %v", person.UserID)
	}

	balance, err := f.q.GetAllowanceBalance(f.ctx, dbgen.GetAllowanceBalanceParams{
		PersonID: f.teenPerson, HouseholdID: f.householdID,
	})
	if err != nil {
		t.Fatalf("allowance balance after login deletion: %v", err)
	}
	if got, want := balance.StringFixed(2), "10.00"; got != want {
		t.Errorf("allowance history lost: balance = %s, want %s", got, want)
	}

	if _, err := f.q.GetGoal(f.ctx, dbgen.GetGoalParams{
		ID: goal.ID, HouseholdID: f.householdID, AllPersonGoals: true,
	}); err != nil {
		t.Errorf("person goal lost after login deletion: %v", err)
	}
}

// TestHouseholdScopingOnPersonQueries: a valid id from another household must
// never be addressable.
func TestHouseholdScopingOnPersonQueries(t *testing.T) {
	f := newPeopleFixture(t)
	other := newPeopleFixture(t)

	if _, err := f.q.GetPerson(f.ctx, dbgen.GetPersonParams{
		ID: other.kidPerson, HouseholdID: f.householdID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("read another household's person: err = %v, want ErrNoRows", err)
	}

	// The insert guards resolve the person through the household, so a
	// cross-household write inserts nothing rather than crossing the boundary.
	if _, err := f.q.CreateAllowanceEntry(f.ctx, dbgen.CreateAllowanceEntryParams{
		PersonID: other.kidPerson, HouseholdID: f.householdID,
		Kind: "gift", Amount: decimal.RequireFromString("100.00"),
		OccurredOn: time.Now(),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("wrote an entry into another household: err = %v, want ErrNoRows", err)
	}

	if _, err := f.q.UpdatePerson(f.ctx, dbgen.UpdatePersonParams{
		ID: other.kidPerson, HouseholdID: f.householdID,
		DisplayName: "Hijacked",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("updated another household's person: err = %v, want ErrNoRows", err)
	}
}

func strptr(s string) *string { return &s }
