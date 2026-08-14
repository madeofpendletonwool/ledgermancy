package plaid

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/categorize"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/crypto"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/networth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/rules"
)

// maxBackfillPages bounds a single backfill run. Plaid offers up to ~24 months
// of history at 500 transactions per page, so this is far above any real
// household's volume and exists purely to stop a pathological loop.
const maxBackfillPages = 200

// Syncer pulls Plaid data into the database.
type Syncer struct {
	Pool    *pgxpool.Pool
	Queries *dbgen.Queries
	Client  *Client
	Cipher  *crypto.Cipher
}

// SyncResult reports what a sync did, for logging and for the UI.
type SyncResult struct {
	ItemID           uuid.UUID
	Pages            int
	Added            int
	Modified         int
	Removed          int
	AccountsUpserted int
	Categorised      int
	// RulesChanged is how many transactions the household's own IF-THEN rules
	// moved. Counted separately from Categorised because they answer different
	// questions and a user debugging "why did this get tagged?" needs to know
	// which of the two did it.
	RulesChanged int
	Holdings         int
	Securities       int
	InvestmentTxns   int
	Liabilities      int
	BackfillComplete bool
	Earliest         *time.Time
	Latest           *time.Time
}

// SyncItem pulls every pending update for one item, following the cursor until
// Plaid reports no more pages.
//
// The loop is the historical backfill: on a brand-new item the cursor is empty
// and Plaid replays the item's full available history (up to ~24 months),
// paging until has_more is false. On subsequent runs the stored cursor means
// the same code only fetches what changed.
func (s *Syncer) SyncItem(ctx context.Context, itemID uuid.UUID) (SyncResult, error) {
	result := SyncResult{ItemID: itemID}

	item, err := s.Queries.GetPlaidItem(ctx, itemID)
	if err != nil {
		return result, fmt.Errorf("load item: %w", err)
	}

	accessToken, err := s.Cipher.OpenString(item.AccessTokenEncrypted)
	if err != nil {
		return result, fmt.Errorf("decrypt access token: %w", err)
	}

	// Refresh accounts first: transactions reference them by foreign key, so
	// a transaction on a newly-opened account would otherwise have nowhere to
	// land.
	accountIDs, kinds, err := s.syncAccounts(ctx, item.ID, accessToken)
	if err != nil {
		return result, err
	}
	result.AccountsUpserted = len(accountIDs)

	// Optional modules. Each runs when the operator enabled the product and the
	// item has accounts it could apply to — see the gate note in modules.go for
	// why neither of those is plaid_items.products. A module failing is logged
	// but does not fail the sync: transactions are the priority and must still
	// land.
	//
	// A skip is logged rather than passed over. The modules do not know their
	// item id, so this is the only place the two can be said together, and
	// "which item got no liabilities, and why" is precisely the question that
	// had no answer when a household's debts silently never arrived.
	if mod, err := s.SyncInvestments(ctx, accessToken, accountIDs, kinds); err != nil {
		slog.Error("investments sync", "error", err, "item_id", item.ID)
	} else {
		result.Holdings, result.Securities = mod.Holdings, mod.Securities
		result.InvestmentTxns = mod.InvestmentTransactions
		if mod.Skipped {
			slog.Info("investments module skipped", "item_id", item.ID, "reason", mod.SkipReason)
		}
	}
	if mod, err := s.SyncLiabilities(ctx, accessToken, accountIDs, kinds); err != nil {
		slog.Error("liabilities sync", "error", err, "item_id", item.ID)
	} else {
		result.Liabilities = mod.Liabilities
		if mod.Skipped {
			slog.Info("liabilities module skipped", "item_id", item.ID, "reason", mod.SkipReason)
		}
	}

	// Transactions are the priority product and always run.
	if !HasProduct(item.Products, ProductTransactions) {
		return result, nil
	}

	cursor := ""
	if item.SyncCursor != nil {
		cursor = *item.SyncCursor
	}

	for page := 0; page < maxBackfillPages; page++ {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		default:
		}

		batch, err := s.Client.SyncTransactions(ctx, accessToken, cursor)
		if err != nil {
			s.recordItemError(ctx, item.ID, err)
			return result, err
		}
		result.Pages++

		if err := s.applyPage(ctx, item.ID, accountIDs, batch); err != nil {
			return result, err
		}

		result.Added += len(batch.Added)
		result.Modified += len(batch.Modified)
		result.Removed += len(batch.Removed)

		// Advance the cursor only after the page is committed. If the process
		// dies mid-page, the next run replays that page — which the upsert
		// makes harmless — rather than losing transactions.
		if err := s.Queries.UpdateItemCursor(ctx, dbgen.UpdateItemCursorParams{
			ID: item.ID, SyncCursor: &batch.NextCursor,
		}); err != nil {
			return result, fmt.Errorf("save cursor: %w", err)
		}
		cursor = batch.NextCursor

		if !batch.HasMore {
			break
		}
	}

	if !item.BackfillComplete {
		if err := s.Queries.MarkBackfillComplete(ctx, item.ID); err != nil {
			return result, fmt.Errorf("mark backfill complete: %w", err)
		}
	}
	result.BackfillComplete = true

	// Categorise what just arrived, so the data is immediately usable rather
	// than sitting uncategorised until some later pass.
	if householdID, err := s.Queries.GetHouseholdForItem(ctx, item.ID); err != nil {
		slog.Warn("could not resolve household for categorisation", "error", err, "item_id", item.ID)
	} else if n, err := categorize.CategoriseHousehold(ctx, s.Queries, householdID, 500); err != nil {
		// Categorisation failing does not invalidate the sync; the rows are
		// stored and a later pass will pick them up.
		slog.Error("categorise after sync", "error", err, "item_id", item.ID)
	} else {
		result.Categorised = n
	}

	// Pair internal transfers: a debit on one of the household's accounts
	// matched to an equal credit on another, within a date window. This is
	// name-independent, so it catches transfers whose two legs carry different
	// counterparty names (and legs the merchant cache misfiled). Runs after
	// categorisation so legs have their categories before the pairer links them.
	if hid, err := s.Queries.GetHouseholdForItem(ctx, item.ID); err != nil {
		slog.Warn("could not resolve household for transfer pairing", "error", err, "item_id", item.ID)
	} else if paired, err := categorize.PairTransfers(ctx, s.Queries, hid, time.Now().UTC(), categorize.SyncPairLookback); err != nil {
		slog.Error("pair transfers after sync", "error", err, "item_id", item.ID)
	} else if paired > 0 {
		result.Categorised += paired
	}

	// The household's own IF-THEN rules, LAST.
	//
	// The order is the contract between the two systems, and is stated in
	// internal/rules' package doc as well as here. categorize answers one
	// question — "which category?" — from a fixed precedence; the rule engine
	// answers the broader "what else should be true of this row?" on top of
	// whatever category came out of that. Running it earlier would mean a
	// rule's set-category was immediately overwritten by the resolver, which is
	// the exact bug this ordering exists to prevent. A manual category still
	// beats both: the engine refuses to touch category_source='manual'.
	//
	// A nil viewer means the whole household is in scope. There is nobody on
	// the other end of a sync to scope to, exactly as CategoriseHousehold above
	// already works, and nothing here is returned to a user.
	if hid, err := s.Queries.GetHouseholdForItem(ctx, item.ID); err != nil {
		slog.Warn("could not resolve household for rules", "error", err, "item_id", item.ID)
	} else if engine, err := rules.Load(ctx, s.Queries, hid); err != nil {
		slog.Error("load rules after sync", "error", err, "item_id", item.ID)
	} else if len(engine.Rules()) > 0 {
		// Rules failing does not invalidate the sync: the rows are stored, and
		// the user can re-run a rule over history at any time.
		if summary, err := engine.RunHousehold(ctx, s.Queries, hid, nil); err != nil {
			slog.Error("run rules after sync", "error", err, "item_id", item.ID)
		} else {
			result.RulesChanged = summary.Changed
		}
	}

	// Record today's net worth now that balances are fresh. Balances carry no
	// history, so if this is not written down as it goes there is no trend to
	// show later.
	if householdID, err := s.Queries.GetHouseholdForItem(ctx, item.ID); err == nil {
		if _, err := networth.Snapshot(ctx, s.Queries, householdID, nil); err != nil {
			slog.Error("net worth snapshot", "error", err, "item_id", item.ID)
		}
		// The per-account analog: write today's balance to each Plaid account's
		// own history so an account-level trend exists, not just the household
		// total. Same trigger and same caveat as the snapshot above — the trend
		// for an account starts the day the app began recording it, not earlier.
		if _, err := networth.SnapshotAccountBalances(ctx, s.Queries, householdID); err != nil {
			slog.Error("account balance snapshot", "error", err, "item_id", item.ID)
		}
	}

	// Report the history actually retrieved, so the caller can warn when an
	// institution returned less than a usable year.
	earliest, latest, err := s.historyRange(ctx, item.ID)
	if err != nil {
		// A missing range is cosmetic; the sync itself succeeded.
		slog.Warn("could not determine history range", "error", err, "item_id", item.ID)
	}
	result.Earliest, result.Latest = earliest, latest

	return result, nil
}

