package db

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// TestInvestmentQueriesExecute runs every query behind the Investments page
// against a real Postgres.
//
// Same reason as TestReportQueriesExecute: sqlc validates syntax, not
// execution, and these queries carry the visibility join and several casts that
// only fail when a row actually goes through them.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/db/
func TestInvestmentQueriesExecute(t *testing.T) {
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

	q := dbgen.New(pool)

	householdID := uuid.New()
	userID := uuid.New()
	itemID := uuid.New()
	brokerageID := uuid.New()
	iraID := uuid.New()
	securityID := uuid.New()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, sql)
		}
	}

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Investments Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM securities WHERE id = $1`, securityID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM asset_prices WHERE ticker = 'TESTSPY'`)
	})

	exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
	      VALUES ($1, $2, $3, 'x', 'Tester')`, userID, householdID, userID.String()+"@example.test")
	exec(`INSERT INTO plaid_items (id, user_id, plaid_item_id, access_token_encrypted, products, status)
	      VALUES ($1, $2, $3, '\x00', '{investments}', 'active')`, itemID, userID, itemID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Brokerage', 'investment', 'brokerage', 25000.00)`,
		brokerageID, itemID, brokerageID.String())
	exec(`INSERT INTO accounts (id, plaid_item_id, plaid_account_id, name, type, subtype, current_balance)
	      VALUES ($1, $2, $3, 'Roth IRA', 'investment', 'roth', 15000.00)`,
		iraID, itemID, iraID.String())
	exec(`INSERT INTO securities (id, plaid_security_id, name, ticker, type, close_price)
	      VALUES ($1, $2, 'Vanguard Total Market', 'VTI', 'etf', 275.50)`,
		securityID, securityID.String())
	exec(`INSERT INTO holdings (account_id, security_id, quantity, cost_basis, institution_price, institution_value)
	      VALUES ($1, $2, 80.5, 18000.00, 275.50, 22177.75)`, brokerageID, securityID)
	// A second position with NO cost basis: the coverage disclosure only works
	// if a partial basis is representable.
	exec(`INSERT INTO holdings (account_id, security_id, quantity, institution_price, institution_value)
	      VALUES ($1, $2, 50.0, 275.50, 13775.00)`, iraID, securityID)

	t.Run("ListInvestmentAccounts", func(t *testing.T) {
		rows, err := q.ListInvestmentAccounts(ctx, dbgen.ListInvestmentAccountsParams{
			HouseholdID: householdID, UserID: userID,
		})
		if err != nil {
			t.Fatalf("ListInvestmentAccounts: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("got %d accounts, want 2", len(rows))
		}
		for _, r := range rows {
			if r.TaxTreatment != nil {
				t.Errorf("account %s came back pre-tagged as %q; the column must start NULL",
					r.Name, *r.TaxTreatment)
			}
		}
	})

	t.Run("SetAccountTaxTreatment", func(t *testing.T) {
		managed := false
		roth := "roth_ira"
		row, err := q.SetAccountTaxTreatment(ctx, dbgen.SetAccountTaxTreatmentParams{
			ID: iraID, HouseholdID: householdID, UserID: userID,
			TaxTreatment: &roth, IsManaged: &managed,
		})
		if err != nil {
			t.Fatalf("SetAccountTaxTreatment: %v", err)
		}
		if row.TaxTreatment == nil || *row.TaxTreatment != "roth_ira" {
			t.Errorf("tax_treatment = %v, want roth_ira", row.TaxTreatment)
		}

		// Clearing back to untagged must be possible: "I do not know" is a
		// better stored state than a wrong guess.
		cleared, err := q.SetAccountTaxTreatment(ctx, dbgen.SetAccountTaxTreatmentParams{
			ID: iraID, HouseholdID: householdID, UserID: userID,
		})
		if err != nil {
			t.Fatalf("clear tax treatment: %v", err)
		}
		if cleared.TaxTreatment != nil {
			t.Errorf("tax_treatment = %v after clearing, want nil", cleared.TaxTreatment)
		}

		// And another household must not be able to reach it at all.
		if _, err := q.SetAccountTaxTreatment(ctx, dbgen.SetAccountTaxTreatmentParams{
			ID: iraID, HouseholdID: uuid.New(), UserID: uuid.New(),
			TaxTreatment: &roth,
		}); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("cross-household tag returned %v, want pgx.ErrNoRows", err)
		}
	})

	t.Run("ListVisibleHoldingsDetailed", func(t *testing.T) {
		rows, err := q.ListVisibleHoldingsDetailed(ctx, dbgen.ListVisibleHoldingsDetailedParams{
			HouseholdID: householdID, UserID: userID,
		})
		if err != nil {
			t.Fatalf("ListVisibleHoldingsDetailed: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("got %d holdings, want 2", len(rows))
		}
		var withBasis, withoutBasis int
		for _, r := range rows {
			if r.CostBasis.Valid {
				withBasis++
			} else {
				withoutBasis++
			}
		}
		if withBasis != 1 || withoutBasis != 1 {
			t.Errorf("basis coverage = %d with / %d without, want 1 / 1", withBasis, withoutBasis)
		}
	})

	t.Run("ListInvestmentAccountValues", func(t *testing.T) {
		rows, err := q.ListInvestmentAccountValues(ctx)
		if err != nil {
			t.Fatalf("ListInvestmentAccountValues: %v", err)
		}

		byID := map[uuid.UUID]dbgen.ListInvestmentAccountValuesRow{}
		for _, r := range rows {
			byID[r.AccountID] = r
		}

		brokerage, ok := byID[brokerageID]
		if !ok {
			t.Fatal("brokerage account missing from values")
		}
		// Market value comes from the account balance, which includes
		// uninvested cash the holdings sum would miss.
		if !brokerage.MarketValue.Equal(decimal.RequireFromString("25000.00")) {
			t.Errorf("brokerage market_value = %s, want 25000.00", brokerage.MarketValue)
		}
		if brokerage.BasisHoldings != 1 {
			t.Errorf("brokerage basis_holdings = %d, want 1", brokerage.BasisHoldings)
		}

		ira := byID[iraID]
		// The IRA's only holding reports no basis, so the COALESCEd sum is an
		// artefact and basis_holdings is what tells the caller to store NULL.
		if ira.BasisHoldings != 0 {
			t.Errorf("ira basis_holdings = %d, want 0", ira.BasisHoldings)
		}
	})

	t.Run("SnapshotUpsertIsIdempotent", func(t *testing.T) {
		// Month end and a leap day: the two dates most likely to be mangled by
		// date arithmetic anywhere in the chain.
		for _, dateStr := range []string{"2024-02-29", "2026-01-31", "2026-06-30"} {
			asOf, err := time.Parse(time.DateOnly, dateStr)
			if err != nil {
				t.Fatalf("parse %s: %v", dateStr, err)
			}

			first := decimal.RequireFromString("1000.00")
			second := decimal.RequireFromString("2000.00")

			if err := q.UpsertInvestmentSnapshot(ctx, dbgen.UpsertInvestmentSnapshotParams{
				AccountID: brokerageID, AsOf: &asOf, MarketValue: first,
			}); err != nil {
				t.Fatalf("first upsert %s: %v", dateStr, err)
			}
			// Re-running the job must replace the day's row, not add a second.
			if err := q.UpsertInvestmentSnapshot(ctx, dbgen.UpsertInvestmentSnapshotParams{
				AccountID: brokerageID, AsOf: &asOf, MarketValue: second,
				CostBasis: decimal.NullDecimal{Decimal: decimal.RequireFromString("1500.00"), Valid: true},
			}); err != nil {
				t.Fatalf("second upsert %s: %v", dateStr, err)
			}

			var count int
			var value decimal.Decimal
			if err := pool.QueryRow(ctx,
				`SELECT COUNT(*), MAX(market_value) FROM investment_snapshots
				 WHERE account_id = $1 AND as_of = $2`, brokerageID, asOf).
				Scan(&count, &value); err != nil {
				t.Fatalf("count snapshots %s: %v", dateStr, err)
			}
			if count != 1 {
				t.Errorf("%s: %d rows after two upserts, want 1", dateStr, count)
			}
			if !value.Equal(second) {
				t.Errorf("%s: market_value = %s, want the second write %s", dateStr, value, second)
			}
		}
	})

	t.Run("ListInvestmentSnapshots", func(t *testing.T) {
		from, _ := time.Parse(time.DateOnly, "2024-01-01")
		to, _ := time.Parse(time.DateOnly, "2027-01-01")

		rows, err := q.ListInvestmentSnapshots(ctx, dbgen.ListInvestmentSnapshotsParams{
			HouseholdID: householdID, UserID: userID, AsOf: from, AsOf_2: to,
		})
		if err != nil {
			t.Fatalf("ListInvestmentSnapshots: %v", err)
		}
		if len(rows) != 3 {
			t.Fatalf("got %d snapshot days, want 3", len(rows))
		}
		if !rows[0].AsOf.Before(rows[1].AsOf) {
			t.Error("snapshots are not ordered ascending by date")
		}

		perAccount, err := q.ListInvestmentSnapshotsForAccount(ctx, dbgen.ListInvestmentSnapshotsForAccountParams{
			HouseholdID: householdID, UserID: userID, AccountID: brokerageID,
			AsOf: from, AsOf_2: to,
		})
		if err != nil {
			t.Fatalf("ListInvestmentSnapshotsForAccount: %v", err)
		}
		if len(perAccount) != 3 {
			t.Errorf("got %d per-account snapshots, want 3", len(perAccount))
		}
	})

	t.Run("GetEarliestInvestmentSnapshot", func(t *testing.T) {
		earliest, err := q.GetEarliestInvestmentSnapshot(ctx, dbgen.GetEarliestInvestmentSnapshotParams{
			HouseholdID: householdID, UserID: userID,
		})
		if err != nil {
			t.Fatalf("GetEarliestInvestmentSnapshot: %v", err)
		}
		if earliest.Format(time.DateOnly) != "2024-02-29" {
			t.Errorf("earliest = %s, want 2024-02-29", earliest.Format(time.DateOnly))
		}

		// A household with no history must return no rows rather than a NULL
		// that cannot be scanned into a date.
		_, err = q.GetEarliestInvestmentSnapshot(ctx, dbgen.GetEarliestInvestmentSnapshotParams{
			HouseholdID: uuid.New(), UserID: uuid.New(),
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("empty household returned %v, want pgx.ErrNoRows", err)
		}
	})

	t.Run("InvestmentTransactionsAndDividends", func(t *testing.T) {
		buy := uuid.New().String()
		contribution := uuid.New().String()
		dividend := uuid.New().String()
		date, _ := time.Parse(time.DateOnly, "2026-03-15")

		upsert := func(plaidID, txnType, subtype, amount string) {
			t.Helper()
			sub := subtype
			name := "Test " + txnType
			if err := q.UpsertInvestmentTransaction(ctx, dbgen.UpsertInvestmentTransactionParams{
				AccountID:                    brokerageID,
				SecurityID:                   &securityID,
				PlaidInvestmentTransactionID: &plaidID,
				Type:                         txnType,
				Subtype:                      &sub,
				Amount:                       decimal.RequireFromString(amount),
				Date:                         date,
				Name:                         &name,
				Currency:                     "USD",
			}); err != nil {
				t.Fatalf("upsert %s: %v", plaidID, err)
			}
		}

		// Plaid's sign convention: positive means cash was debited.
		upsert(buy, "buy", "buy", "500.00")
		upsert(contribution, "cash", "contribution", "-1000.00")
		upsert(dividend, "cash", "dividend", "-42.50")

		// Re-upserting the same Plaid id must refresh in place, not duplicate.
		upsert(buy, "buy", "buy", "550.00")

		from, _ := time.Parse(time.DateOnly, "2026-01-01")
		to, _ := time.Parse(time.DateOnly, "2026-12-31")

		txns, err := q.ListInvestmentTransactionsInRange(ctx, dbgen.ListInvestmentTransactionsInRangeParams{
			HouseholdID: householdID, UserID: userID, Date: from, Date_2: to,
		})
		if err != nil {
			t.Fatalf("ListInvestmentTransactionsInRange: %v", err)
		}
		if len(txns) != 3 {
			t.Fatalf("got %d investment transactions, want 3 (the re-upsert must not duplicate)", len(txns))
		}

		divs, err := q.GetDividendIncome(ctx, dbgen.GetDividendIncomeParams{
			HouseholdID: householdID, UserID: userID, Date: from, Date_2: to,
		})
		if err != nil {
			t.Fatalf("GetDividendIncome: %v", err)
		}
		if len(divs) != 1 {
			t.Fatalf("got %d dividend months, want 1", len(divs))
		}
		// Negated out of Plaid's convention so it reads as positive income.
		if !divs[0].Total.Equal(decimal.RequireFromString("42.50")) {
			t.Errorf("dividend total = %s, want 42.50", divs[0].Total)
		}
	})

	t.Run("AssetPrices", func(t *testing.T) {
		asOf, _ := time.Parse(time.DateOnly, "2026-07-27")

		if err := q.UpsertAssetPrice(ctx, dbgen.UpsertAssetPriceParams{
			Ticker: "TESTSPY", AsOf: asOf, Close: decimal.RequireFromString("412.10"),
		}); err != nil {
			t.Fatalf("UpsertAssetPrice: %v", err)
		}
		// A restated close replaces the day rather than adding a second row.
		if err := q.UpsertAssetPrice(ctx, dbgen.UpsertAssetPriceParams{
			Ticker: "TESTSPY", AsOf: asOf, Close: decimal.RequireFromString("414.25"),
		}); err != nil {
			t.Fatalf("UpsertAssetPrice (replace): %v", err)
		}

		from, _ := time.Parse(time.DateOnly, "2026-01-01")
		to, _ := time.Parse(time.DateOnly, "2026-12-31")
		prices, err := q.ListAssetPrices(ctx, dbgen.ListAssetPricesParams{
			Column1: []string{"TESTSPY"}, AsOf: from, AsOf_2: to,
		})
		if err != nil {
			t.Fatalf("ListAssetPrices: %v", err)
		}
		if len(prices) != 1 {
			t.Fatalf("got %d prices, want 1", len(prices))
		}
		if !prices[0].Close.Equal(decimal.RequireFromString("414.25")) {
			t.Errorf("close = %s, want the replacing value 414.25", prices[0].Close)
		}
	})
}
