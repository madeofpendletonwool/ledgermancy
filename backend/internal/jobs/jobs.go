// Package jobs defines Ledgermancy's background work and the River queue that
// runs it. Job arguments live here so the api (which enqueues) and the worker
// (which executes) share one definition.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"

	"github.com/apex42group/ledgermancy/backend/internal/ai"
	"github.com/apex42group/ledgermancy/backend/internal/alerts"
	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/categorize"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
	"github.com/apex42group/ledgermancy/backend/internal/insights"
	"github.com/apex42group/ledgermancy/backend/internal/networth"
	"github.com/apex42group/ledgermancy/backend/internal/notify"
	"github.com/apex42group/ledgermancy/backend/internal/plaid"
)

// QueueDefault is the single queue everything runs on. Volume for a household
// ledger is low enough that separate queues would only add moving parts.
const QueueDefault = "default"

// SyncItemArgs requests a Plaid sync for one item. On an item that has never
// synced this performs the full historical backfill.
type SyncItemArgs struct {
	ItemID uuid.UUID `json:"item_id"`
}

func (SyncItemArgs) Kind() string { return "plaid_sync" }

// InsertOpts makes the job idempotent per item: if a sync for this item is
// already queued or running, a duplicate request is dropped rather than
// running two overlapping syncs against the same cursor.
//
// ByState must include River's four required states (available, pending,
// running, scheduled) or every insert is rejected outright — so this starts
// from the library's default set and adds `retryable`, which keeps a failing
// item from stacking up duplicate syncs while it backs off.
func (a SyncItemArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: QueueDefault,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByState:  append(rivertype.UniqueOptsByStateDefault(), rivertype.JobStateRetryable),
			ByPeriod: time.Minute,
		},
	}
}

// SyncItemWorker executes a Plaid sync.
type SyncItemWorker struct {
	river.WorkerDefaults[SyncItemArgs]
	Syncer *plaid.Syncer
	// Client and Queries let a finished sync kick off follow-up work for the
	// item's household — alert evaluation always, and AI categorisation when
	// EnqueueLLM is set. The periodic sweeps are the backstop if these are nil.
	Client     *river.Client[pgx.Tx]
	Queries    *dbgen.Queries
	EnqueueLLM bool
}

func (w *SyncItemWorker) Work(ctx context.Context, job *river.Job[SyncItemArgs]) error {
	start := time.Now()

	result, err := w.Syncer.SyncItem(ctx, job.Args.ItemID)
	if err != nil {
		// Returning the error lets River retry with exponential backoff. An
		// item needing re-authentication has already been marked
		// login_required by the syncer, so retries stop being useful — but
		// they are cheap and self-correct once the user reconnects.
		return fmt.Errorf("sync item %s: %w", job.Args.ItemID, err)
	}

	slog.Info("plaid sync complete",
		"item_id", result.ItemID,
		"pages", result.Pages,
		"added", result.Added,
		"modified", result.Modified,
		"removed", result.Removed,
		"accounts", result.AccountsUpserted,
		"duration_ms", time.Since(start).Milliseconds(),
	)

	// Kick off follow-up work for this household. Failures here never fail the
	// sync — the periodic sweeps are a backstop.
	if w.Client != nil && w.Queries != nil {
		householdID, err := w.Queries.GetHouseholdForItem(ctx, job.Args.ItemID)
		if err != nil {
			slog.Warn("resolve household for post-sync jobs", "error", err, "item_id", job.Args.ItemID)
			return nil
		}
		if _, err := w.Client.Insert(ctx, EvaluateAlertsArgs{HouseholdID: householdID}, nil); err != nil {
			slog.Error("enqueue alert evaluation", "error", err, "household_id", householdID)
		}
		// Insight generation is deterministic (phrasing is separately AI-gated
		// inside the engine), so a fresh sync always refreshes the feed.
		if _, err := w.Client.Insert(ctx, GenerateInsightsArgs{HouseholdID: householdID}, nil); err != nil {
			slog.Error("enqueue insight generation", "error", err, "household_id", householdID)
		}
		if w.EnqueueLLM {
			if _, err := w.Client.Insert(ctx, LLMCategoriseArgs{HouseholdID: householdID}, nil); err != nil {
				slog.Error("enqueue llm categorise", "error", err, "household_id", householdID)
			}
		}
	}
	return nil
}

