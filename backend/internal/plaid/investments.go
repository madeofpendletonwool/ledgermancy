package plaid

import (
	"context"
	"encoding/json"
	"time"

	plaidapi "github.com/plaid/plaid-go/v40/plaid"
	"github.com/shopspring/decimal"
)

// Security is a tradable instrument, normalized for storage.
type Security struct {
	PlaidSecurityID  string
	Name             *string
	Ticker           *string
	Type             *string
	CUSIP            *string
	ISIN             *string
	ClosePrice       decimal.NullDecimal
	ClosePriceAsOf   *time.Time
	Currency         string
	IsCashEquivalent bool
}

// Holding is a position in one security within one account.
type Holding struct {
	PlaidAccountID   string
	PlaidSecurityID  string
	Quantity         decimal.Decimal
	CostBasis        decimal.NullDecimal
	InstitutionPrice decimal.NullDecimal
	InstitutionValue decimal.NullDecimal
	Currency         string
}

// HoldingsPage is one /investments/holdings/get response.
type HoldingsPage struct {
	Securities []Security
	Holdings   []Holding
}

// GetHoldings fetches current investment positions.
//
// Part of the Investments module: only called for items whose products include
// it, so an item linked for transactions alone never reaches this.
func (c *Client) GetHoldings(ctx context.Context, accessToken string) (HoldingsPage, error) {
	resp, _, err := c.api.InvestmentsHoldingsGet(ctx).
		InvestmentsHoldingsGetRequest(*plaidapi.NewInvestmentsHoldingsGetRequest(accessToken)).
		Execute()
	if err != nil {
		return HoldingsPage{}, wrapErr("get holdings", err)
	}

	page := HoldingsPage{
		Securities: make([]Security, 0, len(resp.GetSecurities())),
		Holdings:   make([]Holding, 0, len(resp.GetHoldings())),
	}

	for _, s := range resp.GetSecurities() {
		page.Securities = append(page.Securities, convertSecurity(s))
	}
	for _, h := range resp.GetHoldings() {
		page.Holdings = append(page.Holdings, convertHolding(h))
	}
	return page, nil
}

func convertSecurity(s plaidapi.Security) Security {
	currency := s.GetIsoCurrencyCode()
	if currency == "" {
		currency = "USD"
	}

	var asOf *time.Time
	if raw := s.GetClosePriceAsOf(); raw != "" {
		if parsed, err := time.Parse(time.DateOnly, raw); err == nil {
			asOf = &parsed
		}
	}

	return Security{
		PlaidSecurityID:  s.GetSecurityId(),
		Name:             optionalString(s.GetName()),
		Ticker:           optionalString(s.GetTickerSymbol()),
		Type:             optionalString(s.GetType()),
		CUSIP:            optionalString(s.GetCusip()),
		ISIN:             optionalString(s.GetIsin()),
		ClosePrice:       money(s.ClosePrice.Get()),
		ClosePriceAsOf:   asOf,
		Currency:         currency,
		IsCashEquivalent: s.GetIsCashEquivalent(),
	}
}

func convertHolding(h plaidapi.Holding) Holding {
	currency := h.GetIsoCurrencyCode()
	if currency == "" {
		currency = "USD"
	}

	price := h.GetInstitutionPrice()
	value := h.GetInstitutionValue()

	return Holding{
		PlaidAccountID:  h.GetAccountId(),
		PlaidSecurityID: h.GetSecurityId(),
		// Quantity goes through the same exact conversion as money: fractional
		// share counts are multiplied by prices, so drift here would show up
		// directly in portfolio value.
		Quantity:         amountToDecimal(h.GetQuantity()),
		CostBasis:        money(h.CostBasis.Get()),
		InstitutionPrice: money(&price),
		InstitutionValue: money(&value),
		Currency:         currency,
	}
}

// Liability is a debt account with its terms.
type Liability struct {
	PlaidAccountID         string
	Kind                   string
	APR                    decimal.NullDecimal
	APRType                *string
	Balance                decimal.NullDecimal
	MinimumPayment         decimal.NullDecimal
	LastPaymentAmount      decimal.NullDecimal
	LastPaymentDate        *time.Time
	NextPaymentDueDate     *time.Time
	OriginationDate        *time.Time
	OriginationPrincipal   decimal.NullDecimal
	InterestRatePercentage decimal.NullDecimal
	IsOverdue              *bool
	Raw                    []byte
}

