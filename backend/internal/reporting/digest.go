package reporting

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

const (
	// digestBudgetLines caps how many budgets ride along, worst-off first — the
	// ones actually worth looking at on a Monday morning.
	digestBudgetLines = 6
	// digestBillDays is how far ahead the upcoming-bills section looks. Two
	// weeks covers the gap to the next digest on either cadence with room to
	// spare, without turning the digest into the whole calendar.
	digestBillDays  = 14
	digestBillLines = 6
)

// DigestPayload is a digest's figures, frozen at generation time.
//
// Every money value is a display-ready string, already formatted by the same
// moneyfmt the recap and the insight feed use. That is not laziness about types:
// this struct is serialised into digest_entries.payload and rendered verbatim
// months later, so anything that could be re-formatted differently by a future
// client is a way for history to change. The client renders what it is given.
//
// PayloadVersion lets a later shape change be additive: an old row keeps its
// version and its meaning, and nothing rewrites it.
type DigestPayload struct {
	Version int    `json:"version"`
	Cadence string `json:"cadence"`
	Label   string `json:"label"`
	// PeriodStart/PeriodEnd are YYYY-MM-DD, matching how every other date
	// crosses the wire in this app.
	PeriodStart string `json:"period_start"`
	PeriodEnd   string `json:"period_end"`
	// InProgress marks a window that had not finished when the digest was
	// generated — true for every weekly digest, since those report month-to-date.
	InProgress bool `json:"in_progress"`

	Income   string `json:"income"`
	Spending string `json:"spending"`
	Leftover string `json:"leftover"`
	// PriorSpending is the previous month's spend, for a one-line comparison.
	// Empty when there is nothing to compare against.
	PriorSpending string `json:"prior_spending,omitempty"`
	// SavingsRate is leftover as a share of income ("36%"); GrossSavingsRate is
	// the same against confirmed paystub gross, and is empty on the many
	// installs with no paystubs on file. Both carry exactly the meaning they do
	// in the monthly recap — see ai.MonthlySummaryInput.
	SavingsRate      string `json:"savings_rate,omitempty"`
	GrossPay         string `json:"gross_pay,omitempty"`
	GrossSavingsRate string `json:"gross_savings_rate,omitempty"`
	// RecurringTotal is the estimated monthly cost of still-active recurring
	// charges; empty when none were detected.
	RecurringTotal   string `json:"recurring_total,omitempty"`
	TransactionCount int    `json:"transaction_count"`

	TopCategories []DigestCategoryLine `json:"top_categories"`
	// AboveBaseline is the "running hotter than usual" signal, biggest overage
	// first — normally the most useful part of a recap.
	AboveBaseline       []DigestCategoryDelta `json:"above_baseline"`
	Budgets             []DigestBudgetLine    `json:"budgets"`
	LargestTransactions []DigestTxnLine       `json:"largest_transactions"`
	NetWorth            *DigestNetWorth       `json:"net_worth,omitempty"`
	Insights            []DigestInsightLine   `json:"insights"`
	// UpcomingBills is populated from the bill calendar (doc 13) when the
	// household keeps one, and is simply absent otherwise.
	UpcomingBills []DigestBillLine `json:"upcoming_bills"`
}

// HasContent reports whether the digest has anything worth showing. A period
// with no transactions, no insights and no bills is a quiet period, and sending
// or storing an empty recap for it is noise.
func (p DigestPayload) HasContent() bool {
	return p.TransactionCount > 0 || len(p.Insights) > 0 || len(p.UpcomingBills) > 0
}

type DigestCategoryLine struct {
	Name  string `json:"name"`
	Slug  string `json:"slug"`
	Total string `json:"total"`
}

type DigestCategoryDelta struct {
	Name      string `json:"name"`
	ThisMonth string `json:"this_month"`
	Typical   string `json:"typical"`
	Over      string `json:"over"`
}

// DigestBudgetLine is one budget's standing at generation time. PercentUsed is
// an integer 0..N (it can exceed 100 on an overspent envelope) so a bar can be
// drawn without the client dividing two money strings.
type DigestBudgetLine struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Available   string `json:"available"`
	Spent       string `json:"spent"`
	Remaining   string `json:"remaining"`
	PercentUsed int    `json:"percent_used"`
	Over        bool   `json:"over"`
}

type DigestTxnLine struct {
	Merchant string `json:"merchant"`
	Amount   string `json:"amount"`
	Date     string `json:"date"` // short, e.g. "Jul 12"
	Category string `json:"category"`
}

// DigestNetWorth is the household's position over the window. Change/Direction
// are omitted when the window holds fewer than two snapshots, because a single
// point is a level, not a movement.
type DigestNetWorth struct {
	Current string `json:"current"`
	AsOf    string `json:"as_of"`
	Start   string `json:"start,omitempty"`
	Change  string `json:"change,omitempty"`
	// Direction is "up", "down" or "flat"; empty when there is no change to
	// describe. The client should not infer it from the string, which carries a
	// currency symbol and a minus sign in a fixed presentation.
	Direction string `json:"direction,omitempty"`
}

