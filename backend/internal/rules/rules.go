// Package rules is the user-editable IF-THEN engine over transactions.
//
// A rule is one or more TRIGGERS joined by AND ("description contains
// Starbucks" AND "amount is more than 10") plus one or more ACTIONS applied to
// every transaction that matches ("set category to Coffee", "add tag
// Caffeine"). Rules fire when a transaction is created and can be re-run over
// everything already stored.
//
// # Order against internal/categorize
//
// This is the thing most likely to be got wrong later, so it is stated first.
// The two packages do not compete, because they answer different questions:
//
//	categorize  answers ONE question — "which category?" — from a fixed
//	            precedence: manual > category_rules > merchant cache > Plaid's
//	            PFC > LLM. It runs first.
//
//	rules       answers a BROADER question — "what else should be true of this
//	            row?" — and runs AFTER categorize has settled the category. Its
//	            set-category action is the household overriding that answer
//	            deliberately.
//
// Concretely, in internal/plaid.Sync: CategoriseHousehold, then PairTransfers,
// then the rule engine. Anything that changes that order changes which of the
// two wins, so change it deliberately or not at all.
//
// # The invariants
//
// A MANUAL CATEGORY IS STICKY. A rule never overwrites
// category_source = 'manual'. A row the user filed by hand is the one thing no
// automation may touch — the merchant cache already follows this rule, and an
// engine that did not would silently undo the corrections a user makes one row
// at a time. The guarantee is a predicate in ApplyRuleCategory (queries/
// rules.sql), not a Go check that a future caller could route around; the check
// here exists so the user is TOLD the action was refused rather than left to
// notice nothing happened.
//
// IDEMPOTENCE. Running a rule twice over the same transaction produces the same
// row as running it once: no duplicated tag, no note appended twice, no write at
// all when nothing would change. Every action decides that from the snapshot it
// is handed, which is why ListRuleCandidates returns the row's tags and notes
// rather than just its identity.
//
// VISIBILITY. A rule belongs to a household, and which transactions it acts on
// runs through the same account_access predicate as every other transaction
// read. See the header of queries/rules.sql for why the two callers — a member
// asking, and a sync with nobody on the other end — pass different scopes.
//
// NO AI. Nothing here calls a model. The engine is deterministic and the app
// works with AI disabled, which is the whole reason a household can rely on it.
package rules

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// TriggerType is a condition's kind. The authoritative list is here rather than
// in a CHECK constraint or a Postgres enum: the set is expected to grow, and
// adding one should be a Go change with a test, not a migration deployed in
// lockstep with the code that emits the new string.
type TriggerType string

const (
	// Description triggers read the transaction's own description — the text
	// on the charge (transactions.name), NOT the merchant. The merchant has its
	// own trigger below, because the two differ constantly on Plaid rows
	// ("SQ *BLUE BOTTLE 0421" vs "Blue Bottle Coffee") and a user who wants one
	// is rarely served by the other.
	TriggerDescriptionContains TriggerType = "description_contains"
	TriggerDescriptionStarts   TriggerType = "description_starts"
	TriggerDescriptionEnds     TriggerType = "description_ends"
	TriggerDescriptionIs       TriggerType = "description_is"

	// Amount triggers compare the SIZE of the transaction, ignoring direction.
	// The schema's sign convention is Plaid's (positive = money out), so a
	// signed comparison would make "more than 10" quietly exclude a 50 refund —
	// a rule about ten dollars is about ten dollars whichever way the money
	// went. Firefly III compares absolute amounts for the same reason.
	TriggerAmountMore    TriggerType = "amount_more"
	TriggerAmountLess    TriggerType = "amount_less"
	TriggerAmountExactly TriggerType = "amount_exactly"

	// The merchant as the ledger shows it: merchant_name, falling back to the
	// description when Plaid supplied none (which is also what the ledger
	// renders, so the rule matches what the user is looking at).
	TriggerMerchantIs TriggerType = "merchant_is"

	TriggerCategoryIs     TriggerType = "category_is"
	TriggerHasNoCategory  TriggerType = "has_no_category"
	TriggerAccountIs      TriggerType = "account_is"
	TriggerHasAttachments TriggerType = "has_attachments"
)

