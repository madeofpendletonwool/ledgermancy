package reporting

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// The Investments surface's aggregate layer: allocation, fee drag, and the
// plumbing that turns stored rows into the inputs the return maths in
// returns.go expects.
//
// Nothing here rounds before the end. Percentages are computed from full-
// precision decimals and rounded once, at the point of presentation.

// Period names the window a performance figure covers.
type Period string

const (
	PeriodYTD            Period = "ytd"
	Period1Y             Period = "1y"
	Period3Y             Period = "3y"
	Period5Y             Period = "5y"
	PeriodSinceInception Period = "inception"
)

// ValidPeriod reports whether a raw query value names a period.
func ValidPeriod(raw string) bool {
	switch Period(raw) {
	case PeriodYTD, Period1Y, Period3Y, Period5Y, PeriodSinceInception:
		return true
	}
	return false
}

// PeriodStart resolves a period to its nominal start date.
//
// "Nominal" matters: this is what the user asked for, not what the data can
// support. The caller clamps to available history and reports the real span, so
// a five-year request against three weeks of snapshots reports three weeks.
func PeriodStart(p Period, now time.Time) time.Time {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch p {
	case PeriodYTD:
		return time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	case Period1Y:
		return today.AddDate(-1, 0, 0)
	case Period3Y:
		return today.AddDate(-3, 0, 0)
	case Period5Y:
		return today.AddDate(-5, 0, 0)
	default:
		// Since inception. The snapshot table only goes back as far as the app
		// has been watching, so a wide floor is clamped by the data itself.
		return today.AddDate(-100, 0, 0)
	}
}

// PortfolioPerformance is the whole portfolio's result for one period, plus the
// caveat the UI is required to show alongside it.
type PortfolioPerformance struct {
	Period Period
	Performance
	// HistoryCaveat is non-empty whenever the measured span is materially
	// shorter than the requested one, or too short for the figure to mean much.
	// It is a sentence, ready to render.
	HistoryCaveat string
	// Computable is false when there was not enough history for any figure at
	// all. Everything else is zero in that case.
	Computable bool
}

// thinHistoryDays is the span below which a return figure is presented with a
// prominent caveat. Ninety days is roughly the point at which a single bad week
// stops dominating the number.
const thinHistoryDays = 90

// BuildPortfolioPerformance computes one period's return figures for a
// household.
//
// Returns Computable=false rather than an error when history is too thin: a
// three-day-old install is a normal state, not a failure, and the page must
// render an honest "not yet" instead of an error banner.
func BuildPortfolioPerformance(
	ctx context.Context,
	q *dbgen.Queries,
	householdID, userID uuid.UUID,
	p Period,
	now time.Time,
) (PortfolioPerformance, error) {
	out := PortfolioPerformance{Period: p}

	requested := PeriodStart(p, now)
	snaps, err := q.ListInvestmentSnapshots(ctx, dbgen.ListInvestmentSnapshotsParams{
		HouseholdID: householdID, UserID: userID,
		AsOf: requested, AsOf_2: now,
	})
	if err != nil {
		return out, fmt.Errorf("list investment snapshots: %w", err)
	}
	if len(snaps) < 2 {
		out.HistoryCaveat = "Ledgermancy records what your investments are worth once a day. " +
			"Returns appear once there are at least two days of history."
		return out, nil
	}

	vals := make([]Valuation, 0, len(snaps))
	for _, s := range snaps {
		vals = append(vals, Valuation{Date: s.AsOf, Value: s.MarketValue})
	}

	flows, err := ExternalFlows(ctx, q, householdID, userID, vals[0].Date, vals[len(vals)-1].Date)
	if err != nil {
		return out, err
	}

	perf, ok := ComputePerformance(PerformanceInput{Valuations: vals, Flows: flows})
	if !ok {
		return out, nil
	}
	out.Performance = perf
	out.Computable = true
	out.HistoryCaveat = historyCaveat(p, requested, perf)
	return out, nil
}