// Timeout allows a long initial backfill: two years of history across several
// accounts can take well over the default.
func (w *SyncItemWorker) Timeout(*river.Job[SyncItemArgs]) time.Duration {
	return 10 * time.Minute
}

// SyncAllArgs sweeps every active item that is due for a refresh.
type SyncAllArgs struct{}

func (SyncAllArgs) Kind() string { return "plaid_sync_all" }

// SyncAllWorker asks Plaid to refresh what is due, then enqueues a per-item
// sync for everything due.
type SyncAllWorker struct {
	river.WorkerDefaults[SyncAllArgs]
	Pool   *pgxpool.Pool
	Client *river.Client[pgx.Tx]
	// Syncer performs the refresh. Nil when Plaid is not configured, in which
	// case there is nothing to sweep at all.
	Syncer *plaid.Syncer
	// StaleAfter is how old an item's last sync must be before it is refreshed.
	StaleAfter time.Duration
	// RefreshAfter is how long to wait between asking Plaid to pull fresh data
	// for the same item. Much longer than StaleAfter: reading Plaid's cache is
	// cheap, but making Plaid go to the bank is rate limited per item.
	RefreshAfter time.Duration
}

// dueItemsQuery selects the items a sweep should act on, and reports which of
// them additionally need a bank pull.
//
// $1 is the sync cutoff, $2 the refresh cutoff. The two schedules are
// independent, so an item qualifies on *either*. Gating the refresh behind sync
// staleness instead would starve it: a webhook-driven sync bumps
// last_synced_at, so a busy item can stay perpetually sync-fresh and never
// become eligible for the bank pull it is long overdue for — which is precisely
// the stall this job exists to break.
//
// Every row returned is synced, so only the refresh needs its own flag.
// TestDueItemsQuery pins both halves.
const dueItemsQuery = `
	SELECT id, (last_refresh_at IS NULL OR last_refresh_at < $2) AS refresh_due
	FROM plaid_items
	WHERE status = 'active'
	  AND ((last_synced_at  IS NULL OR last_synced_at  < $1)
	    OR (last_refresh_at IS NULL OR last_refresh_at < $2))`

