package jobs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/shopspring/decimal"
)

// TestParseStooqCSV pins the format the benchmark fetch depends on, including
// the ways a free third-party endpoint fails without saying so.
func TestParseStooqCSV(t *testing.T) {
	body := strings.Join([]string{
		"Date,Open,High,Low,Close,Volume",
		"2026-07-24,412.10,415.00,411.00,414.25,1000",
		"2026-07-27,414.30,418.00,414.00,417.80,1200",
	}, "\n")

	rows, err := parseStooqCSV(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parseStooqCSV: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].date.Format(time.DateOnly) != "2026-07-24" {
		t.Errorf("first date = %s", rows[0].date.Format(time.DateOnly))
	}
	// Parsed from the decimal string, not through a float: 414.25 exactly.
	if !rows[0].close.Equal(decimal.RequireFromString("414.25")) {
		t.Errorf("first close = %s, want 414.25", rows[0].close)
	}
}

// TestParseStooqCSVRejectsEmptySeries covers Stooq's habit of answering an
// unknown symbol with a 200 and a one-line body. Treating that as success would
// store nothing while reporting a healthy fetch forever.
func TestParseStooqCSVRejectsEmptySeries(t *testing.T) {
	for _, body := range []string{
		"",
		"No data",
		"Date,Open,High,Low,Close,Volume",
	} {
		if _, err := parseStooqCSV(strings.NewReader(body)); err == nil {
			t.Errorf("parseStooqCSV(%q) returned no error for an empty series", body)
		}
	}
}

// TestParseStooqCSVSkipsUnreadableRows: one malformed line must not discard the
// rest of a five-year series.
func TestParseStooqCSVSkipsUnreadableRows(t *testing.T) {
	body := strings.Join([]string{
		"Date,Open,High,Low,Close,Volume",
		"not-a-date,1,1,1,1,1",
		"2026-07-27,414.30,418.00,414.00,not-a-price,1200",
		"2026-07-28,414.30,418.00,414.00,420.00,1200",
	}, "\n")

	rows, err := parseStooqCSV(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parseStooqCSV: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 readable row", len(rows))
	}
	if !rows[0].close.Equal(decimal.RequireFromString("420.00")) {
		t.Errorf("close = %s, want 420.00", rows[0].close)
	}
}

// TestFetchBenchmarksDegradesOnFailure is the behaviour the plan calls for
// explicitly: a benchmark that cannot be fetched degrades to a missing series
// and must NOT error the job.
//
// Returning an error here would put River into exponential backoff over a
// decorative chart line, and — worse — would keep retrying an outbound request
// to a third party that is already refusing it.
func TestFetchBenchmarksDegradesOnFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream is down", http.StatusBadGateway)
	}))
	defer server.Close()

	// Queries is nil on purpose: with every fetch failing, nothing should ever
	// reach the database. A nil-pointer panic here would mean the failure path
	// is storing rows it never parsed.
	worker := &FetchBenchmarksWorker{
		Queries: nil,
		Tickers: []string{"SPY", "VTI"},
		HTTP:    server.Client(),
	}

	// Point the fetch at the failing server by overriding the client's transport
	// to rewrite every request to it.
	worker.HTTP = &http.Client{Transport: rewriteTo(server.URL)}

	if err := worker.Work(context.Background(), &river.Job[FetchBenchmarksArgs]{}); err != nil {
		t.Fatalf("Work returned %v; a failed benchmark fetch must not fail the job", err)
	}
}

// TestFetchBenchmarksToleratesJunkBody: a 200 carrying something that is not a
// price series is the same class of failure and must be equally survivable.
func TestFetchBenchmarksToleratesJunkBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>rate limited</html>"))
	}))
	defer server.Close()

	worker := &FetchBenchmarksWorker{
		Queries: nil,
		Tickers: []string{"SPY"},
		HTTP:    &http.Client{Transport: rewriteTo(server.URL)},
	}

	if err := worker.Work(context.Background(), &river.Job[FetchBenchmarksArgs]{}); err != nil {
		t.Fatalf("Work returned %v; an unreadable body must not fail the job", err)
	}
}

// rewriteTo sends every request to the test server regardless of the URL the
// worker built, so the real Stooq host is never contacted by a test.
func rewriteTo(base string) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		target := strings.TrimPrefix(base, "http://")
		clone := r.Clone(r.Context())
		clone.URL.Scheme = "http"
		clone.URL.Host = target
		clone.Host = target
		return http.DefaultTransport.RoundTrip(clone)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
