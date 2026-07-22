package plaid

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
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
	Holdings    int
	Securities  int
	Liabilities int
}

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

	return result, nil
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
