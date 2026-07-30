package merchants

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// ErrNoKeys is returned when a merge is asked for with fewer than two
// descriptors. One descriptor is not a canonicalisation of anything.
var ErrNoKeys = errors.New("a merge needs at least two merchant keys")

// CategoryOutcome reports what reconciliation did, so the UI can say it plainly
// rather than leaving the user to discover a re-filed history on their own.
type CategoryOutcome struct {
	// Applied is the category every descriptor now resolves to, if one was
	// settled on.
	Applied *uuid.UUID
	// Conflict is true when two descriptors carried DIFFERENT manual categories.
	// Nothing is changed in that case: a manual choice is the user's, and this
	// code does not get to pick which of two of them wins.
	Conflict bool
	// FromManual records that the winning category came from a manual choice, so
	// the UI can say the merge honoured it.
	FromManual bool
}

// Merge makes the given descriptor keys one canonical merchant, active
// immediately — this is the confirm path, driven by a person, so the aliases are
// written 'manual' rather than 'suggested'.
//
// It is also the confirm-a-suggestion path: promoting suggested rows and merging
// by hand differ only in where the key list came from, so they share this code
// and therefore share the category reconciliation below.
func Merge(
	ctx context.Context,
	q *dbgen.Queries,
	householdID uuid.UUID,
	keys []string,
	name string,
	entityID *uuid.UUID,
) (uuid.UUID, CategoryOutcome, error) {
	keys = dedupeKeys(keys)
	if entityID == nil && len(keys) < 2 {
		return uuid.Nil, CategoryOutcome{}, ErrNoKeys
	}
	if len(keys) == 0 {
		return uuid.Nil, CategoryOutcome{}, ErrNoKeys
	}

	target := uuid.Nil
	if entityID != nil {
		entity, err := q.GetMerchantEntity(ctx, dbgen.GetMerchantEntityParams{
			ID: *entityID, HouseholdID: householdID,
		})
		if err != nil {
			return uuid.Nil, CategoryOutcome{}, fmt.Errorf("load merchant: %w", err)
		}
		target = entity.ID
	} else {
		if strings.TrimSpace(name) == "" {
			name = DisplayName(keys[0])
		}
		id, err := ensureEntity(ctx, q, householdID, name)
		if err != nil {
			return uuid.Nil, CategoryOutcome{}, err
		}
		target = id
	}

	for _, key := range keys {
		if _, err := q.UpsertMerchantAlias(ctx, dbgen.UpsertMerchantAliasParams{
			HouseholdID: householdID,
			EntityID:    target,
			MerchantKey: key,
			Source:      "manual",
		}); err != nil {
			return uuid.Nil, CategoryOutcome{}, fmt.Errorf("attach %q: %w", key, err)
		}
	}

	outcome, err := reconcileCategories(ctx, q, householdID, target)
	if err != nil {
		return target, outcome, err
	}

	// Merging by hand settles every pending guess about these descriptors.
	if err := q.DeleteSuggestedAliasesForEntity(ctx, dbgen.DeleteSuggestedAliasesForEntityParams{
		HouseholdID: householdID, EntityID: target,
	}); err != nil {
		return target, outcome, fmt.Errorf("clear suggestions: %w", err)
	}
	return target, outcome, nil
}

