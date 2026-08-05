package api

import (
	"context"
	"net/http"
	"time"

	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/reporting"
)

// Inflation-adjusted ("real") views: /api/inflation, and the `real` parameter
// on the long-horizon endpoints (doc 27).
//
// Two rules bind every real surface in this package, and they are not
// negotiable per-endpoint.
//
//  1. NO DEFAULT CHANGES. Nominal remains the default everywhere. With `real`
//     unset, every endpoint returns exactly the bytes it returned before this
//     file existed — every added field is a nil pointer with `omitempty`.
//     Silently switching the meaning of a figure would break every comparison a
//     user has in their head.
//  2. A point that cannot be deflated is REPORTED, never filled in. Its real
//     field is absent and the client renders a gap. The alternative — passing
//     the nominal number through under a "real" name — is undetectable
//     downstream, which is precisely why it is forbidden.

// realRequested reads the opt-in parameter. Accepts the two spellings a client
// might send and nothing else; anything unrecognised means nominal, which is
// the safe direction to resolve an ambiguity in.
func realRequested(r *http.Request) bool {
	v := r.URL.Query().Get("real")
	return v == "1" || v == "true"
}

// loadCPI reads the series for one request.
//
// Read per request rather than cached on the Server, matching how
// networth.LoadBondRates treats the other bundled reference table: ~200 small
// rows, and a cache would buy a few milliseconds in exchange for a staleness
// question nobody wants to answer after the refresh job writes a new month.
func (s *Server) loadCPI(ctx context.Context) (*reporting.CPISeries, error) {
	return reporting.LoadCPISeries(ctx, s.Queries)
}

// minRealSpanMonths is the shortest window on which a real view is offered.
//
// Twelve months. Deflating a single month by a single month's price change is
// noise dressed as precision: the month-over-month CPI move is a few tenths of
// a percent, comfortably inside the noise of the figure being deflated, and
// showing it invites the reader to draw a conclusion the data cannot support.
// The clients use this to decide whether to render the toggle at all.
const minRealSpanMonths = 12

// cpiSeriesLabel and cpiSourceURL travel with every real figure so a reader can
// check the deflator rather than take it on trust — the same standard the
// savings-bond rate table is held to.
const (
	cpiSeriesLabel = "CPI-U, U.S. city average, all items, not seasonally adjusted (BLS CUUR0000SA0)"
	cpiSourceURL   = "https://data.bls.gov/timeseries/CUUR0000SA0"
)

type inflationResponse struct {
	// Available is false when the series is empty — a schema whose seed did not
	// load. Clients hide the real toggle entirely rather than offering one that
	// errors.
	Available bool   `json:"available"`
	Series    string `json:"series"`
	SourceURL string `json:"source_url"`

	// BasePeriod is the month real figures are expressed in, and BaseLabel is
	// how to say it. "In June 2026 dollars" — not "in today's dollars", because
	// the current month's index does not exist yet and claiming otherwise would
	// overstate how fresh the series is.
	BasePeriod string `json:"base_period,omitempty"`
	BaseLabel  string `json:"base_label,omitempty"`

	// Earliest and Latest bound what can be deflated at all. A household with
	// history predating Earliest gets a clear message, not a wrong number.
	Earliest string `json:"earliest,omitempty"`
	Latest   string `json:"latest,omitempty"`

	// Stale means the series has fallen far enough behind to say so. Being one
	// month behind is normal — BLS publishes mid-following-month — so this only
	// fires at two.
	Stale     bool   `json:"stale"`
	StaleNote string `json:"stale_note,omitempty"`

	// Gaps are months inside the covered span that were never published, as
	// "2025-10". Real and permanent, not a sync failure: see GapNote.
	Gaps    []string `json:"gaps"`
	GapNote string   `json:"gap_note,omitempty"`

	// YTDRate is inflation from last December to BasePeriod, as a fraction.
	YTDRate  *decimal.Decimal `json:"ytd_rate,omitempty"`
	YTDFrom  string           `json:"ytd_from,omitempty"`
	YTDLabel string           `json:"ytd_label,omitempty"`

	// Context sets that figure against the household's own numbers. Abstract
	// inflation trivia is forgettable; "prices are up 3.1% and your net worth is
	// up 2.4%" is not.
	Context *inflationContext `json:"context,omitempty"`

	// MinSpanMonths tells the client when NOT to offer the toggle.
	MinSpanMonths int    `json:"min_span_months"`
	Basis         string `json:"basis"`
}

// inflationContext is the household's own year-to-date movement beside the
// price level, nominal and real.
type inflationContext struct {
	// NetWorth* are null when there are not two snapshots in the window to
	// compare, or when the earlier one cannot be deflated.
	NetWorthChange     *decimal.Decimal `json:"net_worth_change,omitempty"`
	NetWorthReal       *decimal.Decimal `json:"net_worth_real_change,omitempty"`
	NetWorthFrom       string           `json:"net_worth_from,omitempty"`
	NetWorthTo         string           `json:"net_worth_to,omitempty"`
	NetWorthStartValue *decimal.Decimal `json:"net_worth_start_value,omitempty"`
	NetWorthEndValue   *decimal.Decimal `json:"net_worth_end_value,omitempty"`

	// Income is this year to date against the same window last year, so the
	// comparison is like-for-like rather than a partial year against a full one.
	IncomeChange *decimal.Decimal `json:"income_change,omitempty"`
	IncomeNote   string           `json:"income_note,omitempty"`
}

const inflationBasis = "Real figures are nominal dollars divided by the change in CPI-U between the month they were recorded and the base month. Deflation is applied when the figure is read; nothing stored is ever rewritten."

