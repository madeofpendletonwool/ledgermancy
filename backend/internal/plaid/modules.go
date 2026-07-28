package plaid

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Product names as stored on plaid_items.products.
const (
	ProductTransactions = "transactions"
	ProductInvestments  = "investments"
	ProductLiabilities  = "liabilities"
)

// HasProduct reports whether an item is authorized for a product.
//
// This is the switch that makes the modules independent: transactions ship and
// work on their own, and Investments or Liabilities only ever run for items
// that were linked with them.
func HasProduct(products []string, product string) bool {
	return slices.Contains(products, product)
}

// ModuleResult reports what an optional module did.
type ModuleResult struct {
	Holdings               int
	Securities             int
	InvestmentTransactions int
	Liabilities            int
}

// investmentTransactionLookback is how far back investment transactions are
// fetched on every sync.
//
// Two years matches the transactions module's maximum history window, and
// re-fetching the whole window each time is deliberate: the upsert is keyed on
// Plaid's transaction id, so a re-fetch corrects an amount an institution
// restated rather than leaving a stale row behind. The volume is trivial — a
// household portfolio produces a few hundred of these a year, not the tens of
// thousands the ordinary transaction feed does, which is why this does not need
// the cursor machinery /transactions/sync has.
const investmentTransactionLookback = 730 * 24 * time.Hour

// SyncInvestments refreshes holdings for one item. A no-op unless the item is
// authorized for the Investments product.
func (s *Syncer) SyncInvestments(ctx context.Context, item dbgen.PlaidItem, accessToken string, accountIDs map[string]uuid.UUID) (ModuleResult, error) {
	var result ModuleResult
	if !HasProduct(item.Products, ProductInvestments) {
		return result, nil
	}

	page, err := s.Client.GetHoldings(ctx, accessToken)
	if err != nil {
		return result, err
	}

	// Securities first: holdings reference them.
	securityIDs := make(map[string]uuid.UUID, len(page.Securities))
	for _, sec := range page.Securities {
		row, err := s.Queries.UpsertSecurity(ctx, dbgen.UpsertSecurityParams{
			PlaidSecurityID:  sec.PlaidSecurityID,
			Name:             sec.Name,
			Ticker:           sec.Ticker,
			Type:             sec.Type,
			Cusip:            sec.CUSIP,
			Isin:             sec.ISIN,
			ClosePrice:       sec.ClosePrice,
			ClosePriceAsOf:   sec.ClosePriceAsOf,
			Currency:         sec.Currency,
			IsCashEquivalent: sec.IsCashEquivalent,
		})
		if err != nil {
			return result, fmt.Errorf("upsert security %s: %w", sec.PlaidSecurityID, err)
		}
		securityIDs[sec.PlaidSecurityID] = row.ID
	}
	result.Securities = len(securityIDs)

	// Track what each account still holds, so positions that have been sold
	// can be removed rather than left frozen at their last known value.
	seen := make(map[uuid.UUID][]uuid.UUID)
	today := time.Now()

	for _, h := range page.Holdings {
		accountID, ok := accountIDs[h.PlaidAccountID]
		if !ok {
			slog.Warn("holding for unknown account", "plaid_account_id", h.PlaidAccountID)
			continue
		}
		securityID, ok := securityIDs[h.PlaidSecurityID]
		if !ok {
			slog.Warn("holding for unknown security", "plaid_security_id", h.PlaidSecurityID)
			continue
		}

		if err := s.Queries.UpsertHolding(ctx, dbgen.UpsertHoldingParams{
			AccountID:        accountID,
			SecurityID:       securityID,
			Quantity:         h.Quantity,
			CostBasis:        h.CostBasis,
			InstitutionPrice: h.InstitutionPrice,
			InstitutionValue: h.InstitutionValue,
			Currency:         h.Currency,
			AsOf:             &today,
		}); err != nil {
			return result, fmt.Errorf("upsert holding: %w", err)
		}

		seen[accountID] = append(seen[accountID], securityID)
		result.Holdings++
	}

	for accountID, securityIDList := range seen {
		if err := s.Queries.DeleteHoldingsNotIn(ctx, dbgen.DeleteHoldingsNotInParams{
			AccountID: accountID, Column2: securityIDList,
		}); err != nil {
			return result, fmt.Errorf("prune holdings: %w", err)
		}
	}

	// Transactions are what let a return figure separate market movement from
	// the user's own deposits. Failing to get them must not lose the holdings
	// that were just written, so the error is logged and the module still
	// reports success — the next sync retries.
	n, err := s.syncInvestmentTransactions(ctx, accessToken, accountIDs, securityIDs, today)
	if err != nil {
		slog.Warn("sync investment transactions", "error", err, "item_id", item.ID)
	}
	result.InvestmentTransactions = n

	return result, nil
}