func (w *SyncAllWorker) Work(ctx context.Context, job *river.Job[SyncAllArgs]) error {
	now := time.Now()

	rows, err := w.Pool.Query(ctx, dueItemsQuery,
		now.Add(-w.StaleAfter), now.Add(-w.RefreshAfter))
	if err != nil {
		return fmt.Errorf("list due items: %w", err)
	}
	defer rows.Close()

	type dueItem struct {
		id             uuid.UUID
		refreshOverdue bool
	}
	var items []dueItem
	for rows.Next() {
		var it dueItem
		if err := rows.Scan(&it.id, &it.refreshOverdue); err != nil {
			return fmt.Errorf("scan item id: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	refreshed, synced := 0, 0
	for _, it := range items {
		// Ask Plaid to go to the bank first. It answers immediately and pulls
		// asynchronously, so the sync enqueued below will usually still read
		// the old cache — that is fine and deliberate. The pull lands moments
		// later as a SYNC_UPDATES_AVAILABLE webhook, which enqueues the sync
		// that actually stores the new rows.
		if it.refreshOverdue && w.Syncer != nil {
			if err := w.Syncer.RefreshItem(ctx, it.id); err != nil {
				// A refresh failing must not stop the sync: the cached data is
				// still worth reading, and the item may simply be rate limited.
				slog.Warn("refresh item", "error", err, "item_id", it.id)
			} else {
				refreshed++
			}
		}

		// Synced whenever the item was touched at all, including when only the
		// refresh was due. The webhook is the expected path for picking up what
		// the refresh produces, so this is the fallback for when it never
		// arrives — without it a broken webhook would leave freshly pulled data
		// sitting at Plaid unread. River's per-item uniqueness collapses the
		// duplicate when the webhook does arrive.
		if _, err := w.Client.Insert(ctx, SyncItemArgs{ItemID: it.id}, nil); err != nil {
			slog.Error("enqueue sync", "error", err, "item_id", it.id)
		} else {
			synced++
		}
	}

	slog.Info("scheduled syncs", "items", len(items), "synced", synced, "refreshed", refreshed)
	return nil
}

// Migrate applies River's own schema. It is separate from the application
// migrations because River owns and versions those tables itself.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("create river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("run river migrations: %w", err)
	}
	return nil
}

// SnapshotNetWorthArgs records every household's net worth for today.
type SnapshotNetWorthArgs struct{}

func (SnapshotNetWorthArgs) Kind() string { return "net_worth_snapshot" }

// SnapshotNetWorthWorker writes the daily net-worth row.
//
// This runs on a schedule as well as after each sync, because a household
// whose institutions are all quiet still needs a point on the trend line —
// otherwise the chart would show gaps wherever nothing happened to sync.
type SnapshotNetWorthWorker struct {
	river.WorkerDefaults[SnapshotNetWorthArgs]
	Queries *dbgen.Queries
}

func (w *SnapshotNetWorthWorker) Work(ctx context.Context, job *river.Job[SnapshotNetWorthArgs]) error {
	n, err := networth.SnapshotAll(ctx, w.Queries)
	if err != nil {
		return fmt.Errorf("snapshot net worth: %w", err)
	}
	slog.Info("net worth snapshots written", "households", n)
	return nil
}

// LLMCategoriseArgs runs the AI categorisation fallback for one household: the
// step-5 pass over transactions the deterministic resolver left uncategorised.
type LLMCategoriseArgs struct {
	HouseholdID uuid.UUID `json:"household_id"`
}

func (LLMCategoriseArgs) Kind() string { return "llm_categorise" }

// InsertOpts collapses a burst of enqueues for one household — several items
// finishing a sync at once, plus the sweep — into a single pass per minute.
// Starts from River's required state set (see SyncItemArgs) so the insert is
// not silently rejected.
func (a LLMCategoriseArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: QueueDefault,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByState:  append(rivertype.UniqueOptsByStateDefault(), rivertype.JobStateRetryable),
			ByPeriod: time.Minute,
		},
	}
}

// LLMCategoriseWorker asks the model to place uncategorised merchants.
type LLMCategoriseWorker struct {
	river.WorkerDefaults[LLMCategoriseArgs]
	Queries *dbgen.Queries
	AI      *ai.Client
}

func (w *LLMCategoriseWorker) Work(ctx context.Context, job *river.Job[LLMCategoriseArgs]) error {
	n, err := categorize.LLMCategoriseHousehold(ctx, w.Queries, w.AI, job.Args.HouseholdID)
	if err != nil {
		return fmt.Errorf("llm categorise household %s: %w", job.Args.HouseholdID, err)
	}
	if n > 0 {
		slog.Info("llm categorisation complete",
			"household_id", job.Args.HouseholdID, "merchants_decided", n)
	}
	return nil
}

// Timeout bounds a pass that may fan out to several batched model calls.
func (w *LLMCategoriseWorker) Timeout(*river.Job[LLMCategoriseArgs]) time.Duration {
	return 5 * time.Minute
}

// LLMCategoriseAllArgs sweeps every household so a quiet household still gets
// its backlog categorised even when no sync is enqueuing per-household passes.
type LLMCategoriseAllArgs struct{}

