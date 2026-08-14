package insights

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Result records what became of one candidate after upserting. Inserted is true
// only when the row was newly created (not a refresh of an existing one), which
// is what the high-priority push keys off — a refresh should not re-notify.
// Title/Body carry the final (AI-phrased or template) text so the caller can
// build a push without re-reading the row; ID links a push back to the feed row.
type Result struct {
	ID        uuid.UUID
	Kind      string
	Priority  int
	Title     string
	Body      string
	DedupeKey string
	Inserted  bool
}

// Generate runs every registered producer for one household and upserts the
// candidates they raise.
//
// Detection is always deterministic. Phrasing is the ONLY AI step and it is
// best-effort: with aiClient disabled, or on any AI error or timeout, the
// producer's template Title/Body is stored verbatim, so the feed is fully
// populated with no API key configured.
func Generate(
	ctx context.Context,
	q *dbgen.Queries,
	aiClient *ai.Client,
	householdID uuid.UUID,
	now time.Time,
) ([]Result, error) {
	var results []Result

	for _, p := range DefaultProducers() {
		candidates, err := p.Detect(ctx, q, householdID, now)
		if err != nil {
			// One producer failing must not sink the rest of the feed.
			slog.Warn("insight producer failed",
				"kind", p.Kind(), "household_id", householdID, "error", err)
			continue
		}

		// Optional AI enrichment (labels only, never numbers), best-effort: a
		// failure leaves the deterministic Data untouched. Runs once per producer
		// over its whole batch, so a classifier can make a single model call.
		if aiClient.Enabled() {
			if cl, ok := p.(Classifier); ok {
				if err := cl.Classify(ctx, aiClient, candidates); err != nil {
					slog.Debug("insight classification fell back",
						"kind", p.Kind(), "household_id", householdID, "error", err)
				}
			}
		}

		for _, c := range candidates {
			title, body := c.Title, c.Body
			if aiClient.Enabled() {
				if text, err := phrase(ctx, aiClient, c); err != nil {
					// Mandatory fallback: keep the template on any AI failure.
					slog.Debug("insight phrasing fell back to template",
						"kind", c.Kind, "dedupe_key", c.DedupeKey, "error", err)
				} else {
					title, body = text.Title, text.Body
				}
			}

			data, err := json.Marshal(c.Data)
			if err != nil {
				slog.Warn("marshal insight data", "dedupe_key", c.DedupeKey, "error", err)
				continue
			}

			row, err := q.UpsertInsight(ctx, dbgen.UpsertInsightParams{
				HouseholdID: householdID,
				Kind:        c.Kind,
				Priority:    int16(c.Priority),
				Title:       title,
				Body:        body,
				Data:        data,
				Period:      c.Period,
				DedupeKey:   c.DedupeKey,
			})
			if err != nil {
				slog.Error("upsert insight", "dedupe_key", c.DedupeKey, "error", err)
				continue
			}
			results = append(results, Result{
				ID:        row.ID,
				Kind:      row.Kind,
				Priority:  int(row.Priority),
				Title:     row.Title,
				Body:      row.Body,
				DedupeKey: row.DedupeKey,
				Inserted:  row.Inserted,
			})
		}

		// Retraction, for the producers that opt in by implementing Retractor.
		//
		// Runs AFTER the upserts above, and only on the success path: the
		// `continue` on a Detect error above is what keeps a transient query
		// failure from being read as "nothing is true any more" and wiping the
		// kind's whole feed. An empty LiveKeys from a SUCCESSFUL detection means
		// exactly that, though, and retracting everything is the right answer.
		if r, ok := p.(Retractor); ok {
			if err := retract(ctx, q, r, p.Kind(), householdID, now); err != nil {
				// The feed is already written and correct except for the stale
				// rows; failing the pass here would re-run every producer to fix
				// a cleanup step. Log and move on, as with the AI fallbacks.
				slog.Warn("insight retraction failed",
					"kind", p.Kind(), "household_id", householdID, "error", err)
			}
		}
	}

	// High-priority push: the engine only stores and reports results. The jobs
	// layer (jobs.GenerateInsightsWorker) iterates these results for
	// Inserted && Priority >= threshold and enqueues a NotifyArgs per household
	// member with a channel configured — mirroring how alert events push once
	// their rule opts in (jobs.EvaluateAlertsWorker). Delivery stays in jobs so
	// this package keeps no dependency on the queue or the Notifier.

	return results, nil
}

// retract closes every live insight of one kind whose fact the producer no
// longer detects. Split out of Generate so the "ask, then withdraw" pair reads
// as one step and the nil-key case is handled in exactly one place.
func retract(
	ctx context.Context,
	q *dbgen.Queries,
	r Retractor,
	kind string,
	householdID uuid.UUID,
	now time.Time,
) error {
	keys, err := r.LiveKeys(ctx, q, householdID, now)
	if err != nil {
		return fmt.Errorf("live keys: %w", err)
	}
	// A nil slice would reach Postgres as NULL, and `x = ANY(NULL)` is NULL —
	// never true, but also never false, so the NOT filters out every row and the
	// statement silently retracts nothing. An empty non-nil slice is a real empty
	// array and retracts the kind wholesale, which is what "detected nothing"
	// must mean.
	if keys == nil {
		keys = []string{}
	}

	n, err := q.RetractStaleInsights(ctx, dbgen.RetractStaleInsightsParams{
		HouseholdID: householdID,
		Kind:        kind,
		LiveKeys:    keys,
	})
	if err != nil {
		return fmt.Errorf("retract stale: %w", err)
	}
	if n > 0 {
		slog.Info("insights retracted",
			"kind", kind, "household_id", householdID, "count", n, "live", len(keys))
	}
	return nil
}

// phrase turns a candidate's deterministic facts into an AI phrasing request.
// The Data map is quoted verbatim — the model rewords around the numbers, it
// never recomputes them. Keys are sorted so the prompt is stable across runs.
func phrase(ctx context.Context, c *ai.Client, cand Candidate) (ai.InsightText, error) {
	keys := make([]string, 0, len(cand.Data))
	for k := range cand.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	facts := make([]ai.InsightFact, 0, len(keys))
	for _, k := range keys {
		facts = append(facts, ai.InsightFact{Label: k, Value: fmt.Sprintf("%v", cand.Data[k])})
	}

	return c.PhraseInsight(ctx, ai.InsightPhraseInput{
		Kind:  cand.Kind,
		Title: cand.Title,
		Body:  cand.Body,
		Facts: facts,
	})
}
