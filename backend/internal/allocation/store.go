package allocation

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// The saved-plan wire format, and the one rule it exists to enforce.
//
// MONEY INSIDE THESE BLOBS IS A STRING, NEVER A JSON NUMBER.
//
// The continuity rule says money is cast to text in SQL and never travels as a
// JSON number, and export.go enforces it for `numeric` COLUMNS. IT CANNOT REACH
// INSIDE A JSONB COLUMN — `normalise` passes jsonb through as json.RawMessage,
// so `{"lump": 30000.50}` would leave the portable export as a JSON number, be
// parsed by whatever reads it as a float64, and the guarantee the rest of the
// export makes would silently not hold for this table.
//
// So every money field and every rate below is a string, marshalled and
// unmarshalled through decimal.Decimal, which round-trips strings exactly.
// Integers that are COUNTS (horizon in years, input_version) stay numbers: they
// are not money and are exact in float64 anyway.
//
// The types are structs with explicit string fields rather than a
// map[string]any precisely so this cannot be got wrong by accident. A future
// field that is money and is typed as decimal.Decimal will marshal as a JSON
// number and the round-trip test will fail — which is the point.

// CurrentInputVersion is the shape version written today.
//
// input_version is load-bearing. Saved plans are kept for years, and a v1 plan
// must still open after a v2 change: version from day one and migrate forward
// explicitly rather than guessing at an old blob's meaning.
const CurrentInputVersion = 1

// StoredSplit is one bucket line as saved.
type StoredSplit struct {
	AccountID uuid.UUID `json:"account_id"`
	// Percentages, 0–100, as decimal strings.
	LumpPct    string `json:"lump_pct"`
	MonthlyPct string `json:"monthly_pct"`
	// RealReturnRate is a FRACTION as a string ("0.06"); absent means "use the
	// household rate", the same convention networth.AccountPlan uses.
	RealReturnRate *string `json:"real_return_rate,omitempty"`
}

// StoredInputs is a saved plan's request.
type StoredInputs struct {
	Lump         string `json:"lump"`
	Monthly      string `json:"monthly"`
	HorizonYears int    `json:"horizon_years"`
	// Target is the optional target nest egg. A pointer so "no target" survives
	// the round trip as absent rather than collapsing to "0.00", which would be
	// a target of zero — a different and always-met claim.
	Target    *string       `json:"target,omitempty"`
	FamilyHSA bool          `json:"family_hsa"`
	Buckets   []StoredSplit `json:"buckets"`
}

// StoredAssumptions is the snapshot taken at save time (doc 28's rule): what the
// plan was BUILT with, kept beside it so a plan reopened two years later can say
// how the household's assumptions have moved even though the projection itself
// is recomputed against today's.
type StoredAssumptions struct {
	RealReturnRate       string `json:"real_return_rate"`
	InflationRate        string `json:"inflation_rate"`
	WithdrawalRate       string `json:"withdrawal_rate"`
	CollegeInflationRate string `json:"college_inflation_rate"`
	CurrentAge           int    `json:"current_age"`
	AgeKnown             bool   `json:"age_known"`
	FilingStatus         string `json:"filing_status,omitempty"`
	// MAGIKnown rather than the MAGI itself. A saved plan is exported in plain
	// JSON, and a household's modified AGI is not a figure that needs to ride in
	// a file somebody may email themselves; whether the eligibility check could
	// run is the part the plan actually needs to explain itself.
	MAGIKnown bool `json:"magi_known"`
	TaxYear   int  `json:"tax_year"`
}

// SnapshotAssumptions captures what a plan was built with.
func SnapshotAssumptions(b Baseline) StoredAssumptions {
	return StoredAssumptions{
		RealReturnRate:       b.Assumptions.RealReturnRate.String(),
		InflationRate:        b.InflationRate.String(),
		WithdrawalRate:       b.Assumptions.WithdrawalRate.String(),
		CollegeInflationRate: b.CollegeInflation.StringFixed(2),
		CurrentAge:           b.Age,
		AgeKnown:             b.AgeKnown,
		FilingStatus:         b.FilingStatus,
		MAGIKnown:            b.MAGI.Valid,
		TaxYear:              b.Now.Year(),
	}
}