func (LLMCategoriseAllArgs) Kind() string { return "llm_categorise_all" }

// LLMCategoriseAllWorker enqueues a per-household categorise pass.
type LLMCategoriseAllWorker struct {
	river.WorkerDefaults[LLMCategoriseAllArgs]
	Queries *dbgen.Queries
	Client  *river.Client[pgx.Tx]
}

func (w *LLMCategoriseAllWorker) Work(ctx context.Context, job *river.Job[LLMCategoriseAllArgs]) error {
	ids, err := w.Queries.ListHouseholdIDs(ctx)
	if err != nil {
		return fmt.Errorf("list households: %w", err)
	}
	for _, id := range ids {
		if _, err := w.Client.Insert(ctx, LLMCategoriseArgs{HouseholdID: id}, nil); err != nil {
			slog.Error("enqueue llm categorise", "error", err, "household_id", id)
		}
	}
	return nil
}

// EvaluateAlertsArgs runs the alert engine for one household. Unlike the AI
// jobs this needs no API key — it is deterministic SQL — so it is always
// registered.
type EvaluateAlertsArgs struct {
	HouseholdID uuid.UUID `json:"household_id"`
}

func (EvaluateAlertsArgs) Kind() string { return "evaluate_alerts" }

// InsertOpts collapses a burst of enqueues (several items syncing, plus the
// sweep) into one evaluation per household per minute. Starts from River's
// required state set so the insert is not silently rejected.
func (a EvaluateAlertsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: QueueDefault,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByState:  append(rivertype.UniqueOptsByStateDefault(), rivertype.JobStateRetryable),
			ByPeriod: time.Minute,
		},
	}
}

// EvaluateAlertsWorker evaluates a household's alert rules and records events,
// then dispatches external pushes for any newly-raised events.
type EvaluateAlertsWorker struct {
	river.WorkerDefaults[EvaluateAlertsArgs]
	Queries *dbgen.Queries
	// Client enqueues the per-user notify jobs. Nil-tolerant: without a queue
	// (which cannot happen in the worker, but keeps the type usable in tests)
	// dispatch is skipped.
	Client *river.Client[pgx.Tx]
	// AppURL is the frontend origin used to build a deep link back to the app.
	AppURL string
}

func (w *EvaluateAlertsWorker) Work(ctx context.Context, job *river.Job[EvaluateAlertsArgs]) error {
	n, err := alerts.Evaluate(ctx, w.Queries, job.Args.HouseholdID, time.Now())
	if err != nil {
		return fmt.Errorf("evaluate alerts for household %s: %w", job.Args.HouseholdID, err)
	}
	if n > 0 {
		slog.Info("alerts raised", "household_id", job.Args.HouseholdID, "events", n)
	}

	// Dispatch pushes for un-notified events. A failure here must not fail
	// evaluation — the events are already stored, and the next sweep retries
	// dispatch (notified_at gates re-sending).
	if err := w.dispatchNotifications(ctx, job.Args.HouseholdID); err != nil {
		slog.Error("dispatch alert notifications", "error", err, "household_id", job.Args.HouseholdID)
	}
	return nil
}

