package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/merchants"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// GET /api/transactions?q=… against a real Postgres.
//
// The parser has its own unit tests; these prove the SQL it emits actually runs,
// that every operator selects what it claims to, and — the one that matters most —
// that a search can only ever narrow what the caller was already allowed to see.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/
type searchFixture struct {
	srv         *Server
	pool        *pgxpool.Pool
	ctx         context.Context
	householdID uuid.UUID
	userID      uuid.UUID
	// The account the visible rows live on, for the account: dimension.
	checkingID uuid.UUID
}

// The fixture's date window. Every transaction sits inside 2026 so the tests can
// pass an explicit from/to and never depend on the wall clock, except where a
// test is specifically about the date grammar's relative forms.
const (
	searchFrom = "2026-01-01"
	searchTo   = "2026-12-31"
)

func setupSearchFixture(t *testing.T) *searchFixture {
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

	f := &searchFixture{
		srv:         &Server{Pool: pool, Queries: dbgen.New(pool)},
		pool:        pool,
		ctx:         ctx,
		householdID: uuid.New(),
		userID:      uuid.New(),
		checkingID:  uuid.New(),
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}

	// --- The household doing the searching -------------------------------
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Search Test')`, f.householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, f.householdID)
	})

	roommateID := uuid.New()
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Searcher')`, f.userID, f.householdID, f.userID.String()+"@example.test")
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Roommate')`, roommateID, f.householdID, roommateID.String()+"@example.test")

	addItem := func(id, owner uuid.UUID, institution string, shared bool) {
		t.Helper()
		exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status, institution_name, is_shared)
		      VALUES ($1, $2, $3, '\x00', '{transactions}', 'active', $4, $5)`,
			id, owner, id.String(), institution, shared)
	}
	addAccount := func(id, itemID uuid.UUID, name string) {
		t.Helper()
		exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type)
		      VALUES ($1, $2, $3, $4, 'depository')`, id, itemID, id.String(), name)
	}

	// Mine: visible because v.user_id matches, even though the item is private.
	myItem := uuid.New()
	addItem(myItem, f.userID, "Ledger Bank", false)
	addAccount(f.checkingID, myItem, "Checking")

	// The roommate's SHARED item: visible to me because is_shared.
	sharedItem, savingsID := uuid.New(), uuid.New()
	addItem(sharedItem, roommateID, "Joint Credit Union", true)
	addAccount(savingsID, sharedItem, "Joint Savings")

	// The roommate's PRIVATE item: never visible to me, no matter what I search.
	privateItem, privateAcctID := uuid.New(), uuid.New()
	addItem(privateItem, roommateID, "Ledger Bank", false)
	addAccount(privateAcctID, privateItem, "Roommate Private")

	// --- Another household entirely --------------------------------------
	otherHouseholdID, otherUserID := uuid.New(), uuid.New()
	otherItem, otherAcctID := uuid.New(), uuid.New()
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Someone Else')`, otherHouseholdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, otherHouseholdID)
	})
	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Stranger')`, otherUserID, otherHouseholdID, otherUserID.String()+"@example.test")
	addItem(otherItem, otherUserID, "Ledger Bank", true)
	addAccount(otherAcctID, otherItem, "Checking")

	// --- Categories -------------------------------------------------------
	coffeeID, groceriesID, payrollID, transferID, fallbackID :=
		uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Coffee', 'coffee')`, coffeeID, f.householdID)
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Groceries', 'groceries')`, groceriesID, f.householdID)
	exec(`INSERT INTO categories (id, household_id, name, slug, is_income) VALUES ($1, $2, 'Paycheck', 'paycheck', TRUE)`, payrollID, f.householdID)
	exec(`INSERT INTO categories (id, household_id, name, slug, is_transfer) VALUES ($1, $2, 'Transfer Out', 'transfer-out', TRUE)`, transferID, f.householdID)
	// The fallback bucket. A row sitting here counts as having NO category, which
	// is what the backlog view means by uncategorised.
	exec(`INSERT INTO categories (id, household_id, name, slug) VALUES ($1, $2, 'Uncategorised', 'uncategorised')`, fallbackID, f.householdID)

	// --- Transactions -----------------------------------------------------
	type tx struct {
		account            uuid.UUID
		date, amount, name string
		merchantName       string
		merchantKey        string
		category           *uuid.UUID
		notes              string
		source             string
		pending            bool
		excluded           bool
		oneTime            bool
		recurring          bool
	}
	add := func(x tx) uuid.UUID {
		t.Helper()
		id := uuid.New()
		source := x.source
		if source == "" {
			source = "plaid"
		}
		var merchant, notes any
		if x.merchantName != "" {
			merchant = x.merchantName
		}
		if x.notes != "" {
			notes = x.notes
		}
		var cat any
		if x.category != nil {
			cat = *x.category
		}
		exec(`INSERT INTO transactions
		        (id, account_id, amount, currency, date, name, merchant_name, merchant_key,
		         category_id, notes, source, pending, excluded_from_reports, is_one_time, is_recurring)
		      VALUES ($1, $2, $3, 'USD', $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			id, x.account, x.amount, x.date, x.name, merchant, x.merchantKey,
			cat, notes, source, x.pending, x.excluded, x.oneTime, x.recurring)
		return id
	}

	add(tx{account: f.checkingID, date: "2026-03-10", amount: "12.50", name: "STARBUCKS STORE 123",
		merchantName: "Starbucks", merchantKey: "starbucks", category: &coffeeID})
	add(tx{account: f.checkingID, date: "2026-01-05", amount: "4.25", name: "STARBUCKS STORE 999",
		merchantName: "Starbucks", merchantKey: "starbucks", category: &coffeeID})
	groceries := add(tx{account: f.checkingID, date: "2026-03-01", amount: "87.10", name: "WHOLE FOODS MKT",
		merchantName: "Whole Foods", merchantKey: "whole foods", category: &groceriesID,
		notes: "weekly shop"})
	add(tx{account: f.checkingID, date: "2026-03-02", amount: "-2500.00", name: "ACME PAYROLL",
		merchantKey: "acme payroll", category: &payrollID, recurring: true})
	add(tx{account: f.checkingID, date: "2026-03-05", amount: "500.00", name: "CARD PAYMENT",
		merchantKey: "card payment", category: &transferID})
	add(tx{account: f.checkingID, date: "2026-02-20", amount: "40.00", name: "MYSTERY CHARGE",
		merchantKey: "mystery charge"})
	add(tx{account: f.checkingID, date: "2026-03-01", amount: "1500.00", name: "RENT",
		merchantKey: "rent", category: &fallbackID})
	add(tx{account: f.checkingID, date: "2026-03-03", amount: "9.99", name: "EXCLUDED THING",
		merchantKey: "excluded thing", excluded: true})
	add(tx{account: f.checkingID, date: "2026-03-12", amount: "3.00", name: "PENDING THING",
		merchantKey: "pending thing", pending: true})
	add(tx{account: f.checkingID, date: "2026-03-04", amount: "999.00", name: "ONE TIMER",
		merchantKey: "one timer", oneTime: true})
	add(tx{account: f.checkingID, date: "2026-03-06", amount: "20.00", name: "MANUAL ROW",
		merchantKey: "manual row", source: "manual"})
	// A descriptor with LIKE metacharacters in it, so escaping is exercised for real.
	add(tx{account: f.checkingID, date: "2026-03-08", amount: "15.00", name: "100% JUICE CO",
		merchantName: "100% Juice", merchantKey: "100% juice"})
	split := add(tx{account: savingsID, date: "2026-03-07", amount: "100.00", name: "SHARED TRANSFER",
		merchantKey: "shared transfer"})

	// Rows I must never see, both named so a leak is unmistakable in the failure.
	add(tx{account: privateAcctID, date: "2026-03-09", amount: "77.00", name: "STARBUCKS ROOMMATE SECRET",
		merchantName: "Starbucks", merchantKey: "starbucks"})
	add(tx{account: otherAcctID, date: "2026-03-09", amount: "88.00", name: "STARBUCKS OTHER HOUSEHOLD",
		merchantName: "Starbucks", merchantKey: "starbucks"})

	// --- Attachment, split and a merchant grouping ------------------------
	docID := uuid.New()
	exec(`INSERT INTO documents (id, household_id, uploaded_by, title, doc_type, filename, mime_type, size_bytes, storage_key, content_hash)
	      VALUES ($1, $2, $3, 'Receipt', 'receipt', 'r.pdf', 'application/pdf', 10, $4, $4)`,
		docID, f.householdID, f.userID, docID.String())
	exec(`INSERT INTO document_links (document_id, transaction_id) VALUES ($1, $2)`, docID, groceries)

	personID := uuid.New()
	exec(`INSERT INTO household_people (id, household_id, display_name) VALUES ($1, $2, 'Roommate')`,
		personID, f.householdID)
	exec(`INSERT INTO transaction_splits (transaction_id, person_id, amount) VALUES ($1, $2, '50.00')`,
		split, personID)

	// Group both Starbucks descriptors under a renamed canonical merchant, so
	// `merchant:` can be checked against the name the row DISPLAYS rather than the
	// one the bank sent.
	if _, _, err := merchants.Merge(ctx, f.srv.Queries, f.householdID,
		[]string{"starbucks"}, "Coffee Habit", nil); err != nil {
		t.Fatalf("merge merchants: %v", err)
	}

	return f
}

// search runs a query and returns the matching transaction names, sorted so a
// case can be written as a set.
func (f *searchFixture) search(t *testing.T, q string, extra ...string) []string {
	t.Helper()
	rec := f.get(t, q, extra...)
	if rec.Code != http.StatusOK {
		t.Fatalf("q=%q: status %d, body %s", q, rec.Code, rec.Body.String())
	}
	var rows []transactionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("q=%q: unmarshal: %v\nbody: %s", q, err, rec.Body.String())
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	return names
}

func (f *searchFixture) get(t *testing.T, q string, extra ...string) *httptest.ResponseRecorder {
	t.Helper()
	params := url.Values{}
	params.Set("from", searchFrom)
	params.Set("to", searchTo)
	params.Set("limit", "100")
	if q != "" {
		params.Set("q", q)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		params.Set(extra[i], extra[i+1])
	}

	req := httptest.NewRequest(http.MethodGet, "/api/transactions?"+params.Encode(), nil)
	req = req.WithContext(auth.ContextWithIdentity(req.Context(), auth.Identity{
		UserID:      f.userID,
		HouseholdID: f.householdID,
		Role:        auth.RoleOwner,
	}))
	rec := httptest.NewRecorder()
	f.srv.handleListTransactions(rec, req)
	return rec
}

func assertNames(t *testing.T, q string, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("q=%q\n got: %v\nwant: %v", q, got, want)
	}
}

// The load-bearing one. A search is intersected with the household visibility
// predicate, never a replacement for it — so no query, however it is spelled,
// reaches another household's rows or a housemate's private account.
func TestSearchNeverEscapesVisibility(t *testing.T) {
	f := setupSearchFixture(t)

	// Each of these matches all three Starbucks rows on the data alone. Only the
	// one on my own visible account may come back.
	for _, q := range []string{
		"starbucks",
		"merchant:starbucks",
		"merchant_is:starbucks",
		"description:STARBUCKS",
		"amount:77",
		"amount:88",
		"over:70 under:90",
		"-has_no_category",
		"is_expense",
		"-merchant:nothing-matches-this",
		// A value that would break out of the pattern if it were interpolated
		// rather than bound, which it is not.
		`merchant:"' OR 1=1 --"`,
		`merchant:"%"`,
		"account:Checking",
	} {
		for _, name := range f.search(t, q) {
			if strings.Contains(name, "ROOMMATE SECRET") || strings.Contains(name, "OTHER HOUSEHOLD") {
				t.Errorf("q=%q leaked %q", q, name)
			}
		}
	}

	// And positively: the one Starbucks row I can see is mine, on my account.
	assertNames(t, "starbucks", f.search(t, "starbucks"),
		"STARBUCKS STORE 123", "STARBUCKS STORE 999")

	// A shared account IS visible — otherwise "no leaks" could pass by returning
	// nothing at all.
	assertNames(t, "merchant:shared", f.search(t, "merchant:shared"), "SHARED TRANSFER")
}

// A bare word means what `q` meant before the grammar existed: the merchant name,
// the raw name and the descriptor key, case-insensitively.
func TestSearchFreeTextIsUnchanged(t *testing.T) {
	f := setupSearchFixture(t)

	assertNames(t, "whole", f.search(t, "whole"), "WHOLE FOODS MKT")
	assertNames(t, "WHOLE", f.search(t, "WHOLE"), "WHOLE FOODS MKT")
	// The canonical name of a grouped merchant, which is what the row displays.
	assertNames(t, "coffee habit", f.search(t, `"coffee habit"`),
		"STARBUCKS STORE 123", "STARBUCKS STORE 999")
	// Two bare words are ANDed, not treated as a phrase.
	assertNames(t, "whole foods", f.search(t, "whole foods"), "WHOLE FOODS MKT")
	assertNames(t, "whole starbucks", f.search(t, "whole starbucks"))
	// Free text does not reach into notes, which `q` never did.
	assertNames(t, "weekly", f.search(t, "weekly"))
	assertNames(t, "notes:weekly", f.search(t, "notes:weekly"), "WHOLE FOODS MKT")
}

func TestSearchTextDimensions(t *testing.T) {
	f := setupSearchFixture(t)

	for _, tc := range []struct {
		q    string
		want []string
	}{
		{"merchant:starbucks", []string{"STARBUCKS STORE 123", "STARBUCKS STORE 999"}},
		{"merchant_is:starbucks", []string{"STARBUCKS STORE 123", "STARBUCKS STORE 999"}},
		{"merchant_starts:whole", []string{"WHOLE FOODS MKT"}},
		{"merchant_ends:foods", []string{"WHOLE FOODS MKT"}},
		{"merchant_is:whole", nil},
		{"description:MKT", []string{"WHOLE FOODS MKT"}},
		{"description_starts:RENT", []string{"RENT"}},
		{"account:Checking", []string{
			"100% JUICE CO", "ACME PAYROLL", "CARD PAYMENT", "MANUAL ROW", "MYSTERY CHARGE",
			"ONE TIMER", "PENDING THING", "RENT", "STARBUCKS STORE 123", "STARBUCKS STORE 999",
			"WHOLE FOODS MKT",
		}},
		{"account:Savings", []string{"SHARED TRANSFER"}},
		{"institution:Credit", []string{"SHARED TRANSFER"}},
		{"category:groceries", []string{"WHOLE FOODS MKT"}},
		{"category:Coffee", []string{"STARBUCKS STORE 123", "STARBUCKS STORE 999"}},
		{"currency:USD is_pending", []string{"PENDING THING"}},
		{"source:manual", []string{"MANUAL ROW"}},
		// A metacharacter in the value is text, not a wildcard.
		{"merchant_is:100%", nil},
		{`merchant:"100%"`, []string{"100% JUICE CO"}},
		{`merchant:"%"`, []string{"100% JUICE CO"}},
	} {
		assertNames(t, tc.q, f.search(t, tc.q), tc.want...)
	}
}

// Negation, including the case a naive NOT gets wrong: excluding on a NULL column
// has to keep the rows where the column is empty.
func TestSearchNegation(t *testing.T) {
	f := setupSearchFixture(t)

	assertNames(t, "-merchant:starbucks over:900",
		f.search(t, "-merchant:starbucks over:900"), "ACME PAYROLL", "ONE TIMER", "RENT")
	assertNames(t, "starbucks -description:999",
		f.search(t, "starbucks -description:999"), "STARBUCKS STORE 123")
	assertNames(t, "-account:Checking", f.search(t, "-account:Checking"), "SHARED TRANSFER")

	// Only WHOLE FOODS MKT has a note. Excluding a note substring must return
	// every OTHER visible row — including the ones with no notes at all, whose
	// t.notes is NULL. A bare NOT would drop those, since NOT (NULL ILIKE …) is
	// NULL rather than true, and the answer would be nothing at all.
	without := f.search(t, "-notes:weekly")
	for _, name := range without {
		if name == "WHOLE FOODS MKT" {
			t.Errorf("-notes:weekly returned the row it should exclude")
		}
	}
	// Twelve visible rows (the thirteenth is hidden as excluded), minus the note's.
	if len(without) != 11 {
		t.Errorf("-notes:weekly returned %d rows (%v); want the 11 visible rows with no matching note",
			len(without), without)
	}
}

func TestSearchFlags(t *testing.T) {
	f := setupSearchFixture(t)

	for _, tc := range []struct {
		q    string
		want []string
	}{
		// The fallback bucket counts as no category, same as the chip filter.
		// EXCLUDED THING also has none, but it is hidden from the ordinary view.
		{"has_no_category", []string{"100% JUICE CO", "MANUAL ROW",
			"MYSTERY CHARGE", "ONE TIMER", "PENDING THING", "RENT", "SHARED TRANSFER"}},
		{"has_notes", []string{"WHOLE FOODS MKT"}},
		{"has_attachment", []string{"WHOLE FOODS MKT"}},
		{"has_split", []string{"SHARED TRANSFER"}},
		{"has_no_split has_attachment", []string{"WHOLE FOODS MKT"}},
		{"is_pending", []string{"PENDING THING"}},
		{"is_recurring", []string{"ACME PAYROLL"}},
		{"is_manual", []string{"MANUAL ROW"}},
		{"is_one_time", []string{"ONE TIMER"}},
		{"is_transfer", []string{"CARD PAYMENT"}},
		{"is_income", []string{"ACME PAYROLL"}},
		// A flag with an explicit false is the negation, so this is every big
		// expense and not the paycheck.
		{"is_income:false over:900", []string{"ONE TIMER", "RENT"}},
	} {
		assertNames(t, tc.q, f.search(t, tc.q), tc.want...)
	}

	// A row hidden from reports is not in the ordinary view, so asking for it by
	// name has to switch that default off — otherwise the term could only ever
	// match nothing.
	assertNames(t, "is_excluded", f.search(t, "is_excluded"), "EXCLUDED THING")
	for _, name := range f.search(t, "merchant:excluded") {
		if name == "EXCLUDED THING" {
			t.Errorf("a query that does not mention exclusion should not surface an excluded row")
		}
	}
}

// Amounts compare on magnitude, in SQL, against NUMERIC. The paycheck is stored
// negative (money in on Plaid's sign convention) and still answers to over:2000.
func TestSearchAmounts(t *testing.T) {
	f := setupSearchFixture(t)

	for _, tc := range []struct {
		q    string
		want []string
	}{
		{"amount:12.50", []string{"STARBUCKS STORE 123"}},
		{"amount:12.5", []string{"STARBUCKS STORE 123"}},
		{"over:1000", []string{"ACME PAYROLL", "RENT"}},
		{"over:1000 is_expense", []string{"RENT"}},
		{"over:1000 is_income", []string{"ACME PAYROLL"}},
		{"under:5", []string{"PENDING THING", "STARBUCKS STORE 999"}},
		{"over:10 under:16", []string{"100% JUICE CO", "STARBUCKS STORE 123"}},
		// The window has an inclusive lower bound and an exclusive-of-nothing
		// upper: over/under are strict, so the endpoints drop out.
		{"over:12.50 under:15.00", nil},
	} {
		assertNames(t, tc.q, f.search(t, tc.q), tc.want...)
	}
}

func TestSearchDates(t *testing.T) {
	f := setupSearchFixture(t)

	for _, tc := range []struct {
		q    string
		want []string
	}{
		{"on:2026-01-05", []string{"STARBUCKS STORE 999"}},
		{"before:2026-01-31", []string{"STARBUCKS STORE 999"}},
		{"since:2026-03-10", []string{"PENDING THING", "STARBUCKS STORE 123"}},
		{"since:2026-03-01 before:2026-03-02", []string{"ACME PAYROLL", "RENT", "WHOLE FOODS MKT"}},
		// Both bounds are inclusive, so a single-day range is spelled either way.
		{"since:2026-01-05 before:2026-01-05", []string{"STARBUCKS STORE 999"}},
		{"starbucks since:2026-02-01", []string{"STARBUCKS STORE 123"}},
	} {
		assertNames(t, tc.q, f.search(t, tc.q), tc.want...)
	}
}

// A query that names its own dates owns the window. The page always sends a
// from/to, so without this rule `since:2026-01-01` would be silently ANDed with
// whatever the date pickers happened to say.
func TestSearchDateTermOverridesTheWindow(t *testing.T) {
	f := setupSearchFixture(t)

	// A window that excludes every row, and a query that asks for January anyway.
	got := f.search(t, "on:2026-01-05", "from", "2026-06-01", "to", "2026-06-30")
	assertNames(t, "on:2026-01-05 with a June window", got, "STARBUCKS STORE 999")

	// Without a date term the window still applies, or the chips would be dead.
	assertNames(t, "starbucks with a June window",
		f.search(t, "starbucks", "from", "2026-06-01", "to", "2026-06-30"))
}

// The chips and the grammar compose: a search narrows what the chips selected
// rather than replacing it.
func TestSearchComposesWithTheChips(t *testing.T) {
	f := setupSearchFixture(t)

	assertNames(t, "starbucks + account chip",
		f.search(t, "starbucks", "accounts", f.checkingID.String()),
		"STARBUCKS STORE 123", "STARBUCKS STORE 999")

	// The same query with the chip set to the OTHER account must come back empty:
	// the chip is still doing its job.
	got := f.search(t, "starbucks", "accounts", uuid.New().String())
	assertNames(t, "starbucks + a foreign account chip", got)

	assertNames(t, "uncategorised chip + a term",
		f.search(t, "over:1000", "uncategorised", "true"), "RENT")
}

// The whole point of the feature, in one query.
func TestSearchComposedQuery(t *testing.T) {
	f := setupSearchFixture(t)

	q := `starbucks over:5 since:2026-02-01 -description:999 has_category`
	assertNames(t, q, f.search(t, q), "STARBUCKS STORE 123")
}

// Paging happens where the rows are chosen, so page 2 continues page 1 rather
// than re-filtering a page that has already been cut.
func TestSearchPages(t *testing.T) {
	f := setupSearchFixture(t)

	first := f.search(t, "is_expense", "limit", "3", "offset", "0")
	second := f.search(t, "is_expense", "limit", "3", "offset", "3")
	if len(first) != 3 || len(second) != 3 {
		t.Fatalf("expected 3 rows per page, got %d and %d", len(first), len(second))
	}
	for _, a := range first {
		for _, b := range second {
			if a == b {
				t.Errorf("%q appears on both pages", a)
			}
		}
	}
}

// A value the user can fix is a 400 with a message that names it, not a 500 and
// not an empty list — an empty list would read as "you have no such transactions".
func TestSearchRejectsBadValues(t *testing.T) {
	f := setupSearchFixture(t)

	for _, tc := range []struct{ q, mentions string }{
		{"over:banana", "banana"},
		{"since:banana", "banana"},
		{"amount:1.2.3", "1.2.3"},
	} {
		rec := f.get(t, tc.q)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("q=%q: status %d, want 400; body %s", tc.q, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), tc.mentions) {
			t.Errorf("q=%q: 400 body %s does not name the offending value", tc.q, rec.Body.String())
		}
	}
}

// A query matching nothing is an empty list. The id path has to short-circuit
// there: an empty array handed to the transaction_ids narg would read as "no
// filter" and return the whole page.
func TestSearchNoMatchesIsEmpty(t *testing.T) {
	f := setupSearchFixture(t)

	if got := f.search(t, "merchant:definitelynotamerchant"); len(got) != 0 {
		t.Errorf("a query matching nothing returned %d rows: %v", len(got), got)
	}
}

// An unknown operator degrades to free text rather than erroring, so a pasted
// descriptor containing a colon still finds its charge.
func TestSearchUnknownOperatorIsFreeText(t *testing.T) {
	f := setupSearchFixture(t)

	if got := f.search(t, "nosuchfield:starbucks"); len(got) != 0 {
		t.Errorf("nosuchfield:starbucks matched %v; as free text it should match nothing", got)
	}

	// Free text matches COALESCE(merchant_name, name) — the bank's merchant name
	// when it sent one, the description otherwise — which is exactly what `q` did
	// before the grammar existed. So the raw descriptor of a row that HAS a
	// merchant name is not free-text searchable, and `description:` is how to
	// reach it. Asserted rather than described, because it is a sharp edge.
	assertNames(t, `"STARBUCKS STORE"`, f.search(t, `"STARBUCKS STORE"`))
	assertNames(t, `description:"STARBUCKS STORE"`, f.search(t, `description:"STARBUCKS STORE"`),
		"STARBUCKS STORE 123", "STARBUCKS STORE 999")
}

// The search-operators endpoint is what the search bar's autocomplete is built
// from, so it has to describe the parser it is paired with.
func TestSearchOperatorsEndpoint(t *testing.T) {
	f := setupSearchFixture(t)

	req := httptest.NewRequest(http.MethodGet, "/api/transactions/search-operators", nil)
	rec := httptest.NewRecorder()
	f.srv.handleSearchOperators(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	var ops []struct {
		Name       string `json:"name"`
		TakesValue bool   `json:"takes_value"`
		Help       string `json:"help"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ops); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byName := map[string]bool{}
	for _, op := range ops {
		byName[op.Name] = op.TakesValue
	}
	for name, takesValue := range map[string]bool{
		"merchant":        true,
		"merchant_starts": true,
		"over":            true,
		"since":           true,
		"has_no_category": false,
		"is_expense":      false,
	} {
		got, ok := byName[name]
		if !ok {
			t.Errorf("operator %q is missing from the endpoint", name)
			continue
		}
		if got != takesValue {
			t.Errorf("operator %q takes_value = %v, want %v", name, got, takesValue)
		}
	}
}