// Store converts a request into its saved form.
func Store(req Request) StoredInputs {
	out := StoredInputs{
		Lump:         req.Lump.StringFixed(2),
		Monthly:      req.Monthly.StringFixed(2),
		HorizonYears: req.HorizonYears,
		FamilyHSA:    req.FamilyHSA,
		Buckets:      make([]StoredSplit, 0, len(req.Splits)),
	}
	if req.TargetNestEgg.Valid {
		t := req.TargetNestEgg.Decimal.StringFixed(2)
		out.Target = &t
	}
	for _, s := range req.Splits {
		line := StoredSplit{
			AccountID:  s.AccountID,
			LumpPct:    s.LumpPct.StringFixed(2),
			MonthlyPct: s.MonthlyPct.StringFixed(2),
		}
		if s.RealReturnRate.Valid {
			// Not StringFixed: a rate is a fraction and 0.0575 is a real rate
			// somebody may have typed. String() round-trips it exactly.
			r := s.RealReturnRate.Decimal.String()
			line.RealReturnRate = &r
		}
		out.Buckets = append(out.Buckets, line)
	}
	return out
}

// Load converts a saved plan back into a request.
//
// version is checked rather than assumed. An input_version this build does not
// know is refused with a readable error, because the alternative — parsing a
// future shape with today's struct and silently dropping the fields it does not
// have — produces a plan that runs and is not the one that was saved.
func Load(raw []byte, version int) (Request, error) {
	if version != CurrentInputVersion {
		// v1 is the only shape so far. When v2 lands, this becomes a switch that
		// migrates v1 forward EXPLICITLY — the field it adds gets a documented
		// default here, not a zero value by accident.
		return Request{}, fmt.Errorf(
			"this plan was saved in format version %d, which this version of Ledgermancy cannot read "+
				"(it understands version %d)", version, CurrentInputVersion)
	}

	var in StoredInputs
	if err := json.Unmarshal(raw, &in); err != nil {
		return Request{}, fmt.Errorf("saved plan is not readable: %w", err)
	}
	return in.ToRequest()
}

// ToRequest parses the stored strings back into exact decimals.
func (in StoredInputs) ToRequest() (Request, error) {
	req := Request{
		HorizonYears: in.HorizonYears,
		FamilyHSA:    in.FamilyHSA,
		Splits:       make([]Split, 0, len(in.Buckets)),
	}
	var err error
	if req.Lump, err = parseMoney(in.Lump, "lump"); err != nil {
		return Request{}, err
	}
	if req.Monthly, err = parseMoney(in.Monthly, "monthly"); err != nil {
		return Request{}, err
	}
	if in.Target != nil {
		t, err := parseMoney(*in.Target, "target")
		if err != nil {
			return Request{}, err
		}
		req.TargetNestEgg = decimal.NewNullDecimal(t)
	}
	for i, b := range in.Buckets {
		s := Split{AccountID: b.AccountID}
		if s.LumpPct, err = parseMoney(b.LumpPct, fmt.Sprintf("buckets[%d].lump_pct", i)); err != nil {
			return Request{}, err
		}
		if s.MonthlyPct, err = parseMoney(b.MonthlyPct, fmt.Sprintf("buckets[%d].monthly_pct", i)); err != nil {
			return Request{}, err
		}
		if b.RealReturnRate != nil {
			r, err := parseMoney(*b.RealReturnRate, fmt.Sprintf("buckets[%d].real_return_rate", i))
			if err != nil {
				return Request{}, err
			}
			s.RealReturnRate = decimal.NewNullDecimal(r)
		}
		req.Splits = append(req.Splits, s)
	}
	return req, nil
}

// parseMoney reads a decimal STRING. It refuses an empty value rather than
// defaulting to zero: a missing money field in a saved plan is a corrupted plan,
// and "$0.00" is a plan somebody might act on.
func parseMoney(raw, field string) (decimal.Decimal, error) {
	if raw == "" {
		return decimal.Zero, errors.New(field + " is missing from the saved plan")
	}
	v, err := decimal.NewFromString(raw)
	if err != nil {
		return decimal.Zero, errors.New(field + " is not a decimal string: " + raw)
	}
	return v, nil
}
