package rules

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Outcome is what one action did, or would do. The four values are the whole
// vocabulary of the preview, so they are chosen to be distinguishable to a
// reader who is asking "why did nothing happen?".
type Outcome string

const (
	// OutcomeApplied — the row changed (or, in a preview, would).
	OutcomeApplied Outcome = "applied"
	// OutcomeUnchanged — the action succeeded and there was nothing to do: the
	// tag is already on the row, the note already says that. This is a SUCCESS,
	// and it is what makes a second run of a rule a no-op. It never triggers
	// StopOnFail.
	OutcomeUnchanged Outcome = "unchanged"
	// OutcomeRefused — the action could not be applied: the category was set by
	// hand, or the thing it names no longer exists. This is the failure
	// StopOnFail reacts to.
	OutcomeRefused Outcome = "refused"
	// OutcomeSkipped — an earlier action in the same rule was refused and had
	// StopOnFail set, so this one never ran.
	OutcomeSkipped Outcome = "skipped"
)

// Effect is one action's result against one transaction.
type Effect struct {
	Action  Action
	Outcome Outcome
	// Reason is a human sentence for anything other than "applied", shown in
	// the rule tester. An outcome with no explanation is the thing that makes
	// users distrust automation.
	Reason string
}

// Result is one rule's effects on one transaction. A rule that did not match
// produces no Result at all rather than an empty one, so "matched but every
// action was a no-op" stays distinguishable from "did not match".
type Result struct {
	RuleID   uuid.UUID
	RuleName string
	Effects  []Effect
}

// Changed reports whether any of this rule's actions moved the row.
func (r Result) Changed() bool {
	for _, e := range r.Effects {
		if e.Outcome == OutcomeApplied {
			return true
		}
	}
	return false
}

// Engine is a compiled set of rules, ready to run against transactions.
//
// Rules are loaded once per engine rather than per transaction: a sync can
// process thousands of rows, and re-reading a household's rules for each one
// would dominate the work. internal/categorize does the same for the same
// reason.
type Engine struct {
	rules []Rule
}

// New builds an engine over an explicit rule set. Order matters and is taken as
// given — Load has already sorted by priority.
func New(rules []Rule) *Engine { return &Engine{rules: rules} }

// Rules returns the compiled set, in the order it runs.
func (e *Engine) Rules() []Rule { return e.rules }

// Load reads a household's ACTIVE rules, in priority order. This is what the
// automatic hooks use: an inactive rule is one the user switched off, and
// switching it off has to actually stop it.
func Load(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID) (*Engine, error) {
	return load(ctx, q, householdID, nil, true)
}

// LoadRule reads ONE rule by id, active or not, and returns pgx.ErrNoRows'
// equivalent — a nil engine and a nil error — when the id is not this
// household's.
//
// Inactive rules are included here on purpose: testing a rule before switching
// it on, and running a freshly written one over history, are exactly the moments
// a user reaches for this, and both happen while it is still off.
func LoadRule(ctx context.Context, q *dbgen.Queries, householdID, ruleID uuid.UUID) (*Engine, error) {
	e, err := load(ctx, q, householdID, &ruleID, false)
	if err != nil {
		return nil, err
	}
	if len(e.rules) == 0 {
		return nil, nil
	}
	return e, nil
}