// dispatchNotifications enqueues one notify job per (new event × member who
// wants that kind pushed), then stamps each event notified so an overlapping
// sweep never re-dispatches it. Enqueue-time stamping (rather than send-time)
// keeps event dedupe unambiguous when a household has several members; the
// per-user NotifyArgs additionally carry UniqueOpts as a second guard.
func (w *EvaluateAlertsWorker) dispatchNotifications(ctx context.Context, householdID uuid.UUID) error {
	if w.Client == nil {
		return nil
	}
	events, err := w.Queries.ListUnnotifiedAlertEvents(ctx, householdID)
	if err != nil {
		return fmt.Errorf("list unnotified events: %w", err)
	}
	if len(events) == 0 {
		return nil
	}
	members, err := w.Queries.ListHouseholdMembers(ctx, householdID)
	if err != nil {
		return fmt.Errorf("list members: %w", err)
	}

	for _, ev := range events {
		// The rule decides whether its events go out externally at all. A rule
		// left in-app only still fires and shows in the feed; we just stamp its
		// event notified so this and later sweeps skip it without pushing.
		if !ev.Push {
			if err := w.Queries.MarkAlertEventNotified(ctx, ev.ID); err != nil {
				slog.Error("mark event notified", "error", err, "event_id", ev.ID)
			}
			continue
		}

		var payload map[string]string
		_ = json.Unmarshal(ev.Payload, &payload)
		n := alertNotification(ev.AlertType, payload, w.AppURL)

		for _, m := range members {
			if !w.hasChannel(ctx, m.ID) {
				continue
			}
			args := NotifyArgs{
				UserID:       m.ID,
				AlertEventID: ev.ID,
				Title:        n.Title,
				Body:         n.Body,
				Priority:     n.Priority,
				Tags:         n.Tags,
				ClickURL:     n.ClickURL,
			}
			if _, err := w.Client.Insert(ctx, args, nil); err != nil {
				slog.Error("enqueue notify", "error", err, "user_id", m.ID, "event_id", ev.ID)
			}
		}

		if err := w.Queries.MarkAlertEventNotified(ctx, ev.ID); err != nil {
			slog.Error("mark event notified", "error", err, "event_id", ev.ID)
		}
	}
	return nil
}

// hasChannel reports whether a user has a real delivery channel configured.
// Which alerts push is now the rule's call (alerts.push); this only answers
// "can we reach this member at all".
func (w *EvaluateAlertsWorker) hasChannel(ctx context.Context, userID uuid.UUID) bool {
	return hasNotifyChannel(ctx, w.Queries, userID)
}

// hasNotifyChannel reports whether a user has a real delivery channel configured.
// A read error or unset pref is treated as "no", never blocking a sweep. Shared
// by alert and insight dispatch so both gate on delivery the same way.
func hasNotifyChannel(ctx context.Context, q *dbgen.Queries, userID uuid.UUID) bool {
	channel := stringPref(ctx, q, userID, "notify.channel")
	return channel != "" && channel != "none"
}

// NotifyArgs delivers one pre-formatted notification to one user. Content is
// baked in at enqueue time so the worker never re-queries. AlertEventID is
// carried only to key uniqueness, so overlapping sweeps collapse.
type NotifyArgs struct {
	UserID       uuid.UUID `json:"user_id"`
	AlertEventID uuid.UUID `json:"alert_event_id"`
	Title        string    `json:"title"`
	Body         string    `json:"body"`
	Priority     int       `json:"priority"`
	Tags         []string  `json:"tags,omitempty"`
	ClickURL     string    `json:"click_url,omitempty"`
}

func (NotifyArgs) Kind() string { return "notify" }

// InsertOpts collapses duplicate pushes for the same (event, user) so a
// re-enqueue after a crash between insert and stamping does not double-send.
func (NotifyArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: QueueDefault,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByState:  append(rivertype.UniqueOptsByStateDefault(), rivertype.JobStateRetryable),
			ByPeriod: time.Hour,
		},
	}
}

// NotifyWorker delivers one notification via the Notifier. The Notifier no-ops
// for users without a configured channel, so this worker never has to gate.
type NotifyWorker struct {
	river.WorkerDefaults[NotifyArgs]
	Notifier notify.Notifier
}

func (w *NotifyWorker) Work(ctx context.Context, job *river.Job[NotifyArgs]) error {
	a := job.Args
	if err := w.Notifier.Send(ctx, a.UserID, notify.Notification{
		Title:    a.Title,
		Body:     a.Body,
		Priority: a.Priority,
		Tags:     a.Tags,
		ClickURL: a.ClickURL,
	}); err != nil {
		return fmt.Errorf("send notification to %s: %w", a.UserID, err)
	}
	return nil
}

