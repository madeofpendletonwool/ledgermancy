// Package alerts evaluates a household's configured alert rules against its
// transactions and records the events they raise. Evaluation is deterministic
// SQL over figures the reporting layer already computes correctly — no AI is
// involved, so alerts work whether or not an AI key is configured.
//
// Visibility is enforced at two layers: aggregate alerts are evaluated over
// shared items only, and transaction-linked events are filtered on read by the
// viewer's access to the underlying transaction. See internal/db/queries/
// alerts.sql for the full model.
package alerts

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// Alert type identifiers, matching the strings stored in alerts.type and the
// options the frontend offers.
const (
	TypeBigSpend        = "big_spend"
	TypeBudgetThreshold = "budget_threshold"
	TypeUnusualMerchant = "unusual_merchant"
	TypeLowLeftover     = "low_leftover"
)

// Types lists every alert type the engine understands, for the config UI.
var Types = []string{TypeBigSpend, TypeBudgetThreshold, TypeUnusualMerchant, TypeLowLeftover}

// IsValidType reports whether t is an alert type the engine can evaluate.
func IsValidType(t string) bool {
	switch t {
	case TypeBigSpend, TypeBudgetThreshold, TypeUnusualMerchant, TypeLowLeftover:
		return true
	default:
		return false
	}
}

// ValidateConfig checks that a config blob is well-formed for its type, so a
// misconfigured rule is rejected at the API rather than silently never firing.
// It reuses the exact structs the evaluators parse, so validation and behaviour
// can never drift apart.
func ValidateConfig(alertType string, raw []byte) error {
	switch alertType {
	case TypeBigSpend:
		var c bigSpendConfig
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}
		if !c.Threshold.IsPositive() {
			return fmt.Errorf("threshold must be a positive decimal string")
		}
	case TypeBudgetThreshold:
		var c budgetThresholdConfig
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}
		if c.Percent < 1 || c.Percent > 1000 {
			return fmt.Errorf("percent must be between 1 and 1000")
		}
	case TypeUnusualMerchant:
		var c unusualMerchantConfig
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}
		if c.RecentDays < 0 || c.MinAmount.IsNegative() {
			return fmt.Errorf("recent_days and min_amount must not be negative")
		}
	case TypeLowLeftover:
		var c lowLeftoverConfig
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}
		if c.Floor.IsNegative() {
			return fmt.Errorf("floor must not be negative")
		}
	default:
		return fmt.Errorf("unknown alert type %q", alertType)
	}
	return nil
}

// bigSpendWindowDays bounds how far back big_spend looks for candidates, so the
// first evaluation after a two-year backfill does not raise hundreds of events
// for long-past purchases.
const bigSpendWindowDays = 30

// defaults applied when a config omits a field, so a half-filled rule still
// behaves sensibly rather than never firing.
const (
	defaultBudgetPercent = 90
	defaultUnusualDays   = 7
	maxUnusualCandidates = 200
)

// Evaluate runs every enabled alert for one household and records new events.
// It returns the number of events raised. now is passed in so the caller
// controls the clock (tests) and so all period maths in one pass agree.
func Evaluate(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) (int, error) {
	configured, err := q.ListEnabledAlerts(ctx, householdID)
	if err != nil {
		return 0, fmt.Errorf("list enabled alerts: %w", err)
	}

	total := 0
	for _, a := range configured {
		var n int
		switch a.Type {
		case TypeBigSpend:
			n, err = evalBigSpend(ctx, q, a, now)
		case TypeBudgetThreshold:
			n, err = evalBudgetThreshold(ctx, q, a, now)
		case TypeUnusualMerchant:
			n, err = evalUnusualMerchant(ctx, q, a, now)
		case TypeLowLeftover:
			n, err = evalLowLeftover(ctx, q, a, now)
		default:
			continue // an unknown type is ignored, not an error
		}
		if err != nil {
			return total, fmt.Errorf("evaluate %s alert %s: %w", a.Type, a.ID, err)
		}
		total += n
	}
	return total, nil
}

type bigSpendConfig struct {
	Threshold decimal.Decimal `json:"threshold"`
}