func load(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID,
	ruleID *uuid.UUID, activeOnly bool) (*Engine, error) {

	rows, err := q.ListRules(ctx, dbgen.ListRulesParams{
		HouseholdID: householdID, ID: ruleID,
	})
	if err != nil {
		return nil, fmt.Errorf("load rules: %w", err)
	}
	triggers, err := q.ListRuleTriggers(ctx, dbgen.ListRuleTriggersParams{
		HouseholdID: householdID, RuleID: ruleID,
	})
	if err != nil {
		return nil, fmt.Errorf("load rule triggers: %w", err)
	}
	actions, err := q.ListRuleActions(ctx, dbgen.ListRuleActionsParams{
		HouseholdID: householdID, RuleID: ruleID,
	})
	if err != nil {
		return nil, fmt.Errorf("load rule actions: %w", err)
	}

	byRule := make(map[uuid.UUID]int, len(rows))
	compiled := make([]Rule, 0, len(rows))
	for _, row := range rows {
		if activeOnly && !row.Active {
			continue
		}
		byRule[row.ID] = len(compiled)
		compiled = append(compiled, Rule{
			ID:          row.ID,
			Name:        row.Name,
			Description: row.Description,
			Active:      row.Active,
			Priority:    row.Priority,
		})
	}
	// The two child reads are already ordered by (rule_id, position), so
	// appending preserves the user's arrangement without a second sort.
	for _, t := range triggers {
		if i, ok := byRule[t.RuleID]; ok {
			compiled[i].Triggers = append(compiled[i].Triggers, Trigger{
				ID: t.ID, Type: TriggerType(t.TriggerType), Value: t.Value,
				Invert: t.Invert, Position: t.Position,
			})
		}
	}
	for _, a := range actions {
		if i, ok := byRule[a.RuleID]; ok {
			compiled[i].Actions = append(compiled[i].Actions, Action{
				ID: a.ID, Type: ActionType(a.ActionType), Value: a.Value,
				StopOnFail: a.StopOnFail, Position: a.Position,
			})
		}
	}
	return &Engine{rules: compiled}, nil
}

// Preview reports what the engine would do to a transaction WITHOUT writing
// anything. It is the same code path Apply uses to decide, which is the point:
// the tester cannot promise something the run would not do.
//
// The snapshot is threaded through the rules, so a later rule sees the effect of
// an earlier one — "set category to Coffee" (priority 10) is observable by a
// rule that triggers on "category is Coffee".
//
// The transaction is taken by value and the RESULTING snapshot returned beside
// the results, so a caller that needs to render the row as it would end up —
// the create hook, echoing a freshly filed transaction — reads it here rather
// than re-deriving it.
func (e *Engine) Preview(t Transaction) (Transaction, []Result) {
	var out []Result
	for _, rule := range e.rules {
		if !rule.Matches(t) {
			continue
		}
		effects := plan(rule, &t)
		out = append(out, Result{RuleID: rule.ID, RuleName: rule.Name, Effects: effects})
	}
	return t, out
}

// plan walks one rule's actions against the snapshot, deciding each action's
// outcome and MUTATING the snapshot as though the applied ones had landed. That
// mutation is what makes "set notes" followed by "append notes" behave, and what
// lets a re-run see its own previous work and report Unchanged.
func plan(rule Rule, t *Transaction) []Effect {
	effects := make([]Effect, 0, len(rule.Actions))
	stopped := false

	for _, action := range rule.Actions {
		if stopped {
			effects = append(effects, Effect{
				Action: action, Outcome: OutcomeSkipped,
				Reason: "an earlier action in this rule stopped it",
			})
			continue
		}

		effect := planAction(action, t)
		effects = append(effects, effect)
		if effect.Outcome == OutcomeRefused && action.StopOnFail {
			stopped = true
		}
	}
	return effects
}

