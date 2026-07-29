package merchants

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// llmBatchSize is how many undecided descriptors go in one model call. Grouping
// needs every candidate to see every other candidate, so unlike categorisation
// the batch is a real constraint on quality, not just on cost — too small and a
// merchant's two descriptors land in different batches and are never compared.
const llmBatchSize = 60

// llmMinConfidence is the floor below which a model grouping is discarded
// outright rather than queued for review. A proposal nobody believes is not
// worth a person's attention; it is just noise in the queue.
const llmMinConfidence = 0.6

// llmMaxDescriptors bounds one pass. Anything left over is picked up next run.
const llmMaxDescriptors = 240

// merchantGrouper is the slice of the AI client this needs, narrowed so the
// orchestration can be tested with a fake and no endpoint — the same shape
// categorize.LLMCategoriseHousehold uses.
type merchantGrouper interface {
	Enabled() bool
	GroupMerchants(ctx context.Context, descriptors []ai.MerchantDescriptor) ([]ai.MerchantGroup, error)
}

// Group is a proposed canonical merchant: a set of raw keys, and either the
// existing entity they should join or a name for a new one.
type Group struct {
	Keys []string
	// EntityID is set when the group extends an entity that already exists,
	// which is the common case once a household has confirmed a few merges.
	EntityID *uuid.UUID
	// Name is the proposed canonical name, used only when EntityID is nil.
	Name       string
	Confidence float64
	Reason     string
	// Source is 'fuzzy' or 'llm' — which pass proposed this. It is recorded for
	// display; the alias itself is always written as 'suggested' and therefore
	// inert either way.
	Source string
}

type pair struct{ a, b string }

// orderedPair puts a key pair in its canonical order, so a rejection recorded
// one way round is found when the pair is generated the other way round.
func orderedPair(a, b string) pair {
	if b < a {
		return pair{b, a}
	}
	return pair{a, b}
}

// FuzzyGroups runs the deterministic pass over a household's descriptor space.
//
// Keys are joined transitively: if A matches B and B matches C, all three are
// one merchant. That is usually right (descriptor families really do chain
// through a middle form) and it is what lets "amz order" reach "amazon com
// abcd1" via "amazon com" without a direct rule between them.
//
// Transitivity is also how a rejection could be silently undone, so a component
// containing ANY rejected pair is dropped whole rather than proposed. The user
// said those two are different businesses; quietly re-merging them through a
// third key would make the reject button a lie. A dropped component is still
// mergeable by hand, so nothing is lost but the suggestion.
func FuzzyGroups(stats []dbgen.ListMerchantKeyStatsRow, rejected map[pair]struct{}) []Group {
	// Only keys with a real merchant name in them are comparable. A bare
	// "BILL PAYMENT" has nothing to compare on and would match far too much.
	usable := make([]dbgen.ListMerchantKeyStatsRow, 0, len(stats))
	for _, s := range stats {
		if significantTokens(s.MerchantKey) != nil {
			usable = append(usable, s)
		}
	}

	uf := newUnionFind(len(usable))
	// Why each key was joined, kept for the review UI. The first reason to fire
	// for a component is the one shown: it is the strongest, since Compare
	// returns rules in descending order of evidence.
	best := make(map[int]Similarity, len(usable))

	for i := range usable {
		for j := i + 1; j < len(usable); j++ {
			if _, no := rejected[orderedPair(usable[i].MerchantKey, usable[j].MerchantKey)]; no {
				continue
			}
			sim := Compare(usable[i].MerchantKey, usable[j].MerchantKey)
			if !sim.Match {
				continue
			}
			uf.union(i, j)
			root := uf.find(i)
			if prev, ok := best[root]; !ok || sim.Confidence > prev.Confidence {
				best[root] = sim
			}
		}
	}

	members := make(map[int][]int)
	for i := range usable {
		root := uf.find(i)
		members[root] = append(members[root], i)
	}

	groups := make([]Group, 0)
	for root, idxs := range members {
		if len(idxs) < 2 {
			continue
		}
		if componentHasRejection(usable, idxs, rejected) {
			continue
		}

		// An existing entity in the component is the one to extend. Two different
		// entities means the component spans a merge the user already made
		// differently — that is a decision to leave alone, not to overrule.
		var entityID *uuid.UUID
		conflict := false
		for _, i := range idxs {
			if usable[i].EntityID == nil {
				continue
			}
			if entityID != nil && *entityID != *usable[i].EntityID {
				conflict = true
				break
			}
			entityID = usable[i].EntityID
		}
		if conflict {
			continue
		}

		// Everything already sitting on the target entity is settled; only the
		// keys not yet attached are a proposal.
		keys := make([]string, 0, len(idxs))
		for _, i := range idxs {
			if usable[i].EntityID != nil {
				continue
			}
			keys = append(keys, usable[i].MerchantKey)
		}
		if len(keys) == 0 || (entityID == nil && len(keys) < 2) {
			continue
		}
		sort.Strings(keys)

		sim := best[root]
		groups = append(groups, Group{
			Keys:       keys,
			EntityID:   entityID,
			Name:       DisplayName(dominantKey(usable, idxs)),
			Confidence: sim.Confidence,
			Reason:     sim.Reason,
			Source:     "fuzzy",
		})
	}

	// Stable output so a pass is reproducible and tests are not order-dependent.
	sort.Slice(groups, func(i, j int) bool { return groups[i].Keys[0] < groups[j].Keys[0] })
	return groups
}

