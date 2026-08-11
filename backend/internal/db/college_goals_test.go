package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// ListCollegeGoals resolves the enrollment horizon from the beneficiary's
// birthdate. That birthdate can come from two links: the goal's own person_id,
// or — when the goal is scoped 'household' and so cannot carry a person_id — the
// linked account's beneficiary_person_id.
//
// These tests pin the second link, which is the one a household actually sets:
// they tag the 529 with the child as beneficiary, create a household college
// goal, and never touch the goal's person scope. Before the fix, that goal was
// reported unprojectable ("beneficiary's age is not on file") even though the
// birthdate was on file and correctly linked to the 529 — because the query
// only read goals.person_id.

// collegeGoalFixture is a peopleFixture plus a 529-style investment account the
// caller can tag a beneficiary onto.
type collegeGoalFixture struct {
	*peopleFixture
	ctx       context.Context
	accountID uuid.UUID
}

func newCollegeGoalFixture(t *testing.T) *collegeGoalFixture {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	pf := newPeopleFixture(t)

	ctx := context.Background()
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}

	itemID := uuid.New()
	accountID := uuid.New()
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, institution_name, is_shared)
	      VALUES ($1,$2,$3,'\x00','Test Brokerage',TRUE)`, itemID, pf.ownerID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1,$2,$3,'Hazel 529','investment','529',20000.00)`,
		accountID, itemID, accountID.String())

	return &collegeGoalFixture{peopleFixture: pf, ctx: ctx, accountID: accountID}
}

func int16ptr(v int16) *int16 { return &v }

