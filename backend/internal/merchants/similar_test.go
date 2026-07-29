package merchants

import (
	"strings"
	"testing"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/plaid"
)

// The descriptor noise a real card feed produces for one merchant. These run
// through plaid.MerchantKey first, exactly as the suggestion engine sees them,
// so the fixture stays honest about what the normaliser has already stripped.
var amazonDescriptors = []string{
	"AMAZON.COM",
	"AMZN.COM/BILL",
	"AMZ*ORDER 1234",
	"AMAZON.COM*ABCD1",
}

func TestCompareMergesAmazonFamily(t *testing.T) {
	keys := make([]string, 0, len(amazonDescriptors))
	for _, d := range amazonDescriptors {
		k := plaid.MerchantKey("", d)
		if k == "" {
			t.Fatalf("MerchantKey(%q) returned empty", d)
		}
		keys = append(keys, k)
	}

	// Every member has to reach every other, directly or through the union-find
	// in FuzzyGroups. Assert the whole family lands in one component.
	rows := statRows(keys...)
	groups := FuzzyGroups(rows, nil)
	if len(groups) != 1 {
		t.Fatalf("want 1 group for the Amazon family, got %d: %+v", len(groups), groups)
	}
	if len(groups[0].Keys) != len(keys) {
		t.Fatalf("want all %d Amazon descriptors merged, got %v", len(keys), groups[0].Keys)
	}
}

// The negative case the design exists to protect: two different businesses that
// share a leading token. A rule loose enough to merge these is worse than no
// rule at all.
func TestCompareKeepsDistinctMerchantsApart(t *testing.T) {
	cases := [][2]string{
		{"SQ *BLUE BOTTLE #4412 OAKLAND CA", "SQ *BLUE RIDGE #0087 BERKELEY CA"},
		{"CITY MARKET", "CITY OPTICAL"},
		{"TST*JOES PIZZA", "TST*JOANNE FABRIC"},
	}
	for _, c := range cases {
		a, b := plaid.MerchantKey("", c[0]), plaid.MerchantKey("", c[1])
		if got := Compare(a, b); got.Match {
			t.Errorf("Compare(%q, %q) matched (%s); these are different businesses", a, b, got.Reason)
		}
	}
}

func TestCompareRules(t *testing.T) {
	tests := []struct {
		name  string
		a, b  string
		match bool
	}{
		{"identical once noise is stripped", "netflix", "netflix com", true},
		{"trailing reference token", "amazon com", "amazon com abcd1", true},
		{"vowel-dropped abbreviation", "amazon com", "amzn com bill", true},
		{"truncated brand", "amazon com", "amz order", true},
		{"same key is not a merge", "netflix", "netflix", false},
		{"no significant tokens", "bill payment", "online order", false},
		{"unrelated", "netflix", "spotify", false},
		{"shares only a noise word", "acme com", "zenith com", false},
		{"short fragment is not an abbreviation", "bp", "bpwholesale", false},
		{"too great a contraction", "tgt", "targetcorporation", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Compare(tc.a, tc.b).Match; got != tc.match {
				t.Errorf("Compare(%q, %q).Match = %v, want %v", tc.a, tc.b, got, tc.match)
			}
		})
	}
}

// A rejected pair must never be proposed again, or the review queue refills with
// the same merge every pass and the reject button means nothing.
func TestFuzzyGroupsHonoursRejections(t *testing.T) {
	rows := statRows("netflix", "netflix com")

	if groups := FuzzyGroups(rows, nil); len(groups) != 1 {
		t.Fatalf("baseline: want 1 group, got %d", len(groups))
	}

	rejected := map[pair]struct{}{orderedPair("netflix", "netflix com"): {}}
	if groups := FuzzyGroups(rows, rejected); len(groups) != 0 {
		t.Fatalf("rejected pair was proposed again: %+v", groups)
	}
}

// Rejection has to survive transitivity too: A~B and B~C would otherwise
// re-merge a pair the user split, just with an extra hop.
//
// The rejection CUTS the component rather than deleting it. Refusing one pair is
// not a statement about the descriptors the user never objected to, and deleting
// the whole family would mean a later descriptor that chains into it could never
// be proposed again either.
func TestFuzzyGroupsSplitsComponentAtRejection(t *testing.T) {
	rows := statRows("amazon com", "amzn com bill", "amz order")
	rejected := map[pair]struct{}{orderedPair("amazon com", "amz order"): {}}

	groups := FuzzyGroups(rows, rejected)
	if len(groups) != 1 {
		t.Fatalf("want 1 group after the cut, got %d: %+v", len(groups), groups)
	}
	if got := strings.Join(groups[0].Keys, "|"); got != "amazon com|amzn com bill" {
		t.Fatalf("want the unobjected pair kept together, got %q", got)
	}
	for _, k := range groups[0].Keys {
		if k == "amz order" {
			t.Fatal("the rejected descriptor was merged back in through a third key")
		}
	}
}

