// Package plaid wraps the Plaid API with the narrow surface Ledgermancy
// needs, translating Plaid's wire types into domain values — most importantly
// converting amounts into exact decimals.
package plaid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	plaidapi "github.com/plaid/plaid-go/v40/plaid"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/config"
)

// maxTransactionHistoryDays is Plaid's maximum lookback (730 days / 2 years).
// Requested at link time because it cannot be raised afterwards.
const maxTransactionHistoryDays = 730

// Client is a thin wrapper over the generated Plaid SDK.
type Client struct {
	api      *plaidapi.PlaidApiService
	products []plaidapi.Products
	webhook  string
}

// New builds a Plaid client for the configured environment.
func New(cfg config.PlaidConfig) (*Client, error) {
	if cfg.ClientID == "" || cfg.Secret == "" {
		return nil, errors.New("PLAID_CLIENT_ID and PLAID_SECRET are required")
	}

	env, err := environment(cfg.Env)
	if err != nil {
		return nil, err
	}

	apiCfg := plaidapi.NewConfiguration()
	apiCfg.AddDefaultHeader("PLAID-CLIENT-ID", cfg.ClientID)
	apiCfg.AddDefaultHeader("PLAID-SECRET", cfg.Secret)
	apiCfg.UseEnvironment(env)
	apiCfg.HTTPClient = &http.Client{Timeout: 60 * time.Second}

	products, err := parseProducts(cfg.Products)
	if err != nil {
		return nil, err
	}

	return &Client{
		api:      plaidapi.NewAPIClient(apiCfg).PlaidApi,
		products: products,
		webhook:  cfg.WebhookURL,
	}, nil
}

func environment(name string) (plaidapi.Environment, error) {
	switch name {
	case "sandbox":
		return plaidapi.Sandbox, nil
	case "production":
		return plaidapi.Production, nil
	default:
		// Plaid retired the separate "development" environment; sandbox and
		// production are the only valid targets.
		return "", fmt.Errorf("PLAID_ENV must be sandbox or production, got %q", name)
	}
}

func parseProducts(names []string) ([]plaidapi.Products, error) {
	if len(names) == 0 {
		return nil, errors.New("at least one Plaid product must be configured")
	}
	out := make([]plaidapi.Products, 0, len(names))
	for _, name := range names {
		switch name {
		case "transactions":
			out = append(out, plaidapi.PRODUCTS_TRANSACTIONS)
		case "investments":
			out = append(out, plaidapi.PRODUCTS_INVESTMENTS)
		case "liabilities":
			out = append(out, plaidapi.PRODUCTS_LIABILITIES)
		default:
			return nil, fmt.Errorf("unsupported Plaid product %q", name)
		}
	}
	return out, nil
}

// Products reports which Plaid products this client requests at link time.
func (c *Client) Products() []string {
	out := make([]string, 0, len(c.products))
	for _, p := range c.products {
		out = append(out, string(p))
	}
	return out
}

// CreateLinkToken returns a short-lived token used to open Plaid Link.
func (c *Client) CreateLinkToken(ctx context.Context, userID, displayName string) (string, error) {
	req := plaidapi.NewLinkTokenCreateRequest(
		"Ledgermancy",
		"en",
		[]plaidapi.CountryCode{plaidapi.COUNTRYCODE_US},
	)

	user := plaidapi.LinkTokenCreateRequestUser{ClientUserId: userID}
	if displayName != "" {
		user.SetLegalName(displayName)
	}
	req.SetUser(user)
	req.SetProducts(c.products)

	// Ask for the maximum history Plaid will give.
	//
	// This is the single most consequential line in the link flow. The default
	// is 90 days, and Plaid's own documentation is explicit that "once
	// Transactions has been added to an Item, this value cannot be updated" —
	// so an item linked without it is capped at 90 days *permanently*, and the
	// only remedy is to unlink and relink the institution. Since the whole
	// point of this app is a year or more of spending history, that default
	// would quietly defeat it.
	//
	// The cost is a slower first sync, which is a one-time price worth paying.
	if HasProduct(c.Products(), ProductTransactions) {
		transactions := plaidapi.NewLinkTokenTransactions()
		transactions.SetDaysRequested(maxTransactionHistoryDays)
		req.SetTransactions(*transactions)
	}
	// Only set a webhook when one is configured; Plaid rejects an empty string.
	if c.webhook != "" {
		req.SetWebhook(c.webhook)
	}

	resp, _, err := c.api.LinkTokenCreate(ctx).LinkTokenCreateRequest(*req).Execute()
	if err != nil {
		return "", wrapErr("create link token", err)
	}
	return resp.GetLinkToken(), nil
}

