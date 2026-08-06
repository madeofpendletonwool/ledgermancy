package allocation

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Cash: the accrual bucket, and the cash-drag detector.
//
// A cash bucket is the one place in this package where nothing compounds at a
// market return. It accrues at the account's own deposit_apy, σ=0, and it is
// liquid by definition — which is exactly why an emergency fund lives here and
// why the horizon check refuses to route short-horizon money anywhere else
// without saying so.

// fillCash projects a deposit account: the balance, the lump, and the monthly
// deposits, accruing at the account's own yield.
//
// THE REAL-RETURN CONVERSION IS THE POINT OF THIS FUNCTION. deposit_apy is a
// NOMINAL yield — it is what the bank prints — and every other figure in this
// app is in today's dollars. Compounding 4.5% nominal beside a 5% REAL equity
// return would flatter cash by the whole inflation rate, roughly 3 points a
// year, which over a long horizon is the difference between "cash is fine" and
// "cash lost a third of its purchasing power". So the real rate is
// (apy − inflation), and it is very often NEGATIVE. That is not a bug in the
// arithmetic; it is the thing a household holding $40k in checking most needs
// to see.
func fillCash(r *BucketResult, bucket Bucket, months int, inflation decimal.Decimal) {
	r.Engine = "accrual"

	nominal := decimal.Zero
	if bucket.DepositAPY.Valid {
		apy := bucket.DepositAPY.Decimal
		r.DepositAPY = &apy
		nominal = apy.Div(hundred)
	} else {
		r.Notes = append(r.Notes,
			"No yield is recorded for this account, so it is projected as earning nothing. "+
				"That is an assumption, not a measurement — enter the rate on the Accounts page.")
	}
	real := nominal.Sub(inflation)
	r.ReturnRate = real.Round(4)
	r.RateIsHousehold = false

	monthlyRate := real.Div(twelve)
	balance := bucket.Balance.Add(r.AppliedLump)
	contributed := r.AppliedLump
	growth := decimal.Zero

	for m := 1; m <= months; m++ {
		// Growth before the deposit, the same rule the compounding engine uses,
		// so a cash bucket and an investment bucket cannot disagree about how a
		// month works.
		g := balance.Mul(monthlyRate)
		balance = balance.Add(g)
		growth = growth.Add(g)
		if r.AppliedMonthly.IsPositive() {
			balance = balance.Add(r.AppliedMonthly)
			contributed = contributed.Add(r.AppliedMonthly)
		}
	}

	r.ProjectedValue = balance.Round(2)
	r.Contributed = contributed.Round(2)
	r.Growth = growth.Round(2)

	r.Formula = fmt.Sprintf(
		"$%s starting balance plus $%s up front and $%s a month, accruing at %s%% nominal less %s%% assumed "+
			"inflation = %s%% REAL, over %d months. No volatility: cash does not go down, it just buys less.",
		bucket.Balance.StringFixed(2), r.AppliedLump.StringFixed(2), r.AppliedMonthly.StringFixed(2),
		nominal.Mul(hundred).Round(2).String(), inflation.Mul(hundred).Round(2).String(),
		real.Mul(hundred).Round(2).String(), months)

	if real.IsNegative() {
		r.Notes = append(r.Notes, fmt.Sprintf(
			"This account's yield is below your assumed inflation, so its real return is %s%% — money held here "+
				"loses purchasing power even as the balance grows.",
			real.Mul(hundred).Round(2).String()))
	}
}

// --------------------------------------------------------------------------
// Cash drag
// --------------------------------------------------------------------------

// IdleCashItem is one depository account's drag.
type IdleCashItem struct {
	AccountID   uuid.UUID       `json:"account_id"`
	Name        string          `json:"name"`
	Institution string          `json:"institution,omitempty"`
	Subtype     string          `json:"subtype,omitempty"`
	Balance     decimal.Decimal `json:"balance"`
	// APY is this account's own yield, a PERCENT.
	APY decimal.Decimal `json:"apy"`
	// OperatingFloat is the amount excluded from the drag because it is a
	// transaction account's working balance rather than idle money. Zero on a
	// savings account.
	OperatingFloat decimal.Decimal `json:"operating_float"`
	// IdleBalance is Balance − OperatingFloat: the part that could actually move.
	IdleBalance decimal.Decimal `json:"idle_balance"`
	// AnnualDrag is IdleBalance × (benchmark − APY) ÷ 100. PRE-TAX: savings
	// interest is ordinary income, so the after-tax figure is smaller and this
	// app does not know the household's bracket.
	AnnualDrag decimal.Decimal `json:"annual_drag"`
	Detail     string          `json:"detail"`
}

// IdleCashReport is the whole cash-drag answer.
type IdleCashReport struct {
	// HasBenchmark is false when no depository account in the household has a
	// deposit_apy at all. The detector is then SILENT — no items, no benchmark,
	// no invented market rate.
	HasBenchmark bool `json:"has_benchmark"`
	// Benchmark is the household's OWN best yield, a PERCENT, and the account it
	// came from. That is what makes the claim checkable: "this $14k is earning
	// 0.4% while your own savings account earns 4.5%".
	Benchmark        decimal.Decimal `json:"benchmark"`
	BenchmarkAccount string          `json:"benchmark_account,omitempty"`

	Items []IdleCashItem `json:"items"`
	// TotalAnnualDrag is the sum over Items.
	TotalAnnualDrag decimal.Decimal `json:"total_annual_drag"`
	// TotalIdle is the money that could move.
	TotalIdle decimal.Decimal `json:"total_idle"`

	// UnknownYieldAccounts are depository accounts with no deposit_apy. They are
	// NOT dragging by assumption; they are unmeasured, and naming them is how a
	// household knows the report is partial.
	UnknownYieldAccounts []string `json:"unknown_yield_accounts"`

	Note  string `json:"note"`
	Basis string `json:"basis"`
}

