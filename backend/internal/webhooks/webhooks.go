// Package webhooks is Ledgermancy's outbound event bus: the half of the app
// that talks to software the app does not ship.
//
// Push (ntfy) reaches a phone and SMTP reaches an inbox. Neither reaches Home
// Assistant, a Discord channel, or the script somebody wrote to append every
// large charge to a spreadsheet. A webhook is a standing subscription — a URL, a
// secret, and a set of triggers — and every event matching a subscription is
// written to `webhook_messages` and delivered by a River job with retries.
//
// # What this package owns, and what it does not
//
// This package owns the trigger vocabulary, the payload envelope, the signature,
// and the mechanics of one HTTP delivery. It owns no scheduling: the message
// rows are written by SQL in internal/db/queries/webhooks.sql (which is where
// scoping lives), and the retry/dead-letter policy is enforced by the River
// worker in internal/jobs. That split is deliberate and mirrors internal/notify,
// which likewise knows how to send one thing and nothing about when.
//
// # The rule that everything else is built around
//
// The message row is written BEFORE any delivery is attempted, in the same pass
// that produced the event. A receiver that is down, a worker that is killed
// mid-request, a container that restarts — none of them can lose the event,
// because the event was durable before the first packet went out. This is the
// same write-before-deliver discipline the digest follows, and every producer
// that hooks into this package must keep it.
//
// # Opt-in
//
// The whole subsystem is off unless WEBHOOKS_ENABLED is set. Ledgermancy's
// standing claim is that it phones home to nothing but Plaid and your AI
// provider unless you switch something on, and a feature whose entire purpose is
// outbound requests to arbitrary hosts is the last place to make an exception.
package webhooks

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The trigger vocabulary — the events a subscription can ask for.
//
// Dotted `noun.verb` names, in past tense, because a webhook payload is a report
// of something that already happened and is never a request to do something.
// They are stored verbatim in webhooks.triggers and webhook_messages.trigger_type
// and are part of the app's public API: renaming one silently unsubscribes
// every receiver that asked for it, so treat these as frozen and add rather than
// rename.
const (
	// TriggerInsightCreated fires when the proactive-insight engine raises a NEW
	// insight (not when it refreshes an existing one — a refresh is the same
	// finding restated, and a receiver that treated it as new would announce the
	// same thing every hour).
	TriggerInsightCreated = "insight.created"

	// TriggerAlertFired fires when a configured alert rule raises an event. This
	// is the one trigger whose delivery is filtered per subscriber: an alert on a
	// private account reaches only a webhook whose owner can see the underlying
	// transaction.
	TriggerAlertFired = "alert.fired"

	// TriggerGoalContribution fires when a contribution is recorded against a
	// savings goal, including the payload's view of where the goal now stands.
	TriggerGoalContribution = "goal.contribution.recorded"

	// TriggerTest is what the "send a test" button emits. It is deliberately NOT
	// subscribable — it is not in Triggers, so no rule can select it — because a
	// test is addressed to one named webhook rather than fanned out to whoever
	// asked. A receiver that wants to ignore tests can switch on this value.
	TriggerTest = "webhook.test"
)

// Triggers lists every trigger a subscription may select, in the order the
// settings UI offers them.
var Triggers = []string{
	TriggerInsightCreated,
	TriggerAlertFired,
	TriggerGoalContribution,
}

// IsValidTrigger reports whether t is a subscribable trigger. TriggerTest is
// excluded on purpose; see its doc comment.
func IsValidTrigger(t string) bool {
	for _, known := range Triggers {
		if known == t {
			return true
		}
	}
	return false
}

// NormalizeTriggers validates a requested trigger set and returns it in
// Triggers order, so two subscriptions asking for the same events are stored
// identically and the UI never has to sort.
//
// Duplicates collapse; an unknown trigger is rejected rather than dropped.
// Silently dropping one would hand back a subscription that listens to less than
// the caller was told it does, and the first anybody would learn of it is an
// event that never arrived.
func NormalizeTriggers(requested []string) ([]string, bool) {
	wanted := make(map[string]bool, len(requested))
	for _, t := range requested {
		t = strings.TrimSpace(strings.ToLower(t))
		if !IsValidTrigger(t) {
			return nil, false
		}
		wanted[t] = true
	}
	if len(wanted) == 0 {
		return nil, false
	}

	out := make([]string, 0, len(wanted))
	for _, t := range Triggers {
		if wanted[t] {
			out = append(out, t)
		}
	}
	return out, true
}

