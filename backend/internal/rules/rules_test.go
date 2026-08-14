package rules

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Matching and planning, with no database in sight.
//
// These are the assertions that have to hold for the engine to be trustworthy at
// all, and they are all pure functions of a snapshot — so they run in
// milliseconds and are not skipped when TEST_DATABASE_URL is unset. The
// DB-backed half (visibility scoping, the sticky-manual predicate, the
// reconciliation) lives in engine_test.go.

func money(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

// EVERY trigger type, matched and unmatched, plus the inverted reading of each.
//
// Enumerated rather than sampled: a trigger that silently always returns false
// is a rule that never fires, and a trigger that silently always returns true is
// a rule that tags the entire household. Neither shows up in a spot check of the
// three most interesting cases.
func TestTriggersMatch(t *testing.T) {
	coffee, food := uuid.New(), uuid.New()
	checking, savings := uuid.New(), uuid.New()

	base := Transaction{
		AccountID:      checking,
		Name:           "SQ *BLUE BOTTLE 0421",
		MerchantName:   "Blue Bottle Coffee",
		Amount:         money("12.50"),
		CategoryID:     &coffee,
		HasAttachments: false,
	}

	cases := []struct {
		name    string
		trigger Trigger
		tx      Transaction
		want    bool
	}{
		// --- Description. Reads transactions.name, NOT the merchant. ---
		{"contains hit", Trigger{Type: TriggerDescriptionContains, Value: "blue bottle"}, base, true},
		{"contains miss", Trigger{Type: TriggerDescriptionContains, Value: "starbucks"}, base, false},
		// Case folding is not a nicety: a user typing a pattern is not choosing
		// a case convention, and Plaid descriptors are shouted.
		{"contains folds case", Trigger{Type: TriggerDescriptionContains, Value: "BlUe BoTtLe"}, base, true},
		{"starts hit", Trigger{Type: TriggerDescriptionStarts, Value: "sq *"}, base, true},
		{"starts miss", Trigger{Type: TriggerDescriptionStarts, Value: "blue"}, base, false},
		{"ends hit", Trigger{Type: TriggerDescriptionEnds, Value: "0421"}, base, true},
		{"ends miss", Trigger{Type: TriggerDescriptionEnds, Value: "bottle"}, base, false},
		{"is hit", Trigger{Type: TriggerDescriptionIs, Value: "sq *blue bottle 0421"}, base, true},
		{"is is not contains", Trigger{Type: TriggerDescriptionIs, Value: "blue bottle"}, base, false},
		// The description trigger must NOT quietly also read the merchant.
		// Conflating the two is the single easiest way to make "description is"
		// mean something no user asked for.
		{"description ignores merchant", Trigger{Type: TriggerDescriptionIs, Value: "Blue Bottle Coffee"}, base, false},

		// --- Merchant, which falls back to the description. ---
		{"merchant hit", Trigger{Type: TriggerMerchantIs, Value: "blue bottle coffee"}, base, true},
		{"merchant miss", Trigger{Type: TriggerMerchantIs, Value: "sq *blue bottle 0421"}, base, false},
		{"merchant falls back to name", Trigger{Type: TriggerMerchantIs, Value: "corner store"},
			Transaction{Name: "Corner Store"}, true},

		// --- Amount. Compares SIZE, so direction never changes the answer. ---
		{"more hit", Trigger{Type: TriggerAmountMore, Value: "10"}, base, true},
		{"more miss", Trigger{Type: TriggerAmountMore, Value: "12.50"}, base, false},
		{"less hit", Trigger{Type: TriggerAmountLess, Value: "20"}, base, true},
		{"less miss", Trigger{Type: TriggerAmountLess, Value: "12.50"}, base, false},
		{"exactly hit", Trigger{Type: TriggerAmountExactly, Value: "12.5"}, base, true},
		{"exactly miss", Trigger{Type: TriggerAmountExactly, Value: "12.51"}, base, false},
		// A refund of the same size answers the same question. If this ever
		// flips, "amount more than 10" silently stops seeing every credit.
		{"amount ignores direction", Trigger{Type: TriggerAmountMore, Value: "10"},
			Transaction{Name: "refund", Amount: money("-12.50")}, true},
		// A cents-level comparison a float would get wrong. Money is never a
		// float, at any layer, and this is the layer people forget.
		{"amount is exact at the cent", Trigger{Type: TriggerAmountExactly, Value: "0.10"},
			Transaction{Amount: money("0.10")}, true},
		{"amount does not round", Trigger{Type: TriggerAmountExactly, Value: "0.10"},
			Transaction{Amount: money("0.1000001")}, false},

		// --- Category. ---
		{"category hit", Trigger{Type: TriggerCategoryIs, Value: coffee.String()}, base, true},
		{"category miss", Trigger{Type: TriggerCategoryIs, Value: food.String()}, base, false},
		{"category miss when none", Trigger{Type: TriggerCategoryIs, Value: coffee.String()},
			Transaction{Name: "x"}, false},
		{"has no category hit", Trigger{Type: TriggerHasNoCategory}, Transaction{Name: "x"}, true},
		{"has no category miss", Trigger{Type: TriggerHasNoCategory}, base, false},

		// --- Account and attachments. ---
		{"account hit", Trigger{Type: TriggerAccountIs, Value: checking.String()}, base, true},
		{"account miss", Trigger{Type: TriggerAccountIs, Value: savings.String()}, base, false},
		{"attachments hit", Trigger{Type: TriggerHasAttachments},
			Transaction{Name: "x", HasAttachments: true}, true},
		{"attachments miss", Trigger{Type: TriggerHasAttachments}, base, false},

		// --- The unreadable cases. An unknown type, and an operand that does
		// not parse, must NOT hold. Holding would widen the rule into one that
		// matches more than the user wrote. ---
		{"unknown type never holds", Trigger{Type: TriggerType("teleports"), Value: "x"}, base, false},
		{"unparseable amount never holds", Trigger{Type: TriggerAmountMore, Value: "ten dollars"}, base, false},
		{"unparseable uuid never holds", Trigger{Type: TriggerCategoryIs, Value: "not-a-uuid"}, base, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.trigger.Holds(tc.tx); got != tc.want {
				t.Fatalf("Holds() = %v, want %v", got, tc.want)
			}
			// Invert is the whole NOT vocabulary, so it is asserted for every
			// case rather than for one: a trigger whose inversion is not its
			// negation is a rule that means neither what it says nor its
			// opposite.
			//
			// The unreadable cases are the deliberate exception, and the reason
			// this loop is worth the extra assertion. Reading "unknown" as
			// false and then inverting would turn a rule this build cannot
			// understand into one that matches EVERY transaction in the
			// household. They must stay false inverted.
			inverted := tc.trigger
			inverted.Invert = true
			want := !tc.want
			if strings.HasSuffix(tc.name, "never holds") {
				want = false
			}
			if got := inverted.Holds(tc.tx); got != want {
				t.Fatalf("inverted Holds() = %v, want %v", got, want)
			}
		})
	}
}