func planAction(action Action, t *Transaction) Effect {
	refuse := func(reason string) Effect {
		return Effect{Action: action, Outcome: OutcomeRefused, Reason: reason}
	}
	unchanged := func(reason string) Effect {
		return Effect{Action: action, Outcome: OutcomeUnchanged, Reason: reason}
	}
	applied := Effect{Action: action, Outcome: OutcomeApplied}

	switch action.Type {
	case ActionSetCategory:
		id, err := uuid.Parse(strings.TrimSpace(action.Value))
		if err != nil {
			return refuse("this action does not name a category")
		}
		// THE STICKY-MANUAL INVARIANT. Checked here so the user is told, and
		// enforced again as a predicate in ApplyRuleCategory so the guarantee
		// does not depend on this check being reached.
		if t.CategorySource == "manual" {
			return refuse("the category was set by hand, and a rule never overwrites that")
		}
		if t.CategoryID != nil && *t.CategoryID == id {
			return unchanged("already in that category")
		}
		t.CategoryID = &id
		t.CategorySource = "rule"
		return applied

	case ActionAddTag:
		id, err := uuid.Parse(strings.TrimSpace(action.Value))
		if err != nil {
			return refuse("this action does not name a tag")
		}
		if t.HasTag(id) {
			return unchanged("already tagged")
		}
		t.TagIDs = append(t.TagIDs, id)
		return applied

	case ActionSetNotes:
		if t.Notes == action.Value {
			return unchanged("the notes already say that")
		}
		t.Notes = action.Value
		return applied

	case ActionAppendNotes:
		next := appendNote(t.Notes, action.Value)
		if next == t.Notes {
			return unchanged("that line is already in the notes")
		}
		t.Notes = next
		return applied
	}
	return refuse(fmt.Sprintf("%q is not an action this version understands", string(action.Type)))
}

// appendNote adds a line to a note field unless that exact line is already one
// of them.
//
// THIS FUNCTION IS THE IDEMPOTENCE of append-notes, and the reason the action
// exists in this shape at all. A plain concatenation would grow the field on
// every sync and every re-run until the note was the same sentence forty times —
// the single most obvious way a rule engine ruins data. Comparison is per LINE
// and trimmed, so re-running matches what the previous run wrote.
func appendNote(existing, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return existing
	}
	if strings.TrimSpace(existing) == "" {
		return value
	}
	for _, line := range strings.Split(existing, "\n") {
		if strings.TrimSpace(line) == value {
			return existing
		}
	}
	return existing + "\n" + value
}

// Apply runs the engine against one transaction and writes what it decides.
//
// The returned results are what the DATABASE did, not what the plan hoped: an
// action the plan called applied is downgraded to refused when its statement
// matches no rows (the category was deleted between the read and the write, the
// tag belongs to another household). The preview and the run share the planner,
// so they can only disagree about facts that changed underneath them.
func (e *Engine) Apply(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID,
	t Transaction) (Transaction, []Result, error) {

	var out []Result
	for _, rule := range e.rules {
		if !rule.Matches(t) {
			continue
		}
		effects := plan(rule, &t)
		for i := range effects {
			if effects[i].Outcome != OutcomeApplied {
				continue
			}
			if err := apply(ctx, q, householdID, t, &effects[i]); err != nil {
				return t, nil, err
			}
		}
		out = append(out, Result{RuleID: rule.ID, RuleName: rule.Name, Effects: effects})
	}
	return t, out, nil
}

// apply performs one planned effect's write, downgrading the outcome when the
// statement matched nothing.
//
// It reads the POST-plan snapshot: `t` has already been mutated by plan, so
// t.Notes here is the text the notes should end up as, computed in Go rather
// than by a SQL-side concatenation. That is deliberate — a database-side append
// would be a read-modify-write with no snapshot to compare against, and a
// retried statement would append twice.
func apply(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID,
	t Transaction, effect *Effect) error {

	switch effect.Action.Type {
	case ActionSetCategory:
		rows, err := q.ApplyRuleCategory(ctx, dbgen.ApplyRuleCategoryParams{
			ID: t.ID, CategoryID: t.CategoryID, HouseholdID: householdID,
		})
		if err != nil {
			return fmt.Errorf("rule set category: %w", err)
		}
		if rows == 0 {
			effect.Outcome = OutcomeRefused
			effect.Reason = "that category is no longer available, or the row was filed by hand"
		}

	case ActionAddTag:
		id, err := uuid.Parse(strings.TrimSpace(effect.Action.Value))
		if err != nil {
			effect.Outcome = OutcomeRefused
			effect.Reason = "this action does not name a tag"
			return nil
		}
		rows, err := q.AddRuleTag(ctx, dbgen.AddRuleTagParams{
			TransactionID: t.ID, TagID: id, HouseholdID: householdID,
		})
		if err != nil {
			return fmt.Errorf("rule add tag: %w", err)
		}
		// Zero rows here is ON CONFLICT DO NOTHING as well as "no such tag".
		// A conflict means the label was already there, which is a success, so
		// this reports refused only when the snapshot did NOT already have it —
		// and the snapshot check ran first, in plan.
		if rows == 0 {
			effect.Outcome = OutcomeRefused
			effect.Reason = "that tag is no longer available"
		}

	case ActionSetNotes, ActionAppendNotes:
		var notes *string
		if t.Notes != "" {
			notes = &t.Notes
		}
		rows, err := q.SetRuleNotes(ctx, dbgen.SetRuleNotesParams{
			ID: t.ID, Notes: notes, HouseholdID: householdID,
		})
		if err != nil {
			return fmt.Errorf("rule set notes: %w", err)
		}
		if rows == 0 {
			effect.Outcome = OutcomeRefused
			effect.Reason = "that transaction is no longer available"
		}
	}
	return nil
}