// componentHasRejection reports whether any two members of a component are a
// rejected pair — the transitivity escape hatch described on FuzzyGroups.
func componentHasRejection(rows []dbgen.ListMerchantKeyStatsRow, idxs []int, rejected map[pair]struct{}) bool {
	for a := range idxs {
		for b := a + 1; b < len(idxs); b++ {
			if _, no := rejected[orderedPair(rows[idxs[a]].MerchantKey, rows[idxs[b]].MerchantKey)]; no {
				return true
			}
		}
	}
	return false
}

// dominantKey is the component's most-used descriptor, which is the best basis
// for a display name: the form the merchant bills under most often is the form
// the user recognises.
func dominantKey(rows []dbgen.ListMerchantKeyStatsRow, idxs []int) string {
	bestIdx := idxs[0]
	for _, i := range idxs[1:] {
		if rows[i].TransactionCount > rows[bestIdx].TransactionCount {
			bestIdx = i
		}
	}
	return rows[bestIdx].MerchantKey
}

// SuggestHousehold runs both passes and records their output as SUGGESTED
// aliases — rows that are pending review and, by the contract every reporting
// query honours, change nothing until a person confirms them.
//
// Returns the number of keys newly proposed.
func SuggestHousehold(
	ctx context.Context,
	q *dbgen.Queries,
	model merchantGrouper,
	householdID, userID uuid.UUID,
) (int, error) {
	stats, err := q.ListMerchantKeyStats(ctx, dbgen.ListMerchantKeyStatsParams{
		HouseholdID: householdID, UserID: userID,
	})
	if err != nil {
		return 0, fmt.Errorf("list merchant keys: %w", err)
	}
	if len(stats) < 2 {
		return 0, nil
	}

	rejections, err := q.ListMergeRejections(ctx, householdID)
	if err != nil {
		return 0, fmt.Errorf("list merge rejections: %w", err)
	}
	rejected := make(map[pair]struct{}, len(rejections))
	for _, r := range rejections {
		rejected[orderedPair(r.KeyA, r.KeyB)] = struct{}{}
	}

	groups := FuzzyGroups(stats, rejected)
	applied, err := applyGroups(ctx, q, householdID, groups)
	if err != nil {
		return applied, err
	}

	// The LLM only ever sees what the deterministic pass could not place. That
	// keeps the call small, and it keeps the model off the cases string rules
	// already answer correctly and for free.
	claimed := make(map[string]struct{})
	for _, g := range groups {
		for _, k := range g.Keys {
			claimed[k] = struct{}{}
		}
	}
	llmGroups, err := llmSuggest(ctx, model, stats, claimed, rejected)
	if err != nil {
		// A model failure is not a job failure: the deterministic suggestions
		// already landed and are the more valuable half.
		return applied, nil
	}
	more, err := applyGroups(ctx, q, householdID, llmGroups)
	return applied + more, err
}