// A rule fires only when EVERY trigger holds, and a rule with none fires never.
func TestRuleMatchesIsAnd(t *testing.T) {
	tx := Transaction{Name: "STARBUCKS #1234", Amount: money("12.50")}

	both := Rule{Triggers: []Trigger{
		{Type: TriggerDescriptionContains, Value: "starbucks"},
		{Type: TriggerAmountMore, Value: "10"},
	}}
	if !both.Matches(tx) {
		t.Fatal("both conditions hold, rule should match")
	}

	// One failing condition is enough. There is no OR, and no partial credit.
	one := Rule{Triggers: []Trigger{
		{Type: TriggerDescriptionContains, Value: "starbucks"},
		{Type: TriggerAmountMore, Value: "50"},
	}}
	if one.Matches(tx) {
		t.Fatal("second condition fails, rule must not match")
	}

	// THE DANGEROUS ONE. An empty AND is TRUE in logic, which would make a
	// trigger-less rule apply its actions to every transaction in the
	// household. The engine reads it as "match nothing" instead, and the API
	// refuses to store one at all.
	if (Rule{}).Matches(tx) {
		t.Fatal("a rule with no conditions must match nothing, not everything")
	}
}

// Every action type: what it does, and what it refuses.
func TestActionsPlan(t *testing.T) {
	coffee, food := uuid.New(), uuid.New()
	tagA := uuid.New()

	t.Run("set category files an unfiled row", func(t *testing.T) {
		tx := Transaction{Name: "x"}
		effects := plan(Rule{Actions: []Action{
			{Type: ActionSetCategory, Value: coffee.String()},
		}}, &tx)

		assertOutcome(t, effects[0], OutcomeApplied)
		if tx.CategoryID == nil || *tx.CategoryID != coffee {
			t.Fatalf("category = %v, want %v", tx.CategoryID, coffee)
		}
		// The row is now a rule's doing, and says so. Reports and the ledger
		// read this to explain where a category came from.
		if tx.CategorySource != "rule" {
			t.Fatalf("category source = %q, want \"rule\"", tx.CategorySource)
		}
	})

	// THE LOAD-BEARING INVARIANT. A row the user filed by hand is the one thing
	// automation may not overwrite. If this test ever goes green with the check
	// removed, every correction a user makes one row at a time is undone on the
	// next sync.
	t.Run("set category refuses a manual choice", func(t *testing.T) {
		tx := Transaction{Name: "x", CategoryID: &food, CategorySource: "manual"}
		effects := plan(Rule{Actions: []Action{
			{Type: ActionSetCategory, Value: coffee.String()},
		}}, &tx)

		assertOutcome(t, effects[0], OutcomeRefused)
		if *tx.CategoryID != food {
			t.Fatal("a manual category was overwritten")
		}
		if effects[0].Reason == "" {
			t.Fatal("a refusal with no reason is what makes users distrust automation")
		}
	})

	// A category the rule engine itself set is fair game — otherwise the first
	// rule to touch a row would freeze it against every later one.
	t.Run("set category overwrites a rule's own earlier choice", func(t *testing.T) {
		tx := Transaction{Name: "x", CategoryID: &food, CategorySource: "rule"}
		effects := plan(Rule{Actions: []Action{
			{Type: ActionSetCategory, Value: coffee.String()},
		}}, &tx)
		assertOutcome(t, effects[0], OutcomeApplied)
	})

	t.Run("add tag adds once", func(t *testing.T) {
		tx := Transaction{Name: "x"}
		action := Action{Type: ActionAddTag, Value: tagA.String()}

		first := plan(Rule{Actions: []Action{action}}, &tx)
		assertOutcome(t, first[0], OutcomeApplied)

		second := plan(Rule{Actions: []Action{action}}, &tx)
		assertOutcome(t, second[0], OutcomeUnchanged)
		if len(tx.TagIDs) != 1 {
			t.Fatalf("tags = %v, want exactly one", tx.TagIDs)
		}
	})

	t.Run("set notes replaces", func(t *testing.T) {
		tx := Transaction{Name: "x", Notes: "old"}
		effects := plan(Rule{Actions: []Action{
			{Type: ActionSetNotes, Value: "new"},
		}}, &tx)
		assertOutcome(t, effects[0], OutcomeApplied)
		if tx.Notes != "new" {
			t.Fatalf("notes = %q, want %q", tx.Notes, "new")
		}
	})

	t.Run("unknown action is refused, not ignored", func(t *testing.T) {
		tx := Transaction{Name: "x"}
		effects := plan(Rule{Actions: []Action{{Type: ActionType("teleport")}}}, &tx)
		assertOutcome(t, effects[0], OutcomeRefused)
	})
}