// RefreshItem asks Plaid to pull fresh data from the institution for one item.
//
// This is separate from SyncItem on purpose. SyncItem reads Plaid's cache and
// is safe to run hourly; this makes Plaid go to the bank and is rate limited
// per item, so only the periodic sweep calls it, and only when the item is due.
//
// The pull is asynchronous: Plaid answers immediately and delivers the result
// later via a SYNC_UPDATES_AVAILABLE webhook, which enqueues the sync that
// actually stores it.
func (s *Syncer) RefreshItem(ctx context.Context, itemID uuid.UUID) error {
	item, err := s.Queries.GetPlaidItem(ctx, itemID)
	if err != nil {
		return fmt.Errorf("load item: %w", err)
	}
	if !HasProduct(item.Products, ProductTransactions) {
		return nil
	}

	accessToken, err := s.Cipher.OpenString(item.AccessTokenEncrypted)
	if err != nil {
		return fmt.Errorf("decrypt access token: %w", err)
	}

	// Marked before the call, not after: a request that Plaid accepted but that
	// failed on our side has still consumed the item's quota, and retrying it
	// immediately would only earn a rate-limit error.
	if err := s.Queries.MarkItemRefreshed(ctx, item.ID); err != nil {
		return fmt.Errorf("mark item refreshed: %w", err)
	}

	if err := s.Client.RefreshTransactions(ctx, accessToken); err != nil {
		// Deliberately not recorded as a generic item error. recordItemError
		// sets status to 'error', and the sweep only considers active items —
		// so treating an expected, benign rejection like a rate limit as an
		// item fault would stop the item syncing altogether. Re-authentication
		// is the one case that genuinely needs recording, and the sync running
		// straight after this would surface any real fault anyway.
		if strings.Contains(err.Error(), "ITEM_LOGIN_REQUIRED") {
			s.recordItemError(ctx, item.ID, err)
		}
		return err
	}
	return nil
}