// handleInflation describes the deflator: what it covers, how fresh it is, and
// what the household's own year looks like against it.
//
// Every client that renders a real figure reads this first, because the base
// period is not optional decoration — a real number without the month it is
// denominated in is not a number anybody can use.
func (s *Server) handleInflation(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()
	now := time.Now().UTC()

	series, err := s.loadCPI(ctx)
	if err != nil {
		s.internalError(w, "load cpi series", err)
		return
	}

	resp := inflationResponse{
		Series:        cpiSeriesLabel,
		SourceURL:     cpiSourceURL,
		Gaps:          []string{},
		MinSpanMonths: minRealSpanMonths,
		Basis:         inflationBasis,
	}
	if series.Empty() {
		// Not an error: a deployment can legitimately be in this state only if
		// the seed failed, and answering "no series" lets every client hide the
		// feature instead of showing a broken toggle.
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Available = true

	if earliest, ok := series.Earliest(); ok {
		resp.Earliest = earliest.Format("2006-01")
	}
	if latest, ok := series.Latest(); ok {
		resp.Latest = latest.Format("2006-01")
	}
	if base, ok := series.BasePeriod(now); ok {
		resp.BasePeriod = base.Format("2006-01")
		resp.BaseLabel = base.Format("January 2006")
	}

	resp.Stale = series.Stale(now)
	if resp.Stale {
		resp.StaleNote = "The CPI series has not gained a month recently, so real figures are expressed in older dollars than today's. " +
			"Automatic refresh is off by default; the bundled series still deflates correctly, it just stops moving forward."
	}

	for _, gap := range series.Gaps() {
		resp.Gaps = append(resp.Gaps, gap.Format("2006-01"))
	}
	if len(resp.Gaps) > 0 {
		resp.GapNote = "Some months have no published index and never will — BLS did not collect them. " +
			"Figures dated in those months are shown nominal and marked, rather than deflated against an invented number."
	}

	if rate, from, _, ok := series.YearToDate(now); ok {
		resp.YTDRate = &rate
		resp.YTDFrom = from.Format("2006-01")
		resp.YTDLabel = "since " + from.Format("December 2006")
	}

	resp.Context = s.inflationContext(ctx, identity, series, now)

	writeJSON(w, http.StatusOK, resp)
}

// inflationContext builds the household's own comparison. Every part of it is
// optional: a household two weeks old has no year to compare, and the strip
// then simply says what inflation was.
func (s *Server) inflationContext(
	ctx context.Context,
	identity auth.Identity,
	series *reporting.CPISeries,
	now time.Time,
) *inflationContext {
	base, ok := series.BasePeriod(now)
	if !ok {
		return nil
	}

	out := &inflationContext{}
	any := false

	// Net worth: first and last recorded snapshot this calendar year. Snapshots
	// are the only place a past net worth exists — balances carry no history of
	// their own — so a household that started mid-year gets a shorter window,
	// which is reported rather than papered over.
	yearStart := time.Date(now.Year(), time.January, 1, 0, 0, 0, 0, time.UTC)
	snapshots, err := s.Queries.ListNetWorthSnapshots(ctx, dbgen.ListNetWorthSnapshotsParams{
		HouseholdID: identity.HouseholdID,
		AsOf:        yearStart,
		AsOf_2:      now,
	})
	if err == nil && len(snapshots) >= 2 {
		first, last := snapshots[0], snapshots[len(snapshots)-1]
		if first.NetWorth.IsPositive() {
			change := percentChange(first.NetWorth, last.NetWorth)
			out.NetWorthChange = &change
			out.NetWorthFrom = first.AsOf.Format(time.DateOnly)
			out.NetWorthTo = last.AsOf.Format(time.DateOnly)
			start, end := first.NetWorth, last.NetWorth
			out.NetWorthStartValue = &start
			out.NetWorthEndValue = &end
			any = true

			// Real growth: put the opening figure into the same dollars as the
			// closing one before comparing. Absent, not approximated, when
			// either month has no index.
			if realStart, err := reporting.Real(first.NetWorth, first.AsOf, base, series); err == nil {
				if realEnd, err := reporting.Real(last.NetWorth, last.AsOf, base, series); err == nil && realStart.IsPositive() {
					realChange := percentChange(realStart, realEnd)
					out.NetWorthReal = &realChange
				}
			}
		}
	}

	// Income: this year to date against the same span last year. Nominal on
	// both sides, so the reader compares its growth against the inflation figure
	// beside it rather than being handed a pre-chewed conclusion.
	thisYear, err1 := s.Queries.GetSpendingSummary(ctx, dbgen.GetSpendingSummaryParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
		Date: yearStart, Date_2: now,
	})
	lastYear, err2 := s.Queries.GetSpendingSummary(ctx, dbgen.GetSpendingSummaryParams{
		HouseholdID: identity.HouseholdID, UserID: identity.UserID,
		Date: yearStart.AddDate(-1, 0, 0), Date_2: now.AddDate(-1, 0, 0),
	})
	if err1 == nil && err2 == nil && lastYear.Income.IsPositive() {
		change := percentChange(lastYear.Income, thisYear.Income)
		out.IncomeChange = &change
		out.IncomeNote = "Income so far this year against the same span last year."
		any = true
	}

	if !any {
		return nil
	}
	return out
}

// percentChange is (to − from) / from, as a fraction. The caller guarantees
// `from` is positive; a change against a zero or negative base is not a
// percentage anybody should be shown.
func percentChange(from, to decimal.Decimal) decimal.Decimal {
	return to.Sub(from).DivRound(from, 6)
}
