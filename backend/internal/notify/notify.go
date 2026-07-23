// Package notify delivers external push notifications. It mirrors the shape of
// internal/ai: a client is always constructed and reports Enabled(), and Send
// no-ops cleanly when the recipient has no channel configured, so callers never
// have to branch.
package notify

import (
	"context"

	"github.com/google/uuid"
)

// Notification is one message to deliver. All figures are already formatted by
// the caller — the notifier only transports strings.
type Notification struct {
	Title    string
	Body     string
	Priority int      // maps to ntfy 1..5; 0 means "unset", left to the server default
	Tags     []string // ntfy tags / emoji shortcodes
	ClickURL string   // deep link back into the app
}

// Notifier delivers a Notification to one user's configured channel.
type Notifier interface {
	// Enabled reports whether delivery will be attempted at all (server config
	// present). Per-user gating is separate and lives inside Send.
	Enabled() bool
	// Send delivers to one user. It is a no-op returning nil when the user has
	// no channel configured, so callers never branch on configuration.
	Send(ctx context.Context, userID uuid.UUID, n Notification) error
}
