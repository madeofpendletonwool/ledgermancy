package insights

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// --------------------------------------------------------------------------
// Anomaly detection: merchant_outlier and duplicate_charge.
//
// Both producers hand SQL the whole statistical job (ListMerchantOutlierCandidates
// and ListDuplicateChargeCandidates in queries/anomaly.sql) and do nothing here
// but pick a threshold and compare — the same split largeTransactionProducer
// already documents. Nothing in this file adds, averages or divides a figure
// that ends up in front of a user; every money value in a Candidate.Data is a
// StringFixed(2) of a number Postgres computed.
// --------------------------------------------------------------------------

const (
	// Baseline window. Longer than largeTxnBaselineMonths (6) on purpose: a
	// per-merchant sample is far thinner than a household-wide one, and six
	// months at a quarterly merchant is two charges.
	anomalyBaselineMonths = 12

	// Recent window for outlier candidates, matching largeTxnRecentDays so the
	// two producers consider the same charges.
	outlierRecentDays = 14

	// Recent window for duplicates. Shorter because a duplicate is only news
	// while it can still be disputed, and because the pair must both land in it.
	duplicateRecentDays = 7

	// Cap on charges examined per pass. They arrive amount-descending, so this
	// keeps the biggest ones and drops a long tail that could not clear a
	// threshold anyway.
	anomalyMaxCandidates = 25

	// outlierCap is a VALIDITY test on the sample, not a taste knob, which is
	// why it is fixed rather than scaled by sensitivity. See outlierThreshold.
	outlierCap = 5

	// priceCreepCeiling bounds the price-creep hand-off. See merchantOutlier's
	// Detect: a recurring merchant whose charge is within this multiple of its
	// median belongs to the subscription producer, not to this one.
	priceCreepCeiling = 3

	// outlierMinSamplesFloor is the LOWEST minSamples across every sensitivity.
	// largeTransactionProducer yields to this producer at exactly this count, so
	// using the lowest value means the two can never both go quiet on a merchant
	// when a household picks a stricter setting.
	outlierMinSamplesFloor = 5

	anomalySensitivityKey = "anomaly.sensitivity"
)

// sensitivity is the household's tolerance for the two detectors. Stored as a
// household preference (JSONB, no migration); the authoritative default lives
// here rather than in the preferences handler, because a producer must never
// depend on the API having been called.
type sensitivity struct {
	name           string
	medianMultiple decimal.Decimal
	p95Multiple    decimal.Decimal
	minSamples     int64
	outlierFloor   decimal.Decimal // absolute dollars, outlier producer
	duplicateFloor decimal.Decimal // absolute dollars, duplicate producer
}

func balancedSensitivity() sensitivity {
	return sensitivity{
		name:           "balanced",
		medianMultiple: decimal.NewFromFloat(3),
		p95Multiple:    decimal.NewFromFloat(1.5),
		minSamples:     5,
		outlierFloor:   decimal.NewFromInt(50),
		duplicateFloor: decimal.NewFromInt(25),
	}
}

// sensitivityByName maps the stored preference to its knobs. An unrecognised or
// unset value is balanced — the detectors must keep working through a typo.
func sensitivityByName(name string) sensitivity {
	switch name {
	case "conservative":
		return sensitivity{
			name:           "conservative",
			medianMultiple: decimal.NewFromFloat(4),
			p95Multiple:    decimal.NewFromFloat(2),
			minSamples:     8,
			outlierFloor:   decimal.NewFromInt(100),
			duplicateFloor: decimal.NewFromInt(50),
		}
	case "sensitive":
		return sensitivity{
			name:           "sensitive",
			medianMultiple: decimal.NewFromFloat(2),
			p95Multiple:    decimal.NewFromFloat(1.25),
			minSamples:     5,
			outlierFloor:   decimal.NewFromInt(25),
			duplicateFloor: decimal.NewFromInt(15),
		}
	default:
		return balancedSensitivity()
	}
}

// sensitivityFor reads the household's setting, degrading to the default on any
// error. Detection must never fail because a preference row is missing or
// malformed — the same reasoning as jobs.stringPref.
func sensitivityFor(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID) sensitivity {
	hh := householdID
	raw, err := q.GetHouseholdPreference(ctx, dbgen.GetHouseholdPreferenceParams{
		HouseholdID: &hh, Key: anomalySensitivityKey,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("read anomaly sensitivity", "error", err, "household_id", householdID)
		}
		return balancedSensitivity()
	}
	var name string
	_ = json.Unmarshal(raw, &name)
	return sensitivityByName(name)
}

