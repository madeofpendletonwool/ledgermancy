package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
	"github.com/apex42group/ledgermancy/backend/internal/jobs"
	"github.com/apex42group/ledgermancy/backend/internal/notify"
)

// handleTestNotification delivers one throwaway push to the caller's configured
// channel, synchronously, so the Settings "Send test" button reports the real
// outcome. A wrong topic or an unreachable server surfaces here instead of
// failing silently on the next real alert.
func (s *Server) handleTestNotification(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	if s.Notify == nil || !s.Notify.Enabled() {
		writeError(w, http.StatusServiceUnavailable,
			"No ntfy server is configured on this deployment.")
		return
	}
	if s.userStringPref(r.Context(), identity.UserID, "notify.channel") != "ntfy" {
		writeError(w, http.StatusBadRequest,
			"Choose the ntfy channel and Save before sending a test.")
		return
	}
	if s.userStringPref(r.Context(), identity.UserID, "notify.ntfy_topic") == "" {
		writeError(w, http.StatusBadRequest,
			"Add an ntfy topic and Save before sending a test.")
		return
	}

	// Send resolves topic/token from the same preferences and returns the real
	// transport error on a non-2xx, which we relay verbatim.
	if err := s.Notify.Send(r.Context(), identity.UserID, notify.Notification{
		Title:    "Ledgermancy test",
		Body:     "If you can read this, your notifications are working.",
		Priority: 3,
		Tags:     []string{"white_check_mark"},
	}); err != nil {
		writeError(w, http.StatusBadGateway, "Delivery failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// handleSendDigestNow queues a one-off digest for the caller, bypassing cadence
// and the per-period dedupe. It's async (a digest may call the model), so the
// button confirms "queued" and the push arrives shortly after.
func (s *Server) handleSendDigestNow(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	channel := s.userStringPref(r.Context(), identity.UserID, "notify.channel")
	if channel == "" || channel == "none" {
		writeError(w, http.StatusBadRequest,
			"Set up a notification channel first — a digest has nowhere to go without one.")
		return
	}

	if err := jobs.EnqueueDigestNow(r.Context(), s.Jobs, identity.UserID, identity.HouseholdID); err != nil {
		s.internalError(w, "enqueue digest", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// userStringPref reads a JSON-string user preference, returning "" when unset or
// malformed — callers treat "" as "not configured".
func (s *Server) userStringPref(ctx context.Context, userID uuid.UUID, key string) string {
	raw, err := s.Queries.GetUserPreference(ctx, dbgen.GetUserPreferenceParams{UserID: &userID, Key: key})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("read preference", "error", err, "key", key)
		}
		return ""
	}
	var str string
	_ = json.Unmarshal(raw, &str)
	return str
}
