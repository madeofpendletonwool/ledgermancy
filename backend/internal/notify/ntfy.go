package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/apex42group/ledgermancy/backend/internal/config"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// The reserved preference keys this notifier reads. They are owned by the
// preferences store (doc 02); duplicated as constants here only to avoid magic
// strings.
const (
	prefChannel   = "notify.channel"
	prefNtfyTopic = "notify.ntfy_topic"
)

// Ntfy delivers notifications over ntfy (https://ntfy.sh or a self-hosted
// instance). It resolves each recipient's topic and channel from the
// preferences store at send time.
type Ntfy struct {
	http    *http.Client
	baseURL string
	token   string
	queries *dbgen.Queries
}

// New builds a notifier from NTFY config and a query handle for preference
// lookups. Like ai.New it never fails and never returns nil; an unconfigured
// server or an unconfigured user simply yields no-op sends.
func New(cfg config.NTFYConfig, queries *dbgen.Queries) *Ntfy {
	return &Ntfy{
		// Short timeout: a push is fire-and-forget, and the River job retries,
		// so we would rather fail fast than hold a worker on a stalled server.
		http:    &http.Client{Timeout: 10 * time.Second},
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		token:   cfg.Token,
		queries: queries,
	}
}

// Enabled reports whether a server is configured to deliver to at all.
func (n *Ntfy) Enabled() bool { return n != nil && n.baseURL != "" }

// Send resolves the user's channel and topic, then POSTs the message. It is a
// no-op (nil) when the server is unconfigured, the user's channel is not
// "ntfy", or they have no topic set — so callers can always call.
func (n *Ntfy) Send(ctx context.Context, userID uuid.UUID, msg Notification) error {
	if !n.Enabled() {
		return nil
	}

	channel, err := n.stringPref(ctx, userID, prefChannel)
	if err != nil {
		return err
	}
	if channel != "ntfy" {
		return nil
	}
	topic, err := n.stringPref(ctx, userID, prefNtfyTopic)
	if err != nil {
		return err
	}
	if topic == "" {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.baseURL+"/"+topic, strings.NewReader(msg.Body))
	if err != nil {
		return fmt.Errorf("build ntfy request: %w", err)
	}
	req.Header.Set("Title", msg.Title)
	if msg.Priority > 0 {
		req.Header.Set("Priority", strconv.Itoa(msg.Priority))
	}
	if len(msg.Tags) > 0 {
		req.Header.Set("Tags", strings.Join(msg.Tags, ","))
	}
	if msg.ClickURL != "" {
		req.Header.Set("Click", msg.ClickURL)
	}
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}

	resp, err := n.http.Do(req)
	if err != nil {
		return fmt.Errorf("post to ntfy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		// Return an error so the River job retries; ntfy returns 4xx/5xx with a
		// short body we don't need to surface beyond the status.
		return fmt.Errorf("ntfy responded %d", resp.StatusCode)
	}
	return nil
}

// stringPref reads a JSON-string preference for a user, returning "" when the
// key is unset. A stored value that is not a JSON string is treated as unset
// rather than an error, so a malformed pref never blocks delivery to others.
func (n *Ntfy) stringPref(ctx context.Context, userID uuid.UUID, key string) (string, error) {
	raw, err := n.queries.GetUserPreference(ctx, dbgen.GetUserPreferenceParams{
		UserID: &userID,
		Key:    key,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read preference %s: %w", key, err)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", nil
	}
	return s, nil
}