// historyCaveat writes the sentence that keeps a thin-history return honest.
// Empty when the measured span really does cover what was asked for.
func historyCaveat(p Period, requested time.Time, perf Performance) string {
	switch {
	case perf.Days < thinHistoryDays:
		return fmt.Sprintf(
			"Based on only %d days of recorded history — too short to say much about performance. "+
				"Ledgermancy cannot backfill this: your institution reports what you hold today, not what you held last year.",
			perf.Days)
	case perf.Start.After(requested.AddDate(0, 0, 1)) && p != PeriodSinceInception:
		return fmt.Sprintf(
			"Based on %d days, from %s — Ledgermancy has not been watching for the full period.",
			perf.Days, perf.Start.Format(time.DateOnly))
	default:
		return ""
	}
}

// ExternalFlows loads the period's boundary-crossing cash movements, already
// flipped into the investor's sign convention (positive = into the portfolio).
//
// This is the only place Plaid's sign is inverted. Everything downstream — TWR,
// IRR, the net-flows figure on the page — reads the same direction, so there is
// no second place for the two to disagree.
func ExternalFlows(
	ctx context.Context,
	q *dbgen.Queries,
	householdID, userID uuid.UUID,
	from, to time.Time,
) ([]Flow, error) {
	rows, err := q.ListInvestmentTransactionsInRange(ctx, dbgen.ListInvestmentTransactionsInRangeParams{
		HouseholdID: householdID, UserID: userID, Date: from, Date_2: to,
	})
	if err != nil {
		return nil, fmt.Errorf("list investment transactions: %w", err)
	}

	flows := make([]Flow, 0)
	for _, r := range rows {
		subtype := ""
		if r.Subtype != nil {
			subtype = strings.ToLower(*r.Subtype)
		}
		if !IsExternalFlow(strings.ToLower(r.Type), subtype) {
			continue
		}
		// Plaid: positive means cash was DEBITED from the account. From the
		// investor's side a debit is money leaving the portfolio, so the sign
		// flips exactly once, here.
		flows = append(flows, Flow{Date: r.Date, Amount: r.Amount.Neg()})
	}
	return flows, nil
}