func evalBigSpend(ctx context.Context, q *dbgen.Queries, a dbgen.Alert, now time.Time) (int, error) {
	var cfg bigSpendConfig
	if err := json.Unmarshal(a.Config, &cfg); err != nil {
		return 0, fmt.Errorf("parse config: %w", err)
	}
	if !cfg.Threshold.IsPositive() {
		return 0, nil // a zero/absent threshold would alert on everything
	}

	since := now.AddDate(0, 0, -bigSpendWindowDays)
	rows, err := q.BigSpendCandidates(ctx, dbgen.BigSpendCandidatesParams{
		HouseholdID: a.HouseholdID,
		Amount:      cfg.Threshold,
		Date:        since,
		AlertID:     a.ID,
	})
	if err != nil {
		return 0, err
	}

	count := 0
	for _, row := range rows {
		txID := row.ID
		payload := mustJSON(map[string]string{
			"merchant":  row.Merchant,
			"amount":    row.Amount.StringFixed(2),
			"date":      row.Date.Format(time.DateOnly),
			"threshold": cfg.Threshold.StringFixed(2),
		})
		if _, err := q.InsertAlertEvent(ctx, dbgen.InsertAlertEventParams{
			AlertID: a.ID, TransactionID: &txID, Payload: payload,
		}); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

type unusualMerchantConfig struct {
	RecentDays int             `json:"recent_days"`
	MinAmount  decimal.Decimal `json:"min_amount"`
}

func evalUnusualMerchant(ctx context.Context, q *dbgen.Queries, a dbgen.Alert, now time.Time) (int, error) {
	var cfg unusualMerchantConfig
	if err := json.Unmarshal(a.Config, &cfg); err != nil {
		return 0, fmt.Errorf("parse config: %w", err)
	}
	if cfg.RecentDays <= 0 {
		cfg.RecentDays = defaultUnusualDays
	}

	cutoff := now.AddDate(0, 0, -cfg.RecentDays)
	rows, err := q.UnusualMerchantCandidates(ctx, dbgen.UnusualMerchantCandidatesParams{
		HouseholdID: a.HouseholdID,
		Date:        cutoff,
		Amount:      cfg.MinAmount, // zero means "any amount"
		AlertID:     a.ID,
	})
	if err != nil {
		return 0, err
	}

	count := 0
	for i, row := range rows {
		if i >= maxUnusualCandidates {
			break // guard against a flood on a very first evaluation
		}
		txID := row.ID
		payload := mustJSON(map[string]string{
			"merchant":     row.Merchant,
			"merchant_key": derefKey(row.MerchantKey),
			"amount":       row.Amount.StringFixed(2),
			"date":         row.Date.Format(time.DateOnly),
		})
		if _, err := q.InsertAlertEvent(ctx, dbgen.InsertAlertEventParams{
			AlertID: a.ID, TransactionID: &txID, Payload: payload,
		}); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

type budgetThresholdConfig struct {
	Percent int `json:"percent"`
}

func evalBudgetThreshold(ctx context.Context, q *dbgen.Queries, a dbgen.Alert, now time.Time) (int, error) {
	var cfg budgetThresholdConfig
	if err := json.Unmarshal(a.Config, &cfg); err != nil {
		return 0, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Percent <= 0 {
		cfg.Percent = defaultBudgetPercent
	}

	from, to := monthBounds(now)
	period := now.Format("2006-01")
	rows, err := q.BudgetSpendShared(ctx, dbgen.BudgetSpendSharedParams{
		HouseholdID: a.HouseholdID, Date: from, Date_2: to,
	})
	if err != nil {
		return 0, err
	}

	pct := decimal.NewFromInt(int64(cfg.Percent))
	count := 0
	for _, row := range rows {
		if !row.Budgeted.IsPositive() {
			continue
		}
		// spent >= budgeted * percent/100, all in exact decimal.
		trigger := row.Budgeted.Mul(pct).Div(decimal.NewFromInt(100))
		if row.Spent.LessThan(trigger) {
			continue
		}

		exists, err := q.AlertEventExistsForPeriod(ctx, dbgen.AlertEventExistsForPeriodParams{
			AlertID: a.ID, Period: period, CategorySlug: row.CategorySlug,
		})
		if err != nil {
			return count, err
		}
		if exists {
			continue
		}

		payload := mustJSON(map[string]string{
			"period":        period,
			"category_slug": row.CategorySlug,
			"category_name": row.CategoryName,
			"budgeted":      row.Budgeted.StringFixed(2),
			"spent":         row.Spent.StringFixed(2),
			"percent":       fmt.Sprintf("%d", cfg.Percent),
		})
		if _, err := q.InsertAlertEvent(ctx, dbgen.InsertAlertEventParams{
			AlertID: a.ID, TransactionID: nil, Payload: payload,
		}); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

type lowLeftoverConfig struct {
	Floor decimal.Decimal `json:"floor"`
}

func evalLowLeftover(ctx context.Context, q *dbgen.Queries, a dbgen.Alert, now time.Time) (int, error) {
	var cfg lowLeftoverConfig
	if err := json.Unmarshal(a.Config, &cfg); err != nil {
		return 0, fmt.Errorf("parse config: %w", err)
	}

	from, to := monthBounds(now)
	period := now.Format("2006-01")
	row, err := q.SharedMonthCashflow(ctx, dbgen.SharedMonthCashflowParams{
		HouseholdID: a.HouseholdID, Date: from, Date_2: to,
	})
	if err != nil {
		return 0, err
	}

	// Only meaningful once income has landed: at the start of a month, before a
	// paycheck arrives, leftover is trivially under any floor. Requiring income
	// avoids firing that false alarm every month.
	if !row.Income.IsPositive() {
		return 0, nil
	}
	leftover := row.Income.Sub(row.Spending)
	if !leftover.LessThan(cfg.Floor) {
		return 0, nil
	}

	exists, err := q.AlertEventExistsForPeriod(ctx, dbgen.AlertEventExistsForPeriodParams{
		AlertID: a.ID, Period: period, CategorySlug: "",
	})
	if err != nil {
		return 0, err
	}
	if exists {
		return 0, nil
	}

	payload := mustJSON(map[string]string{
		"period":   period,
		"income":   row.Income.StringFixed(2),
		"spending": row.Spending.StringFixed(2),
		"leftover": leftover.StringFixed(2),
		"floor":    cfg.Floor.StringFixed(2),
	})
	if _, err := q.InsertAlertEvent(ctx, dbgen.InsertAlertEventParams{
		AlertID: a.ID, TransactionID: nil, Payload: payload,
	}); err != nil {
		return 0, err
	}
	return 1, nil
}

// monthBounds returns the first and last day of now's calendar month, in UTC,
// matching the reporting layer's period convention.
func monthBounds(now time.Time) (from, to time.Time) {
	from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	to = from.AddDate(0, 1, -1)
	return from, to
}

func mustJSON(v any) []byte {
	// The inputs are always plain string maps, so marshalling cannot fail; if
	// it somehow does, an empty object is a safe, non-crashing payload.
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func derefKey(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