// outlierThreshold picks the bar a charge must clear at one merchant.
//
// MEDIAN-anchored, not mean-anchored. Spend distributions are right-skewed and σ
// is not robust: one past $600 charge at a $40 merchant drags mean+3σ to $571.80,
// so nothing ever fires again — and the charge that broke the detector is exactly
// the kind of charge it existed to catch. The median ignores it entirely.
//
// p95 RAISES the bar for merchants that are legitimately wide. A grocery store
// ranges $20–$200; 3× its $60 median would flag every big shop. But p95 has the
// same small-sample failure as σ — PERCENTILE_DISC(0.95) over a dozen charges
// lands ON the historical outlier — so it may only raise the bar when the
// merchant's spread is ordinary. outlierCap is that test: a p95 more than 5× the
// median means the sample is contaminated rather than wide, and p95 is discarded.
//
// The threshold is only half the gate. The caller must also apply the absolute
// dollar floor: a $4 coffee at a $1.30 merchant is a statistical outlier and
// practically noise. Both gates, always.
func outlierThreshold(median, p95 decimal.Decimal, s sensitivity) decimal.Decimal {
	th := median.Mul(s.medianMultiple)
	if p95.LessThanOrEqual(median.Mul(decimal.NewFromInt(outlierCap))) {
		if p := p95.Mul(s.p95Multiple); p.GreaterThan(th) {
			th = p
		}
	}
	return th
}

// anomalyPriority maps a dollar amount onto the feed's priority scale.
//
// The bands exist because priority is the ONLY push control: dispatchInsightPushes
// pushes an insight at >= insightPushMinPriority (4) and there is no separate
// flag. A duplicate $900 charge is worth interrupting someone for; a duplicate $6
// one is not, and pushing it teaches them to ignore the channel.
func anomalyPriority(amount, pushAt decimal.Decimal) int {
	switch {
	case amount.GreaterThanOrEqual(decimal.NewFromInt(500)):
		return 5
	case amount.GreaterThanOrEqual(pushAt):
		return 4
	default:
		return 3
	}
}

var (
	outlierPushAt   = decimal.NewFromInt(150)
	duplicatePushAt = decimal.NewFromInt(100)
)

// --------------------------------------------------------------------------
// merchant_outlier — a single charge far above what THIS merchant normally bills.
// --------------------------------------------------------------------------

type merchantOutlierProducer struct{}

func (merchantOutlierProducer) Kind() string { return "merchant_outlier" }

