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
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/apex42group/ledgermancy/backend/internal/ai"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
	"github.com/apex42group/ledgermancy/backend/internal/notify"
	"github.com/apex42group/ledgermancy/backend/internal/reporting"
)

// digestInsightLimit bounds how many top insights ride along in one digest.
const digestInsightLimit = 5

// --------------------------------------------------------------------------
// Cadence gating (deterministic)
//
// The sweep runs frequently (hourly); these predicates decide who is actually
// due, so the sweep is cheap when nobody is. The digest_deliveries period_key
// makes "already sent this week/month" a single existence check, so exact sweep
// timing never causes duplicates.
// --------------------------------------------------------------------------

// digestDue reports whether a user on the given cadence is due now, and the
// period_key identifying what a digest sent now would cover. Weekly users are
// due on Monday (covering the ISO week); monthly users on the 1st or 2nd — day 2
// gives the prior month's last day time to settle — covering the prior month.
func digestDue(cadence string, now time.Time) (bool, string) {
	now = now.UTC()
	switch cadence {
	case "monthly":
		prev := firstOfMonth(now).AddDate(0, -1, 0)
		return now.Day() <= 2, prev.Format("2006-01")
	default: // weekly
		year, week := now.ISOWeek()
		return now.Weekday() == time.Monday, fmt.Sprintf("%d-W%02d", year, week)
	}
}

// digestWindow resolves the reporting window and cache behaviour for a cadence.
//
//   - monthly → the just-completed calendar month; cacheable, so the job doubles
//     as a warmer for the on-demand monthly_summaries cache (safe: the month is
//     complete and stable).
//   - weekly → the current month-to-date. MonthlySummary is month-shaped, so a
//     weekly recap reuses that view, but it is NOT cached — persisting a partial
//     month would overwrite the canonical full-month summary a user generates
//     on demand.
func digestWindow(cadence string, now time.Time) (monthDate, from, to time.Time, label string, cacheable bool) {
	now = now.UTC()
	if cadence == "monthly" {
		prev := firstOfMonth(now).AddDate(0, -1, 0)
		return prev, prev, prev.AddDate(0, 1, -1), prev.Format("January 2006"), true
	}
	first := firstOfMonth(now)
	return first, first, now, first.Format("January 2006"), false
}

func firstOfMonth(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// --------------------------------------------------------------------------
// Sweep → per-user fan-out
// --------------------------------------------------------------------------

// DigestSweepArgs is the periodic entry point. Because opt-in is per user, it
// enumerates users (not households) and enqueues one DigestArgs per user who is
// both due now and not already served for the current period.
type DigestSweepArgs struct{}

func (DigestSweepArgs) Kind() string { return "digest_sweep" }

// DigestSweepWorker lists digest-enabled users and fans out due ones.
type DigestSweepWorker struct {
	river.WorkerDefaults[DigestSweepArgs]
	Queries *dbgen.Queries
	Client  *river.Client[pgx.Tx]
}

func (w *DigestSweepWorker) Work(ctx context.Context, job *river.Job[DigestSweepArgs]) error {
	now := time.Now()
	users, err := w.Queries.ListDigestEnabledUsers(ctx)
	if err != nil {
		return fmt.Errorf("list digest-enabled users: %w", err)
	}

	enqueued := 0
	for _, u := range users {
		due, periodKey := digestDue(u.Cadence, now)
		if !due {
			continue
		}
		// Cheap pre-check so a busy Monday doesn't enqueue 24 identical jobs; the
		// worker re-checks authoritatively to close the race.
		exists, err := w.Queries.DigestDeliveryExists(ctx, dbgen.DigestDeliveryExistsParams{
			UserID: u.UserID, PeriodKey: periodKey,
		})
		if err != nil {
			slog.Error("digest dedupe check", "error", err, "user_id", u.UserID)
			continue
		}
		if exists {
			continue
		}
		if _, err := w.Client.Insert(ctx, DigestArgs{
			UserID: u.UserID, HouseholdID: u.HouseholdID,
		}, nil); err != nil {
			slog.Error("enqueue digest", "error", err, "user_id", u.UserID)
			continue
		}
		enqueued++
	}
	if enqueued > 0 {
		slog.Info("digests enqueued", "users", enqueued)
	}
	return nil
}

// DigestArgs assembles and delivers one user's digest. HouseholdID is carried so
// the worker need not re-resolve it.
type DigestArgs struct {
	UserID      uuid.UUID `json:"user_id"`
	HouseholdID uuid.UUID `json:"household_id"`
}

func (DigestArgs) Kind() string { return "digest" }

// InsertOpts collapses a burst of enqueues for one user within a period (e.g.
// several hourly sweeps on the same Monday) into one job. The dedupe table is
// the authoritative guard; this just avoids needless work.
func (DigestArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: QueueDefault,
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByState:  append(rivertype.UniqueOptsByStateDefault(), rivertype.JobStateRetryable),
			ByPeriod: 6 * time.Hour,
		},
	}
}

// DigestWorker builds one user's digest — the (cache-first) monthly narrative
// plus the top unread insights — and hands it to the delivery job. It recomputes
// the dedupe guard, re-checks opt-in, and skips a user with no notification
// channel (a channel-less Send is a no-op, and skipping avoids burning the
// period's dedupe slot before they configure one).
type DigestWorker struct {
	river.WorkerDefaults[DigestArgs]
	Queries *dbgen.Queries
	AI      *ai.Client
	Client  *river.Client[pgx.Tx]
	AppURL  string
}