// historyRange returns the oldest and newest transaction dates for an item,
// or (nil, nil) when it has none yet.
//
// This is hand-written rather than generated: sqlc infers min()/max() over a
// NOT NULL column as NOT NULL, but both are NULL for an item with no
// transactions, which would fail to scan.
func (s *Syncer) historyRange(ctx context.Context, itemID uuid.UUID) (*time.Time, *time.Time, error) {
	var earliest, latest *time.Time
	err := s.Pool.QueryRow(ctx, `
		SELECT min(t.date), max(t.date)
		FROM transactions t
		JOIN accounts a ON a.id = t.account_id
		WHERE a.plaid_item_id = $1`, itemID).Scan(&earliest, &latest)
	if err != nil {
		return nil, nil, err
	}
	return earliest, latest, nil
}

// syncAccounts refreshes the item's accounts and returns a lookup from Plaid
// account id to our primary key.
// syncAccounts upserts the item's accounts and reports both their ids and what
// kinds of account the item holds. The kinds decide which optional modules are
// worth calling — an item with no credit or loan account is never asked about
// its liabilities.
func (s *Syncer) syncAccounts(ctx context.Context, itemID uuid.UUID, accessToken string) (map[string]uuid.UUID, itemAccountKinds, error) {
	var kinds itemAccountKinds

	accounts, err := s.Client.GetAccounts(ctx, accessToken)
	if err != nil {
		s.recordItemError(ctx, itemID, err)
		return nil, kinds, err
	}

	ids := make(map[string]uuid.UUID, len(accounts))
	for _, a := range accounts {
		kinds.noteAccountType(a.Type)
		row, err := s.Queries.UpsertAccount(ctx, dbgen.UpsertAccountParams{
			PlaidItemID:      &itemID,
			PlaidAccountID:   plaidID(a.PlaidAccountID),
			Name:             a.Name,
			OfficialName:     a.OfficialName,
			Mask:             a.Mask,
			Type:             a.Type,
			Subtype:          a.Subtype,
			CurrentBalance:   a.CurrentBalance,
			AvailableBalance: a.AvailableBalance,
			CreditLimit:      a.CreditLimit,
			Currency:         a.Currency,
		})
		if err != nil {
			return nil, kinds, fmt.Errorf("upsert account %s: %w", a.PlaidAccountID, err)
		}
		ids[a.PlaidAccountID] = row.ID
	}
	return ids, kinds, nil
}

