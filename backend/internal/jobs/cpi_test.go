package jobs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The BLS response handling, tested against the shapes the real API actually
// returns — including the ones that look like data and are not.

// blsBody is a trimmed real response. Note October 2025: BLS returned it with a
// value of "-" and a footnote, because the month was never collected.
const blsBody = `{
  "status": "REQUEST_SUCCEEDED",
  "responseTime": 84,
  "message": [],
  "Results": {"series": [{"seriesID": "CUUR0000SA0", "data": [
    {"year":"2026","period":"M06","periodName":"June","latest":"true","value":"333.952","footnotes":[{}]},
    {"year":"2026","period":"M05","periodName":"May","value":"335.123","footnotes":[{}]},
    {"year":"2025","period":"M13","periodName":"Annual","value":"320.000","footnotes":[{}]},
    {"year":"2025","period":"M12","periodName":"December","value":"324.054","footnotes":[{}]},
    {"year":"2025","period":"M10","periodName":"October","value":"-","footnotes":[{"code":"X","text":"Data unavailable due to the 2025 lapse in appropriations"}]},
    {"year":"2025","period":"M09","periodName":"September","value":"324.800","footnotes":[{}]}
  ]}]}
}`

func serve(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchCPISkipsUnpublishedAndAnnualRows(t *testing.T) {
	srv := serve(t, http.StatusOK, blsBody)

	points, err := fetchCPI(context.Background(), srv.Client(), srv.URL, 2025, 2026)
	if err != nil {
		t.Fatalf("fetchCPI: %v", err)
	}

	got := map[string]string{}
	for _, p := range points {
		got[p.Period.Format("2006-01")] = p.Index.String()
	}

	// Four real months. Not five: October 2025 was never published, and a
	// placeholder for it would let deflation succeed against a made-up index.
	if len(points) != 4 {
		t.Errorf("got %d points (%v), want 4", len(points), got)
	}
	if _, present := got["2025-10"]; present {
		t.Error("October 2025 must be skipped, not stored")
	}
	// M13 is the annual average, not a month.
	if _, present := got["2025-13"]; present {
		t.Error("M13 (annual average) must not become a period")
	}
	if got["2026-06"] != "333.952" {
		t.Errorf("June 2026 = %q, want 333.952 parsed exactly from the string", got["2026-06"])
	}

	// Periods must be the first of the month, at midnight UTC — the table's
	// CHECK constraint requires it and a lookup keys on it.
	for _, p := range points {
		if p.Period.Day() != 1 || p.Period.Location() != time.UTC {
			t.Errorf("period %v is not a first-of-month UTC date", p.Period)
		}
	}
}

// BLS answers 200 with the real outcome in a status field. Treating that as
// success would store nothing and log a cheerful zero.
func TestFetchCPIRejectsNonSuccessStatusField(t *testing.T) {
	srv := serve(t, http.StatusOK,
		`{"status":"REQUEST_NOT_PROCESSED","message":["daily threshold reached"],"Results":{}}`)

	if _, err := fetchCPI(context.Background(), srv.Client(), srv.URL, 2025, 2026); err == nil {
		t.Fatal("a REQUEST_NOT_PROCESSED body must be an error")
	}
}

func TestFetchCPIRejectsHTTPError(t *testing.T) {
	srv := serve(t, http.StatusServiceUnavailable, "")

	if _, err := fetchCPI(context.Background(), srv.Client(), srv.URL, 2025, 2026); err == nil {
		t.Fatal("a non-200 response must be an error")
	}
}

// A successful call carrying no usable month is a failure, not an empty
// success: it means the series ID or the response shape changed, and a silent
// no-op would leave the series quietly frozen.
func TestFetchCPIRejectsEmptySeries(t *testing.T) {
	srv := serve(t, http.StatusOK,
		`{"status":"REQUEST_SUCCEEDED","message":[],"Results":{"series":[{"seriesID":"CUUR0000SA0","data":[]}]}}`)

	if _, err := fetchCPI(context.Background(), srv.Client(), srv.URL, 2025, 2026); err == nil {
		t.Fatal("a response with no monthly observations must be an error")
	}
}

func TestCPIPointFrom(t *testing.T) {
	cases := []struct {
		name                string
		year, period, value string
		wantOK              bool
	}{
		{"a normal month", "2026", "M06", "333.952", true},
		{"never published", "2025", "M10", "-", false},
		{"annual average", "2025", "M13", "320.000", false},
		{"a quarterly period", "2025", "Q01", "320.000", false},
		{"month out of range", "2025", "M14", "320.000", false},
		{"unparseable value", "2025", "M06", "n/a", false},
		{"non-positive index", "2025", "M06", "0", false},
		{"unparseable year", "two thousand", "M06", "320.000", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := cpiPointFrom(tc.year, tc.period, tc.value)
			if ok != tc.wantOK {
				t.Errorf("cpiPointFrom(%q, %q, %q) ok = %v, want %v",
					tc.year, tc.period, tc.value, ok, tc.wantOK)
			}
		})
	}
}