type DigestInsightLine struct {
	ID       uuid.UUID `json:"id"`
	Kind     string    `json:"kind"`
	Title    string    `json:"title"`
	Body     string    `json:"body"`
	Priority int16     `json:"priority"`
}

type DigestBillLine struct {
	Label   string `json:"label"`
	Amount  string `json:"amount"`
	DueDate string `json:"due_date"` // YYYY-MM-DD
}

// digestPayloadVersion is bumped when the shape changes incompatibly. Stored
// rows keep the version they were written with.
const digestPayloadVersion = 1

// BuildDigestPayload assembles everything a digest reports over [from, to],
// scoped to userID's visibility.
//
// It is built on top of BuildMonthlySummaryInput rather than beside it. That is
// the whole point: the digest's headline figures are then, by construction, the
// same numbers the AI narrative was written from and the same ones the Report
// page shows. A second set of queries here would eventually drift, and a digest
// whose prose and figures disagree is worse than no digest.
//
// Sections whose feature the household does not use (budgets, bills, net-worth
// history) come back empty rather than erroring, so this degrades cleanly on a
// fresh install.
func BuildDigestPayload(
	ctx context.Context,
	q *dbgen.Queries,
	householdID, userID uuid.UUID,
	cadence string,
	from, to time.Time,
	label string,
	asOf time.Time,
	insights []dbgen.Insight,
) (DigestPayload, error) {
	input, err := BuildMonthlySummaryInput(ctx, q, householdID, userID, from, to, label, asOf)
	if err != nil {
		return DigestPayload{}, err
	}

	payload := DigestPayload{
		Version:          digestPayloadVersion,
		Cadence:          cadence,
		Label:            label,
		PeriodStart:      from.Format(time.DateOnly),
		PeriodEnd:        to.Format(time.DateOnly),
		InProgress:       input.InProgress,
		Income:           input.Income,
		Spending:         input.Spending,
		Leftover:         input.Leftover,
		PriorSpending:    input.PriorSpending,
		SavingsRate:      input.SavingsRate,
		GrossPay:         input.GrossPay,
		GrossSavingsRate: input.GrossSavingsRate,
		RecurringTotal:   input.RecurringTotal,
		TransactionCount: input.TransactionCount,

		TopCategories:       make([]DigestCategoryLine, 0, len(input.TopCategories)),
		AboveBaseline:       make([]DigestCategoryDelta, 0, len(input.AboveBaseline)),
		Budgets:             []DigestBudgetLine{},
		LargestTransactions: make([]DigestTxnLine, 0, len(input.BiggestTransactions)),
		Insights:            make([]DigestInsightLine, 0, len(insights)),
		UpcomingBills:       []DigestBillLine{},
	}

	// BuildMonthlySummaryInput's category lines carry no slug (the prompt has no
	// use for one), but the client wants it to link through to the category. Map
	// name → slug from the same breakdown it read.
	slugs := map[string]string{}
	if cats, err := q.GetSpendingByCategory(ctx, dbgen.GetSpendingByCategoryParams{
		HouseholdID: householdID, UserID: userID, Date: from, Date_2: to,
	}); err == nil {
		for _, c := range cats {
			slugs[c.CategoryName] = c.CategorySlug
		}
	}

	for _, c := range input.TopCategories {
		payload.TopCategories = append(payload.TopCategories, DigestCategoryLine{
			Name: c.Name, Slug: slugs[c.Name], Total: c.Total,
		})
	}
	for _, d := range input.AboveBaseline {
		payload.AboveBaseline = append(payload.AboveBaseline, DigestCategoryDelta{
			Name: d.Name, ThisMonth: d.ThisMonth, Typical: d.Typical, Over: d.Over,
		})
	}
	for _, t := range input.BiggestTransactions {
		payload.LargestTransactions = append(payload.LargestTransactions, DigestTxnLine{
			Merchant: t.Merchant, Amount: t.Amount, Date: t.Date, Category: t.Category,
		})
	}
	for _, i := range insights {
		payload.Insights = append(payload.Insights, DigestInsightLine{
			ID: i.ID, Kind: i.Kind, Title: i.Title, Body: i.Body, Priority: i.Priority,
		})
	}

	payload.Budgets = buildDigestBudgets(ctx, q, householdID, userID, from, to, asOf)
	payload.NetWorth = buildDigestNetWorth(ctx, q, householdID, from, to)
	payload.UpcomingBills = buildDigestBills(ctx, q, householdID, userID, asOf)

	return payload, nil
}

