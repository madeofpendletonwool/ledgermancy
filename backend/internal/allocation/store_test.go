package allocation

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/shopspring/decimal"
)

// MONEY IN JSONB, and why this needs a test rather than a comment.
//
// The continuity rule says money is cast to text in SQL and never travels as a
// JSON number, and export.go enforces it for `numeric` COLUMNS. IT CANNOT REACH
// INSIDE A JSONB COLUMN — normalise passes jsonb through as json.RawMessage. So
// the rule holds for allocation_plans.inputs only because the WRITER never puts
// a JSON number there, and nothing structural stops the next field from being
// typed as a decimal.Decimal and marshalling as one.
//
// Without the assertion below, the rule is a comment nobody notices breaking.

// moneyFields are the keys whose values must be JSON STRINGS. Listed explicitly
// rather than inferred, so adding a money field without adding it here is a
// conscious act rather than a silent gap.
var moneyFields = []string{"lump", "monthly", "target", "lump_pct", "monthly_pct", "real_return_rate"}

// bareNumber matches `"key": 123.45` — a JSON number where a decimal string
// belongs.
func bareNumberPattern(field string) *regexp.Regexp {
	return regexp.MustCompile(`"` + field + `"\s*:\s*-?[0-9]`)
}

func sampleRequest() Request {
	return Request{
		// Values chosen for the reason 0.1 + 0.2 is the canonical float example:
		// none of them survives a float64 round trip unchanged, so a regression
		// to JSON numbers shows up as a changed value rather than as nothing.
		Lump:          dec("30000.50"),
		Monthly:       dec("1800.10"),
		HorizonYears:  17,
		TargetNestEgg: decimal.NewNullDecimal(dec("1500000.20")),
		FamilyHSA:     true,
		Splits: []Split{
			{AccountID: rothID, LumpPct: dec("0.1"), MonthlyPct: dec("0.2")},
			{
				AccountID: brokID, LumpPct: dec("33.33"), MonthlyPct: dec("66.67"),
				RealReturnRate: decimal.NewNullDecimal(dec("0.0575")),
			},
		},
	}
}

