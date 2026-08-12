// Package audit records field-level change history for user-editable objects.
//
// Every state-changing handler calls Record inside the same database
// transaction as the mutation, passing the old and new state. Record writes one
// object_changes row per changed field; an unchanged object writes nothing, and
// a rolled-back transaction writes nothing either — the record can never claim
// a change landed when it did not. That is the load-bearing invariant the rest
// of this package and its callers exist to preserve.
package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Object kinds. Kept in step with object_changes.object_changes_kind_check in
// migration 00062; widening the set means widening that CHECK too.
const (
	KindTransaction = "transaction"
	KindBudget      = "budget"
	KindGoal        = "goal"
)

// FieldCreated is the synthetic field name recorded for an object's first
// appearance. A create is a state change too, but rendering it as a wall of
// first-value rows (every field old=NULL) is noise; the History panel renders
// this one row as "created by X" and subsequent edits as field-level diffs.
const FieldCreated = "created"

// Change is one field-level diff. Old and New are values json.Marshal can
// encode; a nil Old means the field was set on create, a nil New means it was
// cleared. The JSONB column stores NULL for nil, which is how the History panel
// tells "cleared" / "set on create" apart from a value that happens to be null.
type Change struct {
	Field string
	Old   any
	New   any
}

// RecordParams identifies the object a change belongs to and who made it.
type RecordParams struct {
	HouseholdID uuid.UUID
	ObjectKind  string
	ObjectID    uuid.UUID
	// ActorUserID. uuid.Nil records a NULL actor (no authenticated caller, e.g.
	// a system job). The column is a plain UUID with no FK, so this never blocks
	// a later user deletion — the actor is a historical fact, not a live link.
	ActorUserID uuid.UUID
}

// Record writes one object_changes row per change through q, which the caller
// has already bound to its mutation transaction (q = queries.WithTx(tx)).
//
// History is part of the mutation here, not a best-effort side effect: an error
// is returned and the caller rolls back, so the invariant is "the record and
// the change land together or not at all" rather than "the change lands and we
// hope the record does". This differs on purpose from auth_events, which is
// fire-and-forget — object history that silently drops rows defeats the point,
// since "who changed this" has to be a complete answer to be worth keeping.
func Record(ctx context.Context, q *dbgen.Queries, p RecordParams, changes []Change) error {
	var actor *uuid.UUID
	if p.ActorUserID != uuid.Nil {
		id := p.ActorUserID
		actor = &id
	}
	for _, c := range changes {
		old, err := encode(c.Old)
		if err != nil {
			return fmt.Errorf("audit: encode old %s: %w", c.Field, err)
		}
		new, err := encode(c.New)
		if err != nil {
			return fmt.Errorf("audit: encode new %s: %w", c.Field, err)
		}
		if err := q.InsertObjectChange(ctx, dbgen.InsertObjectChangeParams{
			HouseholdID: p.HouseholdID,
			ObjectKind:  p.ObjectKind,
			ObjectID:    p.ObjectID,
			ActorUserID: actor,
			Field:       c.Field,
			OldValue:    old,
			NewValue:    new,
		}); err != nil {
			return fmt.Errorf("audit: record %s: %w", c.Field, err)
		}
	}
	return nil
}

// encode marshals a value to JSONB bytes. nil stays nil so the column reads
// NULL rather than the JSON literal null — the distinction matters for the
// History panel (a cleared field is NULL, a field whose value is the JSON
// literal null would be a different fact).
func encode(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}