// THE OTHER LOAD-BEARING INVARIANT: appending a note twice must not write it
// twice. A plain concatenation would grow the field on every sync until the note
// was the same sentence forty times, which is the most obvious way a rule engine
// ruins data.
func TestAppendNoteIsIdempotent(t *testing.T) {
	cases := []struct{ name, existing, add, want string }{
		{"empty takes the value", "", "reviewed", "reviewed"},
		{"whitespace counts as empty", "   ", "reviewed", "reviewed"},
		{"appends on a new line", "old", "reviewed", "old\nreviewed"},
		{"a second append changes nothing", "old\nreviewed", "reviewed", "old\nreviewed"},
		{"matches the line, not a substring", "old\nreviewed twice", "reviewed",
			"old\nreviewed twice\nreviewed"},
		{"ignores surrounding whitespace on both sides", "old\n  reviewed  ", "reviewed",
			"old\n  reviewed  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := appendNote(tc.existing, tc.add); got != tc.want {
				t.Fatalf("appendNote(%q, %q) = %q, want %q", tc.existing, tc.add, got, tc.want)
			}
		})
	}

	// The property that actually matters, stated as one: running the same
	// action any number of times is the same as running it once.
	tx := Transaction{Name: "x", Notes: "seen"}
	rule := Rule{Actions: []Action{{Type: ActionAppendNotes, Value: "filed by rule"}}}
	plan(rule, &tx)
	once := tx.Notes
	for range 5 {
		plan(rule, &tx)
	}
	if tx.Notes != once {
		t.Fatalf("notes after six runs = %q, want %q", tx.Notes, once)
	}
}

