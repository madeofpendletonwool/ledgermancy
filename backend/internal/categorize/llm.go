package categorize

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/apex42group/ledgermancy/backend/internal/ai"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// llmChunkSize is how many merchants go in one model call. Batching keeps the
// initial backfill (potentially hundreds of merchants) to a handful of calls;
// each merchant is only ever sent once because every answer is cached.
const llmChunkSize = 25

// llmCandidateLimit bounds how many fallback transactions one pass pulls. A
// pass caches every merchant it sees, so a later pass simply picks up the rest.
const llmCandidateLimit = 2000

// minConfidence is the floor below which an LLM verdict is treated as an
// abstention. A guess we are not sure of is worse than leaving a transaction in
// the fallback category, where it is visibly uncategorised rather than wrong.
const minConfidence = 0.5

// merchantCategoriser is the slice of the AI client this needs. Narrowing it to
// an interface lets the orchestration be tested with a fake, no endpoint.
type merchantCategoriser interface {
	Enabled() bool
	CategoriseMerchants(ctx context.Context, merchants []ai.MerchantInput, cats []ai.CategoryOption) ([]ai.MerchantCategory, error)
}

// LLMCategoriseHousehold asks the model to categorise merchants that the
// deterministic pass left in the fallback category. It is the step-5 fallback
// from the resolver's doc comment, run out of band so a slow model call never
// blocks a sync.
//
// Every merchant it considers is written to merchant_category_map — with a real
// category when the model is confident, or pinned to the fallback category when
// it abstains — so the same merchant is never sent to the model twice. That
// cache write is what keeps the running cost near zero. It never touches a
// manual choice. When AI is disabled it is a no-op.
//
// Returns the number of merchants newly decided (confident placements).
func LLMCategoriseHousehold(ctx context.Context, q *dbgen.Queries, model merchantCategoriser, householdID uuid.UUID) (int, error) {
	if model == nil || !model.Enabled() {
		return 0, nil
	}

	// The fallback category is where the deterministic pass parks anything it
	// could not resolve — exactly the set the model should look at.
	fallback, err := q.GetCategoryBySlug(ctx, dbgen.GetCategoryBySlugParams{
		Slug: "uncategorised", HouseholdID: &householdID,
	})
	if err != nil {
		return 0, fmt.Errorf("load fallback category: %w", err)
	}
	fallbackID := fallback.ID

	candidates, err := q.ListLLMCandidates(ctx, dbgen.ListLLMCandidatesParams{
		HouseholdID: householdID,
		CategoryID:  &fallbackID,
		Limit:       llmCandidateLimit,
	})
	if err != nil {
		return 0, fmt.Errorf("list llm candidates: %w", err)
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	// One MerchantInput per distinct merchant_key; the query already orders by
	// merchant_key so duplicates arrive together, but a map is clearer than
	// relying on ordering.
	inputs := make([]ai.MerchantInput, 0)
	seen := make(map[string]struct{})
	for _, c := range candidates {
		key := deref(c.MerchantKey) // non-empty: the query filters NULLs out
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		inputs = append(inputs, ai.MerchantInput{
			MerchantKey:  key,
			MerchantName: deref(c.MerchantName),
			SampleName:   c.Name,
			PFCPrimary:   deref(c.PlaidPfcPrimary),
			PFCDetailed:  deref(c.PlaidPfcDetailed),
		})
	}

	options, slugToID, err := selectableCategories(ctx, q, householdID)
	if err != nil {
		return 0, err
	}
	if len(options) == 0 {
		return 0, nil
	}

	decided := 0
	for start := 0; start < len(inputs); start += llmChunkSize {
		end := min(start+llmChunkSize, len(inputs))
		chunk := inputs[start:end]

		verdicts, err := model.CategoriseMerchants(ctx, chunk, options)
		if err != nil {
			return decided, fmt.Errorf("categorise merchants: %w", err)
		}

		// Index confident verdicts by merchant_key so we can decide, for every
		// merchant in the chunk, whether it was placed or abstained.
		placed := make(map[string]uuid.UUID, len(verdicts))
		for _, v := range verdicts {
			if v.Confidence < minConfidence {
				continue
			}
			if id, ok := slugToID[v.Slug]; ok {
				placed[v.MerchantKey] = id
			}
		}

		for _, m := range chunk {
			categoryID, ok := placed[m.MerchantKey]
			if !ok {
				// Abstention (or low confidence, or not returned): pin the
				// merchant to the fallback so it is never sent again, but leave
				// its transactions where they are — visibly uncategorised.
				if err := q.UpsertMerchantCategory(ctx, dbgen.UpsertMerchantCategoryParams{
					HouseholdID: householdID, MerchantKey: m.MerchantKey,
					CategoryID: fallbackID, Source: string(SourceLLM),
				}); err != nil {
					return decided, fmt.Errorf("cache abstention: %w", err)
				}
				continue
			}

			// Confident placement: cache it so future syncs resolve it for free
			// at step 3, then lift every existing fallback transaction for this
			// merchant into the chosen category.
			if err := q.UpsertMerchantCategory(ctx, dbgen.UpsertMerchantCategoryParams{
				HouseholdID: householdID, MerchantKey: m.MerchantKey,
				CategoryID: categoryID, Source: string(SourceLLM),
			}); err != nil {
				return decided, fmt.Errorf("cache llm category: %w", err)
			}
			mk := m.MerchantKey
			if err := q.ApplyMerchantCategory(ctx, dbgen.ApplyMerchantCategoryParams{
				HouseholdID: householdID, MerchantKey: &mk, CategoryID: &categoryID,
			}); err != nil {
				return decided, fmt.Errorf("apply llm category: %w", err)
			}
			decided++
		}
	}

	return decided, nil
}

// selectableCategories returns the categories the model may choose from and a
// slug→id map for applying its answers. Income, transfer, and the fallback
// category itself are excluded: a mislabelled transfer or income would drop out
// of spend totals entirely, so those are left to the deterministic PFC pass and
// to manual choice, never guessed.
func selectableCategories(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID) ([]ai.CategoryOption, map[string]uuid.UUID, error) {
	cats, err := q.ListCategories(ctx, &householdID)
	if err != nil {
		return nil, nil, fmt.Errorf("list categories: %w", err)
	}

	options := make([]ai.CategoryOption, 0, len(cats))
	slugToID := make(map[string]uuid.UUID, len(cats))
	for _, c := range cats {
		if c.IsIncome || c.IsTransfer || c.Slug == "uncategorised" {
			continue
		}
		options = append(options, ai.CategoryOption{Slug: c.Slug, Name: c.Name})
		slugToID[c.Slug] = c.ID
	}
	return options, slugToID, nil
}