// dragThreshold is the annual figure below which the detector stays quiet.
//
// Silence beats noise. A $6/year drag is arithmetically real and not worth a
// household's attention, and a report that lists every account with a
// rounding-error gap trains people to ignore the one that matters.
var dragThreshold = decimal.NewFromInt(50)

// BuildIdleCash computes the cash-drag report.
//
// THE BENCHMARK IS THE HOUSEHOLD'S OWN BEST RATE, and the alternative was
// considered and rejected. Bundling a market HYSA rate would mean either an
// outbound fetch the README promises against, or a transcribed constant that
// goes stale and nobody can verify — the same trade doc 15 refused for its
// return series. Using the household's own best yield needs no new column, no
// market data and no maintenance, and it makes the claim strictly true: the
// household demonstrably CAN get that rate, because it already does somewhere.
//
// A household with no deposit_apy filled in anywhere has no benchmark, and the
// detector stays silent rather than inventing one. That silence is the right
// default: the column is user-entered precisely because Plaid does not serve it
// reliably, so an empty field means "unknown", never "zero".
func BuildIdleCash(b Baseline) IdleCashReport {
	out := IdleCashReport{
		Items:                []IdleCashItem{},
		UnknownYieldAccounts: []string{},
		Basis: "Drag is measured against the best yield you already earn somewhere in this household, not against " +
			"a market rate we made up. Figures are PRE-TAX — savings interest is ordinary income.",
	}

	// The benchmark: the highest non-NULL deposit_apy across the household's own
	// depository accounts.
	for _, bucket := range b.Buckets {
		if bucket.Kind != BucketCash {
			continue
		}
		if !bucket.DepositAPY.Valid {
			out.UnknownYieldAccounts = append(out.UnknownYieldAccounts, bucket.Name)
			continue
		}
		if !out.HasBenchmark || bucket.DepositAPY.Decimal.GreaterThan(out.Benchmark) {
			out.HasBenchmark = true
			out.Benchmark = bucket.DepositAPY.Decimal
			out.BenchmarkAccount = bucket.Name
		}
	}

	if !out.HasBenchmark {
		out.Note = "No deposit yields are recorded, so there is nothing to compare against and no drag is reported. " +
			"Enter the APY on a savings account to turn this on — an empty field means unknown, not zero."
		return out
	}

	for _, bucket := range b.Buckets {
		if bucket.Kind != BucketCash || !bucket.DepositAPY.Valid {
			continue
		}
		gap := out.Benchmark.Sub(bucket.DepositAPY.Decimal)
		if !gap.IsPositive() || !bucket.Balance.IsPositive() {
			continue
		}

		// A CHECKING ACCOUNT'S OPERATING FLOAT IS NOT IDLE MONEY. Without this
		// exclusion the detector tells every household to empty its current
		// account, which is advice nobody can follow. One month of trailing
		// typical fixed costs is the float — the same figure the Budgets page
		// calls fixed costs, so the two cannot disagree.
		float := decimal.Zero
		if isTransactionAccount(bucket.Subtype) {
			float = b.MonthlyFixedCosts
			if float.GreaterThan(bucket.Balance) {
				float = bucket.Balance
			}
		}
		idle := bucket.Balance.Sub(float)
		if !idle.IsPositive() {
			continue
		}

		drag := idle.Mul(gap).Div(hundred).Round(2)
		if drag.LessThan(dragThreshold) {
			continue // silence beats noise
		}

		detail := fmt.Sprintf(
			"$%s earning %s%% while %s earns %s%% — moving it earns about $%s a year, before tax.",
			idle.StringFixed(2), bucket.DepositAPY.Decimal.StringFixed(2),
			out.BenchmarkAccount, out.Benchmark.StringFixed(2), drag.StringFixed(2))
		if float.IsPositive() {
			detail += fmt.Sprintf(
				" $%s is left in place as a month of operating float.", float.StringFixed(2))
		}

		out.Items = append(out.Items, IdleCashItem{
			AccountID: bucket.AccountID, Name: bucket.Name,
			Institution: bucket.Institution, Subtype: bucket.Subtype,
			Balance: bucket.Balance.Round(2), APY: bucket.DepositAPY.Decimal,
			OperatingFloat: float.Round(2), IdleBalance: idle.Round(2),
			AnnualDrag: drag, Detail: detail,
		})
		out.TotalAnnualDrag = out.TotalAnnualDrag.Add(drag)
		out.TotalIdle = out.TotalIdle.Add(idle)
	}

	out.TotalAnnualDrag = out.TotalAnnualDrag.Round(2)
	out.TotalIdle = out.TotalIdle.Round(2)
	if len(out.Items) == 0 {
		out.Note = fmt.Sprintf(
			"Nothing worth moving: no account is more than $%s a year behind your own best rate of %s%%.",
			dragThreshold.StringFixed(2), out.Benchmark.StringFixed(2))
	}
	if len(out.UnknownYieldAccounts) > 0 {
		out.Note += fmt.Sprintf(
			" %d account(s) have no yield recorded and are not included — unknown, not zero.",
			len(out.UnknownYieldAccounts))
	}
	return out
}

// isTransactionAccount reports whether an account is one money flows THROUGH
// rather than sits in. Only these get the operating-float exclusion; a savings
// account's whole balance is idle by definition.
func isTransactionAccount(subtype string) bool {
	switch subtype {
	case "checking", "paypal", "prepaid", "cash management":
		return true
	default:
		return false
	}
}