// maxURLLen bounds the one field that becomes an outbound request. Nothing
// legitimate is near this; an unbounded string is an unbounded row.
const maxURLLen = 2000

// ValidateURL checks a destination is something this app is willing to POST to.
//
// The bar is deliberately low: an absolute http:// or https:// URL with a host.
// It is NOT an SSRF filter, and it should not become one — the flagship
// destination for a self-hosted household finance app is a Home Assistant, a
// n8n, or a shell script on the same LAN, so blocking private addresses would
// block the main use case and leave the feature useful only to people who
// already run something on the public internet.
//
// What makes that acceptable is who can reach this: creating a webhook requires
// an authenticated adult member of the household, i.e. somebody who can already
// read every figure a webhook could ever carry and who can already make the
// machine talk to their own LAN in a dozen other ways. The threat SSRF filters
// exist to stop — an untrusted user steering the server at internal services —
// is not the threat model here. The subsystem being off by default is the
// control that matters.
func ValidateURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("url is required")
	}
	if len(raw) > maxURLLen {
		return "", fmt.Errorf("url is too long")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("url is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("url must start with http:// or https://")
	}
	if u.Host == "" {
		return "", fmt.Errorf("url must include a host")
	}
	return u.String(), nil
}

// SecretPrefix marks a webhook signing secret in a log or a config file, the
// same way api_tokens' prefix marks a bearer token. It is not a namespace and
// carries no meaning to the code.
const SecretPrefix = "whsec_"

// NewSecret mints a signing secret. 32 bytes of crypto/rand, hex-encoded, which
// is comfortably beyond what HMAC-SHA256 needs and is a value a receiver's
// config file can hold verbatim without encoding questions.
//
// The HMAC key is the UTF-8 bytes of the WHOLE returned string, prefix included
// — the receiver copies exactly what it was shown, and there is nothing to strip
// or decode first. The docs say this explicitly, because a signing scheme whose
// key derivation is a matter of opinion is a signing scheme nobody can verify.
func NewSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate webhook secret: %w", err)
	}
	return SecretPrefix + hex.EncodeToString(raw), nil
}

// Envelope is the shape of every delivery body.
//
// Flat and boring on purpose. A receiver switches on `trigger` and reads `data`;
// everything above `data` is the same for every event, so a generic handler can
// be written once. `event_id` is the id of the DOMAIN object that fired (the
// insight, the alert event) rather than the delivery — deliveries are identified
// by the X-Ledgermancy-Delivery header, and keeping the two apart is what lets a
// receiver dedupe on the event across retries.
type Envelope struct {
	Trigger     string    `json:"trigger"`
	EventID     string    `json:"event_id"`
	HouseholdID uuid.UUID `json:"household_id"`
	OccurredAt  time.Time `json:"occurred_at"`
	Data        any       `json:"data"`
}

// NewEnvelope builds a delivery body. occurredAt is passed rather than taken
// from the clock so a producer stamps the event's own time, and so tests can pin
// it.
func NewEnvelope(trigger, eventID string, householdID uuid.UUID, occurredAt time.Time, data any) Envelope {
	return Envelope{
		Trigger:     trigger,
		EventID:     eventID,
		HouseholdID: householdID,
		OccurredAt:  occurredAt.UTC(),
		Data:        data,
	}
}

// DedupeKey is the idempotency key a producer stamps on a message.
//
// Composed from the trigger and the domain object's id, so the periodic sweeps —
// which by design re-examine the same insights and alert events every pass —
// enqueue each event exactly once per subscriber rather than once per sweep. Two
// different triggers over the same object stay distinct.
func DedupeKey(trigger, eventID string) string {
	return trigger + ":" + eventID
}