func (merchantOutlierProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	s := sensitivityFor(ctx, q, householdID)

	rows, err := q.ListMerchantOutlierCandidates(ctx, dbgen.ListMerchantOutlierCandidatesParams{
		HouseholdID:   householdID,
		UserID:        sharedUser,
		BaselineFrom:  now.AddDate(0, -anomalyBaselineMonths, 0),
		RecentFrom:    now.AddDate(0, 0, -outlierRecentDays),
		RecentTo:      now,
		MaxCandidates: anomalyMaxCandidates,
	})
	if err != nil {
		return nil, err
	}

	// Price creep is already shipped (subscription.go) and must not be raised
	// twice. The two rarely collide — a subscription creeping $15.99 to $17.99
	// clears neither gate below — but a subscription whose price merely doubles
	// could. Resolved merchant keys, so a merged subscription matches.
	recurring := map[string]bool{}
	if recs, rerr := q.GetRecurringMerchants(ctx, dbgen.GetRecurringMerchantsParams{
		HouseholdID: householdID, UserID: sharedUser,
		Date: now.AddDate(0, -anomalyBaselineMonths, 0),
	}); rerr == nil {
		for _, r := range recs {
			recurring[r.MerchantKey] = true
		}
	} else {
		// Not fatal: without this the worst case is one duplicated insight on a
		// subscription that tripled, which is at least a true statement.
		slog.Warn("anomaly: recurring merchants unavailable", "error", rerr, "household_id", householdID)
	}

	var out []Candidate
	for _, r := range rows {
		if r.SampleCount < s.minSamples {
			continue
		}
		if r.MedianAmount.IsZero() {
			continue
		}
		if r.Amount.LessThan(outlierThreshold(r.MedianAmount, r.P95Amount, s)) {
			continue
		}
		if r.Amount.LessThan(s.outlierFloor) {
			continue
		}
		// A recurring merchant within the creep ceiling belongs to the
		// subscription producer.
		if recurring[r.MerchantKey] &&
			r.Amount.LessThan(r.MedianAmount.Mul(decimal.NewFromInt(priceCreepCeiling))) {
			continue
		}

		amount := r.Amount.Round(2)
		median := r.MedianAmount.Round(2)
		dateStr := r.Date.Format(time.DateOnly)
		period := monthStart(r.Date)

		out = append(out, Candidate{
			Kind:     "merchant_outlier",
			Priority: anomalyPriority(amount, outlierPushAt),
			Title:    fmt.Sprintf("Unusual charge at %s", r.Merchant),
			Body: fmt.Sprintf(
				"A %s charge at %s on %s is well above the %s you typically pay there, across %d previous charges.",
				money(amount), r.Merchant, r.Date.Format("Jan 2"), money(median), r.SampleCount),
			Data: map[string]any{
				"merchant":      r.Merchant,
				"merchant_key":  r.MerchantKey,
				"amount":        amount.StringFixed(2),
				"date":          dateStr,
				"category":      r.CategoryName,
				"typical":       median.StringFixed(2),
				"p95":           r.P95Amount.Round(2).StringFixed(2),
				"sample_count":  r.SampleCount,
				"times_typical": r.Amount.Div(r.MedianAmount).Round(1).String(),
				// Rendered nowhere. Kept so the design's central claim — that a
				// mean+3σ gate would have stayed silent here — stays auditable
				// from a stored insight rather than only from the test suite.
				"mean":   r.MeanAmount.Round(2).StringFixed(2),
				"stddev": r.StddevAmount.Round(2).StringFixed(2),
			},
			Period: &period,
			// Keyed on the transaction id, not merchant:date:amount the way
			// large_transaction is. Transaction UUIDs are stable across syncs
			// (UpsertTransaction conflicts on plaid_transaction_id and never
			// rewrites the PK), whereas a merchant-keyed dedupe key CHANGES the
			// moment the user merges that merchant — silently resurrecting an
			// insight they had already dismissed.
			DedupeKey: fmt.Sprintf("merchant_outlier:%s", r.TxID),
		})
	}
	return out, nil
}

// --------------------------------------------------------------------------
// duplicate_charge — the same merchant billing the same amount twice on one card
// within a day. The classic billing error, and the one a user most wants caught.
// --------------------------------------------------------------------------

type duplicateChargeProducer struct{}

func (duplicateChargeProducer) Kind() string { return "duplicate_charge" }

func (duplicateChargeProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	s := sensitivityFor(ctx, q, householdID)

	rows, err := q.ListDuplicateChargeCandidates(ctx, dbgen.ListDuplicateChargeCandidatesParams{
		HouseholdID:  householdID,
		UserID:       sharedUser,
		BaselineFrom: now.AddDate(0, -anomalyBaselineMonths, 0),
		RecentFrom:   now.AddDate(0, 0, -duplicateRecentDays),
		RecentTo:     now,
	})
	if err != nil {
		return nil, err
	}

	var out []Candidate
	for _, r := range rows {
		// The floor is what separates a billing error from two coffees. Exact
		// amount equality does not: two $5.75 lattes ARE exactly equal.
		if r.Amount.LessThan(s.duplicateFloor) {
			continue
		}

		amount := r.Amount.Round(2)
		count := int64(r.ChargeCount)
		total := amount.Mul(decimal.NewFromInt(count))
		dateStr := r.FirstDate.Format(time.DateOnly)
		period := monthStart(r.FirstDate)

		out = append(out, Candidate{
			Kind:     "duplicate_charge",
			Priority: anomalyPriority(total, duplicatePushAt),
			Title:    fmt.Sprintf("Possible duplicate charge at %s", r.Merchant),
			Body: fmt.Sprintf(
				"%s charged %s %d times around %s, totalling %s. If that was one purchase, it is worth checking.",
				r.Merchant, money(amount), count, r.FirstDate.Format("Jan 2"), money(total)),
			Data: map[string]any{
				"merchant":     r.Merchant,
				"merchant_key": r.MerchantKey,
				"amount":       amount.StringFixed(2),
				"date":         dateStr,
				"category":     r.CategoryName,
				"charge_count": count,
				"total":        total.StringFixed(2),
			},
			Period: &period,
			// Anchored on the group's earliest charge, so a third identical
			// charge refreshes this insight to charge_count 3 rather than
			// raising a second one. Merge-stable for the same reason as above.
			DedupeKey: fmt.Sprintf("duplicate_charge:%s", r.TxID),
		})
	}
	return out, nil
}
