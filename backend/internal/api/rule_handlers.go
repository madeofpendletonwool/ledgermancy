package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/rules"
)

// Rules: the user-editable IF-THEN surface over transactions.
//
// WHAT THIS FILE IS AND IS NOT. It is CRUD plus two verbs — test, and run over
// existing rows. It contains no matching logic whatsoever: whether a rule fires,
// and what firing does, lives entirely in internal/rules, and the tester and the
// runner call the same planner so a preview cannot promise something the run
// would not do. The one thing this file owns that the engine does not is
// VALIDATION ON WRITE — a rule that names a category from another household, or
// a condition this build does not understand, is refused here rather than stored
// and discovered as a silent no-op months later.
//
// SCOPE. A rule is household data, guarded by household_id in every statement.
// The TRANSACTIONS a rule reaches are per-member data, and both verbs below pass
// the CALLER's id as the viewer — so a test's match count, and a run's changed
// count, can never describe a charge on the other member's private account. The
// automatic hooks (a Plaid sync) pass nil instead, because there is no caller
// and nothing is returned to a user; see queries/rules.sql.

// ruleTriggerResponse is one condition as the editor renders it.
type ruleTriggerResponse struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	// Value is the operand as stored: a text fragment, a decimal string, or a
	// UUID. Never a JSON number — an amount that round-tripped through a float
	// would be a different rule than the one the user wrote.
	Value  string `json:"value"`
	Invert bool   `json:"invert"`
}

// ruleActionResponse is one action as the editor renders it.
type ruleActionResponse struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Value      string `json:"value"`
	StopOnFail bool   `json:"stop_on_fail"`
}

type ruleResponse struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	Active      bool    `json:"active"`
	Priority    int32   `json:"priority"`
	// Triggers are AND-joined; every one must hold. Actions run in order.
	Triggers  []ruleTriggerResponse `json:"triggers"`
	Actions   []ruleActionResponse  `json:"actions"`
	CreatedAt string                `json:"created_at"`
}