// ExchangePublicToken swaps the short-lived public token Link returns for a
// long-lived access token plus its item id.
func (c *Client) ExchangePublicToken(ctx context.Context, publicToken string) (accessToken, itemID string, err error) {
	req := plaidapi.NewItemPublicTokenExchangeRequest(publicToken)
	resp, _, err := c.api.ItemPublicTokenExchange(ctx).ItemPublicTokenExchangeRequest(*req).Execute()
	if err != nil {
		return "", "", wrapErr("exchange public token", err)
	}
	return resp.GetAccessToken(), resp.GetItemId(), nil
}

// Institution describes the bank behind an item.
type Institution struct {
	ID   string
	Name string
}

// GetInstitution resolves the institution for an access token. A failure here
// is not fatal to linking — the item still works, it just displays unnamed —
// so callers may ignore the error.
func (c *Client) GetInstitution(ctx context.Context, accessToken string) (Institution, error) {
	itemResp, _, err := c.api.ItemGet(ctx).
		ItemGetRequest(*plaidapi.NewItemGetRequest(accessToken)).Execute()
	if err != nil {
		return Institution{}, wrapErr("get item", err)
	}

	id := itemResp.Item.GetInstitutionId()
	if id == "" {
		return Institution{}, nil
	}

	instResp, _, err := c.api.InstitutionsGetById(ctx).
		InstitutionsGetByIdRequest(*plaidapi.NewInstitutionsGetByIdRequest(
			id, []plaidapi.CountryCode{plaidapi.COUNTRYCODE_US})).Execute()
	if err != nil {
		return Institution{ID: id}, wrapErr("get institution", err)
	}
	return Institution{ID: id, Name: instResp.Institution.GetName()}, nil
}

// Account is a Plaid account normalized for storage.
type Account struct {
	PlaidAccountID   string
	Name             string
	OfficialName     *string
	Mask             *string
	Type             string
	Subtype          *string
	CurrentBalance   decimal.NullDecimal
	AvailableBalance decimal.NullDecimal
	CreditLimit      decimal.NullDecimal
	Currency         string
}

// GetAccounts returns the item's accounts with current balances.
func (c *Client) GetAccounts(ctx context.Context, accessToken string) ([]Account, error) {
	resp, _, err := c.api.AccountsGet(ctx).
		AccountsGetRequest(*plaidapi.NewAccountsGetRequest(accessToken)).Execute()
	if err != nil {
		return nil, wrapErr("get accounts", err)
	}

	out := make([]Account, 0, len(resp.GetAccounts()))
	for _, a := range resp.GetAccounts() {
		out = append(out, convertAccount(a))
	}
	return out, nil
}

func convertAccount(a plaidapi.AccountBase) Account {
	bal := a.GetBalances()

	currency := bal.GetIsoCurrencyCode()
	if currency == "" {
		currency = "USD"
	}

	var subtype *string
	if s := a.GetSubtype(); s != "" {
		v := string(s)
		subtype = &v
	}

	return Account{
		PlaidAccountID:   a.GetAccountId(),
		Name:             a.GetName(),
		OfficialName:     optionalString(a.GetOfficialName()),
		Mask:             optionalString(a.GetMask()),
		Type:             string(a.GetType()),
		Subtype:          subtype,
		CurrentBalance:   money(bal.Current.Get()),
		AvailableBalance: money(bal.Available.Get()),
		CreditLimit:      money(bal.Limit.Get()),
		Currency:         currency,
	}
}

// SyncPage is one page of /transactions/sync output.
type SyncPage struct {
	Added      []Transaction
	Modified   []Transaction
	Removed    []string
	NextCursor string
	HasMore    bool
}

// Transaction is a Plaid transaction normalized for storage.
type Transaction struct {
	PlaidTransactionID   string
	PlaidAccountID       string
	Amount               decimal.Decimal
	Currency             string
	Date                 time.Time
	AuthorizedDate       *time.Time
	Name                 string
	MerchantName         *string
	Pending              bool
	PendingTransactionID *string
	PFCPrimary           *string
	PFCDetailed          *string
	Raw                  []byte
}

// RefreshTransactions asks Plaid to pull fresh data from the institution now.
//
// This is necessary because /transactions/sync only ever returns what Plaid
// has already cached. Plaid refreshes a background item on its own schedule —
// typically a few times a day, but observed to stall for far longer on some
// institutions — and until it does, syncing on any cadence just re-reads the
// same rows. This is the only call that makes Plaid go to the bank.
//
// It returns as soon as Plaid accepts the request; the pull itself is
// asynchronous and lands via a SYNC_UPDATES_AVAILABLE webhook. Plaid rate
// limits this per item, so callers must space it out rather than calling it on
// every sync.
func (c *Client) RefreshTransactions(ctx context.Context, accessToken string) error {
	req := plaidapi.NewTransactionsRefreshRequest(accessToken)
	if _, _, err := c.api.TransactionsRefresh(ctx).TransactionsRefreshRequest(*req).Execute(); err != nil {
		return wrapErr("refresh transactions", err)
	}
	return nil
}