// Actions run in order against a snapshot that carries the previous action's
// work, which is the only reason "set notes" then "append notes" is meaningful.
func TestActionsSeeEachOther(t *testing.T) {
	tx := Transaction{Name: "x", Notes: "original"}
	effects := plan(Rule{Actions: []Action{
		{Type: ActionSetNotes, Value: "filed"},
		{Type: ActionAppendNotes, Value: "by rule"},
	}}, &tx)

	assertOutcome(t, effects[0], OutcomeApplied)
	assertOutcome(t, effects[1], OutcomeApplied)
	if tx.Notes != "filed\nby rule" {
		t.Fatalf("notes = %q, want %q", tx.Notes, "filed\nby rule")
	}
}

// stop_on_fail halts the REST of a rule when an action is REFUSED — and only
// then. An action that was already satisfied succeeded, and stopping on it would
// make a rule get shorter every time it ran.
func TestStopOnFail(t *testing.T) {
	coffee, food := uuid.New(), uuid.New()
	tag := uuid.New()

	t.Run("a refusal stops what follows", func(t *testing.T) {
		tx := Transaction{Name: "x", CategoryID: &food, CategorySource: "manual"}
		effects := plan(Rule{Actions: []Action{
			{Type: ActionSetCategory, Value: coffee.String(), StopOnFail: true},
			{Type: ActionAddTag, Value: tag.String()},
		}}, &tx)

		assertOutcome(t, effects[0], OutcomeRefused)
		assertOutcome(t, effects[1], OutcomeSkipped)
		if len(tx.TagIDs) != 0 {
			t.Fatal("the second action ran after the first was refused and stopped the rule")
		}
	})

	t.Run("without the flag a refusal is not fatal", func(t *testing.T) {
		tx := Transaction{Name: "x", CategoryID: &food, CategorySource: "manual"}
		effects := plan(Rule{Actions: []Action{
			{Type: ActionSetCategory, Value: coffee.String()},
			{Type: ActionAddTag, Value: tag.String()},
		}}, &tx)

		assertOutcome(t, effects[0], OutcomeRefused)
		assertOutcome(t, effects[1], OutcomeApplied)
	})

	t.Run("an already-satisfied action stops nothing", func(t *testing.T) {
		tx := Transaction{Name: "x", CategoryID: &coffee, CategorySource: "rule"}
		effects := plan(Rule{Actions: []Action{
			{Type: ActionSetCategory, Value: coffee.String(), StopOnFail: true},
			{Type: ActionAddTag, Value: tag.String()},
		}}, &tx)

		assertOutcome(t, effects[0], OutcomeUnchanged)
		assertOutcome(t, effects[1], OutcomeApplied)
	})
}