// syncInvestmentTransactions fetches and stores the item's investment
// transactions for the lookback window.
//
// securityIDs comes from the holdings pass above, but a transaction can
// reference a security no longer held (a position sold last year), so a security
// this map does not know is stored with a NULL security_id rather than dropped:
// the *cash flow* is what the return maths needs, and it is correct with or
// without the instrument attached.
func (s *Syncer) syncInvestmentTransactions(
	ctx context.Context,
	accessToken string,
	accountIDs map[string]uuid.UUID,
	securityIDs map[string]uuid.UUID,
	now time.Time,
) (int, error) {
	txns, err := s.Client.GetInvestmentTransactions(
		ctx, accessToken, now.Add(-investmentTransactionLookback), now)
	if err != nil {
		return 0, err
	}

	stored := 0
	for _, t := range txns {
		accountID, ok := accountIDs[t.PlaidAccountID]
		if !ok {
			slog.Warn("investment transaction for unknown account",
				"plaid_account_id", t.PlaidAccountID)
			continue
		}
		// An unparseable date leaves the zero value, which would sort before
		// every real flow and silently distort the return series. Dropping it is
		// the honest outcome.
		if t.Date.IsZero() {
			slog.Warn("investment transaction with unreadable date",
				"plaid_investment_transaction_id", t.PlaidInvestmentTransactionID)
			continue
		}

		var securityID *uuid.UUID
		if t.PlaidSecurityID != nil {
			if id, ok := securityIDs[*t.PlaidSecurityID]; ok {
				securityID = &id
			}
		}

		if err := s.Queries.UpsertInvestmentTransaction(ctx, dbgen.UpsertInvestmentTransactionParams{
			AccountID:                    accountID,
			SecurityID:                   securityID,
			PlaidInvestmentTransactionID: t.PlaidInvestmentTransactionID,
			Type:                         t.Type,
			Subtype:                      optionalString(t.Subtype),
			Amount:                       t.Amount,
			Quantity:                     t.Quantity,
			Price:                        t.Price,
			Fees:                         t.Fees,
			Date:                         t.Date,
			Name:                         t.Name,
			Currency:                     t.Currency,
			Raw:                          t.Raw,
		}); err != nil {
			return stored, fmt.Errorf("upsert investment transaction: %w", err)
		}
		stored++
	}

	return stored, nil
}

// SyncLiabilities refreshes debt terms for one item. A no-op unless the item is
// authorized for the Liabilities product.
func (s *Syncer) SyncLiabilities(ctx context.Context, item dbgen.PlaidItem, accessToken string, accountIDs map[string]uuid.UUID) (ModuleResult, error) {
	var result ModuleResult
	if !HasProduct(item.Products, ProductLiabilities) {
		return result, nil
	}

	liabilities, err := s.Client.GetLiabilities(ctx, accessToken)
	if err != nil {
		return result, err
	}

	for _, l := range liabilities {
		accountID, ok := accountIDs[l.PlaidAccountID]
		if !ok {
			slog.Warn("liability for unknown account", "plaid_account_id", l.PlaidAccountID)
			continue
		}

		if err := s.Queries.UpsertLiability(ctx, dbgen.UpsertLiabilityParams{
			AccountID:              accountID,
			Kind:                   l.Kind,
			Apr:                    l.APR,
			AprType:                l.APRType,
			Balance:                l.Balance,
			MinimumPayment:         l.MinimumPayment,
			LastPaymentAmount:      l.LastPaymentAmount,
			LastPaymentDate:        l.LastPaymentDate,
			NextPaymentDueDate:     l.NextPaymentDueDate,
			OriginationDate:        l.OriginationDate,
			OriginationPrincipal:   l.OriginationPrincipal,
			InterestRatePercentage: l.InterestRatePercentage,
			IsOverdue:              l.IsOverdue,
			Raw:                    l.Raw,
		}); err != nil {
			return result, fmt.Errorf("upsert liability: %w", err)
		}
		result.Liabilities++
	}

	return result, nil
}
