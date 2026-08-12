package audit

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// This file holds the diff helpers and the per-domain builders.
//
// Each changeX helper returns nil when old and new are equal, so a builder
// appends freely — an unchanged field contributes no row. Values are normalised
// to JSON-friendly scalars here (decimals to strings, dates to YYYY-MM-DD,
// uuids to canonical text) so the JSONB columns and the History panel both
// render the same canonical form without re-parsing money as a float.

// appendChange appends ch to c unless ch is nil (the field was unchanged).
func appendChange(c []Change, ch *Change) []Change {
	if ch == nil {
		return c
	}
	return append(c, *ch)
}

func changeString(field, old, new string) *Change {
	if old == new {
		return nil
	}
	return &Change{Field: field, Old: old, New: new}
}

func changeStringPtr(field string, old, new *string) *Change {
	if ptrEqString(old, new) {
		return nil
	}
	return &Change{Field: field, Old: strVal(old), New: strVal(new)}
}

func strVal(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func ptrEqString(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// changeDecimal compares by value (1.20 == 1.2) and records the canonical
// string form. Money never travels as a JSON number elsewhere in this app, and
// the audit log is no exception — a string stays exact through JSONB round
// trips and reads cleanly in the panel.
func changeDecimal(field string, old, new decimal.Decimal) *Change {
	if old.Equal(new) {
		return nil
	}
	return &Change{Field: field, Old: old.String(), New: new.String()}
}

func changeDate(field string, old, new time.Time) *Change {
	if old.Equal(new) {
		return nil
	}
	return &Change{Field: field, Old: old.Format(time.DateOnly), New: new.Format(time.DateOnly)}
}

func changeDatePtr(field string, old, new *time.Time) *Change {
	if ptrEqTime(old, new) {
		return nil
	}
	return &Change{Field: field, Old: dateVal(old), New: dateVal(new)}
}

func dateVal(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format(time.DateOnly)
}

func ptrEqTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func changeUUIDPtr(field string, old, new *uuid.UUID) *Change {
	if ptrEqUUID(old, new) {
		return nil
	}
	return &Change{Field: field, Old: uuidVal(old), New: uuidVal(new)}
}

func uuidVal(u *uuid.UUID) any {
	if u == nil {
		return nil
	}
	return u.String()
}

func ptrEqUUID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func changeInt16(field string, old, new int16) *Change {
	if old == new {
		return nil
	}
	return &Change{Field: field, Old: old, New: new}
}

func changeBool(field string, old, new bool) *Change {
	if old == new {
		return nil
	}
	return &Change{Field: field, Old: old, New: new}
}

// Created is the single change recorded for a freshly created object. See the
// FieldCreated constant for why a create is one row rather than one per field.
func Created() []Change { return []Change{{Field: FieldCreated}} }

// TransactionDiff computes the user-facing field changes between two
// transaction rows. Only the fields a member can edit are tracked; internal
// bookkeeping (sync ids, plaid descriptors, timestamps) is not, because those
// never appear in "who changed what" — they move in the sync hot path, not on a
// member's edit.
func TransactionDiff(old, new dbgen.Transaction) []Change {
	var c []Change
	c = appendChange(c, changeDecimal("amount", old.Amount, new.Amount))
	c = appendChange(c, changeDate("date", old.Date, new.Date))
	c = appendChange(c, changeString("name", old.Name, new.Name))
	c = appendChange(c, changeStringPtr("merchant_name", old.MerchantName, new.MerchantName))
	c = appendChange(c, changeUUIDPtr("category_id", old.CategoryID, new.CategoryID))
	c = appendChange(c, changeStringPtr("notes", old.Notes, new.Notes))
	return c
}

// BudgetDiff tracks the fields a household budget upsert can change. category
// and owner are the budget's identity (one per category per scope) and so are
// not part of an edit.
func BudgetDiff(old, new dbgen.Budget) []Change {
	var c []Change
	c = appendChange(c, changeDecimal("amount", old.Amount, new.Amount))
	c = appendChange(c, changeString("period", old.Period, new.Period))
	c = appendChange(c, changeBool("rollover", old.Rollover, new.Rollover))
	return c
}

// GoalDiff tracks the fields a goal edit can change. Scope/kind are the goal's
// identity, not its editable state; remind is a notification toggle with its
// own surface.
func GoalDiff(old, new dbgen.Goal) []Change {
	var c []Change
	c = appendChange(c, changeString("name", old.Name, new.Name))
	c = appendChange(c, changeDecimal("target_amount", old.TargetAmount, new.TargetAmount))
	c = appendChange(c, changeDatePtr("target_date", old.TargetDate, new.TargetDate))
	c = appendChange(c, changeUUIDPtr("account_id", old.AccountID, new.AccountID))
	c = appendChange(c, changeUUIDPtr("category_id", old.CategoryID, new.CategoryID))
	c = appendChange(c, changeInt16("college_years", old.CollegeYears, new.CollegeYears))
	return c
}