// stringPref reads a JSON-string user preference, returning "" when unset or
// malformed — a dispatch decision should degrade to "off", never error.
func stringPref(ctx context.Context, q *dbgen.Queries, userID uuid.UUID, key string) string {
	raw, err := q.GetUserPreference(ctx, dbgen.GetUserPreferenceParams{UserID: &userID, Key: key})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("read preference", "error", err, "key", key, "user_id", userID)
		}
		return ""
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// alertNotification renders an alert event into a push. Money in the payload is
// already a fixed-2 decimal string; this only decorates it. Unknown types get a
// generic message rather than being dropped.
func alertNotification(alertType string, p map[string]string, appURL string) notify.Notification {
	click := ""
	if appURL != "" {
		click = strings.TrimRight(appURL, "/") + "/alerts"
	}
	n := notify.Notification{Priority: 3, ClickURL: click}
	switch alertType {
	case alerts.TypeBigSpend:
		n.Title = "Large purchase"
		n.Body = fmt.Sprintf("%s — $%s on %s", p["merchant"], p["amount"], p["date"])
		n.Priority, n.Tags = 4, []string{"dollar"}
	case alerts.TypeUnusualMerchant:
		n.Title = "New merchant"
		n.Body = fmt.Sprintf("First charge from %s: $%s on %s", p["merchant"], p["amount"], p["date"])
		n.Tags = []string{"question"}
	case alerts.TypeBudgetThreshold:
		n.Title = "Budget alert"
		n.Body = fmt.Sprintf("%s is at %s%% — $%s of $%s", p["category_name"], p["percent"], p["spent"], p["budgeted"])
		n.Priority, n.Tags = 4, []string{"chart_with_upwards_trend"}
	case alerts.TypeLowLeftover:
		n.Title = "Low leftover"
		n.Body = fmt.Sprintf("Only $%s left this period (floor $%s)", p["leftover"], p["floor"])
		n.Priority, n.Tags = 4, []string{"warning"}
	default:
		n.Title = "Alert"
		n.Body = "A financial alert was triggered."
	}
	return n
}

// EvaluateAlertsAllArgs sweeps every household so alerts still fire for a
// household whose institutions are all quiet (e.g. a budget crossing driven by
// the calendar rolling over, not by a sync).
type EvaluateAlertsAllArgs struct{}

func (EvaluateAlertsAllArgs) Kind() string { return "evaluate_alerts_all" }

// EvaluateAlertsAllWorker enqueues a per-household evaluation.
type EvaluateAlertsAllWorker struct {
	river.WorkerDefaults[EvaluateAlertsAllArgs]
	Queries *dbgen.Queries
	Client  *river.Client[pgx.Tx]
}

func (w *EvaluateAlertsAllWorker) Work(ctx context.Context, job *river.Job[EvaluateAlertsAllArgs]) error {
	ids, err := w.Queries.ListHouseholdIDs(ctx)
	if err != nil {
		return fmt.Errorf("list households: %w", err)
	}
	for _, id := range ids {
		if _, err := w.Client.Insert(ctx, EvaluateAlertsArgs{HouseholdID: id}, nil); err != nil {
			slog.Error("enqueue alert evaluation", "error", err, "household_id", id)
		}
	}
	return nil
}

// --------------------------------------------------------------------------
// Insight generation
// --------------------------------------------------------------------------

// GenerateInsightsArgs runs the proactive-insight engine for one household: the
// deterministic detectors, plus optional AI phrasing when a key is configured.
//
// Detection needs no API key, so unlike the LLM categorise job this worker is
// always registered — gating the whole job on AI would leave the feed empty
// without a key, defeating the point. Only the phrasing inside
// insights.Generate is AI-gated.
type GenerateInsightsArgs struct {
	HouseholdID uuid.UUID `json:"household_id"`
}

func (GenerateInsightsArgs) Kind() string { return "insights" }