// ActionType is a mutation's kind. Same reasoning as TriggerType for why the
// list lives in code.
type ActionType string

const (
	// Refuses to overwrite a manual category. See the package doc.
	ActionSetCategory ActionType = "set_category"
	// Adds one tag; never removes any. A transaction carries several tags, so
	// a replace would strip labels put there for unrelated reasons.
	ActionAddTag ActionType = "add_tag"
	// Replaces the notes outright. An empty value clears them.
	ActionSetNotes ActionType = "set_notes"
	// Adds a line to the notes, unless that exact line is already one of them.
	// That check is what stops a re-run from growing the field forever.
	ActionAppendNotes ActionType = "append_notes"
)

// Target says what kind of object a trigger or action's value names, so the
// handler knows which existence check to run before storing it. A rule that
// points at a category from another household must be refused on write, not
// discovered as a silent no-op months later.
type Target string

const (
	TargetNone     Target = ""
	TargetCategory Target = "category"
	TargetTag      Target = "tag"
	TargetAccount  Target = "account"
)

// Trigger is one condition of a rule.
type Trigger struct {
	ID    uuid.UUID
	Type  TriggerType
	Value string
	// Invert flips the condition ("description does NOT contain"). One flag
	// rather than a mirror type per trigger halves the vocabulary the user has
	// to learn and the engine has to implement.
	Invert   bool
	Position int32
}

// Action is one thing a rule does to a matching transaction.
type Action struct {
	ID    uuid.UUID
	Type  ActionType
	Value string
	// StopOnFail abandons the REST of this rule's actions for this transaction
	// when this one is REFUSED. "Refused" is not "changed nothing": an action
	// that was already satisfied succeeded, and stops nothing — which is what
	// keeps a re-run a no-op instead of a progressively shorter rule.
	StopOnFail bool
	Position   int32
}

// Rule is a household's IF-THEN statement: triggers AND-joined, actions in
// order.
type Rule struct {
	ID          uuid.UUID
	Name        string
	Description *string
	Active      bool
	// Priority orders rules against each other, higher first. It is
	// load-bearing rather than cosmetic: rules apply in sequence to a snapshot
	// that later rules see the effect of.
	Priority int32
	Triggers []Trigger
	Actions  []Action
}

// Transaction is the snapshot a rule is evaluated against.
//
// It carries everything a trigger can ask about AND everything an action needs
// in order to decide it would change nothing — the tags already on the row, the
// notes already written, how the category was decided. That completeness is
// what makes idempotence cheap: no action has to go back to the database to
// find out whether it has already run.
type Transaction struct {
	ID        uuid.UUID
	AccountID uuid.UUID
	// Name is the raw description on the charge; the description triggers read
	// this.
	Name string
	// MerchantName is empty when the source supplied none, in which case
	// Merchant() falls back to Name.
	MerchantName string
	Amount       decimal.Decimal
	CategoryID   *uuid.UUID
	// CategorySource is how CategoryID was decided: manual | rule | cache |
	// plaid | llm | heuristic | pairing. "manual" is the one value that makes a
	// set-category action refuse.
	CategorySource string
	Notes          string
	TagIDs         []uuid.UUID
	HasAttachments bool

	// Display-only, carried so a dry-run preview can render the row it is
	// talking about without a second query. Nothing matches on these.
	Date        string
	Currency    string
	AccountName string
}

// Merchant is the merchant as the ledger shows it: the supplied merchant name,
// or the description when there is none.
func (t Transaction) Merchant() string {
	if t.MerchantName != "" {
		return t.MerchantName
	}
	return t.Name
}