// readRules renders this household's rules, or one of them when id is set.
//
// Three queries, not one per rule: the triggers and actions come back for the
// whole set in one round trip each and are grouped here, so a rules page with
// twenty rules costs three queries rather than forty-one. It is also the ONLY
// place a rule is turned into JSON, so a rule created a second ago is described
// by the same code as one that has existed for a year.
func (s *Server) readRules(ctx context.Context, identity auth.Identity,
	id *uuid.UUID) ([]ruleResponse, error) {

	rows, err := s.Queries.ListRules(ctx, dbgen.ListRulesParams{
		HouseholdID: identity.HouseholdID, ID: id,
	})
	if err != nil {
		return nil, err
	}
	triggers, err := s.Queries.ListRuleTriggers(ctx, dbgen.ListRuleTriggersParams{
		HouseholdID: identity.HouseholdID, RuleID: id,
	})
	if err != nil {
		return nil, err
	}
	actions, err := s.Queries.ListRuleActions(ctx, dbgen.ListRuleActionsParams{
		HouseholdID: identity.HouseholdID, RuleID: id,
	})
	if err != nil {
		return nil, err
	}

	out := make([]ruleResponse, 0, len(rows))
	index := make(map[uuid.UUID]int, len(rows))
	for _, row := range rows {
		index[row.ID] = len(out)
		out = append(out, ruleResponse{
			ID:          row.ID.String(),
			Name:        row.Name,
			Description: row.Description,
			Active:      row.Active,
			Priority:    row.Priority,
			// Non-nil so the JSON is [] rather than null: an editor that has to
			// distinguish "no conditions" from "not loaded" gets it wrong once.
			Triggers:  []ruleTriggerResponse{},
			Actions:   []ruleActionResponse{},
			CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	for _, t := range triggers {
		if i, ok := index[t.RuleID]; ok {
			out[i].Triggers = append(out[i].Triggers, ruleTriggerResponse{
				ID: t.ID.String(), Type: t.TriggerType, Value: t.Value, Invert: t.Invert,
			})
		}
	}
	for _, a := range actions {
		if i, ok := index[a.RuleID]; ok {
			out[i].Actions = append(out[i].Actions, ruleActionResponse{
				ID: a.ID.String(), Type: a.ActionType, Value: a.Value, StopOnFail: a.StopOnFail,
			})
		}
	}
	return out, nil
}

func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	out, err := s.readRules(r.Context(), identity, nil)
	if err != nil {
		s.internalError(w, "list rules", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) readRule(ctx context.Context, identity auth.Identity,
	id uuid.UUID) (ruleResponse, error) {

	out, err := s.readRules(ctx, identity, &id)
	if err != nil {
		return ruleResponse{}, err
	}
	if len(out) == 0 {
		return ruleResponse{}, pgx.ErrNoRows
	}
	return out[0], nil
}

type ruleTriggerInput struct {
	Type   string `json:"type"`
	Value  string `json:"value"`
	Invert bool   `json:"invert"`
}

type ruleActionInput struct {
	Type       string `json:"type"`
	Value      string `json:"value"`
	StopOnFail bool   `json:"stop_on_fail"`
}

// upsertRuleRequest is the whole rule, every time. Create and update share it,
// and an update REPLACES the trigger and action lists rather than applying
// deltas — the editor is a set of rows the user confirms, and a delta API would
// make the client responsible for diffing against a list it may have fetched a
// minute ago.
type upsertRuleRequest struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
	// Active is a pointer so "not sent" (a new rule) can mean ON while "sent
	// false" means the user switched it off.
	Active   *bool              `json:"active"`
	Priority int32              `json:"priority"`
	Triggers []ruleTriggerInput `json:"triggers"`
	Actions  []ruleActionInput  `json:"actions"`
}

// Limits on the shape of a rule. Not security boundaries — the household owns
// its own data — but a rule with three hundred conditions is a mis-click or a
// runaway client, and every one of them is evaluated against every transaction
// on every sync.
const (
	maxRuleTriggers = 25
	maxRuleActions  = 25
)

// parsedRule is the validated, normalised rule ready to store.
type parsedRule struct {
	name        string
	description *string
	active      bool
	priority    int32
	triggers    []ruleTriggerInput
	actions     []ruleActionInput
}

// parse validates the whole rule, normalising every operand through
// internal/rules so the stored value and the value the engine compares are
// produced by one function.
//
// It returns a human-readable message on the first problem. It does NOT check
// that the ids a rule names still exist — that needs the database, and lives in
// checkRuleTargets.
func (req upsertRuleRequest) parse() (parsedRule, string) {
	var p parsedRule

	p.name = strings.TrimSpace(req.Name)
	if p.name == "" {
		return p, "a rule name is required"
	}
	if len([]rune(p.name)) > 100 {
		return p, "a rule name must be 100 characters or fewer"
	}
	if req.Description != nil {
		p.description = nilIfEmpty(strings.TrimSpace(*req.Description))
	}
	p.active = req.Active == nil || *req.Active
	p.priority = req.Priority

	// A rule with no conditions would match every transaction in the household.
	// The engine refuses to fire one anyway (see rules.Rule.Matches), but a rule
	// that stores fine and then does nothing is a worse experience than one that
	// will not save.
	if len(req.Triggers) == 0 {
		return p, "a rule needs at least one condition — without one it would match every transaction"
	}
	if len(req.Triggers) > maxRuleTriggers {
		return p, "a rule can have at most 25 conditions"
	}
	if len(req.Actions) == 0 {
		return p, "a rule needs at least one action"
	}
	if len(req.Actions) > maxRuleActions {
		return p, "a rule can have at most 25 actions"
	}

	p.triggers = make([]ruleTriggerInput, 0, len(req.Triggers))
	for _, t := range req.Triggers {
		value, msg := rules.ValidateTrigger(rules.TriggerType(t.Type), t.Value)
		if msg != "" {
			return p, msg
		}
		p.triggers = append(p.triggers, ruleTriggerInput{
			Type: t.Type, Value: value, Invert: t.Invert,
		})
	}

	p.actions = make([]ruleActionInput, 0, len(req.Actions))
	for _, a := range req.Actions {
		value, msg := rules.ValidateAction(rules.ActionType(a.Type), a.Value)
		if msg != "" {
			return p, msg
		}
		p.actions = append(p.actions, ruleActionInput{
			Type: a.Type, Value: value, StopOnFail: a.StopOnFail,
		})
	}
	return p, ""
}

// checkRuleTargets verifies that every category, tag and account a rule names
// belongs to this household and still exists.
//
// This is the check that keeps a rule honest. Without it, a rule naming another
// household's category saves cleanly, matches transactions, and then refuses
// every action forever — which the user reads as "the rule engine is broken"
// rather than as "that category is not mine". Refusing on write says so once, at
// the moment it can still be fixed.
func (s *Server) checkRuleTargets(ctx context.Context, identity auth.Identity,
	p parsedRule) (string, error) {

	check := func(target rules.Target, raw, label string) (string, error) {
		id, err := uuid.Parse(raw)
		if err != nil {
			// parse() already rejected an unparseable id for these types.
			return "that " + string(target) + " is not one this household can use", nil
		}
		params := dbgen.CountRuleTargetsParams{HouseholdID: &identity.HouseholdID}
		switch target {
		case rules.TargetCategory:
			params.CategoryID = &id
		case rules.TargetTag:
			params.TagID = &id
		case rules.TargetAccount:
			params.AccountID = &id
		default:
			return "", nil
		}
		found, err := s.Queries.CountRuleTargets(ctx, params)
		if err != nil {
			return "", err
		}
		if found == 0 {
			return label + " names a " + string(target) + " this household cannot use", nil
		}
		return "", nil
	}

	for _, t := range p.triggers {
		target := rules.TriggerType(t.Type).Target()
		if target == rules.TargetNone {
			continue
		}
		if msg, err := check(target, t.Value, "the condition \""+rules.Label(t.Type)+"\""); err != nil || msg != "" {
			return msg, err
		}
	}
	for _, a := range p.actions {
		target := rules.ActionType(a.Type).Target()
		if target == rules.TargetNone {
			continue
		}
		if msg, err := check(target, a.Value, "the action \""+rules.Label(a.Type)+"\""); err != nil || msg != "" {
			return msg, err
		}
	}
	return "", nil
}

// writeRuleChildren replaces a rule's conditions and actions inside the caller's
// transaction.
//
// Delete-then-insert, in ONE transaction, is what stops a rule from ever being
// briefly condition-less: a rule with no triggers is a rule that would match
// everything, and a sync landing between the two statements must not be able to
// see that state.
func writeRuleChildren(ctx context.Context, q *dbgen.Queries, ruleID uuid.UUID, p parsedRule) error {
	if err := q.DeleteRuleTriggers(ctx, ruleID); err != nil {
		return err
	}
	if err := q.DeleteRuleActions(ctx, ruleID); err != nil {
		return err
	}
	for i, t := range p.triggers {
		if err := q.CreateRuleTrigger(ctx, dbgen.CreateRuleTriggerParams{
			RuleID: ruleID, TriggerType: t.Type, Value: t.Value,
			Invert: t.Invert, Position: int32(i),
		}); err != nil {
			return err
		}
	}
	for i, a := range p.actions {
		if err := q.CreateRuleAction(ctx, dbgen.CreateRuleActionParams{
			RuleID: ruleID, ActionType: a.Type, Value: a.Value,
			StopOnFail: a.StopOnFail, Position: int32(i),
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	var req upsertRuleRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, msg := req.parse()
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	msg, err := s.checkRuleTargets(r.Context(), identity, p)
	if err != nil {
		s.internalError(w, "validate rule targets", err)
		return
	}
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	ctx := r.Context()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		s.internalError(w, "create rule", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.Queries.WithTx(tx)

	id, err := qtx.CreateRule(ctx, dbgen.CreateRuleParams{
		HouseholdID: identity.HouseholdID,
		Name:        p.name,
		Description: p.description,
		Active:      p.active,
		Priority:    p.priority,
	})
	if err != nil {
		s.internalError(w, "create rule", err)
		return
	}
	if err := writeRuleChildren(ctx, qtx, id, p); err != nil {
		s.internalError(w, "create rule conditions", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.internalError(w, "create rule", err)
		return
	}

	created, err := s.readRule(r.Context(), identity, id)
	if err != nil {
		s.internalError(w, "read created rule", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "ruleID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid rule id")
		return
	}
	var req upsertRuleRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, msg := req.parse()
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	msg, err = s.checkRuleTargets(r.Context(), identity, p)
	if err != nil {
		s.internalError(w, "validate rule targets", err)
		return
	}
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	ctx := r.Context()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		s.internalError(w, "update rule", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.Queries.WithTx(tx)

	// The household guard lives in the UPDATE, so an id from another household
	// matches nothing and comes back as ErrNoRows rather than as a silent no-op
	// the client would read as success — and, critically, before the child
	// writes below could touch a rule that is not this household's.
	if _, err := qtx.UpdateRule(ctx, dbgen.UpdateRuleParams{
		ID:          id,
		HouseholdID: identity.HouseholdID,
		Name:        p.name,
		Description: p.description,
		Active:      p.active,
		Priority:    p.priority,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "rule not found")
			return
		}
		s.internalError(w, "update rule", err)
		return
	}
	if err := writeRuleChildren(ctx, qtx, id, p); err != nil {
		s.internalError(w, "update rule conditions", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.internalError(w, "update rule", err)
		return
	}

	updated, err := s.readRule(r.Context(), identity, id)
	if err != nil {
		s.internalError(w, "read updated rule", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "ruleID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid rule id")
		return
	}
	// Conditions and actions cascade. Nothing a rule has already DONE is undone:
	// the categories it set and the tags it added are the household's data now,
	// exactly as they would be if a member had set them by hand.
	rows, err := s.Queries.DeleteRule(r.Context(), dbgen.DeleteRuleParams{
		ID: id, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "delete rule", err)
		return
	}
	if rows == 0 {
		writeError(w, http.StatusNotFound, "rule not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ruleChangeResponse is one action's outcome against one transaction.
//
// Deliberately carries the raw action type and value rather than a rendered
// sentence: the client already holds the category and tag lists for the editor's
// pickers, so it can name them without this endpoint re-reading both. `reason`
// is the one piece of prose, and it comes from the engine — an outcome with no
// explanation is what makes users distrust automation.
type ruleChangeResponse struct {
	Action  string `json:"action"`
	Value   string `json:"value"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
}

// ruleTestMatchResponse is one transaction the rule fired on, with a trimmed
// ledger row for recognising it.
type ruleTestMatchResponse struct {
	TransactionID string               `json:"transaction_id"`
	Date          string               `json:"date"`
	Name          string               `json:"name"`
	Merchant      string               `json:"merchant"`
	Amount        string               `json:"amount"`
	Currency      string               `json:"currency"`
	AccountName   string               `json:"account_name"`
	Changes       []ruleChangeResponse `json:"changes"`
}

// ruleTestResponse is the dry run. The counts describe the WHOLE household; the
// list is capped. A truncated list beside an exact count is honest — a truncated
// count would understate what the user is about to do.
type ruleTestResponse struct {
	Scanned     int                     `json:"scanned"`
	Matched     int                     `json:"matched"`
	WouldChange int                     `json:"would_change"`
	Truncated   bool                    `json:"truncated"`
	Matches     []ruleTestMatchResponse `json:"matches"`
}

// ruleTestLimit caps the previewed rows. Past a screenful the answer the user
// wants is the count, which the header carries exactly.
const ruleTestLimit = 50

func (s *Server) handleTestRule(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	engine, msg, err := s.loadRuleForRun(r.Context(), identity, chi.URLParam(r, "ruleID"))
	if err != nil {
		s.internalError(w, "load rule", err)
		return
	}
	if msg != "" {
		writeError(w, ruleLoadStatus(msg), msg)
		return
	}

	// The CALLER's id as the viewer: a preview must never describe, or even
	// count, a charge on the other member's private account.
	summary, matches, err := engine.PreviewHousehold(r.Context(), s.Queries,
		identity.HouseholdID, &identity.UserID, ruleTestLimit)
	if err != nil {
		s.internalError(w, "test rule", err)
		return
	}

	out := ruleTestResponse{
		Scanned:     summary.Scanned,
		Matched:     summary.Matched,
		WouldChange: summary.Changed,
		Truncated:   summary.Matched > len(matches),
		Matches:     make([]ruleTestMatchResponse, 0, len(matches)),
	}
	for _, m := range matches {
		row := ruleTestMatchResponse{
			TransactionID: m.Transaction.ID.String(),
			Date:          m.Transaction.Date,
			Name:          m.Transaction.Name,
			Merchant:      m.Transaction.Merchant(),
			Amount:        m.Transaction.Amount.StringFixed(2),
			Currency:      m.Transaction.Currency,
			AccountName:   m.Transaction.AccountName,
			Changes:       []ruleChangeResponse{},
		}
		for _, result := range m.Results {
			for _, effect := range result.Effects {
				row.Changes = append(row.Changes, ruleChangeResponse{
					Action:  string(effect.Action.Type),
					Value:   effect.Action.Value,
					Outcome: string(effect.Outcome),
					Reason:  effect.Reason,
				})
			}
		}
		out.Matches = append(out.Matches, row)
	}
	writeJSON(w, http.StatusOK, out)
}

// ruleTriggerResultResponse is what running a rule over existing rows did.
//
// `changed` is the number that matters, and the number that must be 0 the second
// time the button is pressed. `matched` staying high while `changed` falls to
// zero is idempotence working, not the rule breaking, which is why both are
// reported.
type ruleTriggerResultResponse struct {
	Scanned int `json:"scanned"`
	Matched int `json:"matched"`
	Changed int `json:"changed"`
}

func (s *Server) handleTriggerRule(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	engine, msg, err := s.loadRuleForRun(r.Context(), identity, chi.URLParam(r, "ruleID"))
	if err != nil {
		s.internalError(w, "load rule", err)
		return
	}
	if msg != "" {
		writeError(w, ruleLoadStatus(msg), msg)
		return
	}

	// Same viewer scoping as the test, for the same reason plus a stronger one:
	// this WRITES. A member running a rule must not be able to re-file charges
	// they were never allowed to read.
	summary, err := engine.RunHousehold(r.Context(), s.Queries,
		identity.HouseholdID, &identity.UserID)
	if err != nil {
		s.internalError(w, "run rule", err)
		return
	}
	writeJSON(w, http.StatusOK, ruleTriggerResultResponse{
		Scanned: summary.Scanned, Matched: summary.Matched, Changed: summary.Changed,
	})
}

// loadRuleForRun resolves the {ruleID} path parameter into a single-rule engine,
// shared by test and trigger so the two cannot disagree about which rules run.
//
// It loads the named rule whether or not it is ACTIVE. Testing a rule before
// switching it on, and running a freshly written one over history, both happen
// while it is still off — refusing them would make the tester useless at exactly
// the moment it is wanted.
func (s *Server) loadRuleForRun(ctx context.Context, identity auth.Identity,
	raw string) (*rules.Engine, string, error) {

	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, "invalid rule id", nil
	}
	engine, err := rules.LoadRule(ctx, s.Queries, identity.HouseholdID, id)
	if err != nil {
		return nil, "", err
	}
	if engine == nil {
		return nil, "rule not found", nil
	}
	return engine, "", nil
}

func ruleLoadStatus(msg string) int {
	if msg == "rule not found" {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}
