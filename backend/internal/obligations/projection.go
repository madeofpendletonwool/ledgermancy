package obligations

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// MaxHorizonDays bounds a projection request. Past a few months the answer is
// dominated by obligations nobody has entered yet, so a longer horizon would
// look more informative than it is — and it also bounds the occurrence
// expansion, which grows with the window.
const MaxHorizonDays = 365

// DefaultHorizonDays is the window the Schedule page opens on.
const DefaultHorizonDays = 30

// Point is one day of a projected balance series.
type Point struct {
	Date    time.Time
	Balance decimal.Decimal
	// Due is what falls due on this day — zero on most days. Carried so the UI
	// can mark the step down without re-deriving it from the occurrence list.
	Due decimal.Decimal
}

// AccountProjection is one depository account's balance carried forward through
// the obligations assigned to it.
type AccountProjection struct {
	AccountID       uuid.UUID
	Name            string
	Mask            *string
	InstitutionName *string
	CurrentBalance  decimal.Decimal
	// LowestBalance / LowestDate are the worst point of the series, which is the
	// only part of it that demands action.
	LowestBalance decimal.Decimal
	LowestDate    time.Time
	GoesNegative  bool
	Points        []Point
}

// Projection is the whole forward view: one series per depository account, plus
// a combined-cash series.
type Projection struct {
	From     time.Time
	To       time.Time
	Accounts []AccountProjection
	// Combined is every depository balance against every obligation, including
	// the ones not assigned to a particular account. It is the honest headline:
	// per-account series can only see bills someone has assigned.
	Combined AccountProjection
	// UnassignedTotal is how much of the window's obligations name no account.
	// Surfaced so the UI can say plainly that the per-account lines are missing
	// that money rather than quietly understating the drop.
	UnassignedTotal decimal.Decimal
	// TotalDue is every obligation in the window, assigned or not.
	TotalDue decimal.Decimal
}

// Project carries each depository account's current balance forward through the
// obligations due in [from, from+days], subtracting exactly what is known and
// guessing at nothing.
//
// Two rules from the doc are enforced here rather than left to the caller:
//
//   - Only depository accounts get a series (ListProjectionAccounts filters to
//     them). Projecting a credit card's balance against that card's own bills
//     subtracts the balance from itself.
//   - Only obligations are subtracted. Discretionary spending is not modelled,
//     because a number invented for it would be indistinguishable, to a reader,
//     from the ones that are real.
func Project(
	ctx context.Context,
	q *dbgen.Queries,
	householdID, userID uuid.UUID,
	from time.Time,
	days int,
) (Projection, error) {
	if days <= 0 {
		days = DefaultHorizonDays
	}
	if days > MaxHorizonDays {
		days = MaxHorizonDays
	}
	from = dateOnly(from)
	to := from.AddDate(0, 0, days)

	accounts, err := q.ListProjectionAccounts(ctx, dbgen.ListProjectionAccountsParams{
		HouseholdID: householdID, UserID: userID,
	})
	if err != nil {
		return Projection{}, fmt.Errorf("list projection accounts: %w", err)
	}

	occurrences, err := ListUpcoming(ctx, q, householdID, userID, from, to)
	if err != nil {
		return Projection{}, err
	}

	// Bucket the window's obligations by day: per account for the account
	// series, and in total for the combined one.
	perAccount := make(map[uuid.UUID]map[string]decimal.Decimal)
	allDue := make(map[string]decimal.Decimal)
	total := decimal.Zero
	unassigned := decimal.Zero
	// decimal.Decimal's zero value is zero, so a missing map entry adds nothing
	// and needs no first-write special case.
	for _, o := range occurrences {
		key := o.DueDate.Format(time.DateOnly)
		allDue[key] = allDue[key].Add(o.Amount)
		total = total.Add(o.Amount)
		if o.AccountID == nil {
			unassigned = unassigned.Add(o.Amount)
			continue
		}
		byDay, ok := perAccount[*o.AccountID]
		if !ok {
			byDay = make(map[string]decimal.Decimal)
			perAccount[*o.AccountID] = byDay
		}
		byDay[key] = byDay[key].Add(o.Amount)
	}

	proj := Projection{
		From:            from,
		To:              to,
		Accounts:        make([]AccountProjection, 0, len(accounts)),
		UnassignedTotal: unassigned.Round(2),
		TotalDue:        total.Round(2),
	}

	combinedBalance := decimal.Zero
	for _, a := range accounts {
		combinedBalance = combinedBalance.Add(a.CurrentBalance)
		proj.Accounts = append(proj.Accounts, AccountProjection{
			AccountID:       a.ID,
			Name:            a.Name,
			Mask:            a.Mask,
			InstitutionName: a.InstitutionName,
			CurrentBalance:  a.CurrentBalance.Round(2),
		})
	}
	for i := range proj.Accounts {
		fillSeries(&proj.Accounts[i], perAccount[proj.Accounts[i].AccountID], from, days)
	}

	proj.Combined = AccountProjection{
		Name:           "All cash accounts",
		CurrentBalance: combinedBalance.Round(2),
	}
	fillSeries(&proj.Combined, allDue, from, days)

	return proj, nil
}

// fillSeries walks the horizon day by day, subtracting that day's obligations
// from the running balance and recording the low-water mark. Every step is exact
// decimal; nothing here is ever recomputed downstream.
func fillSeries(p *AccountProjection, due map[string]decimal.Decimal, from time.Time, days int) {
	balance := p.CurrentBalance
	p.Points = make([]Point, 0, days+1)
	p.LowestBalance = balance
	p.LowestDate = from

	for i := 0; i <= days; i++ {
		day := from.AddDate(0, 0, i)
		today := due[day.Format(time.DateOnly)]
		balance = balance.Sub(today)
		if balance.LessThan(p.LowestBalance) {
			p.LowestBalance = balance
			p.LowestDate = day
		}
		p.Points = append(p.Points, Point{
			Date: day, Balance: balance.Round(2), Due: today.Round(2),
		})
	}
	p.LowestBalance = p.LowestBalance.Round(2)
	p.GoesNegative = p.LowestBalance.IsNegative()
}