// Rules run in priority order against a snapshot later rules see, so ordering is
// a feature the user can rely on rather than an accident of the planner.
func TestRulesRunInOrderAndSeeEachOther(t *testing.T) {
	coffee := uuid.New()
	tag := uuid.New()

	engine := New([]Rule{
		{
			Name:     "file coffee",
			Priority: 10,
			Triggers: []Trigger{{Type: TriggerDescriptionContains, Value: "blue bottle"}},
			Actions:  []Action{{Type: ActionSetCategory, Value: coffee.String()}},
		},
		{
			// Fires only because the rule above already ran.
			Name:     "tag anything filed as coffee",
			Priority: 5,
			Triggers: []Trigger{{Type: TriggerCategoryIs, Value: coffee.String()}},
			Actions:  []Action{{Type: ActionAddTag, Value: tag.String()}},
		},
	})

	final, results := engine.Preview(Transaction{Name: "SQ *BLUE BOTTLE"})
	if len(results) != 2 {
		t.Fatalf("results = %d, want both rules to fire", len(results))
	}
	if !final.HasTag(tag) {
		t.Fatal("the second rule did not see the first rule's category")
	}
	// Preview is a pure function of the snapshot: the second call must agree
	// with the first, or the tester and the run cannot be trusted to agree.
	if _, again := engine.Preview(Transaction{Name: "SQ *BLUE BOTTLE"}); len(again) != 2 {
		t.Fatal("Preview is not repeatable")
	}
}

// Validation on write. The point is not that bad input is rejected — it is that
// it is rejected AT THE MOMENT IT CAN STILL BE FIXED, rather than stored and
// discovered as a rule that quietly never does anything.
func TestValidation(t *testing.T) {
	id := uuid.New()

	triggers := []struct {
		name    string
		typ     TriggerType
		value   string
		wantErr bool
		want    string
	}{
		{"text needs text", TriggerDescriptionContains, "   ", true, ""},
		{"text is trimmed", TriggerDescriptionContains, "  starbucks ", false, "starbucks"},
		{"amount must parse", TriggerAmountMore, "ten", true, ""},
		{"amount is normalised", TriggerAmountMore, " 10.500 ", false, "10.5"},
		// A negative operand describes a comparison against a size, which is
		// never negative — a rule that is always true or always false.
		{"amount must not be negative", TriggerAmountMore, "-5", true, ""},
		{"category must be a uuid", TriggerCategoryIs, "coffee", true, ""},
		{"category uuid is kept", TriggerCategoryIs, id.String(), false, id.String()},
		// A stale operand from an editor that switched types is dropped rather
		// than stored, so switching back cannot resurrect it.
		{"operandless types drop the value", TriggerHasNoCategory, "junk", false, ""},
		{"unknown type is refused", TriggerType("teleports"), "x", true, ""},
	}
	for _, tc := range triggers {
		t.Run("trigger/"+tc.name, func(t *testing.T) {
			got, msg := ValidateTrigger(tc.typ, tc.value)
			if (msg != "") != tc.wantErr {
				t.Fatalf("message = %q, wantErr %v", msg, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("value = %q, want %q", got, tc.want)
			}
		})
	}

	actions := []struct {
		name    string
		typ     ActionType
		value   string
		wantErr bool
	}{
		{"category must be a uuid", ActionSetCategory, "food", true},
		{"tag must be a uuid", ActionAddTag, "", true},
		{"valid tag", ActionAddTag, id.String(), false},
		// Empty set-notes is how a rule CLEARS notes, so it is valid...
		{"set notes may be empty", ActionSetNotes, "  ", false},
		// ...while an empty append is an action that can never do anything.
		{"append notes may not be empty", ActionAppendNotes, "  ", true},
		{"unknown type is refused", ActionType("teleport"), "x", true},
	}
	for _, tc := range actions {
		t.Run("action/"+tc.name, func(t *testing.T) {
			if _, msg := ValidateAction(tc.typ, tc.value); (msg != "") != tc.wantErr {
				t.Fatalf("message = %q, wantErr %v", msg, tc.wantErr)
			}
		})
	}
}

func assertOutcome(t *testing.T, got Effect, want Outcome) {
	t.Helper()
	if got.Outcome != want {
		t.Fatalf("outcome = %q (%s), want %q", got.Outcome, got.Reason, want)
	}
}