// buildDigestBudgets returns the budgets most worth knowing about, closest to
// (or furthest past) their limit first. A household with no budgets gets an
// empty list, and a failure degrades to the same — a digest is worth sending
// without its budget section.
func buildDigestBudgets(
	ctx context.Context,
	q *dbgen.Queries,
	householdID, userID uuid.UUID,
	from, to, asOf time.Time,
) []DigestBudgetLine {
	rows, err := BuildBudgetProgress(ctx, q, householdID, userID, from, to, asOf)
	if err != nil {
		return []DigestBudgetLine{}
	}

	hundred := decimal.NewFromInt(100)
	lines := make([]DigestBudgetLine, 0, len(rows))
	for _, b := range rows {
		// An envelope with nothing available cannot express a percentage; treat
		// any spend against it as fully used rather than dividing by zero.
		percent := 0
		switch {
		case b.Available.IsPositive():
			percent = int(b.Spent.Div(b.Available).Mul(hundred).Round(0).IntPart())
		case b.Spent.IsPositive():
			percent = 100
		}
		lines = append(lines, DigestBudgetLine{
			Name:        b.Name,
			Slug:        b.Slug,
			Available:   moneyUSD(b.Available),
			Spent:       moneyUSD(b.Spent),
			Remaining:   moneyUSD(b.Remaining),
			PercentUsed: percent,
			Over:        b.Remaining.IsNegative(),
		})
	}

	// Most-consumed first: the budget you are about to blow is the one that
	// belongs in a recap, not the one you have barely touched.
	sortByPercentDesc(lines)
	if len(lines) > digestBudgetLines {
		lines = lines[:digestBudgetLines]
	}
	return lines
}

// buildDigestNetWorth measures the household's position across the window from
// the recorded snapshots. Net worth is household-wide in this app (see
// ComputeNetWorth), so unlike the spending figures it is not visibility-scoped.
//
// Returns nil when nothing has been snapshotted in the window — a brand-new
// install, or one whose worker has not run yet.
func buildDigestNetWorth(
	ctx context.Context,
	q *dbgen.Queries,
	householdID uuid.UUID,
	from, to time.Time,
) *DigestNetWorth {
	snaps, err := q.ListNetWorthSnapshots(ctx, dbgen.ListNetWorthSnapshotsParams{
		HouseholdID: householdID, AsOf: from, AsOf_2: to,
	})
	if err != nil || len(snaps) == 0 {
		return nil
	}

	last := snaps[len(snaps)-1]
	out := &DigestNetWorth{
		Current: moneyUSD(last.NetWorth),
		AsOf:    last.AsOf.Format(time.DateOnly),
	}
	if len(snaps) < 2 {
		// One point is a level, not a movement. Saying "up $0" would be a
		// statement about the data collection, not about the money.
		return out
	}

	first := snaps[0]
	change := last.NetWorth.Sub(first.NetWorth)
	out.Start = moneyUSD(first.NetWorth)
	out.Change = moneyUSD(change)
	switch {
	case change.IsPositive():
		out.Direction = "up"
	case change.IsNegative():
		out.Direction = "down"
	default:
		out.Direction = "flat"
	}
	return out
}

// buildDigestBills lists what is due in the next fortnight, from the bill
// calendar. Conditional by construction: a household that keeps no obligations
// gets an empty list and the section simply does not render.
func buildDigestBills(
	ctx context.Context,
	q *dbgen.Queries,
	householdID, userID uuid.UUID,
	asOf time.Time,
) []DigestBillLine {
	rows, err := q.ListUpcomingObligations(ctx, dbgen.ListUpcomingObligationsParams{
		HouseholdID: householdID,
		UserID:      &userID,
		Column3:     asOf,
		Column4:     asOf.AddDate(0, 0, digestBillDays),
	})
	if err != nil {
		return []DigestBillLine{}
	}
	if len(rows) > digestBillLines {
		rows = rows[:digestBillLines]
	}
	bills := make([]DigestBillLine, 0, len(rows))
	for _, o := range rows {
		bills = append(bills, DigestBillLine{
			Label:   o.Label,
			Amount:  moneyUSD(o.Amount),
			DueDate: o.DueDate.Format(time.DateOnly),
		})
	}
	return bills
}

// sortByPercentDesc orders budget lines by how consumed they are, descending.
// Insertion sort: the slice is one per household budget, so this is well under
// the size where anything cleverer earns its keep.
func sortByPercentDesc(lines []DigestBudgetLine) {
	for i := 1; i < len(lines); i++ {
		for j := i; j > 0 && lines[j].PercentUsed > lines[j-1].PercentUsed; j-- {
			lines[j], lines[j-1] = lines[j-1], lines[j]
		}
	}
}

// moneyUSD is formatUSD under a name that reads at these call sites. Same
// function, same moneyfmt output — the digest must never format a dollar figure
// differently from the recap it sits next to.
func moneyUSD(d decimal.Decimal) string { return formatUSD(d) }
