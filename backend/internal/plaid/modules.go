package plaid

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
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

// HasProduct reports whether a product appears in a product list.
func HasProduct(products []string, product string) bool {
	return slices.Contains(products, product)
}

// The optional modules are gated on TWO things, and neither of them is
// plaid_items.products.
//
// That column records which products were requested when the Item was linked. It
// is a snapshot of one moment, and using it as the gate was a real bug: an
// operator who linked with `transactions` and later enabled `liabilities` got
// nothing forever, because the only way to change a stored products list is to
// relink — which orphans the transaction history tied to the old item and cannot
// re-widen the history window. An access token does not work that way. Plaid
// serves /liabilities/get and /investments/holdings/get on a transactions-only
// token perfectly well, and does so for Items that already exist.
//
// So the gate is:
//
//   - the OPERATOR enabled the product (PLAID_PRODUCTS, via Client.Products) —
//     these calls are billable, so an operator who has not asked for a product
//     must never be charged for it; and
//   - the ITEM actually has accounts of the relevant type, so no call is made to
//     ask a chequing-only bank about its mortgages.
//
// A per-Item refusal is then handled as data, not as failure: see
// productUnavailable.

// itemAccountKinds records which optional modules an item's accounts could
// possibly feed. Derived from the accounts Plaid just returned, so it is current
// by construction rather than remembered from link time.
type itemAccountKinds struct {
	debt       bool
	investment bool
}

// noteAccountType folds one account's type into the set.
func (k *itemAccountKinds) noteAccountType(accountType string) {
	switch accountType {
	case "credit", "loan":
		k.debt = true
	case "investment", "brokerage":
		k.investment = true
	}
}

// productUnavailable reports whether a module error is Plaid declining to serve
// the product for this Item, rather than something being broken.
//
// These are expected and permanent-ish: an institution that does not offer the
// product, or an Item whose consent does not cover it. Treating them as errors
// would log noise on every sweep for every unaffected Item, so they are a quiet
// skip. Anything else — a network fault, a bad token, a Plaid outage — is a real
// error and still surfaces.
//
// Matched on the message because wrapErr formats Plaid's error_code into it;
// this mirrors the ITEM_LOGIN_REQUIRED check in sync.go.
func productUnavailable(err error) bool {
	for _, code := range []string{
		"PRODUCTS_NOT_SUPPORTED",      // the institution does not offer it
		"PRODUCT_NOT_READY",           // still being pulled; the next sweep gets it
		"NO_LIABILITY_ACCOUNTS",       // nothing to report on this Item
		"NO_INVESTMENT_ACCOUNTS",      //
		"ADDITIONAL_CONSENT_REQUIRED", // consent does not cover it (EU/UK Items)
		"INSUFFICIENT_CREDENTIALS",
	} {
		if strings.Contains(err.Error(), code) {
			return true
		}
	}
	return false
}

// ModuleResult reports what an optional module did.
type ModuleResult struct {
	Holdings               int
	Securities             int
	InvestmentTransactions int
	Liabilities            int
	// Skipped is true when the module did not run: not enabled, nothing for it
	// to read, or Plaid declined to serve it for this Item.
	Skipped bool
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

// SyncInvestments refreshes holdings for one item. A no-op unless the operator
// enabled the Investments product and the item has investment accounts.
func (s *Syncer) SyncInvestments(ctx context.Context, accessToken string, accountIDs map[string]uuid.UUID, kinds itemAccountKinds) (ModuleResult, error) {
	var result ModuleResult
	if !HasProduct(s.Client.SyncProducts(), ProductInvestments) || !kinds.investment {
		result.Skipped = true
		return result, nil
	}

	page, err := s.Client.GetHoldings(ctx, accessToken)
	if err != nil {
		if productUnavailable(err) {
			result.Skipped = true
			return result, nil
		}
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
		slog.Warn("sync investment transactions", "error", err)
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

// SyncLiabilities refreshes debt terms for one item. A no-op unless the operator
// enabled the Liabilities product and the item has credit or loan accounts.
func (s *Syncer) SyncLiabilities(ctx context.Context, accessToken string, accountIDs map[string]uuid.UUID, kinds itemAccountKinds) (ModuleResult, error) {
	var result ModuleResult
	if !HasProduct(s.Client.SyncProducts(), ProductLiabilities) || !kinds.debt {
		result.Skipped = true
		return result, nil
	}

	liabilities, err := s.Client.GetLiabilities(ctx, accessToken)
	if err != nil {
		if productUnavailable(err) {
			result.Skipped = true
			return result, nil
		}
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
