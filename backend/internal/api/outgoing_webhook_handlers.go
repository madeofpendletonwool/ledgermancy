package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/jobs"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/webhooks"
)

// Outgoing webhooks: the CRUD and the delivery inspector.
//
// The file is named for the direction because webhook_handlers.go was already
// taken by the INCOMING one — Plaid's push notifications — and the two have
// nothing in common beyond the word. That one is unauthenticated and treats its
// payload as an untrusted hint; everything here is behind a session and treats
// the household's data as something to be careful about sending out.
//
// What is not in this file: scoping (it lives in the SQL, see
// internal/db/queries/webhooks.sql), signing and delivery (internal/webhooks),
// and the retry policy (internal/jobs). A handler here writes a row and, at
// most, asks the queue to do something.

// maxWebhookNameLen bounds the one free-text field, the same way
// maxAPITokenNameLen does and for the same reason.
const maxWebhookNameLen = 100

// webhookMessagePageSize is how much delivery history the inspector reads.
//
// Fixed rather than a caller-supplied limit: the inspector is a debugging panel
// for one subscription, fifty rows is more than anybody scrolls, and the older
// history is being collected on a thirty-day retention anyway.
const webhookMessagePageSize = 50

// webhookResponse is a subscription as the settings page sees it. There is no
// secret here and no way to ask for one — after creation the plaintext is gone,
// exactly as it is for a personal API token.
type webhookResponse struct {
	ID        uuid.UUID `json:"id"`
	UserID    uuid.UUID `json:"user_id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Active    bool      `json:"active"`
	Triggers  []string  `json:"triggers"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// createdWebhookResponse is the ONE response that carries the signing secret.
//
// Returned by POST and by a rotate, never stored in the clear, and never
// readable again — the row holds it sealed and nothing above the delivery worker
// opens it. Same shape and same reasoning as createdAPITokenResponse.
type createdWebhookResponse struct {
	webhookResponse
	Secret string `json:"secret"`
}

type webhookMessageResponse struct {
	ID          uuid.UUID       `json:"id"`
	WebhookID   uuid.UUID       `json:"webhook_id"`
	Trigger     string          `json:"trigger"`
	Payload     json.RawMessage `json:"payload"`
	Status      string          `json:"status"`
	Attempts    int32           `json:"attempts"`
	DeliveredAt *time.Time      `json:"delivered_at"`
	LastError   *string         `json:"last_error"`
	CreatedAt   time.Time       `json:"created_at"`
}

type webhookAttemptResponse struct {
	ID              uuid.UUID       `json:"id"`
	Attempt         int32           `json:"attempt"`
	RequestHeaders  json.RawMessage `json:"request_headers"`
	RequestBody     string          `json:"request_body"`
	ResponseStatus  *int32          `json:"response_status"`
	ResponseHeaders json.RawMessage `json:"response_headers"`
	ResponseBody    *string         `json:"response_body"`
	Error           *string         `json:"error"`
	DurationMS      int32           `json:"duration_ms"`
	CreatedAt       time.Time       `json:"created_at"`
}

// webhooksAvailable gates every route in this file.
//
// 503 rather than 404, matching the Plaid and document handlers: the route
// exists, the feature is simply switched off on this instance, and saying so is
// what stops an operator hunting for a typo in their frontend. The message names
// the variable, because the next question is always "how do I turn it on".
func (s *Server) webhooksAvailable(w http.ResponseWriter) bool {
	if !s.Config.Webhooks.Enabled {
		writeError(w, http.StatusServiceUnavailable,
			"outgoing webhooks are not enabled on this instance (set WEBHOOKS_ENABLED=true)")
		return false
	}
	if s.Cipher == nil {
		// Cannot happen in a booted app — the cipher is required at startup —
		// but a handler that would otherwise nil-panic while sealing a secret is
		// worth one branch.
		writeError(w, http.StatusServiceUnavailable, "encryption is not configured")
		return false
	}
	return true
}

// handleListWebhookTriggers publishes the trigger vocabulary so the settings UI
// offers exactly what the backend understands, rather than a hardcoded list that
// drifts the first time one is added.
func (s *Server) handleListWebhookTriggers(w http.ResponseWriter, r *http.Request) {
	if !s.webhooksAvailable(w) {
		return
	}
	writeJSON(w, http.StatusOK, webhooks.Triggers)
}

func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	if !s.webhooksAvailable(w) {
		return
	}
	identity := auth.MustFromContext(r.Context())

	rows, err := s.Queries.ListWebhooks(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "list webhooks", err)
		return
	}

	out := make([]webhookResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, webhookResponse{
			ID:        row.ID,
			UserID:    row.UserID,
			Name:      row.Name,
			URL:       row.Url,
			Active:    row.Active,
			Triggers:  row.Triggers,
			CreatedAt: row.CreatedAt,
			UpdatedAt: row.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type webhookWriteRequest struct {
	Name     string   `json:"name"`
	URL      string   `json:"url"`
	Triggers []string `json:"triggers"`
	// Absent means active. A subscription created switched off would be a
	// subscription somebody spends an afternoon wondering about.
	Active *bool `json:"active"`
}

// validate normalises a create or update body, returning the message to send
// back on the first problem. Shared so a webhook cannot be created with one set
// of rules and edited under another.
func (req webhookWriteRequest) validate() (name, url string, triggers []string, active bool, problem string) {
	name = strings.TrimSpace(req.Name)
	if name == "" {
		return "", "", nil, false, "name is required"
	}
	if len(name) > maxWebhookNameLen {
		return "", "", nil, false, "name is too long"
	}

	url, err := webhooks.ValidateURL(req.URL)
	if err != nil {
		return "", "", nil, false, err.Error()
	}

	triggers, ok := webhooks.NormalizeTriggers(req.Triggers)
	if !ok {
		return "", "", nil, false,
			"triggers must be a non-empty selection from " + strings.Join(webhooks.Triggers, ", ")
	}

	active = true
	if req.Active != nil {
		active = *req.Active
	}
	return name, url, triggers, active, ""
}

func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.webhooksAvailable(w) {
		return
	}
	identity := auth.MustFromContext(r.Context())

	var req webhookWriteRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name, url, triggers, active, problem := req.validate()
	if problem != "" {
		writeError(w, http.StatusBadRequest, problem)
		return
	}

	secret, err := webhooks.NewSecret()
	if err != nil {
		s.internalError(w, "generate webhook secret", err)
		return
	}
	sealed, err := s.Cipher.SealString(secret)
	if err != nil {
		s.internalError(w, "seal webhook secret", err)
		return
	}

	// user_id is the caller, not a field of the request. It is what the delivery
	// filter uses to decide which alert events this subscription may receive, so
	// letting a body choose it would be letting a body choose whose private
	// accounts it can see.
	row, err := s.Queries.CreateWebhook(r.Context(), dbgen.CreateWebhookParams{
		HouseholdID:     identity.HouseholdID,
		UserID:          identity.UserID,
		Name:            name,
		Url:             url,
		SecretEncrypted: sealed,
		Active:          active,
		Triggers:        triggers,
	})
	if err != nil {
		s.internalError(w, "create webhook", err)
		return
	}

	// The URL, not the secret. An audit log is read by whoever can already read
	// the database, but it is also the thing most likely to be pasted into a bug
	// report — and where a household's events are being sent is exactly the fact
	// worth having in it.
	s.audit(r.Context(), r, identity.UserID, "", eventWebhookCreated,
		map[string]any{"webhook_id": row.ID.String(), "name": name, "url": url, "triggers": triggers})

	writeJSON(w, http.StatusCreated, createdWebhookResponse{
		webhookResponse: toWebhookResponse(row.ID, row.UserID, row.Name, row.Url, row.Active, row.Triggers, row.CreatedAt, row.UpdatedAt),
		Secret:          secret,
	})
}

func (s *Server) handleUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.webhooksAvailable(w) {
		return
	}
	identity := auth.MustFromContext(r.Context())

	webhookID, err := uuid.Parse(chi.URLParam(r, "webhookID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid webhook id")
		return
	}

	var req webhookWriteRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	name, url, triggers, active, problem := req.validate()
	if problem != "" {
		writeError(w, http.StatusBadRequest, problem)
		return
	}

	row, err := s.Queries.UpdateWebhook(r.Context(), dbgen.UpdateWebhookParams{
		ID:          webhookID,
		HouseholdID: identity.HouseholdID,
		Name:        name,
		Url:         url,
		Active:      active,
		Triggers:    triggers,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "webhook not found")
		return
	}
	if err != nil {
		s.internalError(w, "update webhook", err)
		return
	}

	// user_id is deliberately NOT updated to the editor. A webhook keeps the
	// visibility of whoever created it: letting an edit re-anchor it would mean
	// renaming a partner's webhook silently widened what it can see.
	s.audit(r.Context(), r, identity.UserID, "", eventWebhookUpdated,
		map[string]any{"webhook_id": row.ID.String(), "name": name, "url": url, "triggers": triggers, "active": active})

	writeJSON(w, http.StatusOK,
		toWebhookResponse(row.ID, row.UserID, row.Name, row.Url, row.Active, row.Triggers, row.CreatedAt, row.UpdatedAt))
}