// The regression suite for the false merges this engine actually produced on real
// card data. Every pair here was proposed as one merchant at some point; each one
// is two different businesses that a loose rule chained together.
func TestCompareRejectsRealWorldFalseMerges(t *testing.T) {
	apart := [][2]string{
		// "home" truncates "homegoods", so a hardware store read as a homeware
		// store. The rule that fired had no business comparing one word of a
		// two-word name.
		{"home depot", "homegoods"},
		{"the home depot #4905", "homegoods #0265"},
		// "taco" is a subsequence of "target.com" once the domain tail is in play.
		{"taco bell", "target.com"},
		{"taco johns", "tacomex"},
		// "par" truncates "park", and "panera" starts with "par".
		{"panera bread", "par moka - west ave"},
		{"par moka - west ave", "park mgm-eataly qsr ba"},
		// An unstripped Toast prefix made every restaurant an abbreviation of
		// every other one — 15 unrelated businesses in a single proposal.
		{"tst* 608 brewing co", "tst*bleu duck- pato azul"},
		{"tst* caribou coffee -", "tst*taco broz"},
		// Ledger verbs are not merchants, and stripping them makes a deposit look
		// like the matching withdrawal — and both look like the carrier US Mobile.
		{"deposit mobile transfer to", "withdrawal mobile transfer to"},
		{"deposit mobile transfer to", "us mobile"},
	}
	for _, c := range apart {
		if got := Compare(c[0], c[1]); got.Match {
			t.Errorf("Compare(%q, %q) matched (%s); these are different businesses", c[0], c[1], got.Reason)
		}
	}

	// The flip side: the punctuation and domain variants that the narrowed
	// abbreviation rule no longer catches must now match OUTRIGHT, at the highest
	// confidence rather than the 0.70 guess they used to earn.
	exact := [][2]string{
		{"wal-mart", "walmart"},
		{"walmart", "walmart.com"},
		{"kwik trip", "kwik-trip"},
		{"target", "target.com"},
		{"par moka - west ave", "par*moka - west ave"},
		{"teacherspayteachers com", "teacherspayteachers.com"},
	}
	for _, c := range exact {
		got := Compare(c[0], c[1])
		if !got.Match {
			t.Errorf("Compare(%q, %q) did not match; these are one business", c[0], c[1])
			continue
		}
		if got.Confidence != confIdentical {
			t.Errorf("Compare(%q, %q) = %.2f, want %.2f: a punctuation variant is an exact match, not a guess",
				c[0], c[1], got.Confidence, confIdentical)
		}
	}

	// Store numbers and reference tails still land on the containment rule, which
	// is the weaker claim they deserve — "walmart plus a store number" is evidence,
	// not identity. (These collapse to one key outright once the import path runs
	// them through plaid.MerchantKey.)
	contained := [][2]string{
		{"wal-mart #1679", "walmart"},
		{"kwik trip", "kwik trip #1114"},
		{"home depot", "the home depot #4905"},
	}
	for _, c := range contained {
		got := Compare(c[0], c[1])
		if !got.Match {
			t.Errorf("Compare(%q, %q) did not match; these are one business", c[0], c[1])
			continue
		}
		if got.Confidence != confContained {
			t.Errorf("Compare(%q, %q) = %.2f, want %.2f", c[0], c[1], got.Confidence, confContained)
		}
	}
}

func TestDisplayName(t *testing.T) {
	tests := map[string]string{
		"amazon com":   "Amazon",
		"blue bottle":  "Blue Bottle",
		"netflix":      "Netflix",
		"bill payment": "Bill Payment", // nothing significant survives; keep it whole
	}
	for key, want := range tests {
		if got := DisplayName(key); got != want {
			t.Errorf("DisplayName(%q) = %q, want %q", key, got, want)
		}
	}
}

// statRows builds the query-row shape FuzzyGroups consumes. Counts descend so
// the first key is the dominant one and naming is deterministic.
func statRows(keys ...string) []dbgen.ListMerchantKeyStatsRow {
	rows := make([]dbgen.ListMerchantKeyStatsRow, 0, len(keys))
	for i, k := range keys {
		rows = append(rows, dbgen.ListMerchantKeyStatsRow{
			MerchantKey:      k,
			TransactionCount: int64(len(keys) - i),
			SampleName:       k,
		})
	}
	return rows
}