// InsertOpts collapses a burst of enqueues for one household (several items
// finishing a sync at once, plus the sweep) into a single pass per minute.
// Starts from River's required state set (see SyncItemArgs) so the insert is not
// silently rejected.
func (a GenerateInsightsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: QueueDefault,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByState:  append(rivertype.UniqueOptsByStateDefault(), rivertype.JobStateRetryable),
			ByPeriod: time.Minute,
		},
	}
}

// insightPushMinPriority is the floor for pushing an insight out of the app. The
// feed carries everything (priority 1–5); only genuinely urgent items — a big
// projected shortfall, an outsized charge, a real income change — earn a phone
// buzz. Celebratory or low-stakes insights (milestones, new_recurring) stay
// in-app. 4 keeps the push channel quiet enough to stay trusted.
const insightPushMinPriority = 4

// GenerateInsightsWorker runs the insight engine for one household. AI is passed
// through to insights.Generate, which uses it only for phrasing and falls back
// to template text when it is disabled or errors. Client/AppURL are used to push
// the highest-priority new insights, mirroring alert dispatch; both may be zero
// (no queue / no configured URL), in which case the push is simply skipped.
type GenerateInsightsWorker struct {
	river.WorkerDefaults[GenerateInsightsArgs]
	Queries *dbgen.Queries
	AI      *ai.Client
	Client  *river.Client[pgx.Tx]
	AppURL  string
}

func (w *GenerateInsightsWorker) Work(ctx context.Context, job *river.Job[GenerateInsightsArgs]) error {
	results, err := insights.Generate(ctx, w.Queries, w.AI, job.Args.HouseholdID, time.Now())
	if err != nil {
		return fmt.Errorf("generate insights for household %s: %w", job.Args.HouseholdID, err)
	}
	if len(results) > 0 {
		slog.Info("insights generated",
			"household_id", job.Args.HouseholdID, "candidates", len(results))
	}
	if err := w.dispatchInsightPushes(ctx, job.Args.HouseholdID, results); err != nil {
		// A push failure must not fail (and retry) the whole generation pass — the
		// feed is already written. Log and move on.
		slog.Warn("insight push dispatch", "error", err, "household_id", job.Args.HouseholdID)
	}
	return nil
}

// dispatchInsightPushes enqueues one notify job per (newly-created high-priority
// insight × member who has a channel). It mirrors EvaluateAlertsWorker's
// dispatch, minus the per-event notified stamp: insight pushes dedupe purely on
// NotifyArgs uniqueness (a refreshed insight is not Inserted, so it never reaches
// here; an identical re-insert collapses via ByArgs/ByPeriod). Only pushes when a
// queue is wired.
func (w *GenerateInsightsWorker) dispatchInsightPushes(ctx context.Context, householdID uuid.UUID, results []insights.Result) error {
	if w.Client == nil {
		return nil
	}

	// Cheap pre-filter so a quiet pass touches nothing.
	var pushable []insights.Result
	for _, r := range results {
		if r.Inserted && r.Priority >= insightPushMinPriority {
			pushable = append(pushable, r)
		}
	}
	if len(pushable) == 0 {
		return nil
	}

	members, err := w.Queries.ListHouseholdMembers(ctx, householdID)
	if err != nil {
		return fmt.Errorf("list members: %w", err)
	}

	for _, r := range pushable {
		n := insightNotification(r, w.AppURL)
		for _, m := range members {
			if !hasNotifyChannel(ctx, w.Queries, m.ID) {
				continue
			}
			if _, err := w.Client.Insert(ctx, NotifyArgs{
				UserID:   m.ID,
				Title:    n.Title,
				Body:     n.Body,
				Priority: n.Priority,
				Tags:     n.Tags,
				ClickURL: n.ClickURL,
			}, nil); err != nil {
				slog.Error("enqueue insight notify", "error", err, "user_id", m.ID, "insight_id", r.ID)
			}
		}
	}
	return nil
}