func (w *DigestWorker) Work(ctx context.Context, job *river.Job[DigestArgs]) error {
	userID := job.Args.UserID
	householdID := job.Args.HouseholdID
	now := time.Now()

	// Re-check opt-in — a user may have turned it off between sweep and run.
	if !boolPref(ctx, w.Queries, userID, "digest.enabled") {
		return nil
	}
	// No channel → nothing to deliver to. Skip without recording, so the digest
	// resumes for this period once they configure one.
	channel := stringPref(ctx, w.Queries, userID, "notify.channel")
	if channel == "" || channel == "none" {
		return nil
	}

	cadence := stringPref(ctx, w.Queries, userID, "digest.cadence")
	if cadence == "" {
		cadence = "weekly"
	}
	_, periodKey := digestDue(cadence, now)

	exists, err := w.Queries.DigestDeliveryExists(ctx, dbgen.DigestDeliveryExistsParams{
		UserID: userID, PeriodKey: periodKey,
	})
	if err != nil {
		return fmt.Errorf("digest dedupe check: %w", err)
	}
	if exists {
		return nil
	}

	monthDate, from, to, label, cacheable := digestWindow(cadence, now)

	// Cache-first narrative. A user who already generated this month in-app gets
	// that exact text; on a miss (and only with AI) we generate, and warm the
	// cache only when the window is a completed month.
	narrative := ""
	if cached, err := w.Queries.GetMonthlySummary(ctx, dbgen.GetMonthlySummaryParams{
		HouseholdID: householdID, Month: monthDate,
	}); err == nil {
		narrative = cached.Summary
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("get cached summary: %w", err)
	} else if w.AI.Enabled() {
		// Generate with the recipient's own visibility, matching what they'd get
		// from the on-demand button.
		input, err := reporting.BuildMonthlySummaryInput(ctx, w.Queries, householdID, userID, from, to, label)
		if err != nil {
			return fmt.Errorf("build summary input: %w", err)
		}
		text, err := w.AI.MonthlySummary(ctx, input)
		if err != nil && !errors.Is(err, ai.ErrDisabled) {
			// A model failure must not sink the digest — send the insights alone.
			slog.Warn("digest summary generation failed", "error", err, "user_id", userID)
		} else if err == nil {
			narrative = text
			if cacheable {
				if _, err := w.Queries.UpsertMonthlySummary(ctx, dbgen.UpsertMonthlySummaryParams{
					HouseholdID: householdID, Month: monthDate, Summary: text, Model: w.AI.Model(),
				}); err != nil {
					slog.Warn("warm summary cache", "error", err, "household_id", householdID)
				}
			}
		}
	}

	insights, err := w.Queries.ListUnreadInsightsForDigest(ctx, dbgen.ListUnreadInsightsForDigestParams{
		HouseholdID: householdID, Limit: digestInsightLimit,
	})
	if err != nil {
		return fmt.Errorf("list digest insights: %w", err)
	}

	// Nothing to say this period: no narrative and no insights. Skip without
	// recording, so a digest still goes out if insights appear later this period.
	if strings.TrimSpace(narrative) == "" && len(insights) == 0 {
		return nil
	}

	n := buildDigestNotification(label, narrative, insights, w.AppURL)
	if _, err := w.Client.Insert(ctx, NotifyArgs{
		UserID:   userID,
		Title:    n.Title,
		Body:     n.Body,
		Priority: n.Priority,
		Tags:     n.Tags,
		ClickURL: n.ClickURL,
	}, nil); err != nil {
		return fmt.Errorf("enqueue digest delivery: %w", err)
	}

	// Record after enqueue: a crash between the two at worst re-sends next sweep,
	// which is far better than silently dropping a digest.
	if err := w.Queries.RecordDigestDelivery(ctx, dbgen.RecordDigestDeliveryParams{
		UserID: userID, PeriodKey: periodKey,
	}); err != nil {
		return fmt.Errorf("record digest delivery: %w", err)
	}
	slog.Info("digest delivered", "user_id", userID, "period", periodKey, "insights", len(insights))
	return nil
}

// buildDigestNotification composes the push: the narrative (when present) then a
// short list of insight headlines. It quotes stored text verbatim — the job does
// no arithmetic and passes no numbers to any model.
func buildDigestNotification(label, narrative string, insights []dbgen.Insight, appURL string) notify.Notification {
	var body strings.Builder
	if s := strings.TrimSpace(narrative); s != "" {
		body.WriteString(s)
	}
	if len(insights) > 0 {
		if body.Len() > 0 {
			body.WriteString("\n\n")
		}
		body.WriteString("Worth a look:")
		for _, i := range insights {
			fmt.Fprintf(&body, "\n• %s", i.Title)
		}
	}

	click := ""
	if appURL != "" {
		click = strings.TrimRight(appURL, "/") + "/insights"
	}
	return notify.Notification{
		Title:    "Your " + label + " recap",
		Body:     body.String(),
		Priority: 3,
		Tags:     []string{"bar_chart"},
		ClickURL: click,
	}
}

// boolPref reads a JSON-bool user preference, returning false when unset or
// malformed — an opt-in check should degrade to "off", never error.
func boolPref(ctx context.Context, q *dbgen.Queries, userID uuid.UUID, key string) bool {
	raw, err := q.GetUserPreference(ctx, dbgen.GetUserPreferenceParams{UserID: &userID, Key: key})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("read preference", "error", err, "key", key, "user_id", userID)
		}
		return false
	}
	var b bool
	_ = json.Unmarshal(raw, &b)
	return b
}
