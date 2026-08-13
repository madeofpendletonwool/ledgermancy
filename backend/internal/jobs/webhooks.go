package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/crypto"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/webhooks"
)

// Outgoing webhook delivery.
//
// The producers (alert evaluation, insight generation, the goal contribution
// handler) write `webhook_messages` rows and then enqueue one of these per
// message. The row is durable before the job exists, so everything in here is
// free to fail: the worst a failed delivery costs is a retry, and the worst a
// lost JOB costs is a delay until DeliverWebhooksSweepArgs finds the message
// still pending. That is the whole reason the queue is not the system of record.

// WebhookDeps carries what the delivery worker needs, in the same shape as
// BackupDeps and for the same reason: NewWorkerClient's positional argument list
// is already long enough that adding a switch and a cipher to it would be a
// worse trade than one more named struct.
type WebhookDeps struct {
	Cfg config.WebhooksConfig
	// Cipher opens each subscription's signing secret. Required whenever
	// Cfg.Enabled — the worker is not registered without it, so a wiring mistake
	// leaves messages queued rather than delivering them unsigned.
	Cipher *crypto.Cipher
}

// DeliverWebhookArgs delivers one webhook_messages row.
//
// It carries the message id and nothing else. The URL, the secret and the
// enabled flag are read at delivery time, so a user who fixes a typo in their
// receiver's address does not have to wait out a backlog addressed to the old
// one; the payload is the opposite and was frozen when the event happened.
type DeliverWebhookArgs struct {
	MessageID uuid.UUID `json:"message_id"`
}

func (DeliverWebhookArgs) Kind() string { return "webhook_deliver" }

// InsertOpts pins the attempt budget to webhooks.MaxAttempts rather than
// inheriting the client's default of 5, so the retry policy lives with the
// feature and moving it does not silently move every other job's.
//
// Uniqueness collapses a duplicate enqueue for the same message — which the
// sweep will produce whenever it overlaps a delivery that is already queued —
// into one job. ByState starts from River's required set (see SyncItemArgs for
// what happens when it does not) and adds retryable, so a message that is
// currently backing off is not re-enqueued alongside itself.
func (a DeliverWebhookArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueDefault,
		MaxAttempts: webhooks.MaxAttempts,
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByState: append(rivertype.UniqueOptsByStateDefault(), rivertype.JobStateRetryable),
		},
	}
}

// DeliverWebhookWorker performs one HTTP delivery and records what happened.
type DeliverWebhookWorker struct {
	river.WorkerDefaults[DeliverWebhookArgs]
	Queries *dbgen.Queries
	// Cipher opens the subscription's signing secret. Required — a worker
	// without one cannot sign, and an unsigned delivery is not a degraded
	// delivery, it is a different (worthless) product.
	Cipher *crypto.Cipher
	// HTTP is the delivery client. Nil falls back to webhooks.NewClient(), which
	// is what production uses; tests inject one pointed at an httptest server.
	HTTP *http.Client
}

// NextRetry hands River the feature's own backoff instead of the client-wide
// default policy. See webhooks.RetryDelay for the shape and why it has no
// jitter.
func (w *DeliverWebhookWorker) NextRetry(job *river.Job[DeliverWebhookArgs]) time.Time {
	return time.Now().Add(webhooks.RetryDelay(job.Attempt))
}