// Reject dismisses suggested descriptors for an entity and remembers the
// refusal, so the next suggestion pass does not propose the same merge again and
// the review queue actually empties.
//
// Refusals are recorded as PAIRS, because that is how the engine reasons, and
// which pairs depends on whether anything survives the rejection:
//
//   - SOME descriptors stay (a partial confirm — the user unticked two of five).
//     Each rejected key is recorded against each key that STAYS. Nothing is
//     recorded among the rejected keys themselves: unticking HOMEGOODS and
//     HOMEGOODS #0265 together says they are not THE HOME DEPOT, not that they
//     are not each other — and it is exactly that silence which lets them come
//     back as their own proposal next pass.
//
//   - NOTHING stays ("Not the same" on the whole proposal). Then every pair in
//     the group is recorded. There is no survivor to record against, and the
//     alternative is recording nothing at all — which is what this used to do,
//     making the button a no-op beyond clearing the queue: the identical
//     grouping was re-proposed on the very next pass, forever. With
//     per-descriptor selection in the UI the two intents are now distinct, so
//     "Not the same" can mean what it says: none of these belong together.
func Reject(ctx context.Context, q *dbgen.Queries, householdID, entityID uuid.UUID, keys []string) error {
	keys = dedupeKeys(keys)
	if len(keys) == 0 {
		return nil
	}

	existing, err := q.ListMerchantAliases(ctx, dbgen.ListMerchantAliasesParams{
		HouseholdID: householdID, EntityID: entityID,
	})
	if err != nil {
		return fmt.Errorf("list aliases: %w", err)
	}

	rejecting := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		rejecting[k] = struct{}{}
	}

	surviving := make([]string, 0, len(existing))
	for _, a := range existing {
		if _, gone := rejecting[a.MerchantKey]; !gone {
			surviving = append(surviving, a.MerchantKey)
		}
	}

	record := func(a, b string) error {
		if b < a {
			a, b = b, a
		}
		if err := q.RecordMergeRejection(ctx, dbgen.RecordMergeRejectionParams{
			HouseholdID: householdID, KeyA: a, KeyB: b,
		}); err != nil {
			return fmt.Errorf("record rejection: %w", err)
		}
		return nil
	}

	if len(surviving) > 0 {
		for _, key := range keys {
			for _, other := range surviving {
				if err := record(key, other); err != nil {
					return err
				}
			}
		}
	} else {
		for i := range keys {
			for j := i + 1; j < len(keys); j++ {
				if err := record(keys[i], keys[j]); err != nil {
					return err
				}
			}
		}
	}

	for _, key := range keys {
		if err := q.DeleteMerchantAlias(ctx, dbgen.DeleteMerchantAliasParams{
			HouseholdID: householdID, MerchantKey: key,
		}); err != nil {
			return fmt.Errorf("drop suggestion %q: %w", key, err)
		}
	}

	return cleanupEmpty(ctx, q, householdID)
}

