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

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/mailer"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/notify"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/reporting"
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

// DigestSweepWorker lists candidate users and fans out due ones.
type DigestSweepWorker struct {
	river.WorkerDefaults[DigestSweepArgs]
	Queries *dbgen.Queries
	Client  *river.Client[pgx.Tx]
}

func (w *DigestSweepWorker) Work(ctx context.Context, job *river.Job[DigestSweepArgs]) error {
	now := time.Now()
	// Every adult, not only those who opted into a push. Doc 25's central change:
	// the in-app digest defaults on and needs no notification channel, so a sweep
	// gated on the push preference would keep the whole feature dark for anyone
	// who has never configured ntfy — which is most people.
	users, err := w.Queries.ListDigestCandidateUsers(ctx)
	if err != nil {
		return fmt.Errorf("list digest candidate users: %w", err)
	}

	enqueued := 0
	for _, u := range users {
		if !u.InAppEnabled && !u.PushEnabled && !u.EmailEnabled {
			continue
		}
		due, periodKey := digestDue(u.Cadence, now)
		if !due {
			continue
		}
		// Cheap pre-check so a busy Monday doesn't enqueue 24 identical jobs; the
		// worker re-checks authoritatively to close the race. "Satisfied" spans
		// both surfaces — a stored entry counts as much as a recorded push, so a
		// user with push off is not swept again all day.
		satisfied, err := w.Queries.DigestPeriodSatisfied(ctx, dbgen.DigestPeriodSatisfiedParams{
			UserID: u.UserID, PeriodKey: periodKey,
		})
		if err != nil {
			slog.Error("digest dedupe check", "error", err, "user_id", u.UserID)
			continue
		}
		if satisfied {
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
// the worker need not re-resolve it. Force marks a "send one now" request from
// Settings: it bypasses the opt-in re-check and the per-period dedupe (and does
// not record one), so a manual test always goes out and never consumes the
// scheduled digest's slot.
type DigestArgs struct {
	UserID      uuid.UUID `json:"user_id"`
	HouseholdID uuid.UUID `json:"household_id"`
	Force       bool      `json:"force,omitempty"`
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

// DigestWorker builds one user's digest — the computed figures, the
// (cache-first) monthly narrative, and the top unread insights — then persists
// it and delivers it to whichever surfaces the user has switched on.
//
// The ordering inside Work is load-bearing. The entry is written BEFORE any
// delivery is attempted, because a push that fails must not take the content
// with it: before doc 25 a delivery failure lost the digest entirely, since the
// narrative existed only inside the notification body.
//
// Mail is optional and nil-safe. A send failure is logged, never returned —
// matching how insight-push failures are handled in jobs.go: an unreachable mail
// server must not put the job into a retry loop that regenerates the digest.
type DigestWorker struct {
	river.WorkerDefaults[DigestArgs]
	Queries *dbgen.Queries
	AI      *ai.Client
	Client  *river.Client[pgx.Tx]
	Mail    mailer.Sender
	AppURL  string
}

func (w *DigestWorker) Work(ctx context.Context, job *river.Job[DigestArgs]) error {
	userID := job.Args.UserID
	householdID := job.Args.HouseholdID
	force := job.Args.Force
	now := time.Now()

	// Which surfaces this user wants. Re-read here rather than carried on the
	// job args, because a user may have changed them between sweep and run.
	//
	//   in-app — defaults ON. The digest exists as a page whether or not any
	//            notification channel is configured; that is the feature.
	//   push   — defaults OFF (pre-existing key), and additionally needs a
	//            channel to have anywhere to go.
	//   email  — defaults OFF, and inert unless the operator configured SMTP.
	//
	// A forced ("send one now") digest overrides the in-app switch only: the
	// user is standing in front of the button asking to see one.
	inApp := force || boolPrefDefault(ctx, w.Queries, userID, "digest.in_app", true)
	wantPush := force || boolPref(ctx, w.Queries, userID, "digest.enabled")
	channel := stringPref(ctx, w.Queries, userID, "notify.channel")
	canPush := wantPush && channel != "" && channel != "none"
	wantEmail := boolPref(ctx, w.Queries, userID, "digest.email") &&
		w.Mail != nil && w.Mail.Enabled()

	if !inApp && !canPush && !wantEmail {
		// Nothing switched on, or push is the only thing on and there is no
		// channel for it. Nothing is recorded, so the digest resumes the moment
		// they configure one.
		return nil
	}

	cadence := stringPref(ctx, w.Queries, userID, "digest.cadence")
	if cadence == "" {
		cadence = "weekly"
	}
	_, periodKey := digestDue(cadence, now)

	// The per-period dedupe guards the PUSH on the scheduled path only. A forced
	// send is a deliberate manual action: it neither honours nor records the
	// dedupe, so it always goes out and never burns the real digest's slot.
	//
	// The in-app entry has its own guard — the unique constraint on
	// (user_id, period_key) — so it is not consulted here.
	if canPush && !force {
		exists, err := w.Queries.DigestDeliveryExists(ctx, dbgen.DigestDeliveryExistsParams{
			UserID: userID, PeriodKey: periodKey,
		})
		if err != nil {
			return fmt.Errorf("digest dedupe check: %w", err)
		}
		if exists {
			canPush = false
		}
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
		input, err := reporting.BuildMonthlySummaryInput(ctx, w.Queries, householdID, userID, from, to, label, now)
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

	// The computed figures. Deterministic SQL and decimal throughout — the model
	// never sees a number it could restate, and this is what gets frozen into the
	// stored entry.
	payload, err := reporting.BuildDigestPayload(
		ctx, w.Queries, householdID, userID, cadence, from, to, label, now, insights)
	if err != nil {
		return fmt.Errorf("build digest payload: %w", err)
	}

	// Nothing to say this period: no figures, no insights, no narrative. On the
	// scheduled path, skip without recording anything so a digest still goes out
	// if activity appears later. On a forced test, produce a short confirmation
	// instead — the point is to prove the pipeline works, not to withhold on a
	// quiet period.
	if !payload.HasContent() && strings.TrimSpace(narrative) == "" {
		if !force {
			return nil
		}
		narrative = "No activity to report for " + label +
			" yet — but your digest is set up, and this is what one looks like."
	}

	// ----------------------------------------------------------------------
	// Persist first, deliver second.
	//
	// This ordering is the fix at the heart of doc 25: a failed push used to
	// take the whole digest with it, because the generated narrative existed
	// nowhere but in the notification body.
	// ----------------------------------------------------------------------
	stored := false
	if inApp {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode digest payload: %w", err)
		}
		var storedNarrative *string
		if s := strings.TrimSpace(narrative); s != "" {
			storedNarrative = &s
		}
		rows, err := w.Queries.InsertDigestEntry(ctx, dbgen.InsertDigestEntryParams{
			HouseholdID: householdID,
			UserID:      userID,
			Cadence:     cadence,
			PeriodKey:   periodKey,
			PeriodStart: from,
			PeriodEnd:   to,
			Label:       label,
			Payload:     encoded,
			Narrative:   storedNarrative,
		})
		if err != nil {
			return fmt.Errorf("store digest entry: %w", err)
		}
		// 0 rows means this period was already stored. Entries are write-once, so
		// that is the correct outcome and not an error: what the user read for
		// this period must not change under them.
		stored = rows > 0
	}

	if canPush {
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

		// Record after enqueue: a crash between the two at worst re-sends next
		// sweep, which is far better than silently dropping a digest. A forced
		// send records nothing, so it never blocks the period's real digest.
		if !force {
			if err := w.Queries.RecordDigestDelivery(ctx, dbgen.RecordDigestDeliveryParams{
				UserID: userID, PeriodKey: periodKey,
			}); err != nil {
				return fmt.Errorf("record digest delivery: %w", err)
			}
		}
	}

	if wantEmail {
		w.sendEmail(ctx, userID, label, narrative, payload)
	}

	slog.Info("digest produced",
		"user_id", userID, "period", periodKey, "insights", len(insights),
		"stored", stored, "pushed", canPush, "emailed", wantEmail, "forced", force)
	return nil
}

// sendEmail delivers the digest by mail. Failures are logged and swallowed:
// the entry is already stored and any push already enqueued, so returning an
// error here would re-run the whole job — and a job that retries a digest
// because a mail server is down is worse than a missing email.
func (w *DigestWorker) sendEmail(
	ctx context.Context,
	userID uuid.UUID,
	label, narrative string,
	payload reporting.DigestPayload,
) {
	// The account email, which is the only address the app knows about — there is
	// deliberately no separate "notification address" to keep in sync.
	user, err := w.Queries.GetUserByID(ctx, userID)
	if err != nil {
		slog.Warn("digest email: read address", "error", err, "user_id", userID)
		return
	}
	if err := w.Mail.Send(ctx, mailer.Message{
		To:      user.Email,
		Subject: "Your " + label + " recap",
		Body:    renderDigestEmail(narrative, payload, w.AppURL),
	}); err != nil {
		slog.Warn("digest email delivery failed", "error", err, "user_id", userID)
	}
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
		click = strings.TrimRight(appURL, "/") + "/digest"
	}
	return notify.Notification{
		Title:    "Your " + label + " recap",
		Body:     body.String(),
		Priority: 3,
		Tags:     []string{"bar_chart"},
		ClickURL: click,
	}
}

// renderDigestEmail lays the digest out as plain text.
//
// Plain text rather than HTML, deliberately: the figures are already finished
// strings, an email client cannot re-render them, and there is nothing here that
// a table would explain better than a line would. It also means this function
// can never grow a tracking pixel or a remote image.
//
// Every amount is quoted verbatim from the payload. Nothing here computes.
func renderDigestEmail(narrative string, p reporting.DigestPayload, appURL string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Your %s recap\n", p.Label)
	fmt.Fprintf(&b, "%s to %s\n\n", p.PeriodStart, p.PeriodEnd)

	if s := strings.TrimSpace(narrative); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	}

	fmt.Fprintf(&b, "In: %s\nOut: %s\nLeft over: %s\n", p.Income, p.Spending, p.Leftover)
	if p.SavingsRate != "" {
		fmt.Fprintf(&b, "Savings rate: %s\n", p.SavingsRate)
	}
	if p.GrossSavingsRate != "" {
		fmt.Fprintf(&b, "Savings rate on gross pay: %s\n", p.GrossSavingsRate)
	}

	if len(p.TopCategories) > 0 {
		b.WriteString("\nWhere it went\n")
		for _, c := range p.TopCategories {
			fmt.Fprintf(&b, "  %s — %s\n", c.Name, c.Total)
		}
	}

	if len(p.AboveBaseline) > 0 {
		b.WriteString("\nRunning above usual\n")
		for _, d := range p.AboveBaseline {
			fmt.Fprintf(&b, "  %s — %s, usually %s (%s over)\n", d.Name, d.ThisMonth, d.Typical, d.Over)
		}
	}

	if len(p.Budgets) > 0 {
		b.WriteString("\nBudgets\n")
		for _, bl := range p.Budgets {
			state := "left"
			if bl.Over {
				state = "over"
			}
			fmt.Fprintf(&b, "  %s — %s of %s spent, %s %s\n",
				bl.Name, bl.Spent, bl.Available, strings.TrimPrefix(bl.Remaining, "-"), state)
		}
	}

	if len(p.LargestTransactions) > 0 {
		b.WriteString("\nBiggest purchases\n")
		for _, t := range p.LargestTransactions {
			fmt.Fprintf(&b, "  %s — %s on %s\n", t.Merchant, t.Amount, t.Date)
		}
	}

	if p.NetWorth != nil {
		fmt.Fprintf(&b, "\nNet worth: %s", p.NetWorth.Current)
		if p.NetWorth.Direction != "" {
			fmt.Fprintf(&b, " (%s %s this period)", p.NetWorth.Direction,
				strings.TrimPrefix(p.NetWorth.Change, "-"))
		}
		b.WriteString("\n")
	}

	if len(p.UpcomingBills) > 0 {
		b.WriteString("\nComing up\n")
		for _, bill := range p.UpcomingBills {
			fmt.Fprintf(&b, "  %s — %s due %s\n", bill.Label, bill.Amount, bill.DueDate)
		}
	}

	if len(p.Insights) > 0 {
		b.WriteString("\nWorth a look\n")
		for _, i := range p.Insights {
			fmt.Fprintf(&b, "  %s\n", i.Title)
		}
	}

	if appURL != "" {
		fmt.Fprintf(&b, "\nRead it in the app: %s/digest\n", strings.TrimRight(appURL, "/"))
	}
	return b.String()
}

// boolPref reads a JSON-bool user preference, returning false when unset or
// malformed — an opt-in check should degrade to "off", never error.
func boolPref(ctx context.Context, q *dbgen.Queries, userID uuid.UUID, key string) bool {
	return boolPrefDefault(ctx, q, userID, key, false)
}

// boolPrefDefault is boolPref with a caller-chosen value for "never set".
//
// The default matters here in a way it did not before: `digest.in_app` defaults
// TRUE, because the whole point of doc 25 is that the in-app digest exists
// without anybody configuring anything. A malformed stored value still falls
// back to the default rather than erroring — a preference read must never be
// able to sink a job.
func boolPrefDefault(ctx context.Context, q *dbgen.Queries, userID uuid.UUID, key string, fallback bool) bool {
	raw, err := q.GetUserPreference(ctx, dbgen.GetUserPreferenceParams{UserID: &userID, Key: key})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("read preference", "error", err, "key", key, "user_id", userID)
		}
		return fallback
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return fallback
	}
	return b
}
