package insights

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/apex42group/ledgermancy/backend/internal/ai"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// Result records what became of one candidate after upserting. Inserted is true
// only when the row was newly created (not a refresh of an existing one), which
// is what a high-priority push seam keys off — a refresh should not re-notify.
type Result struct {
	Kind      string
	Priority  int
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
				Kind:      row.Kind,
				Priority:  int(row.Priority),
				DedupeKey: row.DedupeKey,
				Inserted:  row.Inserted,
			})
		}
	}

	// --- High-priority push seam (not yet wired) ----------------------------
	// Insights currently surface in-app only. When insight push lands it should
	// enqueue a NotifyArgs for newly-created insights above a priority threshold
	// for each household member with a channel configured — mirroring how alert
	// events push once their rule opts in (jobs.EvaluateAlertsWorker). Left as a
	// clean seam: iterate `results` for Inserted && Priority >= threshold and
	// hand them to the Notifier here.

	return results, nil
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