// handleRotateWebhookSecret mints a new signing secret and returns it once.
//
// A deliberate, separate action rather than a field on the update: rotating
// breaks every receiver holding the old secret at the moment it lands, and
// burying that inside "I renamed my webhook" would be a trap. Messages already
// queued are signed with whatever is current when they are DELIVERED, so a
// rotation applies to the backlog too — which is the correct behaviour for a
// secret somebody is rotating because it leaked.
func (s *Server) handleRotateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	if !s.webhooksAvailable(w) {
		return
	}
	identity := auth.MustFromContext(r.Context())

	webhookID, err := uuid.Parse(chi.URLParam(r, "webhookID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid webhook id")
		return
	}

	secret, err := webhooks.NewSecret()
	if err != nil {
		s.internalError(w, "generate webhook secret", err)
		return
	}
	sealed, err := s.Cipher.SealString(secret)
	if err != nil {
		s.internalError(w, "seal webhook secret", err)
		return
	}

	affected, err := s.Queries.RotateWebhookSecret(r.Context(), dbgen.RotateWebhookSecretParams{
		ID: webhookID, HouseholdID: identity.HouseholdID, SecretEncrypted: sealed,
	})
	if err != nil {
		s.internalError(w, "rotate webhook secret", err)
		return
	}
	if affected == 0 {
		writeError(w, http.StatusNotFound, "webhook not found")
		return
	}

	s.audit(r.Context(), r, identity.UserID, "", eventWebhookSecretRotated,
		map[string]any{"webhook_id": webhookID.String()})

	writeJSON(w, http.StatusOK, map[string]string{"secret": secret})
}

