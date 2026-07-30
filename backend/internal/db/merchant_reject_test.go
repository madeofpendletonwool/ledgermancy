package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/merchants"
)

// "Not the same" on a WHOLE proposal has to stick.
//
// Rejecting every descriptor in a group used to record nothing at all: refusals
// are recorded pairwise against the descriptors that stay, and when none stay
// there was nothing to record against. The queue emptied, so the button looked
// like it worked, and the identical grouping came back on the very next pass —
// which is the exact failure the reject button exists to prevent.
func TestRejectWholeProposalIsRemembered(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	// Two different businesses the containment rule genuinely groups: a
	// place-named merchant and the symphony that shares the place name. The pair
	// has to be one the matcher really proposes, or the "does not come back"
	// assertion below passes for the wrong reason.
	keys := []string{"la crosse", "la crosse symphony orche"}
	for _, key := range keys {
		f.addTx(t, ctx, "2026-02-14", strings.ToUpper(key), key)
	}

	// Baseline: with no refusal on record, a pass really does propose this pair.
	// Without this the test could pass because the matcher never grouped them.
	if _, err := merchants.SuggestHousehold(ctx, f.q, nil, f.householdID, f.userID); err != nil {
		t.Fatalf("SuggestHousehold baseline: %v", err)
	}
	if !proposesPair(f.suggestedGroups(t, ctx), keys[0], keys[1]) {
		t.Fatalf("baseline: the matcher does not group %q with %q, so this test proves nothing", keys[0], keys[1])
	}

	// Refuse the proposal the pass actually made, which means reading its entity
	// back rather than seeding one. A pass REPLACES the pending queue: it drops
	// every suggested alias and then the entities left holding nothing, so an
	// entity created before it no longer exists afterwards. That matters beyond
	// the dangling reference, because Reject reads the entity's aliases to decide
	// whether anything survives the refusal — handing it an id that owns nothing
	// takes the "nothing stays" branch by accident rather than because the user
	// rejected the whole group.
	entityID := f.suggestedEntityFor(t, ctx, keys[0])
	if err := merchants.Reject(ctx, f.q, f.householdID, entityID, keys); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	// The refusal is on record as a pair.
	rejections, err := f.q.ListMergeRejections(ctx, f.householdID)
	if err != nil {
		t.Fatalf("ListMergeRejections: %v", err)
	}
	if len(rejections) != 1 {
		t.Fatalf("want the pair recorded, got %d rejections: %+v", len(rejections), rejections)
	}

	// And the next pass does not bring it back.
	if _, err := merchants.SuggestHousehold(ctx, f.q, nil, f.householdID, f.userID); err != nil {
		t.Fatalf("SuggestHousehold: %v", err)
	}
	if proposesPair(f.suggestedGroups(t, ctx), keys[0], keys[1]) {
		t.Fatalf("re-proposed a grouping the user rejected outright: %v", f.suggestedGroups(t, ctx))
	}
}

// suggestedEntityFor is the entity a pending suggestion attached a descriptor to.
// Tests that reject a proposal need it, because the entity a pass creates is the
// only one whose aliases Reject can see.
func (f *merchantFixture) suggestedEntityFor(t *testing.T, ctx context.Context, key string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(ctx, `
		SELECT entity_id FROM merchant_aliases
		WHERE household_id = $1 AND merchant_key = $2 AND source = 'suggested'`,
		f.householdID, key).Scan(&id); err != nil {
		t.Fatalf("no pending suggestion for %q: %v", key, err)
	}
	return id
}

// proposesPair reports whether any pending group holds both keys.
func proposesPair(groups map[string][]string, a, b string) bool {
	for _, keys := range groups {
		hasA, hasB := false, false
		for _, k := range keys {
			if k == a {
				hasA = true
			}
			if k == b {
				hasB = true
			}
		}
		if hasA && hasB {
			return true
		}
	}
	return false
}

// The partial case must NOT gain rejections among the descriptors unticked
// together — that silence is what lets them regroup on their own, and it is the
// whole point of per-descriptor selection.
func TestRejectPartialLeavesTheUntickedSetFree(t *testing.T) {
	ctx, f := setupMerchantFixture(t)

	stay := []string{"home depot", "the home depot"}
	drop := []string{"homegoods", "homegoods 0265"}

	entity, err := f.q.CreateMerchantEntity(ctx, dbgen.CreateMerchantEntityParams{
		HouseholdID: f.householdID, CanonicalName: "Home Depot",
	})
	if err != nil {
		t.Fatalf("CreateMerchantEntity: %v", err)
	}
	for _, key := range append(append([]string{}, stay...), drop...) {
		f.addTx(t, ctx, "2026-02-14", strings.ToUpper(key), key)
		if _, err := f.q.UpsertMerchantAlias(ctx, dbgen.UpsertMerchantAliasParams{
			HouseholdID: f.householdID, EntityID: entity.ID, MerchantKey: key,
			Source: "suggested", Confidence: decimal.NullDecimal{},
		}); err != nil {
			t.Fatalf("UpsertMerchantAlias: %v", err)
		}
	}

	if err := merchants.Reject(ctx, f.q, f.householdID, entity.ID, drop); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	rejections, err := f.q.ListMergeRejections(ctx, f.householdID)
	if err != nil {
		t.Fatalf("ListMergeRejections: %v", err)
	}
	if len(rejections) != len(stay)*len(drop) {
		t.Errorf("want %d cross rejections, got %d: %+v", len(stay)*len(drop), len(rejections), rejections)
	}
	for _, r := range rejections {
		if strings.Contains(r.KeyA, "homegoods") && strings.Contains(r.KeyB, "homegoods") {
			t.Errorf("recorded a refusal between two descriptors unticked together: %s / %s", r.KeyA, r.KeyB)
		}
	}
}