// HasTag reports whether the tag is already on the row.
func (t Transaction) HasTag(id uuid.UUID) bool {
	for _, existing := range t.TagIDs {
		if existing == id {
			return true
		}
	}
	return false
}

// Matches reports whether every one of the rule's triggers holds. The join is
// AND, always — see the migration for why there is no OR and no grouping.
//
// A rule with NO triggers matches NOTHING, which is the opposite of what an
// empty AND means in logic. That is deliberate: the empty rule is not a user
// saying "everything", it is a half-built rule or a bad write, and the cost of
// the two readings is wildly asymmetric — one does nothing, the other tags
// every transaction in the household. The API refuses to store a trigger-less
// rule; this is the second line of defence.
func (r Rule) Matches(t Transaction) bool {
	if len(r.Triggers) == 0 {
		return false
	}
	for _, trigger := range r.Triggers {
		if !trigger.Holds(t) {
			return false
		}
	}
	return true
}

// Holds evaluates one trigger against a transaction, applying Invert last.
//
// AN UNREADABLE TRIGGER NEVER HOLDS — not even inverted. A type this build does
// not understand, or an operand that does not parse, returns false whatever
// Invert says, and because triggers are AND-joined that makes the whole rule
// inert.
//
// That short-circuit is the important half. Reading "unknown" as false and THEN
// inverting would turn a rule this build cannot understand into one that matches
// every transaction in the household — a downgrade, or a rule written around the
// API, silently becoming "tag everything". Refusing to fire is the only failure
// mode that cannot destroy data.
func (tr Trigger) Holds(t Transaction) bool {
	result, readable := tr.raw(t)
	if !readable {
		return false
	}
	return result != tr.Invert
}

// raw returns the trigger's plain result and whether it could be read at all.
func (tr Trigger) raw(t Transaction) (bool, bool) {
	switch tr.Type {
	case TriggerDescriptionContains:
		return strings.Contains(fold(t.Name), fold(tr.Value)), true
	case TriggerDescriptionStarts:
		return strings.HasPrefix(fold(t.Name), fold(tr.Value)), true
	case TriggerDescriptionEnds:
		return strings.HasSuffix(fold(t.Name), fold(tr.Value)), true
	case TriggerDescriptionIs:
		return fold(t.Name) == fold(tr.Value), true
	case TriggerMerchantIs:
		return fold(t.Merchant()) == fold(tr.Value), true

	case TriggerAmountMore, TriggerAmountLess, TriggerAmountExactly:
		// Validation refuses an unparseable operand on write, so this is only
		// reachable for a row written around the API.
		operand, err := decimal.NewFromString(strings.TrimSpace(tr.Value))
		if err != nil {
			return false, false
		}
		// Size, not signed value — see TriggerAmountMore's comment.
		size := t.Amount.Abs()
		switch tr.Type {
		case TriggerAmountMore:
			return size.GreaterThan(operand), true
		case TriggerAmountLess:
			return size.LessThan(operand), true
		default:
			return size.Equal(operand), true
		}

	case TriggerCategoryIs:
		id, err := uuid.Parse(strings.TrimSpace(tr.Value))
		if err != nil {
			return false, false
		}
		return t.CategoryID != nil && *t.CategoryID == id, true
	case TriggerHasNoCategory:
		return t.CategoryID == nil, true
	case TriggerAccountIs:
		id, err := uuid.Parse(strings.TrimSpace(tr.Value))
		if err != nil {
			return false, false
		}
		return t.AccountID == id, true
	case TriggerHasAttachments:
		return t.HasAttachments, true
	}
	return false, false
}