// EVERY MONEY FIELD IS A STRING ON THE WIRE. This is the assertion that keeps
// the continuity guarantee true for this table.
func TestNoBareJSONNumberInAMoneyField(t *testing.T) {
	raw, err := json.Marshal(Store(sampleRequest()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for _, field := range moneyFields {
		if bareNumberPattern(field).Match(raw) {
			t.Errorf("%q is a bare JSON number in %s — export.go cannot cast inside a jsonb column, "+
				"so this would leave the portable export as a float", field, raw)
		}
	}

	// Counts stay numbers: they are not money, and they are exact in float64.
	if !regexp.MustCompile(`"horizon_years"\s*:\s*17`).Match(raw) {
		t.Errorf("horizon_years should be a JSON number, not a string: %s", raw)
	}
}

// SAVE → RELOAD IS BYTE-IDENTICAL. Not merely "close enough": decimal round-trips
// strings exactly, and the test asserts the exact representation rather than an
// epsilon, because an epsilon is how a float creeps back in unnoticed.
func TestPlanInputsRoundTripByteIdentical(t *testing.T) {
	original := sampleRequest()

	raw, err := json.Marshal(Store(original))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, err := Load(raw, CurrentInputVersion)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// StringFixed(2) is what was written, so that is what must come back.
	if back.Lump.StringFixed(2) != original.Lump.StringFixed(2) {
		t.Errorf("lump = %s, want %s", back.Lump, original.Lump)
	}
	if back.Monthly.StringFixed(2) != original.Monthly.StringFixed(2) {
		t.Errorf("monthly = %s, want %s", back.Monthly, original.Monthly)
	}
	if !back.TargetNestEgg.Valid {
		t.Fatal("target lost in the round trip")
	}
	if back.TargetNestEgg.Decimal.StringFixed(2) != original.TargetNestEgg.Decimal.StringFixed(2) {
		t.Errorf("target = %s, want %s", back.TargetNestEgg.Decimal, original.TargetNestEgg.Decimal)
	}
	if back.HorizonYears != original.HorizonYears {
		t.Errorf("horizon = %d, want %d", back.HorizonYears, original.HorizonYears)
	}
	if back.FamilyHSA != original.FamilyHSA {
		t.Errorf("family_hsa = %v, want %v", back.FamilyHSA, original.FamilyHSA)
	}

	if len(back.Splits) != len(original.Splits) {
		t.Fatalf("splits = %d, want %d", len(back.Splits), len(original.Splits))
	}
	for i, want := range original.Splits {
		got := back.Splits[i]
		if got.AccountID != want.AccountID {
			t.Errorf("split %d account = %s, want %s", i, got.AccountID, want.AccountID)
		}
		if got.LumpPct.StringFixed(2) != want.LumpPct.StringFixed(2) {
			t.Errorf("split %d lump_pct = %s, want %s", i, got.LumpPct, want.LumpPct)
		}
		if got.MonthlyPct.StringFixed(2) != want.MonthlyPct.StringFixed(2) {
			t.Errorf("split %d monthly_pct = %s, want %s", i, got.MonthlyPct, want.MonthlyPct)
		}
		if want.RealReturnRate.Valid {
			// A RATE is not rounded to cents: 0.0575 is a real number somebody
			// may have typed, and StringFixed(2) would silently make it 0.06.
			if !got.RealReturnRate.Valid || !got.RealReturnRate.Decimal.Equal(want.RealReturnRate.Decimal) {
				t.Errorf("split %d rate = %v, want %s", i, got.RealReturnRate, want.RealReturnRate.Decimal)
			}
		} else if got.RealReturnRate.Valid {
			t.Errorf("split %d gained a rate it did not have", i)
		}
	}
}

// "No target" must survive as ABSENT rather than collapsing to "0.00", which
// would be a target of zero — a different, and always-met, claim.
func TestAbsentTargetStaysAbsent(t *testing.T) {
	req := sampleRequest()
	req.TargetNestEgg = decimal.NullDecimal{}

	raw, err := json.Marshal(Store(req))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if regexp.MustCompile(`"target"`).Match(raw) {
		t.Errorf("an absent target should be omitted, not serialised: %s", raw)
	}

	back, err := Load(raw, CurrentInputVersion)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if back.TargetNestEgg.Valid {
		t.Error("an absent target came back as a value")
	}
}

// INPUT_VERSION IS LOAD-BEARING. Saved plans are kept for years, so a version
// this build does not know is REFUSED with a readable message rather than parsed
// with today's struct — which would run, and would not be the plan that was
// saved.
func TestUnknownInputVersionIsRefusedReadably(t *testing.T) {
	raw, err := json.Marshal(Store(sampleRequest()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if _, err := Load(raw, CurrentInputVersion+1); err == nil {
		t.Fatal("a future input_version was parsed with today's shape")
	} else if !regexp.MustCompile(`version`).MatchString(err.Error()) {
		t.Errorf("error %q does not name the version problem", err)
	}

	// And the current version still opens, which is the other half of the claim.
	if _, err := Load(raw, CurrentInputVersion); err != nil {
		t.Errorf("a v%d plan failed to open: %v", CurrentInputVersion, err)
	}
}

// A corrupted blob is reported rather than defaulted. A missing money field is
// NOT zero: "$0.00" is a plan somebody might act on.
func TestCorruptSavedPlanIsRefusedNotDefaulted(t *testing.T) {
	cases := map[string]string{
		"missing lump":      `{"monthly":"100.00","horizon_years":10,"buckets":[]}`,
		"lump not a string": `{"lump":"","monthly":"100.00","horizon_years":10,"buckets":[]}`,
		"unparseable":       `{"lump":"about thirty grand","monthly":"100.00","horizon_years":10,"buckets":[]}`,
		"bad bucket pct":    `{"lump":"1.00","monthly":"1.00","horizon_years":10,"buckets":[{"account_id":"11111111-1111-4111-8111-111111111111","lump_pct":"","monthly_pct":"0"}]}`,
	}
	for name, raw := range cases {
		if _, err := Load([]byte(raw), CurrentInputVersion); err == nil {
			t.Errorf("%s: want an error rather than a defaulted plan", name)
		}
	}
}

// The assumptions snapshot records what a plan was BUILT with — and deliberately
// does not carry the MAGI itself. A saved plan rides in a plain JSON export a
// user may email themselves, and a household's modified AGI is not a figure that
// belongs in one; whether the check could run is what the plan needs to explain
// itself.
func TestAssumptionsSnapshotWithholdsTheMAGI(t *testing.T) {
	snap := SnapshotAssumptions(testBaseline())

	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if regexp.MustCompile(`90000`).Match(raw) {
		t.Errorf("the MAGI itself leaked into the snapshot: %s", raw)
	}
	if !snap.MAGIKnown {
		t.Error("magi_known should record that the check COULD run")
	}
	if snap.TaxYear != 2026 {
		t.Errorf("tax year = %d, want 2026", snap.TaxYear)
	}
	if snap.CollegeInflationRate != "5.50" {
		t.Errorf("college inflation = %q, want a decimal string", snap.CollegeInflationRate)
	}
}