// applyPage writes one sync page in a single transaction, so a page is either
// fully applied or not at all.
func (s *Syncer) applyPage(ctx context.Context, itemID uuid.UUID, accountIDs map[string]uuid.UUID, page SyncPage) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.Queries.WithTx(tx)

	for _, t := range append(append([]Transaction{}, page.Added...), page.Modified...) {
		accountID, ok := accountIDs[t.PlaidAccountID]
		if !ok {
			// Plaid can return a transaction for an account it did not list
			// (for example an account the user just unlinked). Skipping is
			// correct: there is no valid parent row to attach it to.
			slog.Warn("skipping transaction for unknown account",
				"plaid_account_id", t.PlaidAccountID,
				"plaid_transaction_id", t.PlaidTransactionID)
			continue
		}

		merchantKey := MerchantKey(derefString(t.MerchantName), t.Name)

		if _, err := q.UpsertTransaction(ctx, dbgen.UpsertTransactionParams{
			AccountID:            accountID,
			PlaidTransactionID:   &t.PlaidTransactionID,
			Amount:               t.Amount,
			Currency:             t.Currency,
			Date:                 t.Date,
			AuthorizedDate:       t.AuthorizedDate,
			Name:                 t.Name,
			MerchantName:         t.MerchantName,
			MerchantKey:          optionalString(merchantKey),
			Pending:              t.Pending,
			PendingTransactionID: t.PendingTransactionID,
			PlaidPfcPrimary:      t.PFCPrimary,
			PlaidPfcDetailed:     t.PFCDetailed,
			Raw:                  t.Raw,
		}); err != nil {
			return fmt.Errorf("upsert transaction %s: %w", t.PlaidTransactionID, err)
		}

		// A posted transaction supersedes the pending one it replaces.
		// Without this the pending charge and the posted charge would both
		// count toward the month's spending.
		if t.PendingTransactionID != nil && !t.Pending {
			if _, err := q.DeletePendingSupersededBy(ctx, t.PendingTransactionID); err != nil {
				return fmt.Errorf("remove superseded pending transaction: %w", err)
			}
		}
	}

	for _, removedID := range page.Removed {
		id := removedID
		if _, err := q.DeleteTransactionByPlaidID(ctx, &id); err != nil {
			return fmt.Errorf("delete transaction %s: %w", id, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit page: %w", err)
	}
	return nil
}

// recordItemError marks an item as needing attention. ITEM_LOGIN_REQUIRED
// means the user must re-authenticate through Link; everything else is
// recorded but left active so a transient failure retries.
func (s *Syncer) recordItemError(ctx context.Context, itemID uuid.UUID, syncErr error) {
	status, code := "error", syncErr.Error()
	// wrapErr formats Plaid failures as "…: plaid <type>/<code>: <message>",
	// so the code is matchable in the error string.
	if strings.Contains(syncErr.Error(), "ITEM_LOGIN_REQUIRED") {
		status = "login_required"
		code = "ITEM_LOGIN_REQUIRED"
	}

	if err := s.Queries.SetItemStatus(ctx, dbgen.SetItemStatusParams{
		ID: itemID, Status: status, ErrorCode: &code,
	}); err != nil {
		slog.Error("record item error", "error", err, "item_id", itemID)
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