func (w *DeliverWebhookWorker) Work(ctx context.Context, job *river.Job[DeliverWebhookArgs]) error {
	msg, err := w.Queries.GetWebhookMessageForDelivery(ctx, job.Args.MessageID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The subscription was deleted (cascading the message away) between
		// enqueue and delivery. Nothing to deliver and nothing to fix — return
		// nil so the job completes rather than retrying against a row that will
		// never come back.
		return nil
	}
	if err != nil {
		return fmt.Errorf("load webhook message %s: %w", job.Args.MessageID, err)
	}

	// Terminal already. The sweep and a retry can race here; whichever arrives
	// second finds the work done.
	if msg.Status != "pending" {
		return nil
	}

	// Paused between enqueue and delivery. Not an error and not a dead letter:
	// the message stays pending, and re-enabling the webhook lets the sweep pick
	// up everything that accumulated. That is the difference between `active`
	// and deleting the row, and it is why `active` exists.
	if !msg.Active {
		return nil
	}

	secret, err := w.Cipher.OpenString(msg.SecretEncrypted)
	if err != nil {
		// A secret that will not open is a key change, not a transient fault.
		// Retrying cannot help, so this dead-letters immediately with a message
		// that names the actual problem.
		w.deadLetter(ctx, msg.ID, "the signing secret could not be decrypted; rotate it to send this webhook again")
		return nil
	}

	// Claim the attempt number BEFORE the request, so a worker killed
	// mid-request still leaves the count honest.
	attemptNo, err := w.Queries.BeginWebhookAttempt(ctx, msg.ID)
	if err != nil {
		return fmt.Errorf("begin webhook attempt for %s: %w", msg.ID, err)
	}

	client := w.HTTP
	if client == nil {
		client = webhooks.NewClient()
	}

	attempt, err := webhooks.Send(ctx, client, webhooks.Delivery{
		URL:       msg.Url,
		Secret:    secret,
		MessageID: msg.ID,
		Trigger:   msg.TriggerType,
		Attempt:   int(attemptNo),
		Body:      msg.Payload,
	}, time.Now())
	if err != nil {
		// Only a request that could not even be BUILT reaches here — a URL that
		// passed validation at creation and has since become unparseable. There
		// is nothing to retry.
		w.deadLetter(ctx, msg.ID, err.Error())
		return nil
	}

	w.recordAttempt(ctx, msg.ID, attemptNo, attempt)

	if attempt.OK {
		if err := w.Queries.MarkWebhookMessageSent(ctx, msg.ID); err != nil {
			return fmt.Errorf("mark webhook message %s sent: %w", msg.ID, err)
		}
		return nil
	}

	// Out of attempts: dead-letter now, while we still hold the reason. Waiting
	// for River to discard the job would leave the message pending forever, and
	// the sweep would pick it up and start the whole budget again.
	if job.Attempt >= job.MaxAttempts {
		w.deadLetter(ctx, msg.ID, attempt.Err)
		// Returning the error is what River records as a discard rather than a
		// success, which keeps the job history honest about why it stopped.
		return fmt.Errorf("webhook %s gave up after %d attempts: %s", msg.WebhookID, job.Attempt, attempt.Err)
	}

	// Transient: returning an error is what schedules the retry, at NextRetry's
	// backoff.
	return fmt.Errorf("deliver webhook message %s: %s", msg.ID, attempt.Err)
}

// recordAttempt writes the delivery record. A failure to write it must not fail
// the delivery — the attempt already happened, and losing the audit line is
// strictly better than re-POSTing the household's data to make the audit line
// possible.
func (w *DeliverWebhookWorker) recordAttempt(ctx context.Context, messageID uuid.UUID, attemptNo int32, a webhooks.Attempt) {
	requestHeaders, err := json.Marshal(a.RequestHeaders)
	if err != nil {
		requestHeaders = []byte("{}")
	}
	responseHeaders := []byte("{}")
	if a.ResponseHeaders != nil {
		if encoded, err := json.Marshal(a.ResponseHeaders); err == nil {
			responseHeaders = encoded
		}
	}

	var errPtr, bodyPtr *string
	if a.Err != "" {
		errPtr = &a.Err
	}
	if a.ResponseBody != "" {
		bodyPtr = &a.ResponseBody
	}

	if _, err := w.Queries.RecordWebhookAttempt(ctx, dbgen.RecordWebhookAttemptParams{
		MessageID:       messageID,
		Attempt:         attemptNo,
		RequestHeaders:  requestHeaders,
		RequestBody:     a.RequestBody,
		ResponseStatus:  a.ResponseStatus,
		ResponseHeaders: responseHeaders,
		ResponseBody:    bodyPtr,
		Error:           errPtr,
		DurationMs:      a.DurationMS,
	}); err != nil {
		slog.Error("record webhook attempt", "error", err, "message_id", messageID, "attempt", attemptNo)
	}
}

