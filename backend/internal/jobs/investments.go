package jobs

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/riverqueue/river"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// --------------------------------------------------------------------------
// Investment value snapshots
// --------------------------------------------------------------------------

// SnapshotInvestmentsArgs records today's value for every investment account.
type SnapshotInvestmentsArgs struct{}

func (SnapshotInvestmentsArgs) Kind() string { return "investment_snapshot" }

// SnapshotInvestmentsWorker writes one investment_snapshots row per account.
//
// This exists for the same reason the net-worth snapshot does, and it is worth
// restating because the consequence is permanent: Plaid serves what an account
// holds *now* and keeps no history. Nothing can backfill this. A day not
// recorded is a day whose value is gone, so the job runs on the same schedule as
// the net-worth snapshot and on start, and the unique (account_id, as_of)
// constraint makes re-running it free.
type SnapshotInvestmentsWorker struct {
	river.WorkerDefaults[SnapshotInvestmentsArgs]
	Queries *dbgen.Queries
}

func (w *SnapshotInvestmentsWorker) Work(ctx context.Context, job *river.Job[SnapshotInvestmentsArgs]) error {
	rows, err := w.Queries.ListInvestmentAccountValues(ctx)
	if err != nil {
		return fmt.Errorf("list investment account values: %w", err)
	}

	written := 0
	for _, r := range rows {
		// No holding reported a basis, so the sum is a COALESCE artefact rather
		// than a real zero. Storing NULL keeps "unknown" distinguishable from
		// "$0.00 invested", which is what the unrealised-gain figure depends on.
		basis := decimal.NullDecimal{}
		if r.BasisHoldings > 0 {
			basis = decimal.NullDecimal{Decimal: r.CostBasis, Valid: true}
		}

		if err := w.Queries.UpsertInvestmentSnapshot(ctx, dbgen.UpsertInvestmentSnapshotParams{
			AccountID:   r.AccountID,
			MarketValue: r.MarketValue,
			CostBasis:   basis,
		}); err != nil {
			return fmt.Errorf("upsert investment snapshot for %s: %w", r.AccountID, err)
		}
		written++
	}

	if written > 0 {
		slog.Info("investment snapshots written", "accounts", written)
	}
	return nil
}

// --------------------------------------------------------------------------
// Benchmark prices
// --------------------------------------------------------------------------

// FetchBenchmarksArgs pulls end-of-day closes for the configured tickers.
type FetchBenchmarksArgs struct{}

func (FetchBenchmarksArgs) Kind() string { return "fetch_benchmarks" }

// stooqDailyCSV is Stooq's free end-of-day CSV endpoint. No key, no account, no
// per-request quota worth worrying about at four tickers a day.
//
// Chosen over Yahoo's chart endpoint deliberately: Yahoo's is undocumented,
// requires a rotating crumb/cookie, and breaks without notice. A benchmark that
// silently stops updating is worse than one that was never configured.
const stooqDailyCSV = "https://stooq.com/q/d/l/?s=%s&i=d"

// benchmarkFetchTimeout bounds one ticker's fetch. Generous for a small CSV,
// short enough that four unreachable tickers cannot hold the queue.
const benchmarkFetchTimeout = 20 * time.Second

// benchmarkHistoryDays is how much of each returned series is stored. Stooq
// returns the full history; five years covers the longest period the
// Investments page offers.
const benchmarkHistoryDays = 5 * 365

// FetchBenchmarksWorker populates asset_prices.
//
// This is the app's ONLY outbound call to a host that is neither Plaid nor the
// configured AI provider, so it is off unless the operator turns it on. When it
// is off the worker is not even registered. When it is on, a failure degrades to
// a missing series and never fails the job: a benchmark line is a nice-to-have
// on one chart, and losing it must not retry-storm or mask a real problem.
type FetchBenchmarksWorker struct {
	river.WorkerDefaults[FetchBenchmarksArgs]
	Queries *dbgen.Queries
	Tickers []string
	// HTTP is injectable so the failure path can be tested without a network.
	HTTP *http.Client
}

