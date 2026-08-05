package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/riverqueue/river"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/reporting"
)

// CPI-U refresh (doc 27).
//
// # What this job is not
//
// It is not how the app gets a CPI series. Migration 00052 seeds the series
// from January 2010, and every inflation-adjusted view works with this job
// switched off — which is why it CAN be switched off by default. All this
// fetches is the tail: the one month BLS has published since the seed was cut.
//
// # Two things about BLS that shape the code
//
// BLS publishes a month around the middle of the FOLLOWING month, and it
// REVISES. Both are handled by upserting on period rather than inserting: a
// month arriving late is simply a month that was not there yet, and a revision
// supersedes what it revises. Neither is an error condition.
//
// It also, occasionally, does not publish at all. October 2025 has no index and
// never will — the 2025 lapse in appropriations stopped collection — and the
// API returns that month with a value of "-" and a footnote. Rows like that are
// SKIPPED, not stored as zero and not interpolated. reporting.Real then refuses
// to deflate a figure dated that month, which is the honest outcome.

// RefreshCPIArgs pulls the recent tail of the CPI-U series.
type RefreshCPIArgs struct{}

func (RefreshCPIArgs) Kind() string { return "refresh_cpi" }

// blsSeriesID is CPI-U, U.S. city average, all items, NOT seasonally adjusted.
//
// The NSA series rather than the SA one (CUSR0000SA0) on purpose: BLS revises
// seasonal factors every year for five years running, so a deflated figure a
// user saw last month would quietly change under them. NSA is the published
// index of record and does not move once released.
const blsSeriesID = "CUUR0000SA0"

// blsTimeseriesV1 is the unregistered public endpoint. It serves a limited
// number of series and years without an API key, which is exactly the shape of
// this request: one series, two years.
//
// v1 rather than v2 deliberately — v2 requires a registration key, and making
// an operator sign up with a government API to keep a bundled table current
// would be a worse trade than the slightly lower quota.
const blsTimeseriesV1 = "https://api.bls.gov/publicAPI/v1/timeseries/data/"

// cpiFetchTimeout bounds the single request. Generous for a small JSON body,
// short enough that an unreachable host cannot hold the queue.
const cpiFetchTimeout = 30 * time.Second

// maxCPIResponseBytes caps what is read from the third-party host. Two years of
// monthly points is a few kilobytes; the limit is here so an unexpected
// response cannot be read into memory unbounded. Same guard as the benchmark
// fetch applies to Stooq.
const maxCPIResponseBytes = 4 << 20

// RefreshCPIWorker updates cpi_series from BLS.
//
// Registered only when the operator has opted in, for the same reason as
// FetchBenchmarksWorker: an enqueued job must not be able to make an outbound
// request the operator never authorised.
type RefreshCPIWorker struct {
	river.WorkerDefaults[RefreshCPIArgs]
	Queries *dbgen.Queries
	// HTTP is injectable so the parse and failure paths can be tested without a
	// network.
	HTTP *http.Client
}

func (w *RefreshCPIWorker) Work(ctx context.Context, job *river.Job[RefreshCPIArgs]) error {
	client := w.HTTP
	if client == nil {
		client = &http.Client{Timeout: cpiFetchTimeout}
	}

	now := time.Now().UTC()
	// Two calendar years, which is more than the tail needs and is the point:
	// it re-reads months already stored so a BLS revision to one of them is
	// picked up rather than sitting corrected-upstream and stale here forever.
	points, err := fetchCPI(ctx, client, blsTimeseriesV1, now.Year()-1, now.Year())
	if err != nil {
		return fmt.Errorf("fetch cpi: %w", err)
	}

	stored := 0
	for _, p := range points {
		if _, err := w.Queries.UpsertCPIPoint(ctx, dbgen.UpsertCPIPointParams{
			Period:     p.Period,
			IndexValue: p.Index,
		}); err != nil {
			return fmt.Errorf("upsert cpi %s: %w", p.Period.Format("2006-01"), err)
		}
		stored++
	}

	slog.Info("cpi series refreshed", "months", stored, "series", blsSeriesID)
	return nil
}

// blsResponse is the shape of the v1 timeseries reply, narrowed to what is
// used. Values arrive as STRINGS and are parsed straight into decimal — an
// index that detoured through a float would round in the fourth place, which is
// exactly where the published precision lives.
type blsResponse struct {
	Status  string   `json:"status"`
	Message []string `json:"message"`
	Results struct {
		Series []struct {
			SeriesID string `json:"seriesID"`
			Data     []struct {
				Year   string `json:"year"`
				Period string `json:"period"` // "M01".."M12", or "M13" for the annual average
				Value  string `json:"value"`  // "-" when the month was never published
			} `json:"data"`
		} `json:"series"`
	} `json:"Results"`
}

// fetchCPI requests one series over a year range and returns the monthly points
// it could parse.
//
// The endpoint is a parameter rather than read from the const directly so the
// parsing and failure paths can be driven against a test server — a mutable
// package-level URL would be the other way to do it and would leave a global
// anything could reassign.
func fetchCPI(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	startYear, endYear int,
) ([]reporting.CPIPoint, error) {
	body, err := json.Marshal(map[string]any{
		"seriesid":  []string{blsSeriesID},
		"startyear": strconv.Itoa(startYear),
		"endyear":   strconv.Itoa(endYear),
	})
	if err != nil {
		return nil, fmt.Errorf("build request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var parsed blsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCPIResponseBytes)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	// BLS answers 200 with a status field carrying the real outcome — a throttle
	// or a malformed request comes back as REQUEST_NOT_PROCESSED, not as a 4xx.
	// Treating that as success would store nothing and log a cheerful zero.
	if parsed.Status != "REQUEST_SUCCEEDED" {
		return nil, fmt.Errorf("bls status %s: %s", parsed.Status, strings.Join(parsed.Message, "; "))
	}

	var out []reporting.CPIPoint
	for _, series := range parsed.Results.Series {
		if series.SeriesID != blsSeriesID {
			continue
		}
		for _, d := range series.Data {
			point, ok := cpiPointFrom(d.Year, d.Period, d.Value)
			if !ok {
				continue
			}
			out = append(out, point)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no monthly observations for %s", blsSeriesID)
	}
	return out, nil
}

// cpiPointFrom turns one BLS observation into a point, or reports that it is
// not one.
//
// Three things are deliberately dropped rather than stored:
//
//   - M13, the annual average. It is not a month and would collide with nothing
//     but confuse everything.
//   - "-" values, months BLS never published (October 2025). Storing a
//     placeholder would let deflation silently succeed against a made-up index.
//   - Anything that does not parse. A malformed row is skipped rather than
//     failing the whole refresh; the other twenty-three months are still worth
//     storing.
func cpiPointFrom(year, period, value string) (reporting.CPIPoint, bool) {
	if len(period) != 3 || period[0] != 'M' || period == "M13" {
		return reporting.CPIPoint{}, false
	}
	month, err := strconv.Atoi(period[1:])
	if err != nil || month < 1 || month > 12 {
		return reporting.CPIPoint{}, false
	}
	y, err := strconv.Atoi(year)
	if err != nil {
		return reporting.CPIPoint{}, false
	}
	index, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil || !index.IsPositive() {
		return reporting.CPIPoint{}, false
	}
	return reporting.CPIPoint{
		Period: time.Date(y, time.Month(month), 1, 0, 0, 0, 0, time.UTC),
		Index:  index,
	}, true
}