// llmSuggest asks the model about descriptors neither the fuzzy pass nor a
// previous confirmation has placed.
func llmSuggest(
	ctx context.Context,
	model merchantGrouper,
	stats []dbgen.ListMerchantKeyStatsRow,
	claimed map[string]struct{},
	rejected map[pair]struct{},
) ([]Group, error) {
	if model == nil || !model.Enabled() {
		return nil, nil
	}

	residue := make([]dbgen.ListMerchantKeyStatsRow, 0, len(stats))
	for _, s := range stats {
		if _, taken := claimed[s.MerchantKey]; taken {
			continue
		}
		// A key already attached to an entity is settled, and one with no
		// significant tokens has nothing to reason about.
		if s.EntityID != nil || significantTokens(s.MerchantKey) == nil {
			continue
		}
		residue = append(residue, s)
	}
	if len(residue) < 2 {
		return nil, nil
	}
	if len(residue) > llmMaxDescriptors {
		residue = residue[:llmMaxDescriptors]
	}

	var out []Group
	for start := 0; start < len(residue); start += llmBatchSize {
		end := min(start+llmBatchSize, len(residue))
		batch := residue[start:end]

		inputs := make([]ai.MerchantDescriptor, 0, len(batch))
		for _, s := range batch {
			inputs = append(inputs, ai.MerchantDescriptor{
				Key:    s.MerchantKey,
				Sample: s.SampleName,
				Count:  s.TransactionCount,
				Total:  s.TotalAmount.Round(2).StringFixed(2),
			})
		}

		proposed, err := model.GroupMerchants(ctx, inputs)
		if err != nil {
			return out, err
		}

		sent := make(map[string]struct{}, len(inputs))
		for _, in := range inputs {
			sent[in.Key] = struct{}{}
		}
		used := make(map[string]struct{})

		for _, g := range proposed {
			if g.Confidence < llmMinConfidence {
				continue
			}
			// Keep only keys we actually sent and have not already handed to
			// another group. A model that invents or reformats a key would
			// otherwise create an alias for a descriptor that does not exist.
			keys := make([]string, 0, len(g.Keys))
			for _, k := range g.Keys {
				k = strings.TrimSpace(k)
				if _, ok := sent[k]; !ok {
					continue
				}
				if _, dup := used[k]; dup {
					continue
				}
				keys = append(keys, k)
			}
			if len(keys) < 2 {
				continue
			}
			if pairRejected(keys, rejected) {
				continue
			}
			for _, k := range keys {
				used[k] = struct{}{}
			}
			sort.Strings(keys)

			name := strings.TrimSpace(g.Name)
			if name == "" {
				name = DisplayName(keys[0])
			}
			reason := strings.TrimSpace(g.Reason)
			if reason == "" {
				reason = "suggested by AI as one business"
			}
			out = append(out, Group{
				Keys:       keys,
				Name:       name,
				Confidence: g.Confidence,
				Reason:     reason,
				Source:     "llm",
			})
		}
	}
	return out, nil
}

// pairRejected reports whether any pair inside a proposed group has been
// rejected before. Same rule the fuzzy pass applies: one rejection retires the
// whole proposal rather than quietly re-forming it around the refusal.
func pairRejected(keys []string, rejected map[pair]struct{}) bool {
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if _, no := rejected[orderedPair(keys[i], keys[j])]; no {
				return true
			}
		}
	}
	return false
}

// applyGroups writes groups as suggested aliases, creating an entity per group
// that does not already have one. A brand-new entity here has NO active aliases,
// so it is invisible to every report until the first confirmation — an entity is
// only as real as its active aliases.
func applyGroups(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, groups []Group) (int, error) {
	applied := 0
	for _, g := range groups {
		entityID := g.EntityID
		if entityID == nil {
			id, err := ensureEntity(ctx, q, householdID, g.Name)
			if err != nil {
				return applied, err
			}
			entityID = &id
		}

		conf := decimal.NullDecimal{
			Decimal: decimal.NewFromFloat(g.Confidence).Round(4),
			Valid:   g.Confidence > 0,
		}
		for _, key := range g.Keys {
			if _, err := q.UpsertMerchantAlias(ctx, dbgen.UpsertMerchantAliasParams{
				HouseholdID: householdID,
				EntityID:    *entityID,
				MerchantKey: key,
				Source:      "suggested",
				Confidence:  conf,
			}); err != nil {
				return applied, fmt.Errorf("suggest alias %q: %w", key, err)
			}
			applied++
		}
	}
	return applied, nil
}

// ensureEntity finds or creates an entity by canonical name. Names are unique
// per household, so a second merchant that normalises to an existing name is
// disambiguated rather than silently folded into it.
func ensureEntity(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, name string) (uuid.UUID, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Merchant"
	}
	existing, err := q.FindMerchantEntityByName(ctx, dbgen.FindMerchantEntityByNameParams{
		HouseholdID: householdID, Name: name,
	})
	if err == nil {
		return existing.ID, nil
	}

	created, err := q.CreateMerchantEntity(ctx, dbgen.CreateMerchantEntityParams{
		HouseholdID:   householdID,
		CanonicalName: name,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("create merchant entity %q: %w", name, err)
	}
	return created.ID, nil
}

// unionFind is the standard disjoint-set structure, used to turn pairwise
// matches into merchant components.
type unionFind struct{ parent []int }

func newUnionFind(n int) *unionFind {
	p := make([]int, n)
	for i := range p {
		p[i] = i
	}
	return &unionFind{parent: p}
}

func (u *unionFind) find(i int) int {
	for u.parent[i] != i {
		u.parent[i] = u.parent[u.parent[i]]
		i = u.parent[i]
	}
	return i
}

func (u *unionFind) union(a, b int) {
	ra, rb := u.find(a), u.find(b)
	if ra != rb {
		u.parent[rb] = ra
	}
}