// fold normalises text for comparison: case-insensitive, surrounding whitespace
// ignored. Every text trigger uses it, so "starbucks" and " Starbucks " are the
// same rule — a user typing a pattern is not choosing a case convention.
func fold(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Target says what kind of object this trigger's value names.
func (t TriggerType) Target() Target {
	switch t {
	case TriggerCategoryIs:
		return TargetCategory
	case TriggerAccountIs:
		return TargetAccount
	}
	return TargetNone
}

// Target says what kind of object this action's value names.
func (a ActionType) Target() Target {
	switch a {
	case ActionSetCategory:
		return TargetCategory
	case ActionAddTag:
		return TargetTag
	}
	return TargetNone
}

// ValidateTrigger checks a trigger's type and value, returning the normalised
// value to store and a human-readable message on the first problem (empty when
// it is fine). The handler sends that message as a 400.
//
// Normalising HERE rather than in the handler means the stored value and the
// value the engine compares are produced by one function: a rule cannot be
// saved in a shape the engine reads differently.
func ValidateTrigger(t TriggerType, value string) (string, string) {
	value = strings.TrimSpace(value)

	switch t {
	case TriggerDescriptionContains, TriggerDescriptionStarts,
		TriggerDescriptionEnds, TriggerDescriptionIs, TriggerMerchantIs:
		if value == "" {
			return "", fmt.Sprintf("%q needs some text to match", Label(t))
		}
		// The field is TEXT, but a pattern longer than a description can never
		// match anything, and the editor renders these inline.
		if len([]rune(value)) > 200 {
			return "", "a text condition must be 200 characters or fewer"
		}
		return value, ""

	case TriggerAmountMore, TriggerAmountLess, TriggerAmountExactly:
		d, err := decimal.NewFromString(value)
		if err != nil {
			return "", fmt.Sprintf("%q needs an amount, e.g. \"10.00\"", Label(t))
		}
		// Amounts compare against the SIZE of a transaction, which is never
		// negative, so a negative operand describes a comparison that can never
		// be true (or is always true) — a rule that quietly does nothing.
		if d.IsNegative() {
			return "", "an amount condition must not be negative — comparisons use the size of the transaction, not its direction"
		}
		return d.String(), ""

	case TriggerCategoryIs, TriggerAccountIs:
		id, err := uuid.Parse(value)
		if err != nil {
			return "", fmt.Sprintf("%q needs a %s", Label(t), t.Target())
		}
		return id.String(), ""

	case TriggerHasNoCategory, TriggerHasAttachments:
		// Takes no operand. Anything sent is dropped rather than stored, so a
		// stale value from an editor that switched types cannot resurface if
		// the type is switched back.
		return "", ""
	}
	return "", fmt.Sprintf("%q is not a condition this version understands", string(t))
}

// ValidateAction is ValidateTrigger's counterpart for the THEN half.
func ValidateAction(a ActionType, value string) (string, string) {
	switch a {
	case ActionSetCategory, ActionAddTag:
		id, err := uuid.Parse(strings.TrimSpace(value))
		if err != nil {
			return "", fmt.Sprintf("%q needs a %s", Label(a), a.Target())
		}
		return id.String(), ""

	case ActionSetNotes:
		// Not trimmed to empty-is-invalid: setting notes to nothing is how a
		// rule CLEARS them, which is a real thing to want.
		value = strings.TrimSpace(value)
		if len([]rune(value)) > 2000 {
			return "", "a note must be 2000 characters or fewer"
		}
		return value, ""

	case ActionAppendNotes:
		value = strings.TrimSpace(value)
		if value == "" {
			// Unlike set_notes, an empty append is not a way to clear anything
			// — it is an action that can never do anything.
			return "", "there is nothing to append"
		}
		if len([]rune(value)) > 2000 {
			return "", "a note must be 2000 characters or fewer"
		}
		return value, ""
	}
	return "", fmt.Sprintf("%q is not an action this version understands", string(a))
}

// Label renders a trigger or action type the way the UI names it, so an error
// message says "description contains" rather than "description_contains". Kept
// here beside the constants: a type added without a label is a message that
// reads like a database column.
func Label[T ~string](t T) string {
	return strings.ReplaceAll(string(t), "_", " ")
}