// THE REGRESSION: a household-scoped college goal (person_id forced NULL by the
// goals_scope_target CHECK) must still resolve the beneficiary's birthdate
// through the linked account's beneficiary_person_id. This is the exact prod
// shape: the 529 is tagged with the child, the goal is a household goal, and
// there is no account_contributions row.
func TestListCollegeGoalsResolvesBirthdateViaAccountBeneficiary(t *testing.T) {
	f := newCollegeGoalFixture(t)

	if _, err := f.q.SetAccountBeneficiary(f.ctx, dbgen.SetAccountBeneficiaryParams{
		ID: f.accountID, HouseholdID: f.householdID, PersonID: &f.kidPerson,
	}); err != nil {
		t.Fatalf("set account beneficiary: %v", err)
	}

	// Household-scoped: person_id is NULL and must stay NULL (the CHECK enforces
	// it). This is the goal shape that produced the bug.
	if _, err := f.q.CreateGoal(f.ctx, dbgen.CreateGoalParams{
		HouseholdID:  f.householdID,
		Scope:        "household",
		Kind:         "college",
		Name:         "Get Hazel in College",
		TargetAmount: decimal.RequireFromString("20000.00"),
		AccountID:    &f.accountID,
		CollegeYears: int16ptr(4),
	}); err != nil {
		t.Fatalf("create household college goal: %v", err)
	}

	rows, err := f.q.ListCollegeGoals(f.ctx, f.householdID)
	if err != nil {
		t.Fatalf("list college goals: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("college goals = %d, want 1", len(rows))
	}
	if rows[0].BeneficiaryBirthdate == nil {
		t.Fatal("beneficiary birthdate is nil — the account's beneficiary link was not consulted")
	}
	want := time.Date(2020, 11, 20, 0, 0, 0, 0, time.UTC)
	if !rows[0].BeneficiaryBirthdate.Equal(want) {
		t.Errorf("birthdate = %s, want the 529 beneficiary's %s",
			rows[0].BeneficiaryBirthdate.Format(time.DateOnly), want.Format(time.DateOnly))
	}
}

// PRECEDENCE: when BOTH links exist, the goal's own person_id wins. A goal set
// up for one child against an account whose beneficiary is a different child
// must project the GOAL's child, not the account's.
func TestListCollegeGoalsGoalPersonWinsOverAccountBeneficiary(t *testing.T) {
	f := newCollegeGoalFixture(t)

	// The account's beneficiary is the kid (Ellie, 2020-11-20).
	if _, err := f.q.SetAccountBeneficiary(f.ctx, dbgen.SetAccountBeneficiaryParams{
		ID: f.accountID, HouseholdID: f.householdID, PersonID: &f.kidPerson,
	}); err != nil {
		t.Fatalf("set account beneficiary: %v", err)
	}

	// The goal's person is the teen (Robin, 2010-03-01) — a different person,
	// with a different birthdate. scope 'person' carries the person_id.
	if _, err := f.q.CreateGoal(f.ctx, dbgen.CreateGoalParams{
		HouseholdID:  f.householdID,
		Scope:        "person",
		PersonID:     &f.teenPerson,
		Kind:         "college",
		Name:         "Robin college",
		TargetAmount: decimal.RequireFromString("20000.00"),
		AccountID:    &f.accountID,
		CollegeYears: int16ptr(4),
	}); err != nil {
		t.Fatalf("create person-scoped college goal: %v", err)
	}

	rows, err := f.q.ListCollegeGoals(f.ctx, f.householdID)
	if err != nil {
		t.Fatalf("list college goals: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("college goals = %d, want 1", len(rows))
	}
	if rows[0].BeneficiaryBirthdate == nil {
		t.Fatal("beneficiary birthdate is nil")
	}
	want := time.Date(2010, 3, 1, 0, 0, 0, 0, time.UTC)
	if !rows[0].BeneficiaryBirthdate.Equal(want) {
		t.Errorf("birthdate = %s, want the goal person's %s (teen), not the account beneficiary's (kid)",
			rows[0].BeneficiaryBirthdate.Format(time.DateOnly), want.Format(time.DateOnly))
	}
}

// NEGATIVE CONTROL: with no person link anywhere — not on the goal, not on the
// account, no contribution plan — the birthdate stays nil. The fix adds a
// fallback link, not a guess.
func TestListCollegeGoalsBirthdateNilWhenNoLink(t *testing.T) {
	f := newCollegeGoalFixture(t)

	// No beneficiary set on the account. Household goal carries no person_id.
	if _, err := f.q.CreateGoal(f.ctx, dbgen.CreateGoalParams{
		HouseholdID:  f.householdID,
		Scope:        "household",
		Kind:         "college",
		Name:         "Unlinked college",
		TargetAmount: decimal.RequireFromString("20000.00"),
		AccountID:    &f.accountID,
		CollegeYears: int16ptr(4),
	}); err != nil {
		t.Fatalf("create college goal: %v", err)
	}

	rows, err := f.q.ListCollegeGoals(f.ctx, f.householdID)
	if err != nil {
		t.Fatalf("list college goals: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("college goals = %d, want 1", len(rows))
	}
	if rows[0].BeneficiaryBirthdate != nil {
		t.Errorf("birthdate = %s, want nil when no beneficiary link exists",
			rows[0].BeneficiaryBirthdate.Format(time.DateOnly))
	}
}

// The per-account assumed real return round-trips through UpsertAccountContribution
// and back out through ListProjectableAccounts. NULL means "use the household
// rate" and must survive as a null scan, not collapse to zero — a 0% real return
// and "not set" are opposite inputs to the projection.
func TestAccountContributionAssumedRealReturnRoundTrip(t *testing.T) {
	f := newCollegeGoalFixture(t)

	// Set a 6% real return on the 529's contribution plan.
	if _, err := f.q.UpsertAccountContribution(f.ctx, dbgen.UpsertAccountContributionParams{
		AccountID:           f.accountID,
		HouseholdID:         f.householdID,
		UserID:              f.ownerID,
		MonthlyContribution: decimal.RequireFromString("200.00"),
		AssumedRealReturn:   decimal.NewNullDecimal(decimal.RequireFromString("0.0600")),
	}); err != nil {
		t.Fatalf("upsert contribution with return rate: %v", err)
	}

	rows, err := f.q.ListProjectableAccounts(f.ctx, dbgen.ListProjectableAccountsParams{
		HouseholdID: f.householdID, UserID: f.ownerID,
	})
	if err != nil {
		t.Fatalf("list projectable accounts: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.ID == f.accountID {
			found = true
			if !r.AssumedRealReturn.Valid {
				t.Fatal("assumed_real_return scanned as null after being set to 0.06")
			}
			if want := decimal.RequireFromString("0.06"); !r.AssumedRealReturn.Decimal.Equal(want) {
				t.Errorf("assumed_real_return = %s, want %s", r.AssumedRealReturn.Decimal, want)
			}
		}
	}
	if !found {
		t.Fatal("529 not returned by ListProjectableAccounts")
	}

	// Clearing it back to null must round-trip as null, not zero. A null means
	// "use the household rate"; a zero would mean "this account earns nothing".
	if _, err := f.q.UpsertAccountContribution(f.ctx, dbgen.UpsertAccountContributionParams{
		AccountID:           f.accountID,
		HouseholdID:         f.householdID,
		UserID:              f.ownerID,
		MonthlyContribution: decimal.RequireFromString("200.00"),
		AssumedRealReturn:   decimal.NullDecimal{}, // explicitly unset
	}); err != nil {
		t.Fatalf("upsert contribution clearing return rate: %v", err)
	}
	rows, _ = f.q.ListProjectableAccounts(f.ctx, dbgen.ListProjectableAccountsParams{
		HouseholdID: f.householdID, UserID: f.ownerID,
	})
	for _, r := range rows {
		if r.ID == f.accountID && r.AssumedRealReturn.Valid {
			t.Errorf("assumed_real_return = %s after clearing, want null (use household rate)",
				r.AssumedRealReturn.Decimal)
		}
	}
}