func (w *FetchBenchmarksWorker) Work(ctx context.Context, job *river.Job[FetchBenchmarksArgs]) error {
	client := w.HTTP
	if client == nil {
		client = &http.Client{Timeout: benchmarkFetchTimeout}
	}

	cutoff := time.Now().AddDate(0, 0, -benchmarkHistoryDays)
	fetched, failed := 0, 0

	for _, ticker := range w.Tickers {
		n, err := w.fetchOne(ctx, client, ticker, cutoff)
		if err != nil {
			// Logged, not returned. Returning would retry the whole set with
			// backoff because one index was unreachable, and the other three
			// series are already stored.
			slog.Warn("fetch benchmark", "error", err, "ticker", ticker)
			failed++
			continue
		}
		fetched += n
	}

	slog.Info("benchmark prices fetched",
		"tickers", len(w.Tickers), "rows", fetched, "failed", failed)
	return nil
}

// fetchOne downloads and stores one ticker's series.
func (w *FetchBenchmarksWorker) fetchOne(
	ctx context.Context,
	client *http.Client,
	ticker string,
	cutoff time.Time,
) (int, error) {
	url := fmt.Sprintf(stooqDailyCSV, strings.ToLower(ticker)+".us")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	rows, err := parseStooqCSV(resp.Body)
	if err != nil {
		return 0, err
	}

	stored := 0
	upper := strings.ToUpper(ticker)
	for _, r := range rows {
		if r.date.Before(cutoff) {
			continue
		}
		if err := w.Queries.UpsertAssetPrice(ctx, dbgen.UpsertAssetPriceParams{
			Ticker: upper, AsOf: r.date, Close: r.close,
		}); err != nil {
			return stored, fmt.Errorf("upsert asset price: %w", err)
		}
		stored++
	}
	return stored, nil
}

type stooqRow struct {
	date  time.Time
	close decimal.Decimal
}

// maxBenchmarkCSVBytes caps what is read from a third-party host. Five years of
// daily rows is well under a megabyte; the limit is here so an unexpected
// response cannot be read into memory unbounded.
const maxBenchmarkCSVBytes = 8 << 20

// parseStooqCSV reads Stooq's Date,Open,High,Low,Close,Volume format.
//
// Prices are parsed straight from their decimal STRING representation rather
// than through a float, so a close of 412.35 is stored as exactly that.
func parseStooqCSV(body io.Reader) ([]stooqRow, error) {
	reader := csv.NewReader(io.LimitReader(body, maxBenchmarkCSVBytes))
	// The header row and any malformed line are handled below; a varying field
	// count should not abort the whole file.
	reader.FieldsPerRecord = -1

	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse csv: %w", err)
	}
	if len(records) < 2 {
		// Stooq answers an unknown symbol with a one-line body rather than a
		// non-200, so an absent series arrives looking like a success.
		return nil, fmt.Errorf("no price rows returned")
	}

	header := records[0]
	dateCol, closeCol := -1, -1
	for i, name := range header {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "date":
			dateCol = i
		case "close":
			closeCol = i
		}
	}
	if dateCol < 0 || closeCol < 0 {
		return nil, fmt.Errorf("csv is missing a Date or Close column")
	}

	out := make([]stooqRow, 0, len(records)-1)
	for _, rec := range records[1:] {
		if len(rec) <= dateCol || len(rec) <= closeCol {
			continue
		}
		date, err := time.Parse(time.DateOnly, strings.TrimSpace(rec[dateCol]))
		if err != nil {
			continue
		}
		close, err := decimal.NewFromString(strings.TrimSpace(rec[closeCol]))
		if err != nil {
			continue
		}
		out = append(out, stooqRow{date: date, close: close})
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("no readable price rows")
	}
	return out, nil
}