// SyncTransactions fetches one page of updates. Pass an empty cursor to start
// from the beginning of the item's history.
func (c *Client) SyncTransactions(ctx context.Context, accessToken, cursor string) (SyncPage, error) {
	req := plaidapi.NewTransactionsSyncRequest(accessToken)
	if cursor != "" {
		req.SetCursor(cursor)
	}
	// 500 is Plaid's maximum page size. Larger pages mean fewer round trips
	// during the initial backfill, which can span two years of history.
	req.SetCount(500)

	resp, _, err := c.api.TransactionsSync(ctx).TransactionsSyncRequest(*req).Execute()
	if err != nil {
		return SyncPage{}, wrapErr("sync transactions", err)
	}

	page := SyncPage{
		NextCursor: resp.GetNextCursor(),
		HasMore:    resp.GetHasMore(),
		Added:      make([]Transaction, 0, len(resp.GetAdded())),
		Modified:   make([]Transaction, 0, len(resp.GetModified())),
		Removed:    make([]string, 0, len(resp.GetRemoved())),
	}

	for _, t := range resp.GetAdded() {
		converted, err := convertTransaction(t)
		if err != nil {
			return SyncPage{}, err
		}
		page.Added = append(page.Added, converted)
	}
	for _, t := range resp.GetModified() {
		converted, err := convertTransaction(t)
		if err != nil {
			return SyncPage{}, err
		}
		page.Modified = append(page.Modified, converted)
	}
	for _, r := range resp.GetRemoved() {
		page.Removed = append(page.Removed, r.GetTransactionId())
	}

	return page, nil
}

func convertTransaction(t plaidapi.Transaction) (Transaction, error) {
	date, err := time.Parse(time.DateOnly, t.GetDate())
	if err != nil {
		return Transaction{}, fmt.Errorf("parse transaction date %q: %w", t.GetDate(), err)
	}

	var authorized *time.Time
	if raw := t.GetAuthorizedDate(); raw != "" {
		if parsed, err := time.Parse(time.DateOnly, raw); err == nil {
			authorized = &parsed
		}
	}

	currency := t.GetIsoCurrencyCode()
	if currency == "" {
		currency = "USD"
	}

	var pfcPrimary, pfcDetailed *string
	if pfc, ok := t.GetPersonalFinanceCategoryOk(); ok && pfc != nil {
		p, d := pfc.GetPrimary(), pfc.GetDetailed()
		if p != "" {
			pfcPrimary = &p
		}
		if d != "" {
			pfcDetailed = &d
		}
	}

	// Keep the untouched payload so anything derived can be recomputed later
	// without re-fetching from Plaid.
	raw, err := json.Marshal(t)
	if err != nil {
		return Transaction{}, fmt.Errorf("marshal raw transaction: %w", err)
	}

	return Transaction{
		PlaidTransactionID:   t.GetTransactionId(),
		PlaidAccountID:       t.GetAccountId(),
		Amount:               amountToDecimal(t.GetAmount()),
		Currency:             currency,
		Date:                 date,
		AuthorizedDate:       authorized,
		Name:                 t.GetName(),
		MerchantName:         optionalString(t.GetMerchantName()),
		Pending:              t.GetPending(),
		PendingTransactionID: optionalString(t.GetPendingTransactionId()),
		PFCPrimary:           pfcPrimary,
		PFCDetailed:          pfcDetailed,
		Raw:                  raw,
	}, nil
}

// amountToDecimal converts a Plaid amount into an exact decimal.
//
// The generated SDK decodes amounts as float64, so a float is unavoidable at
// this boundary — it is the only place one appears in the money path.
// decimal.NewFromFloat is exact for these values because it formats with
// strconv 'f'/-1, which produces the shortest decimal that round-trips to the
// same float64. For currency amounts (at most a few significant digits after
// the point) that is always the literal value Plaid sent: 12.34 becomes
// exactly 12.34, not 12.339999999999999. Everything downstream is decimal.
func amountToDecimal(f float64) decimal.Decimal {
	return decimal.NewFromFloat(f)
}

// money converts an optional Plaid balance into a nullable decimal.
func money(f *float64) decimal.NullDecimal {
	if f == nil {
		return decimal.NullDecimal{}
	}
	return decimal.NullDecimal{Decimal: amountToDecimal(*f), Valid: true}
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// wrapErr surfaces Plaid's structured error details, which carry the error
// code needed to distinguish "user must re-authenticate" from a transient
// failure worth retrying.
func wrapErr(what string, err error) error {
	var plaidErr plaidapi.GenericOpenAPIError
	if errors.As(err, &plaidErr) {
		var body struct {
			ErrorType    string `json:"error_type"`
			ErrorCode    string `json:"error_code"`
			ErrorMessage string `json:"error_message"`
		}
		if json.Unmarshal(plaidErr.Body(), &body) == nil && body.ErrorCode != "" {
			return fmt.Errorf("%s: plaid %s/%s: %s",
				what, body.ErrorType, body.ErrorCode, body.ErrorMessage)
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}