func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.webhooksAvailable(w) {
		return
	}
	identity := auth.MustFromContext(r.Context())

	webhookID, err := uuid.Parse(chi.URLParam(r, "webhookID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid webhook id")
		return
	}

	// household_id is in the DELETE predicate, so a caller cannot delete another
	// household's subscription even with a valid id.
	affected, err := s.Queries.DeleteWebhook(r.Context(), dbgen.DeleteWebhookParams{
		ID: webhookID, HouseholdID: identity.HouseholdID,
	})
	if err != nil {
		s.internalError(w, "delete webhook", err)
		return
	}
	if affected == 0 {
		writeError(w, http.StatusNotFound, "webhook not found")
		return
	}

	// Deletion takes the delivery history with it by cascade, and any queued
	// delivery job finds no row and completes quietly (see
	// DeliverWebhookWorker).
	s.audit(r.Context(), r, identity.UserID, "", eventWebhookDeleted,
		map[string]any{"webhook_id": webhookID.String()})
	w.WriteHeader(http.StatusNoContent)
}

// requireWebhookInHousehold resolves a path id to a subscription the caller may
// act on, or writes the response and reports false. Every messages/attempts
// route goes through it, so the household predicate is applied once rather than
// remembered at each site.
func (s *Server) requireWebhookInHousehold(w http.ResponseWriter, r *http.Request) (dbgen.GetWebhookRow, bool) {
	identity := auth.MustFromContext(r.Context())

	webhookID, err := uuid.Parse(chi.URLParam(r, "webhookID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid webhook id")
		return dbgen.GetWebhookRow{}, false
	}

	row, err := s.Queries.GetWebhook(r.Context(), dbgen.GetWebhookParams{
		ID: webhookID, HouseholdID: identity.HouseholdID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "webhook not found")
		return dbgen.GetWebhookRow{}, false
	}
	if err != nil {
		s.internalError(w, "load webhook", err)
		return dbgen.GetWebhookRow{}, false
	}
	return row, true
}