// Summary counts one pass over a household's transactions.
type Summary struct {
	// Scanned is every transaction the pass considered.
	Scanned int
	// Matched is how many at least one rule fired on — including rows where
	// every action turned out to be a no-op, which is the normal state of a
	// second run.
	Matched int
	// Changed is how many actually moved. This is the number a user cares
	// about after pressing "run on existing", and the number that must be 0 the
	// second time they press it.
	Changed int
}

// batchSize is how many transactions one page of a household walk reads.
// Matches the batch internal/categorize uses for the same kind of pass.
const batchSize = 500

// Match is one transaction a preview fired on, with what each rule would do to
// it. Only the preview builds these — a run reports counts, because nobody is
// reading a list of ten thousand rows.
type Match struct {
	Transaction Transaction
	Results     []Result
}

// RunHousehold applies the engine to every transaction in scope, in batches.
//
// viewerUserID scopes which transactions are in scope, and the two callers
// differ deliberately: pass the caller's id when a member asked for this, and
// nil when the system did (a sync, with nobody on the other end of the
// request). See the header of queries/rules.sql.
//
// Safe to re-run by construction: every action decides from the row in front of
// it, so a second pass reports Matched > 0 and Changed == 0.
func (e *Engine) RunHousehold(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID,
	viewerUserID *uuid.UUID) (Summary, error) {

	summary, _, err := e.walk(ctx, q, householdID, viewerUserID, true, 0)
	return summary, err
}

// PreviewHousehold is RunHousehold with the writes removed: the same walk, the
// same planner, no mutations. It backs the rule tester, and it shares a code
// path with the real run so the tester cannot promise something the run would
// not do.
//
// The Summary counts the WHOLE household — an exact "this would change 412
// transactions" — while only the first `limit` matches are returned to render.
// A truncated LIST beside an exact COUNT is honest; a truncated count would
// quietly understate what the user is about to do.
func (e *Engine) PreviewHousehold(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID,
	viewerUserID *uuid.UUID, limit int) (Summary, []Match, error) {

	return e.walk(ctx, q, householdID, viewerUserID, false, limit)
}

