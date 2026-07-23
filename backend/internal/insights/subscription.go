package insights

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/ai"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// subscription — the noteworthy recurring charges, on top of the plain
// new_recurring feed rows. Two flavors, both detected deterministically:
//
//   - price_creep: the merchant's newer charges average materially higher than
//     its older ones (GetRecurringAmountTrend does the split and the delta).
//   - zombie: a small, regular, still-active charge that is easy to forget.
//
// A merchant that is neither gets no subscription row — it already appears in
// the Spending recurring table and, if active, as a new_recurring insight.
//
// Detect leaves the AI `category` label empty — detection is deterministic SQL
// only. The producer additionally implements Classifier: when AI is enabled the
// engine calls Classify over the whole batch (one model call) to fill
// `category` from a fixed label set. Without a key the field stays empty and the
// UI simply omits the type, so nothing breaks.

const (
	// A monthly estimate at or below this is a "zombie" — small enough to slip
	// past a bank statement unnoticed.
	zombieMaxMonthlyDollars = 15
)

type subscriptionProducer struct{}

func (subscriptionProducer) Kind() string { return "subscription" }

func (subscriptionProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	since := now.AddDate(0, -recurringLookbackMonths, 0)

	recurring, err := q.GetRecurringMerchants(ctx, dbgen.GetRecurringMerchantsParams{
		HouseholdID: householdID, UserID: sharedUser, Date: since,
	})
	if err != nil {
		return nil, err
	}

	trend, err := q.GetRecurringAmountTrend(ctx, dbgen.GetRecurringAmountTrendParams{
		HouseholdID: householdID, UserID: sharedUser, Date: since,
	})
	if err != nil {
		return nil, err
	}
	creepByKey := make(map[string]dbgen.GetRecurringAmountTrendRow, len(trend))
	for _, t := range trend {
		if t.MerchantKey != nil {
			creepByKey[*t.MerchantKey] = t
		}
	}

	zombieCeiling := decimal.NewFromInt(zombieMaxMonthlyDollars)
	activeCutoff := now.AddDate(0, 0, -recurringActiveDays)

	var out []Candidate
	for _, m := range recurring {
		// Only currently-active subscriptions, so a cancelled one stops
		// resurfacing — same gate new_recurring uses.
		if m.MerchantKey == nil || m.LastSeen.Before(activeCutoff) {
			continue
		}
		key := *m.MerchantKey

		var monthly decimal.Decimal
		if m.AvgGapDays.IsPositive() {
			monthly = m.AverageAmount.Mul(daysPerMonth).Div(m.AvgGapDays).Round(2)
		}
		avg := m.AverageAmount.Round(2)

		creep, isCreep := creepByKey[key]
		isZombie := monthly.IsPositive() && monthly.LessThanOrEqual(zombieCeiling)
		if !isCreep && !isZombie {
			continue
		}

		data := map[string]any{
			"merchant":         m.Merchant,
			"merchant_key":     key,
			"cadence":          cadenceLabel(m.AvgGapDays),
			"typical_amount":   avg.StringFixed(2),
			"monthly_estimate": monthly.StringFixed(2),
			"category":         "", // AI classification hook; see file header.
			"flavor":           "",
		}

		var (
			priority    int
			title, body string
		)
		switch {
		case isCreep:
			// Price creep outranks a zombie: it is costing more money right now.
			early := creep.EarlyAvg.Round(2)
			recent := creep.RecentAvg.Round(2)
			delta := creep.Delta.Round(2)
			data["flavor"] = "price_creep"
			data["early_amount"] = early.StringFixed(2)
			data["recent_amount"] = recent.StringFixed(2)
			data["delta"] = delta.StringFixed(2)
			priority = 3
			title = fmt.Sprintf("%s costs more than it used to", m.Merchant)
			body = fmt.Sprintf(
				"%s has crept up from %s to %s — about %s more each charge.",
				m.Merchant, money(early), money(recent), money(delta))
		case isZombie:
			data["flavor"] = "zombie"
			priority = 2
			title = fmt.Sprintf("Easy-to-forget charge from %s", m.Merchant)
			body = fmt.Sprintf(
				"%s charges about %s %s — roughly %s a month. Small enough to slip by unnoticed.",
				m.Merchant, money(avg), cadenceLabel(m.AvgGapDays), money(monthly))
		}

		out = append(out, Candidate{
			Kind:      "subscription",
			Priority:  priority,
			Title:     title,
			Body:      body,
			Data:      data,
			DedupeKey: "subscription:" + key,
		})
	}
	return out, nil
}

// Classify fills each candidate's `category` from the fixed label set, in one
// model call over the batch. It is best-effort: on any AI error the candidates
// keep their empty category, so the feed still renders. Numbers in Data are
// never touched — the model only labels.
func (subscriptionProducer) Classify(ctx context.Context, client *ai.Client, candidates []Candidate) error {
	subs := make([]ai.SubscriptionInput, 0, len(candidates))
	for _, c := range candidates {
		key, _ := c.Data["merchant_key"].(string)
		if key == "" {
			continue
		}
		merchant, _ := c.Data["merchant"].(string)
		cadence, _ := c.Data["cadence"].(string)
		subs = append(subs, ai.SubscriptionInput{MerchantKey: key, Merchant: merchant, Cadence: cadence})
	}
	if len(subs) == 0 {
		return nil
	}

	labels, err := client.ClassifySubscriptions(ctx, subs)
	if err != nil {
		return err
	}
	for i := range candidates {
		key, _ := candidates[i].Data["merchant_key"].(string)
		if label, ok := labels[key]; ok {
			candidates[i].Data["category"] = label
		}
	}
	return nil
}