// Split detaches a descriptor from its merchant. An over-eager merge has to be
// reversible — it will happen, and a merge nobody can undo is worse than one
// that never happened.
//
// Splitting also records a rejection against the descriptors left behind.
// Without that, the next suggestion pass would simply re-propose the merge the
// user just undid.
func Split(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, key string) error {
	alias, err := q.GetMerchantAliasByKey(ctx, dbgen.GetMerchantAliasByKeyParams{
		HouseholdID: householdID, MerchantKey: key,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%q is not part of a merged merchant", key)
	}
	if err != nil {
		return fmt.Errorf("load alias: %w", err)
	}

	siblings, err := q.ListMerchantAliases(ctx, dbgen.ListMerchantAliasesParams{
		HouseholdID: householdID, EntityID: alias.EntityID,
	})
	if err != nil {
		return fmt.Errorf("list aliases: %w", err)
	}
	for _, other := range siblings {
		if other.MerchantKey == key {
			continue
		}
		a, b := key, other.MerchantKey
		if b < a {
			a, b = b, a
		}
		if err := q.RecordMergeRejection(ctx, dbgen.RecordMergeRejectionParams{
			HouseholdID: householdID, KeyA: a, KeyB: b,
		}); err != nil {
			return fmt.Errorf("record rejection: %w", err)
		}
	}

	if err := q.DeleteMerchantAlias(ctx, dbgen.DeleteMerchantAliasParams{
		HouseholdID: householdID, MerchantKey: key,
	}); err != nil {
		return fmt.Errorf("detach %q: %w", key, err)
	}
	return cleanupEmpty(ctx, q, householdID)
}

// cleanupEmpty retires entities that no longer canonicalise anything. An entity
// with a single alias is just the raw key wearing a hat, and leaving it behind
// would keep a stale display name on a merchant that is no longer merged.
func cleanupEmpty(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID) error {
	if err := q.DeleteEmptyMerchantEntities(ctx, householdID); err != nil {
		return fmt.Errorf("clean up empty merchants: %w", err)
	}
	return nil
}

// CategorySourceRank orders the ways a merchant category can have been decided.
// It encodes the app's standing rule that a manual category is sticky and
// outranks anything learned.
//
// Exported because any operation that collapses two merchant keys into one has to
// decide which of their category mappings survives, and that decision must be the
// same everywhere it is made — a merge here, and the merchant-key backfill in
// cmd/normalise-merchant-keys.
func CategorySourceRank(source string) int {
	switch source {
	case "manual":
		return 3
	case "rule":
		return 2
	default: // llm
		return 1
	}
}

// reconcileCategories settles on one category for a merged merchant.
//
// Fragments of one business are routinely filed differently — each descriptor
// was categorised on its own, sometimes by the model, sometimes by hand. After a
// merge they are one merchant and need one answer.
//
// The rules, in order:
//
//  1. TWO DIFFERENT MANUAL CATEGORIES: change nothing. Both are the user's own
//     choices and neither this code nor a model gets to overrule one with the
//     other. The conflict is reported so the UI can say so.
//  2. Otherwise the highest-ranked mapping wins (manual, then rule, then llm),
//     most recently updated breaking a tie.
//  3. The winner is written to every descriptor that disagrees, and to the
//     entity as its default. UpsertMerchantCategory itself refuses to overwrite
//     a manual mapping with a non-manual one, so rule 1 is enforced twice: once
//     here and once in SQL.
//
// Existing transactions are re-filed with the rewritable 'cache' source, which
// never touches a manually-pinned row.
func reconcileCategories(ctx context.Context, q *dbgen.Queries, householdID, entityID uuid.UUID) (CategoryOutcome, error) {
	aliases, err := q.ListMerchantAliases(ctx, dbgen.ListMerchantAliasesParams{
		HouseholdID: householdID, EntityID: entityID,
	})
	if err != nil {
		return CategoryOutcome{}, fmt.Errorf("list aliases: %w", err)
	}

	keys := make([]string, 0, len(aliases))
	for _, a := range aliases {
		if a.Source == "suggested" {
			continue
		}
		keys = append(keys, a.MerchantKey)
	}
	if len(keys) < 2 {
		return CategoryOutcome{}, nil
	}

	mappings, err := q.ListMerchantCategoryMappingsForKeys(ctx, dbgen.ListMerchantCategoryMappingsForKeysParams{
		HouseholdID: householdID, Column2: keys,
	})
	if err != nil {
		return CategoryOutcome{}, fmt.Errorf("list merchant categories: %w", err)
	}
	if len(mappings) == 0 {
		return CategoryOutcome{}, nil
	}

	manualCats := make(map[uuid.UUID]struct{})
	for _, m := range mappings {
		if m.Source == "manual" {
			manualCats[m.CategoryID] = struct{}{}
		}
	}
	if len(manualCats) > 1 {
		return CategoryOutcome{Conflict: true}, nil
	}

	winner := mappings[0]
	for _, m := range mappings[1:] {
		if better(m, winner) {
			winner = m
		}
	}

	if err := q.SetMerchantEntityDefaultCategory(ctx, dbgen.SetMerchantEntityDefaultCategoryParams{
		ID: entityID, HouseholdID: householdID, DefaultCategoryID: &winner.CategoryID,
	}); err != nil {
		return CategoryOutcome{}, fmt.Errorf("set default category: %w", err)
	}

	current := make(map[string]uuid.UUID, len(mappings))
	for _, m := range mappings {
		current[m.MerchantKey] = m.CategoryID
	}

	for _, key := range keys {
		if have, ok := current[key]; ok && have == winner.CategoryID {
			continue
		}
		if err := q.UpsertMerchantCategory(ctx, dbgen.UpsertMerchantCategoryParams{
			HouseholdID: householdID,
			MerchantKey: key,
			CategoryID:  winner.CategoryID,
			Source:      winner.Source,
		}); err != nil {
			return CategoryOutcome{}, fmt.Errorf("reconcile category for %q: %w", key, err)
		}
		k, cat := key, winner.CategoryID
		if err := q.ApplyMerchantCategoryRewritable(ctx, dbgen.ApplyMerchantCategoryRewritableParams{
			HouseholdID: householdID, MerchantKey: &k, CategoryID: &cat,
		}); err != nil {
			return CategoryOutcome{}, fmt.Errorf("apply category for %q: %w", key, err)
		}
	}

	cat := winner.CategoryID
	return CategoryOutcome{Applied: &cat, FromManual: winner.Source == "manual"}, nil
}

// better reports whether a beats b as the surviving category mapping.
func better(a, b dbgen.ListMerchantCategoryMappingsForKeysRow) bool {
	ra, rb := CategorySourceRank(a.Source), CategorySourceRank(b.Source)
	if ra != rb {
		return ra > rb
	}
	return updatedAt(a).After(updatedAt(b))
}

func updatedAt(row dbgen.ListMerchantCategoryMappingsForKeysRow) time.Time { return row.UpdatedAt }

// Rename changes a merchant's canonical name — the name every report shows for
// it. Nothing else about the merge changes.
func Rename(ctx context.Context, q *dbgen.Queries, householdID, entityID uuid.UUID, name string) (dbgen.MerchantEntity, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return dbgen.MerchantEntity{}, errors.New("a merchant needs a name")
	}
	entity, err := q.RenameMerchantEntity(ctx, dbgen.RenameMerchantEntityParams{
		ID: entityID, HouseholdID: householdID, CanonicalName: name,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.MerchantEntity{}, errors.New("merchant not found")
	}
	return entity, err
}

func dedupeKeys(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