// walk pages through the household's transactions and runs the engine over each
// one, writing or not.
func (e *Engine) walk(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID,
	viewerUserID *uuid.UUID, write bool, limit int) (Summary, []Match, error) {

	var summary Summary
	var matches []Match
	var after *uuid.UUID

	for {
		// Cancellation is checked per page rather than per row: a household
		// walk is the long operation here, and a half-applied page is
		// harmless — re-running finishes the job without duplicating anything.
		if err := ctx.Err(); err != nil {
			return summary, matches, ctx.Err()
		}

		page, err := q.ListRuleCandidates(ctx, dbgen.ListRuleCandidatesParams{
			HouseholdID:  householdID,
			ViewerUserID: viewerUserID,
			AfterID:      after,
			Lim:          batchSize,
		})
		if err != nil {
			return summary, matches, fmt.Errorf("list rule candidates: %w", err)
		}
		if len(page) == 0 {
			return summary, matches, nil
		}

		for _, row := range page {
			t := FromCandidateRow(row)

			var results []Result
			if write {
				if _, results, err = e.Apply(ctx, q, householdID, t); err != nil {
					return summary, matches, err
				}
			} else {
				_, results = e.Preview(t)
			}

			summary.Scanned++
			if len(results) == 0 {
				continue
			}
			summary.Matched++
			for _, r := range results {
				if r.Changed() {
					summary.Changed++
					break
				}
			}
			if len(matches) < limit {
				matches = append(matches, Match{Transaction: t, Results: results})
			}
		}

		last := page[len(page)-1].ID
		after = &last
		// A short page means there is nothing left to fetch.
		if len(page) < batchSize {
			return summary, matches, nil
		}
	}
}

// FromCandidateRow converts a page row into the engine's snapshot.
//
// FromCandidateRow and FromSingleRow are two functions over two sqlc row types
// with identical columns. The duplication is sqlc's, not a design choice — and
// the engine tests feed both through the same assertions so the day the two
// queries drift, a test says so rather than the batch path and the single-row
// path quietly disagreeing.
func FromCandidateRow(row dbgen.ListRuleCandidatesRow) Transaction {
	return Transaction{
		ID:             row.ID,
		AccountID:      row.AccountID,
		Name:           row.Name,
		MerchantName:   deref(row.MerchantName),
		Amount:         row.Amount,
		CategoryID:     row.CategoryID,
		CategorySource: deref(row.CategorySource),
		Notes:          deref(row.Notes),
		TagIDs:         row.TagIds,
		HasAttachments: row.HasAttachments,
		Date:           row.Date.Format("2006-01-02"),
		Currency:       row.Currency,
		AccountName:    row.AccountName,
	}
}

// FromSingleRow is FromCandidateRow for the one-transaction read.
func FromSingleRow(row dbgen.GetRuleCandidateRow) Transaction {
	return Transaction{
		ID:             row.ID,
		AccountID:      row.AccountID,
		Name:           row.Name,
		MerchantName:   deref(row.MerchantName),
		Amount:         row.Amount,
		CategoryID:     row.CategoryID,
		CategorySource: deref(row.CategorySource),
		Notes:          deref(row.Notes),
		TagIDs:         row.TagIds,
		HasAttachments: row.HasAttachments,
		Date:           row.Date.Format("2006-01-02"),
		Currency:       row.Currency,
		AccountName:    row.AccountName,
	}
}

// ApplyToTransaction is the single-row hook: load the household's active rules,
// read one transaction, apply, and hand back the resulting snapshot so the
// caller can render the row as it now IS rather than as it was inserted.
//
// It returns nil when the household has no rules — the overwhelmingly common
// case, and worth one cheap read rather than a transaction fetch nobody needs.
//
// Fired on CREATE, not on edit. That asymmetry is deliberate: a hand-edit is the
// user stating what the row should be, and re-running automation over it would
// mean a rule fighting the person who just corrected it. The sticky-manual
// invariant covers the category specifically; this covers the rest.
func ApplyToTransaction(ctx context.Context, q *dbgen.Queries, householdID,
	transactionID uuid.UUID) (*Transaction, error) {

	engine, err := Load(ctx, q, householdID)
	if err != nil {
		return nil, err
	}
	if len(engine.rules) == 0 {
		return nil, nil
	}

	row, err := q.GetRuleCandidate(ctx, dbgen.GetRuleCandidateParams{
		ID: transactionID, HouseholdID: householdID,
	})
	if err != nil {
		return nil, fmt.Errorf("read transaction for rules: %w", err)
	}
	final, _, err := engine.Apply(ctx, q, householdID, FromSingleRow(row))
	if err != nil {
		return nil, err
	}
	return &final, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