func (s *Server) handleListWebhookMessages(w http.ResponseWriter, r *http.Request) {
	if !s.webhooksAvailable(w) {
		return
	}
	hook, ok := s.requireWebhookInHousehold(w, r)
	if !ok {
		return
	}

	rows, err := s.Queries.ListWebhookMessages(r.Context(), dbgen.ListWebhookMessagesParams{
		WebhookID: hook.ID, Limit: webhookMessagePageSize,
	})
	if err != nil {
		s.internalError(w, "list webhook messages", err)
		return
	}

	out := make([]webhookMessageResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, webhookMessageResponse{
			ID:        row.ID,
			WebhookID: row.WebhookID,
			Trigger:   row.TriggerType,
			// The payload goes back as JSON rather than a string. It is the exact
			// body that was sent, and a receiver author comparing it to what they
			// got should not have to unescape it first.
			Payload:     row.Payload,
			Status:      row.Status,
			Attempts:    row.Attempts,
			DeliveredAt: row.DeliveredAt,
			LastError:   row.LastError,
			CreatedAt:   row.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListWebhookAttempts(w http.ResponseWriter, r *http.Request) {
	if !s.webhooksAvailable(w) {
		return
	}
	hook, ok := s.requireWebhookInHousehold(w, r)
	if !ok {
		return
	}

	messageID, err := uuid.Parse(chi.URLParam(r, "messageID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid message id")
		return
	}

	// webhook_id is in the query's predicate as well as the guard above, so a
	// message id belonging to another subscription reads as empty rather than
	// leaking through a webhook the caller does happen to own.
	rows, err := s.Queries.ListWebhookAttempts(r.Context(), dbgen.ListWebhookAttemptsParams{
		MessageID: messageID, WebhookID: hook.ID,
	})
	if err != nil {
		s.internalError(w, "list webhook attempts", err)
		return
	}

	out := make([]webhookAttemptResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, webhookAttemptResponse{
			ID:              row.ID,
			Attempt:         row.Attempt,
			RequestHeaders:  row.RequestHeaders,
			RequestBody:     row.RequestBody,
			ResponseStatus:  row.ResponseStatus,
			ResponseHeaders: row.ResponseHeaders,
			ResponseBody:    row.ResponseBody,
			Error:           row.Error,
			DurationMS:      row.DurationMs,
			CreatedAt:       row.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleTestWebhook sends a test delivery.
//
// It writes the message row and queues the delivery, and answers with the
// message id — it does NOT wait for the receiver. That is deliberate: a test
// that blocked on the receiver would give a different answer from a real
// delivery (no retries, a different timeout, a different code path), and the
// question a user is actually asking is "does the thing that will really run
// work". The UI polls the message list, which shows exactly what a real event
// would have shown.
func (s *Server) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.webhooksAvailable(w) {
		return
	}
	identity := auth.MustFromContext(r.Context())

	hook, ok := s.requireWebhookInHousehold(w, r)
	if !ok {
		return
	}

	messageID, err := webhooks.EmitTest(r.Context(), s.Queries, identity.HouseholdID, hook.ID, time.Now())
	if err != nil {
		s.internalError(w, "enqueue webhook test", err)
		return
	}

	// Unlike the fire-and-forget producers, this one reports a missing queue: the
	// user is standing in front of the result, and "queued" when nothing will
	// ever run it is worse than an error.
	if s.Jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "background jobs are not available")
		return
	}
	jobs.EnqueueWebhookDeliveries(r.Context(), s.Jobs, []uuid.UUID{messageID})

	writeJSON(w, http.StatusAccepted, map[string]string{"message_id": messageID.String()})
}

// toWebhookResponse keeps the four places that build one from a generated row
// honest about the field order and the URL/Url spelling difference.
func toWebhookResponse(
	id, userID uuid.UUID,
	name, url string,
	active bool,
	triggers []string,
	createdAt, updatedAt time.Time,
) webhookResponse {
	return webhookResponse{
		ID:        id,
		UserID:    userID,
		Name:      name,
		URL:       url,
		Active:    active,
		Triggers:  triggers,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
}