// GetLiabilities fetches credit, student loan and mortgage terms.
func (c *Client) GetLiabilities(ctx context.Context, accessToken string) ([]Liability, error) {
	resp, _, err := c.api.LiabilitiesGet(ctx).
		LiabilitiesGetRequest(*plaidapi.NewLiabilitiesGetRequest(accessToken)).
		Execute()
	if err != nil {
		return nil, wrapErr("get liabilities", err)
	}

	liabilities := resp.GetLiabilities()
	out := make([]Liability, 0)

	for _, card := range liabilities.GetCredit() {
		raw, _ := json.Marshal(card)
		l := Liability{
			PlaidAccountID:     card.GetAccountId(),
			Kind:               "credit",
			Balance:            money(card.LastStatementBalance.Get()),
			MinimumPayment:     money(card.MinimumPaymentAmount.Get()),
			LastPaymentAmount:  money(card.LastPaymentAmount.Get()),
			LastPaymentDate:    parseOptionalDate(card.GetLastPaymentDate()),
			NextPaymentDueDate: parseOptionalDate(card.GetNextPaymentDueDate()),
			IsOverdue:          card.IsOverdue.Get(),
			Raw:                raw,
		}
		// A card can carry several APRs (purchases, cash advances, balance
		// transfers). The purchase APR is the one that describes everyday use,
		// so it is preferred; otherwise take the first reported.
		if aprs := card.GetAprs(); len(aprs) > 0 {
			chosen := aprs[0]
			for _, apr := range aprs {
				if apr.GetAprType() == "purchase_apr" {
					chosen = apr
					break
				}
			}
			pct := chosen.GetAprPercentage()
			l.APR = money(&pct)
			l.APRType = optionalString(chosen.GetAprType())
		}
		out = append(out, l)
	}

	for _, loan := range liabilities.GetStudent() {
		raw, _ := json.Marshal(loan)
		rate := loan.GetInterestRatePercentage()
		out = append(out, Liability{
			PlaidAccountID:         derefString(loan.AccountId.Get()),
			Kind:                   "student",
			Balance:                money(loan.OutstandingInterestAmount.Get()),
			MinimumPayment:         money(loan.MinimumPaymentAmount.Get()),
			LastPaymentAmount:      money(loan.LastPaymentAmount.Get()),
			LastPaymentDate:        parseOptionalDate(loan.GetLastPaymentDate()),
			NextPaymentDueDate:     parseOptionalDate(loan.GetNextPaymentDueDate()),
			OriginationDate:        parseOptionalDate(loan.GetOriginationDate()),
			OriginationPrincipal:   money(loan.OriginationPrincipalAmount.Get()),
			InterestRatePercentage: money(&rate),
			IsOverdue:              loan.IsOverdue.Get(),
			Raw:                    raw,
		})
	}

	for _, mortgage := range liabilities.GetMortgage() {
		raw, _ := json.Marshal(mortgage)
		// InterestRate is a struct (percentage + type), not a nullable scalar.
		rate := mortgage.GetInterestRate()
		out = append(out, Liability{
			PlaidAccountID:         mortgage.GetAccountId(),
			Kind:                   "mortgage",
			MinimumPayment:         money(mortgage.NextMonthlyPayment.Get()),
			LastPaymentAmount:      money(mortgage.LastPaymentAmount.Get()),
			LastPaymentDate:        parseOptionalDate(mortgage.GetLastPaymentDate()),
			NextPaymentDueDate:     parseOptionalDate(mortgage.GetNextPaymentDueDate()),
			OriginationDate:        parseOptionalDate(mortgage.GetOriginationDate()),
			OriginationPrincipal:   money(mortgage.OriginationPrincipalAmount.Get()),
			InterestRatePercentage: money(rate.Percentage.Get()),
			APRType:                optionalString(rate.GetType()),
			Raw:                    raw,
		})
	}

	return out, nil
}

func parseOptionalDate(raw string) *time.Time {
	if raw == "" {
		return nil
	}
	parsed, err := time.Parse(time.DateOnly, raw)
	if err != nil {
		return nil
	}
	return &parsed
}
