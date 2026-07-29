package networth

import "github.com/shopspring/decimal"

// IRS annual contribution limits, by tax year.
//
// These live in code, not in the database, and the reason is worth stating: they
// change every year, and a stale row in a table nobody remembers to update
// produces wrong headroom silently. A Go map is reviewed when the app is
// upgraded, and an unconfigured year is a visible condition (Limits returns
// ok=false) rather than a quiet fallback to last year's numbers.
//
// The rule for the whole file: when a year is missing, say so. Never apply the
// nearest year — "you have $8,000 of headroom left" computed against a limit
// from two years ago is worse than "we do not have 2028's limits yet".

// TaxYearLimits are the elective-deferral and account caps for one tax year.
// Zero means "this account type has no IRS annual cap" (a taxable brokerage),
// which is a different statement from "we do not know" — the latter is the
// ok=false return from Limits.
type TaxYearLimits struct {
	Year int

	// Elective401k is the employee deferral limit, shared across ALL 401(k)-type
	// plans a person holds — traditional and Roth together, not each. Catchp is
	// the additional amount allowed from age 50; Catchp60 replaces it (it does
	// not stack) for ages 60–63 under SECURE 2.0.
	Elective401k         decimal.Decimal
	Elective401kCatchp   decimal.Decimal
	Elective401kCatchp60 decimal.Decimal

	// IRA is likewise a single limit shared between traditional and Roth.
	IRA       decimal.Decimal
	IRACatchp decimal.Decimal

	// HSA is per-coverage-type. Family is used only when the user says so; the
	// projection defaults to self-only, which is the conservative direction
	// (understating headroom, never overstating a contribution).
	HSASelf   decimal.Decimal
	HSAFamily decimal.Decimal
	HSACatchp decimal.Decimal // from age 55, not 50
}

// Age thresholds at which the catch-up amounts apply. The 60–63 band is a
// SECURE 2.0 window with a higher 401(k) catch-up that ends at 64 — it is a
// range, not a floor, and treating it as a floor would overstate headroom for
// everybody older.
const (
	CatchupAge      = 50
	SuperCatchupMin = 60
	SuperCatchupMax = 63
	HSACatchupAge   = 55
)

// taxYearLimits is the versioned table. Add a year here when the IRS publishes
// it; do not edit a past year in place, because a projection run against
// history should still see what was true then.
var taxYearLimits = map[int]TaxYearLimits{
	2025: {
		Year:                 2025,
		Elective401k:         decimal.NewFromInt(23500),
		Elective401kCatchp:   decimal.NewFromInt(7500),
		Elective401kCatchp60: decimal.NewFromInt(11250),
		IRA:                  decimal.NewFromInt(7000),
		IRACatchp:            decimal.NewFromInt(1000),
		HSASelf:              decimal.NewFromInt(4300),
		HSAFamily:            decimal.NewFromInt(8550),
		HSACatchp:            decimal.NewFromInt(1000),
	},
	2026: {
		Year:                 2026,
		Elective401k:         decimal.NewFromInt(24500),
		Elective401kCatchp:   decimal.NewFromInt(8000),
		Elective401kCatchp60: decimal.NewFromInt(11250),
		IRA:                  decimal.NewFromInt(7500),
		IRACatchp:            decimal.NewFromInt(1100),
		HSASelf:              decimal.NewFromInt(4400),
		HSAFamily:            decimal.NewFromInt(8750),
		HSACatchp:            decimal.NewFromInt(1000),
	},
}

// Limits returns the configured limits for a tax year.
//
// ok=false means the year is not in the table. Callers must surface that — the
// UI says "limits for <year> aren't configured; contributions are projected
// uncapped" — rather than substituting an adjacent year.
func Limits(year int) (TaxYearLimits, bool) {
	l, ok := taxYearLimits[year]
	return l, ok
}

// LatestConfiguredYear is the most recent year in the table, for the message
// shown when the running year is missing ("we have limits through 2026").
func LatestConfiguredYear() int {
	latest := 0
	for y := range taxYearLimits {
		if y > latest {
			latest = y
		}
	}
	return latest
}

// AnnualLimitFor returns the annual employee contribution cap that applies to
// one account, given its tax treatment and the contributor's age.
//
// ok=false means "no cap applies", which is the honest answer for a taxable
// brokerage, a trust, or a 529 — a 529 is bounded by gift-tax rules and state
// aggregate caps, neither of which is a federal annual deferral limit and
// neither of which this app knows. Capping one at a number we invented would be
// worse than not capping it.
//
// Note what this deliberately does NOT model: the shared nature of the limits.
// The 401(k) elective cap is one number across every 401(k) a person holds, and
// the IRA cap is one number across traditional and Roth. Applying the full cap
// per account overstates headroom for somebody with two of the same kind. The
// projection handles that at the household level by pooling accounts of a kind
// before capping — see capContributions.
func AnnualLimitFor(treatment string, age int, l TaxYearLimits, familyHSA bool) (decimal.Decimal, bool) {
	switch treatment {
	case "trad_401k", "roth_401k":
		limit := l.Elective401k
		switch {
		case age >= SuperCatchupMin && age <= SuperCatchupMax:
			limit = limit.Add(l.Elective401kCatchp60)
		case age >= CatchupAge:
			limit = limit.Add(l.Elective401kCatchp)
		}
		return limit, true
	case "trad_ira", "roth_ira":
		limit := l.IRA
		if age >= CatchupAge {
			limit = limit.Add(l.IRACatchp)
		}
		return limit, true
	case "hsa":
		limit := l.HSASelf
		if familyHSA {
			limit = l.HSAFamily
		}
		if age >= HSACatchupAge {
			limit = limit.Add(l.HSACatchp)
		}
		return limit, true
	default:
		// taxable, 529, trust, other: no federal annual deferral limit.
		return decimal.Zero, false
	}
}

// limitGroup is the key contributions are pooled under before a cap is applied.
// Two 401(k)s share one elective limit; a traditional and a Roth IRA share one
// IRA limit. Anything with no shared cap gets its own group so it is never
// pooled with something else.
func limitGroup(treatment string) string {
	switch treatment {
	case "trad_401k", "roth_401k":
		return "401k"
	case "trad_ira", "roth_ira":
		return "ira"
	case "hsa":
		return "hsa"
	default:
		return treatment
	}
}
