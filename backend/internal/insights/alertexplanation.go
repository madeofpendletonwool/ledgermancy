package insights

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/apex42group/ledgermancy/backend/internal/alerts"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// alert_explanation — enrichment for a fired alert: "why is this notable?".
// Detection is never re-run here; the producer reads recent alert_events and
// adds a SQL-computed baseline ("you normally spend ~$X"), leaving the
// deterministic alert and its record untouched. The engine's phrasing pass
// warms the template body when AI is on; without a key the template stands.
// One explanation per event, keyed by event id so re-runs upsert; the query
// only returns events that do not yet have an explanation, so a model call is
// spent once per event.

const alertExplanationWindowDays = 14

type alertExplanationProducer struct{}

func (alertExplanationProducer) Kind() string { return "alert_explanation" }

func (alertExplanationProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	events, err := q.ListRecentAlertEventsForExplanation(ctx, dbgen.ListRecentAlertEventsForExplanationParams{
		HouseholdID: householdID,
		TriggeredAt: now.AddDate(0, 0, -alertExplanationWindowDays),
	})
	if err != nil {
		return nil, err
	}

	var out []Candidate
	for _, e := range events {
		var payload map[string]string
		_ = json.Unmarshal(e.Payload, &payload)

		data := map[string]any{
			"alert_event_id": e.ID.String(),
			"alert_type":     e.AlertType,
		}

		var title, body string
		priority := 3

		switch e.AlertType {
		case alerts.TypeBigSpend, alerts.TypeUnusualMerchant:
			merchant := payload["merchant"]
			amount := payload["amount"]
			date := payload["date"]
			data["merchant"] = merchant
			data["amount"] = amount
			data["date"] = date

			// The comparison baseline is SQL, excluding the flagged transaction
			// so it is a genuine prior. Only available when the event is linked to
			// a transaction with a merchant key.
			if e.MerchantKey != nil && e.TransactionID != nil {
				base, err := q.GetMerchantSpendBaseline(ctx, dbgen.GetMerchantSpendBaselineParams{
					HouseholdID: householdID,
					UserID:      sharedUser,
					MerchantKey: *e.MerchantKey,
					ExcludeTx:   *e.TransactionID,
				})
				if err == nil && base.VisitCount > 0 {
					typical := base.TypicalAmount.Round(2)
					data["typical_amount"] = typical.StringFixed(2)
					data["visit_count"] = fmt.Sprintf("%d", base.VisitCount)
					title = fmt.Sprintf("Unusual charge at %s", merchant)
					body = fmt.Sprintf(
						"This $%s charge at %s is well above the $%s you usually spend there across %d visits.",
						amount, merchant, typical.StringFixed(2), base.VisitCount)
				}
			}
			if body == "" {
				// No prior history (first visit, or an unlinked event): fall back
				// to the plain "this stood out" line.
				title = fmt.Sprintf("Large charge at %s", merchant)
				body = fmt.Sprintf("A $%s charge at %s on %s stood out from your usual spending.", amount, merchant, date)
			}

		case alerts.TypeBudgetThreshold:
			cat := payload["category_name"]
			data["category"] = cat
			data["budgeted"] = payload["budgeted"]
			data["spent"] = payload["spent"]
			data["percent"] = payload["percent"]
			title = fmt.Sprintf("%s budget alert", cat)
			body = fmt.Sprintf(
				"You've spent $%s of your $%s %s budget — about %s%% of it.",
				payload["spent"], payload["budgeted"], cat, payload["percent"])

		case alerts.TypeLowLeftover:
			data["income"] = payload["income"]
			data["spending"] = payload["spending"]
			data["leftover"] = payload["leftover"]
			priority = 4
			title = "Low leftover this period"
			body = fmt.Sprintf(
				"You spent $%s against $%s of income, leaving $%s.",
				payload["spending"], payload["income"], payload["leftover"])

		default:
			continue // an unknown alert type is skipped, not an error
		}

		period := monthStart(e.TriggeredAt)
		out = append(out, Candidate{
			Kind:      "alert_explanation",
			Priority:  priority,
			Title:     title,
			Body:      body,
			Data:      data,
			Period:    &period,
			DedupeKey: "alert_explanation:" + e.ID.String(),
		})
	}
	return out, nil
}