// deadLetter marks a message failed. Detached from the request context for the
// same reason the audit log is: the reason a delivery was given up on is
// precisely what a cancelled context would lose.
func (w *DeliverWebhookWorker) deadLetter(ctx context.Context, messageID uuid.UUID, reason string) {
	if err := w.Queries.MarkWebhookMessageFailed(context.WithoutCancel(ctx), dbgen.MarkWebhookMessageFailedParams{
		ID:        messageID,
		LastError: &reason,
	}); err != nil {
		slog.Error("dead-letter webhook message", "error", err, "message_id", messageID)
	}
}

// webhookSweepMinAge is how old a pending message must be before the sweep
// re-enqueues it. Comfortably longer than the longest retry backoff
// (webhooks.RetryDelay caps at an hour), so the sweep only ever picks up
// messages whose job is genuinely gone rather than racing one that is waiting.
const webhookSweepMinAge = 90 * time.Minute

// webhookSweepBatch bounds one sweep. A backlog larger than this is drained over
// several ticks rather than in one burst at whatever receiver is having a bad
// day.
const webhookSweepBatch = 200

// webhookMessageRetention is how long delivery history is kept.
//
// Thirty days. This table is a debugging aid — "why didn't my webhook fire?" —
// and the useful life of that answer is days, not years. An instance firing a
// few hundred events a day would otherwise grow it without bound, and it rides
// along in every pg_dump.
const webhookMessageRetention = 30 * 24 * time.Hour

// DeliverWebhooksSweepArgs re-enqueues stranded messages and collects old
// history.
//
// The re-enqueue half is what makes "a failed delivery never loses the event"
// true end to end. Everything up to the message row is transactional, but the
// hand-off to the queue is not: a process killed between the INSERT and the
// job insert, or a River insert that errored, leaves a message nobody is coming
// back for. This is who comes back for it.
type DeliverWebhooksSweepArgs struct{}

func (DeliverWebhooksSweepArgs) Kind() string { return "webhook_sweep" }

// DeliverWebhooksSweepWorker runs the sweep.
type DeliverWebhooksSweepWorker struct {
	river.WorkerDefaults[DeliverWebhooksSweepArgs]
	Queries *dbgen.Queries
	Client  *river.Client[pgx.Tx]
}

func (w *DeliverWebhooksSweepWorker) Work(ctx context.Context, job *river.Job[DeliverWebhooksSweepArgs]) error {
	now := time.Now()

	stranded, err := w.Queries.ListPendingWebhookMessages(ctx, dbgen.ListPendingWebhookMessagesParams{
		CreatedAt: now.Add(-webhookSweepMinAge),
		Limit:     webhookSweepBatch,
	})
	if err != nil {
		return fmt.Errorf("list pending webhook messages: %w", err)
	}
	for _, id := range stranded {
		if _, err := w.Client.Insert(ctx, DeliverWebhookArgs{MessageID: id}, nil); err != nil {
			slog.Error("re-enqueue stranded webhook message", "error", err, "message_id", id)
		}
	}
	if len(stranded) > 0 {
		slog.Info("webhook messages re-enqueued", "count", len(stranded))
	}

	// Retention runs in the same job rather than its own: both are housekeeping
	// over one table on the same cadence, and a second periodic job would only
	// be a second thing to register and reason about.
	collected, err := w.Queries.DeleteOldWebhookMessages(ctx, now.Add(-webhookMessageRetention))
	if err != nil {
		return fmt.Errorf("collect old webhook messages: %w", err)
	}
	if collected > 0 {
		slog.Info("webhook delivery history collected", "messages", collected)
	}
	return nil
}

// EnqueueWebhookDeliveries queues a delivery for each message a producer just
// wrote.
//
// Fire-and-forget by design. Every message is already durable, so a failure to
// enqueue costs a delay until the sweep finds it and nothing more — which is
// exactly why producers may call this without handling an error and without
// caring whether a queue is wired at all.
func EnqueueWebhookDeliveries(ctx context.Context, client *river.Client[pgx.Tx], messageIDs []uuid.UUID) {
	if client == nil {
		return
	}
	for _, id := range messageIDs {
		if _, err := client.Insert(ctx, DeliverWebhookArgs{MessageID: id}, nil); err != nil {
			slog.Error("enqueue webhook delivery", "error", err, "message_id", id)
		}
	}
}