// EarliestHistory reports the first day the app recorded an investment value,
// or false when it never has.
func EarliestHistory(
	ctx context.Context,
	q *dbgen.Queries,
	householdID, userID uuid.UUID,
) (time.Time, bool, error) {
	as, err := q.GetEarliestInvestmentSnapshot(ctx, dbgen.GetEarliestInvestmentSnapshotParams{
		HouseholdID: householdID, UserID: userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("earliest investment snapshot: %w", err)
	}
	return as, true, nil
}

// --------------------------------------------------------------------------
// Allocation
// --------------------------------------------------------------------------

// AllocationSlice is one band of the portfolio.
type AllocationSlice struct {
	Label string
	Value decimal.Decimal
	// Percent of the total, 0–100, rounded to two places for presentation.
	Percent decimal.Decimal
}

// Allocation groups holdings into bands.
//
// Plaid's coverage of security type is partial, and this shows the gap as an
// explicit "Unknown" slice rather than redistributing it across the known
// bands. Redistribution would make the chart look complete while being wrong
// about every slice on it — a 60%-covered portfolio presented as 100% is the
// same class of misinformation as the fee number below.
func Allocation(rows []dbgen.ListVisibleHoldingsDetailedRow) []AllocationSlice {
	byLabel := map[string]decimal.Decimal{}
	total := decimal.Zero

	for _, h := range rows {
		if !h.InstitutionValue.Valid {
			continue
		}
		label := "Unknown"
		switch {
		case h.IsCashEquivalent:
			label = "Cash"
		case h.SecurityType != nil && *h.SecurityType != "":
			label = assetClassLabel(*h.SecurityType)
		}
		byLabel[label] = byLabel[label].Add(h.InstitutionValue.Decimal)
		total = total.Add(h.InstitutionValue.Decimal)
	}

	return sliceOut(byLabel, total)
}

// AllocationByTaxTreatment groups holdings by the confirmed tax treatment of the
// account holding them. Untagged accounts are their own band, so the prompt to
// classify them is visible in the chart rather than hidden.
func AllocationByTaxTreatment(rows []dbgen.ListVisibleHoldingsDetailedRow) []AllocationSlice {
	byLabel := map[string]decimal.Decimal{}
	total := decimal.Zero

	for _, h := range rows {
		if !h.InstitutionValue.Valid {
			continue
		}
		label := "Untagged"
		if h.TaxTreatment != nil && *h.TaxTreatment != "" {
			label = TaxTreatmentLabel(*h.TaxTreatment)
		}
		byLabel[label] = byLabel[label].Add(h.InstitutionValue.Decimal)
		total = total.Add(h.InstitutionValue.Decimal)
	}

	return sliceOut(byLabel, total)
}

// sliceOut turns a label→value map into percentage slices, largest first, with
// "Unknown"/"Untagged" pinned last so the honest gap does not masquerade as the
// leading category.
func sliceOut(byLabel map[string]decimal.Decimal, total decimal.Decimal) []AllocationSlice {
	out := make([]AllocationSlice, 0, len(byLabel))
	hundred := decimal.NewFromInt(100)

	for label, value := range byLabel {
		slice := AllocationSlice{Label: label, Value: value}
		if total.IsPositive() {
			slice.Percent = value.Div(total).Mul(hundred).Round(2)
		}
		out = append(out, slice)
	}

	sort.Slice(out, func(i, j int) bool {
		iLast := out[i].Label == "Unknown" || out[i].Label == "Untagged"
		jLast := out[j].Label == "Unknown" || out[j].Label == "Untagged"
		if iLast != jLast {
			return jLast
		}
		return out[i].Value.GreaterThan(out[j].Value)
	})
	return out
}

// assetClassLabel maps Plaid's security type onto a readable band.
func assetClassLabel(securityType string) string {
	switch strings.ToLower(securityType) {
	case "equity":
		return "Equity"
	case "etf":
		return "ETF"
	case "mutual fund":
		return "Mutual fund"
	case "fixed income":
		return "Fixed income"
	case "cash":
		return "Cash"
	case "derivative":
		return "Derivative"
	case "loan":
		return "Loan"
	case "cryptocurrency":
		return "Cryptocurrency"
	default:
		return "Other"
	}
}

// TaxTreatmentLabel renders a stored tax_treatment value for display.
func TaxTreatmentLabel(code string) string {
	switch code {
	case "taxable":
		return "Taxable brokerage"
	case "trad_401k":
		return "Traditional 401(k)"
	case "roth_401k":
		return "Roth 401(k)"
	case "trad_ira":
		return "Traditional IRA"
	case "roth_ira":
		return "Roth IRA"
	case "529":
		return "529"
	case "hsa":
		return "HSA"
	case "trust":
		return "Trust"
	case "other":
		return "Other"
	default:
		return code
	}
}

// SuggestTaxTreatment infers a classification from Plaid's account subtype.
//
// It returns a SUGGESTION, never a stored value. Plaid cannot distinguish a
// Roth 401(k) from a traditional one — both come back as "401k" — and a wrong
// guess there changes every retirement projection built on top of it. So the
// ambiguous cases return "", and the UI asks rather than assumes.
func SuggestTaxTreatment(subtype string) string {
	switch strings.ToLower(strings.TrimSpace(subtype)) {
	case "brokerage", "non-taxable brokerage account", "cash management", "mutual fund":
		return "taxable"
	case "ira", "sep ira", "simple ira", "sarsep":
		return "trad_ira"
	case "roth", "roth ira":
		return "roth_ira"
	case "roth 401k":
		return "roth_401k"
	case "529":
		return "529"
	case "hsa":
		return "hsa"
	case "trust":
		return "trust"
	case "401k", "401a", "403b", "457b", "thrift savings plan", "pension":
		// Ambiguous on the axis that matters: Plaid reports the plan type, not
		// whether contributions were pre- or post-tax. Asking is the only honest
		// option.
		return ""
	default:
		return ""
	}
}

// --------------------------------------------------------------------------
// Fee drag
// --------------------------------------------------------------------------

// FeeDrag is the annual cost of fund expense ratios, with its coverage stated.
type FeeDrag struct {
	// AnnualCost is Σ(expense_ratio × value) over holdings that report a ratio.
	AnnualCost decimal.Decimal
	// CoveredValue is the market value the figure was computed over;
	// UncoveredValue is the rest.
	CoveredValue   decimal.Decimal
	UncoveredValue decimal.Decimal
	// CoveredHoldings / ExcludedHoldings count positions, so the UI can say
	// "excludes 4 holdings" rather than only showing a dollar gap.
	CoveredHoldings  int
	ExcludedHoldings int
}

// ComputeFeeDrag totals annual fund expenses.
//
// Plaid does not supply an expense ratio today — the securities table has no
// column for one — so in practice this reports full exclusion, and the UI says
// so. That is the point: a fee number computed over the 40% of a portfolio that
// happens to carry a ratio, and presented as the total, is misinformation. The
// structure is here so that when a ratio source is added the disclosure comes
// with it rather than being retrofitted.
//
// ratios maps a ticker to its expense ratio as a PERCENTAGE (0.03 means 0.03%,
// matching how fund providers quote it), and is empty until such a source
// exists.
func ComputeFeeDrag(rows []dbgen.ListVisibleHoldingsDetailedRow, ratios map[string]decimal.Decimal) FeeDrag {
	var out FeeDrag
	hundred := decimal.NewFromInt(100)

	for _, h := range rows {
		if !h.InstitutionValue.Valid || h.IsCashEquivalent {
			continue
		}
		value := h.InstitutionValue.Decimal

		var ratio decimal.Decimal
		var known bool
		if h.Ticker != nil {
			ratio, known = ratios[strings.ToUpper(*h.Ticker)]
		}
		if !known {
			out.UncoveredValue = out.UncoveredValue.Add(value)
			out.ExcludedHoldings++
			continue
		}

		out.AnnualCost = out.AnnualCost.Add(value.Mul(ratio).Div(hundred))
		out.CoveredValue = out.CoveredValue.Add(value)
		out.CoveredHoldings++
	}

	return out
}

// --------------------------------------------------------------------------
// Benchmarks
// --------------------------------------------------------------------------

// RebasedPoint is one point on a rebased-to-100 comparison chart.
type RebasedPoint struct {
	Date  time.Time
	Value decimal.Decimal
}

// RebasedSeries is one line: the portfolio, or a benchmark.
type RebasedSeries struct {
	Label  string
	Points []RebasedPoint
}

// Rebase normalises a value series to 100 at its first point, which is what
// makes a portfolio and an index comparable on one axis.
//
// A series whose first value is not positive cannot be rebased and returns
// false: dividing by it would produce a chart that looks authoritative and
// means nothing.
func Rebase(label string, points []RebasedPoint) (RebasedSeries, bool) {
	if len(points) == 0 || !points[0].Value.IsPositive() {
		return RebasedSeries{}, false
	}
	base := points[0].Value
	hundred := decimal.NewFromInt(100)

	out := RebasedSeries{Label: label, Points: make([]RebasedPoint, 0, len(points))}
	for _, p := range points {
		out.Points = append(out.Points, RebasedPoint{
			Date:  p.Date,
			Value: p.Value.Div(base).Mul(hundred).Round(4),
		})
	}
	return out, true
}

// PortfolioGrowthSeries turns the value history into a growth series comparable
// with a benchmark: an index that moves only with market performance, with the
// user's own deposits and withdrawals removed.
//
// Plotting raw portfolio value against an index is the mistake worth avoiding
// here. A portfolio that received a $10,000 deposit would appear to beat every
// benchmark on the chart while having underperformed all of them, and the chart
// would be the most convincing wrong thing on the page. So this chain-links the
// same flow-adjusted sub-period growth TWR uses.
func PortfolioGrowthSeries(vals []Valuation, flows []Flow) []RebasedPoint {
	if len(vals) == 0 {
		return nil
	}

	sorted := make([]Flow, len(flows))
	copy(sorted, flows)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Date.Before(sorted[j].Date) })

	hundred := decimal.NewFromInt(100)
	index := hundred
	out := []RebasedPoint{{Date: vals[0].Date, Value: index}}

	fi := 0
	for i := 1; i < len(vals); i++ {
		prev, cur := vals[i-1], vals[i]

		var periodFlows decimal.Decimal
		for fi < len(sorted) && !sorted[fi].Date.After(cur.Date) {
			if sorted[fi].Date.After(prev.Date) {
				periodFlows = periodFlows.Add(sorted[fi].Amount)
			}
			fi++
		}

		// A non-positive opening value carries the index forward flat rather
		// than dividing by zero. The alternative — dropping the point — would
		// leave a gap in a line chart that reads as missing data.
		if prev.Value.IsPositive() {
			index = index.Mul(cur.Value.Sub(periodFlows).Div(prev.Value))
		}
		out = append(out, RebasedPoint{Date: cur.Date, Value: index.Round(4)})
	}

	return out
}
