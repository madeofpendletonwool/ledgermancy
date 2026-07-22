package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
	"github.com/apex42group/ledgermancy/backend/internal/jobs"
)

// plaidWebhook is the subset of Plaid's webhook payload we act on.
type plaidWebhook struct {
	WebhookType string `json:"webhook_type"`
	WebhookCode string `json:"webhook_code"`
	ItemID      string `json:"item_id"`
	Error       *struct {
		ErrorCode string `json:"error_code"`
	} `json:"error"`
}

// handlePlaidWebhook receives Plaid's push notifications.
//
// This route is deliberately outside both authentication and CSRF: Plaid is
// not a browser and has no session. It is safe because the handler treats the
// payload purely as a *hint* — it never trusts a value from it. The only
// action taken is "re-sync this item", and the re-sync re-reads everything
// from Plaid using our own stored access token. A forged webhook can therefore
// cause an unnecessary sync, but cannot inject or alter any data.
//
// It always answers 200: Plaid retries non-2xx responses, and there is nothing
// useful to retry when the payload names an item we do not have.
func (s *Server) handlePlaidWebhook(w http.ResponseWriter, r *http.Request) {
	var hook plaidWebhook
	if err := decodeJSONLenient(w, r, &hook); err != nil {
		slog.Warn("malformed plaid webhook", "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	slog.Info("plaid webhook",
		"type", hook.WebhookType, "code", hook.WebhookCode, "item_id", hook.ItemID)

	if hook.ItemID == "" {
		w.WriteHeader(http.StatusOK)
		return
	}

	item, err := s.Queries.GetPlaidItemByPlaidID(r.Context(), hook.ItemID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("look up webhook item", "error", err, "item_id", hook.ItemID)
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	switch hook.WebhookCode {
	case "SYNC_UPDATES_AVAILABLE", "INITIAL_UPDATE", "HISTORICAL_UPDATE", "DEFAULT_UPDATE":
		jobs.EnqueueSync(r.Context(), s.Jobs, item.ID)

	case "ITEM_LOGIN_REQUIRED", "PENDING_EXPIRATION", "USER_PERMISSION_REVOKED":
		// The user must reconnect through Link before syncing can resume.
		code := hook.WebhookCode
		status := "login_required"
		if hook.WebhookCode == "USER_PERMISSION_REVOKED" {
			status = "revoked"
		}
		if err := s.Queries.SetItemStatus(r.Context(), dbgen.SetItemStatusParams{
			ID: item.ID, Status: status, ErrorCode: &code,
		}); err != nil {
			slog.Error("set item status from webhook", "error", err, "item_id", item.ID)
		}

	case "TRANSACTIONS_REMOVED":
		jobs.EnqueueSync(r.Context(), s.Jobs, item.ID)
	}

	w.WriteHeader(http.StatusOK)
}
