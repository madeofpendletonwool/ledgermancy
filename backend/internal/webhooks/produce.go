package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Emit writes one message per subscriber in a household that asked for this
// trigger, and returns their ids so the caller can queue a delivery for each.
//
// This is the write-before-deliver point. When it returns, the event is durable
// for every subscriber, and nothing that happens afterwards — a receiver being
// down, the worker being killed, the process restarting — can lose it. Callers
// must therefore treat a non-nil error as "the event was not recorded" and log
// it, and must NOT attempt delivery by any other route.
//
// A household with no matching subscription yields no rows and no error: that is
// the common case, and it costs one indexed read.
func Emit(
	ctx context.Context,
	q *dbgen.Queries,
	householdID uuid.UUID,
	trigger, eventID string,
	occurredAt time.Time,
	data any,
) ([]uuid.UUID, error) {
	body, err := json.Marshal(NewEnvelope(trigger, eventID, householdID, occurredAt, data))
	if err != nil {
		return nil, fmt.Errorf("marshal %s payload: %w", trigger, err)
	}

	ids, err := q.EnqueueWebhookMessages(ctx, dbgen.EnqueueWebhookMessagesParams{
		TriggerType: trigger,
		Payload:     body,
		DedupeKey:   DedupeKey(trigger, eventID),
		HouseholdID: householdID,
	})
	if err != nil {
		return nil, fmt.Errorf("enqueue %s webhook messages: %w", trigger, err)
	}
	return ids, nil
}

// EmitAlertEvent is Emit for alert events, which carry a per-subscriber
// visibility rule the generic path cannot express.
//
// An alert raised by a charge on a PRIVATE account is shown in the app only to
// that account's owner. A webhook is a standing subscription owned by one
// member, so the same rule has to hold on the way out: the enqueue is filtered
// by whether the webhook's owner may see the underlying transaction. Aggregate
// alerts carry no transaction and reach everyone, exactly as they do in the
// feed. See EnqueueAlertEventWebhookMessages for the predicate.
func EmitAlertEvent(
	ctx context.Context,
	q *dbgen.Queries,
	householdID, alertEventID uuid.UUID,
	occurredAt time.Time,
	data any,
) ([]uuid.UUID, error) {
	eventID := alertEventID.String()
	body, err := json.Marshal(NewEnvelope(TriggerAlertFired, eventID, householdID, occurredAt, data))
	if err != nil {
		return nil, fmt.Errorf("marshal alert payload: %w", err)
	}

	ids, err := q.EnqueueAlertEventWebhookMessages(ctx, dbgen.EnqueueAlertEventWebhookMessagesParams{
		TriggerType:  TriggerAlertFired,
		Payload:      body,
		DedupeKey:    DedupeKey(TriggerAlertFired, eventID),
		HouseholdID:  householdID,
		AlertEventID: alertEventID,
	})
	if err != nil {
		return nil, fmt.Errorf("enqueue alert webhook messages: %w", err)
	}
	return ids, nil
}

// EmitTest writes the payload the "send a test" button produces, addressed to
// one named webhook rather than fanned out.
//
// The dedupe key is a fresh UUID because every press genuinely IS a new event —
// a user pressing the button twice while fixing their receiver wants two
// deliveries, and a stable key would silently give them one.
func EmitTest(
	ctx context.Context,
	q *dbgen.Queries,
	householdID, webhookID uuid.UUID,
	occurredAt time.Time,
) (uuid.UUID, error) {
	eventID := uuid.NewString()
	body, err := json.Marshal(NewEnvelope(TriggerTest, eventID, householdID, occurredAt, map[string]any{
		"message": "This is a test delivery from Ledgermancy. If you can read this, your receiver and its signature check are working.",
	}))
	if err != nil {
		return uuid.Nil, fmt.Errorf("marshal test payload: %w", err)
	}

	id, err := q.EnqueueWebhookMessageForWebhook(ctx, dbgen.EnqueueWebhookMessageForWebhookParams{
		TriggerType: TriggerTest,
		Payload:     body,
		DedupeKey:   DedupeKey(TriggerTest, eventID),
		WebhookID:   webhookID,
		HouseholdID: householdID,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("enqueue test webhook message: %w", err)
	}
	return id, nil
}

// InsightData is the `data` object of an insight.created delivery.
//
// It is the feed row as the household sees it, which is the useful thing to
// forward: a receiver that wants to post the insight into a Discord channel
// needs the final (AI-phrased, when a key is configured) title and body, not the
// detectors' intermediate numbers.
type InsightData struct {
	InsightID string `json:"insight_id"`
	Kind      string `json:"kind"`
	Priority  int    `json:"priority"`
	Title     string `json:"title"`
	Body      string `json:"body"`
}

// AlertData is the `data` object of an alert.fired delivery.
//
// Fields carries the rule's own payload verbatim. Every money value in it is
// already a fixed-2 decimal STRING — that is how alerts store it, and it must
// stay a string on the wire: a receiver parsing JSON into a float is the one
// thing this app spends its whole schema avoiding, and handing it a float is
// inviting it.
type AlertData struct {
	AlertEventID string            `json:"alert_event_id"`
	AlertType    string            `json:"alert_type"`
	Fields       map[string]string `json:"fields"`
}

// GoalContributionData is the `data` object of a goal.contribution.recorded
// delivery. Amounts are decimal strings for the reason given on AlertData.
type GoalContributionData struct {
	ContributionID string `json:"contribution_id"`
	GoalID         string `json:"goal_id"`
	GoalName       string `json:"goal_name"`
	PersonID       string `json:"person_id"`
	Amount         string `json:"amount"`
	OccurredOn     string `json:"occurred_on"`
	Note           string `json:"note,omitempty"`
}