// insightNotification renders an insight Result as a push. The feed already
// carries the final (AI-phrased) Title/Body, so this only maps priority to the
// ntfy band and points the click-through at the feed.
func insightNotification(r insights.Result, appURL string) notify.Notification {
	click := ""
	if appURL != "" {
		click = strings.TrimRight(appURL, "/") + "/insights"
	}
	priority := r.Priority
	if priority > 5 {
		priority = 5
	}
	return notify.Notification{
		Title:    r.Title,
		Body:     r.Body,
		Priority: priority,
		Tags:     []string{"bulb"},
		ClickURL: click,
	}
}

// Timeout bounds a pass that may fan out to one phrasing call per candidate.
func (w *GenerateInsightsWorker) Timeout(*river.Job[GenerateInsightsArgs]) time.Duration {
	return 5 * time.Minute
}

// GenerateInsightsAllArgs sweeps every household so a quiet household still gets
// a refreshed feed even when no sync is enqueuing per-household passes (a budget
// crossing driven by the calendar rolling over, say).
type GenerateInsightsAllArgs struct{}

func (GenerateInsightsAllArgs) Kind() string { return "insights_all" }

// GenerateInsightsAllWorker enqueues a per-household generation pass.
type GenerateInsightsAllWorker struct {
	river.WorkerDefaults[GenerateInsightsAllArgs]
	Queries *dbgen.Queries
	Client  *river.Client[pgx.Tx]
}

func (w *GenerateInsightsAllWorker) Work(ctx context.Context, job *river.Job[GenerateInsightsAllArgs]) error {
	ids, err := w.Queries.ListHouseholdIDs(ctx)
	if err != nil {
		return fmt.Errorf("list households: %w", err)
	}
	for _, id := range ids {
		if _, err := w.Client.Insert(ctx, GenerateInsightsArgs{HouseholdID: id}, nil); err != nil {
			slog.Error("enqueue insight generation", "error", err, "household_id", id)
		}
	}
	return nil
}

// --------------------------------------------------------------------------
// Security housekeeping
// --------------------------------------------------------------------------

// SecuritySweepArgs prunes expired auth state.
type SecuritySweepArgs struct{}

func (SecuritySweepArgs) Kind() string { return "security_sweep" }

// SecuritySweepWorker collects rows that are dead but still on disk.
//
// None of this is load-bearing for correctness — every query that reads these
// tables already filters on expiry, so a stale row is never honoured. It is
// housekeeping: without it, sessions, abandoned MFA challenges and audit
// events accumulate forever in a database nobody is watching.
type SecuritySweepWorker struct {
	river.WorkerDefaults[SecuritySweepArgs]
	Queries *dbgen.Queries
	// IdleTTL must match auth.SessionIdleTTL, so the sweep collects exactly
	// the sessions the middleware has already stopped honouring.
	IdleTTL time.Duration
	// AuthEventTTL is how long the audit log is kept. Long enough to
	// investigate something noticed late, short enough that the table stays
	// small on a household-sized deployment.
	AuthEventTTL time.Duration
}

func (w *SecuritySweepWorker) Work(ctx context.Context, job *river.Job[SecuritySweepArgs]) error {
	sessions, err := w.Queries.DeleteExpiredSessions(ctx, auth.Interval(w.IdleTTL))
	if err != nil {
		return fmt.Errorf("delete expired sessions: %w", err)
	}

	challenges, err := w.Queries.DeleteExpiredMFAChallenges(ctx)
	if err != nil {
		return fmt.Errorf("delete expired mfa challenges: %w", err)
	}

	events, err := w.Queries.DeleteOldAuthEvents(ctx, auth.Interval(w.AuthEventTTL))
	if err != nil {
		return fmt.Errorf("delete old auth events: %w", err)
	}

	slog.Info("security sweep",
		"sessions", sessions, "mfa_challenges", challenges, "auth_events", events)
	return nil
}
