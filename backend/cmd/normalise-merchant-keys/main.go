// Command normalise-merchant-keys recomputes transactions.merchant_key with
// plaid.MerchantKey for rows that never went through it.
//
// The CSV import path used to derive its key by lower-casing the description and
// nothing else, so every store number, processor prefix and order id the bank
// printed became part of the key. One business ended up fragmented across dozens
// of keys — twenty-three separate "merchants" for AMAZON MKTPL*<order id>,
// eighteen for KWIK TRIP #<store> — and the merchant canonicalisation feature was
// left guessing its way back to an answer the normaliser already knew.
//
// The import path is fixed; this repairs the rows it already wrote. It is a
// command rather than a migration because the transform lives in Go, and
// reimplementing MerchantKey's regexes in SQL would leave two copies to keep in
// step.
//
// It defaults to a dry run. Pass --apply to write, and take a database snapshot
// first: this rewrites keys that merchant_category_map, merchant_aliases and
// recurring_overrides all point at.
//
// It must move zero money. Nothing here touches an amount, a date or a category
// assignment on a transaction — only the merchant handle and the per-key tables
// that hang off it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/merchants"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/plaid"
)

func main() {
	apply := flag.Bool("apply", false, "write the changes; without this the command only reports what it would do")
	databaseURL := flag.String("database-url", os.Getenv("DATABASE_URL"), "connection string; defaults to $DATABASE_URL")
	flag.Parse()

	if *databaseURL == "" {
		fmt.Fprintln(os.Stderr, "error: set DATABASE_URL or pass --database-url")
		os.Exit(1)
	}

	if err := run(context.Background(), *databaseURL, *apply); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// txnRow is one transaction's merchant identity, with the household it belongs to
// — every uniqueness constraint this command has to respect is per-household.
type txnRow struct {
	ID          uuid.UUID
	HouseholdID uuid.UUID
	OldKey      string
	NewKey      string
}

// run takes the connection string directly rather than going through
// config.Load: a maintenance command that only reads and rewrites merchant keys
// has no business demanding an encryption key, a session secret and a valid
// backup configuration before it will start.
func run(ctx context.Context, databaseURL string, apply bool) error {
	pool, err := db.Connect(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	rows, before, after, err := loadChanges(ctx, pool)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("Every merchant_key is already normalised. Nothing to do.")
		return nil
	}

	report(rows, before, after)

	if !apply {
		fmt.Println("\nDry run — nothing was written. Re-run with --apply to commit.")
		return nil
	}

	// One transaction for the whole rewrite. A half-applied backfill would leave
	// merchant_aliases pointing at keys no transaction carries any more, which
	// reads in the UI as merchants that silently lost their history.
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Households in a stable order so a re-run behaves identically.
	for _, householdID := range households(rows) {
		mapping := keyMapping(rows, householdID)

		if err := remapCategoryMap(ctx, tx, householdID, mapping); err != nil {
			return fmt.Errorf("remap merchant_category_map: %w", err)
		}
		if err := remapAliases(ctx, tx, householdID, mapping); err != nil {
			return fmt.Errorf("remap merchant_aliases: %w", err)
		}
		if err := remapRecurringOverrides(ctx, tx, householdID, mapping); err != nil {
			return fmt.Errorf("remap recurring_overrides: %w", err)
		}
	}

	updated, err := updateTransactions(ctx, tx, rows)
	if err != nil {
		return fmt.Errorf("update transactions: %w", err)
	}

	// Only entities left with no aliases at all. A one-alias entity is a rename
	// the user made and is deliberately kept — see DeleteEmptyMerchantEntities.
	for _, householdID := range households(rows) {
		if _, err := tx.Exec(ctx, `
			DELETE FROM merchant_entities e
			WHERE e.household_id = $1
			  AND NOT EXISTS (SELECT 1 FROM merchant_aliases ma WHERE ma.entity_id = e.id)`,
			householdID); err != nil {
			return fmt.Errorf("retire empty merchant entities: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Printf("\nApplied. %d transactions updated.\n", updated)
	return nil
}

// loadChanges reads every transaction carrying a merchant_key, recomputes it, and
// returns the rows whose key actually changes along with the distinct key count
// before and after.
//
// The two counts are the honest measure of what this does, and neither is
// derivable from the change list alone: a key that normalises to itself is not a
// change, but it is very often the DESTINATION other keys collapse onto — "kwik
// trip" absorbing seventeen "kwik trip #<store>" rows never appears as a change
// while being the whole point of the exercise.
//
// The recompute reads merchant_name and name — the raw text the bank supplied — so
// it is derived from the source, not from the already-damaged key. Plaid-synced
// rows recompute to exactly what they hold today (sync.go feeds MerchantKey the
// same two fields), so they fall out here and are left alone.
func loadChanges(ctx context.Context, pool *pgxpool.Pool) (changes []txnRow, before, after int, err error) {
	rows, err := pool.Query(ctx, `
		SELECT t.id, u.household_id, t.merchant_key, COALESCE(t.merchant_name, ''), t.name
		FROM transactions t
		JOIN accounts a    ON a.id = t.account_id
		JOIN plaid_items i ON i.id = a.plaid_item_id
		JOIN users u       ON u.id = i.user_id
		WHERE t.merchant_key IS NOT NULL
		ORDER BY t.id`)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()

	oldKeys := map[string]struct{}{}
	newKeys := map[string]struct{}{}

	for rows.Next() {
		var (
			id, householdID      uuid.UUID
			oldKey, mName, tName string
		)
		if err := rows.Scan(&id, &householdID, &oldKey, &mName, &tName); err != nil {
			return nil, 0, 0, err
		}
		oldKeys[oldKey] = struct{}{}

		newKey := plaid.MerchantKey(mName, tName)
		// An empty result means nothing meaningful survives normalisation. The
		// existing key is still a usable handle, so keep it rather than dropping
		// the row's merchant identity entirely.
		if newKey == "" {
			newKey = oldKey
		}
		newKeys[newKey] = struct{}{}

		if newKey != oldKey {
			changes = append(changes, txnRow{ID: id, HouseholdID: householdID, OldKey: oldKey, NewKey: newKey})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, err
	}
	return changes, len(oldKeys), len(newKeys), nil
}

func report(rows []txnRow, before, after int) {
	byHousehold := map[uuid.UUID]map[string]string{}
	for _, r := range rows {
		if byHousehold[r.HouseholdID] == nil {
			byHousehold[r.HouseholdID] = map[string]string{}
		}
		byHousehold[r.HouseholdID][r.OldKey] = r.NewKey
	}

	fmt.Printf("%d transactions across %d household(s) will get a new merchant_key.\n",
		len(rows), len(byHousehold))
	fmt.Printf("Distinct merchant keys: %d -> %d (%d fewer).\n", before, after, before-after)

	for _, householdID := range households(rows) {
		mapping := byHousehold[householdID]

		// Group by destination so the collapses — the whole point of this — are
		// visible, rather than a flat list of renames.
		merged := map[string][]string{}
		for old, next := range mapping {
			merged[next] = append(merged[next], old)
		}

		var collapses []string
		for next, olds := range merged {
			if len(olds) > 1 {
				collapses = append(collapses, next)
			}
		}
		sort.Strings(collapses)

		fmt.Printf("\nHousehold %s: %d keys rewritten, %d of them collapsing into %d merchant(s).\n",
			householdID, len(mapping), countIn(merged, collapses), len(collapses))
		for _, next := range collapses {
			olds := merged[next]
			sort.Strings(olds)
			fmt.Printf("  %-32s <- %s\n", next, strings.Join(olds, ", "))
		}
	}
}

func countIn(merged map[string][]string, keys []string) int {
	n := 0
	for _, k := range keys {
		n += len(merged[k])
	}
	return n
}

func households(rows []txnRow) []uuid.UUID {
	seen := map[uuid.UUID]struct{}{}
	var out []uuid.UUID
	for _, r := range rows {
		if _, dup := seen[r.HouseholdID]; dup {
			continue
		}
		seen[r.HouseholdID] = struct{}{}
		out = append(out, r.HouseholdID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// keyMapping is the old→new key map for one household.
func keyMapping(rows []txnRow, householdID uuid.UUID) map[string]string {
	out := map[string]string{}
	for _, r := range rows {
		if r.HouseholdID == householdID {
			out[r.OldKey] = r.NewKey
		}
	}
	return out
}

// target resolves a key through the mapping, leaving unmapped keys alone. A key
// that is not being rewritten still has to take part in collision resolution:
// "aldi" keeps its own key while "aldi 72054" moves onto it, and the winner has
// to be picked across both.
func target(mapping map[string]string, key string) string {
	if next, ok := mapping[key]; ok {
		return next
	}
	return key
}

// group is one destination key and the rows converging on it.
type group[T any] struct {
	target  string
	members []T
}

// regroup buckets rows by destination key and drops the buckets that need no
// work: a single member already sitting on its destination is untouched.
func regroup[T any](members []T, keyOf func(T) string, mapping map[string]string) []group[T] {
	byTarget := map[string][]T{}
	for _, m := range members {
		t := target(mapping, keyOf(m))
		byTarget[t] = append(byTarget[t], m)
	}

	var out []group[T]
	for t, ms := range byTarget {
		if len(ms) == 1 && keyOf(ms[0]) == t {
			continue
		}
		out = append(out, group[T]{target: t, members: ms})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].target < out[j].target })
	return out
}

type categoryRow struct {
	key        string
	categoryID uuid.UUID
	source     string
	confidence *float64
	updatedAt  int64
}

// remapCategoryMap moves category decisions onto the new keys, resolving the
// collisions the collapse creates.
//
// Two old keys landing on one new key may disagree about the category. The winner
// is decided by the same precedence a merge uses — manual outranks a rule, a rule
// outranks the model, most recently updated breaks a tie — via
// merchants.CategorySourceRank, so a hand-filed category is never quietly
// replaced by a guess.
func remapCategoryMap(ctx context.Context, tx pgx.Tx, householdID uuid.UUID, mapping map[string]string) error {
	rows, err := tx.Query(ctx, `
		SELECT merchant_key, category_id, source, confidence, EXTRACT(EPOCH FROM updated_at)::bigint
		FROM merchant_category_map WHERE household_id = $1`, householdID)
	if err != nil {
		return err
	}
	var all []categoryRow
	for rows.Next() {
		var c categoryRow
		if err := rows.Scan(&c.key, &c.categoryID, &c.source, &c.confidence, &c.updatedAt); err != nil {
			rows.Close()
			return err
		}
		all = append(all, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, g := range regroup(all, func(c categoryRow) string { return c.key }, mapping) {
		winner := g.members[0]
		for _, c := range g.members[1:] {
			rc, rw := merchants.CategorySourceRank(c.source), merchants.CategorySourceRank(winner.source)
			if rc > rw || (rc == rw && c.updatedAt > winner.updatedAt) {
				winner = c
			}
		}
		if err := replaceGroup(ctx, tx, householdID, "merchant_category_map", g,
			func(c categoryRow) string { return c.key },
			`INSERT INTO merchant_category_map (household_id, merchant_key, category_id, source, confidence)
			 VALUES ($1, $2, $3, $4, $5)`,
			g.target, winner.categoryID, winner.source, winner.confidence); err != nil {
			return err
		}
	}
	return nil
}

type aliasRow struct {
	key      string
	entityID uuid.UUID
	source   string
	conf     *float64
}

// remapAliases moves merchant groupings onto the new keys.
//
// A collision here can mean two things. Usually the colliding aliases already
// belong to the same entity — twenty-three Amazon descriptors the user grouped by
// hand, now collapsing to one key — and the merge is a no-op beyond dropping the
// duplicates. Occasionally they belong to DIFFERENT entities, which means
// normalisation decided two descriptors are one key while the user had filed them
// as two businesses. That conflict is reported rather than resolved silently; the
// entity with more aliases wins, on the reasoning that it is the more established
// grouping, and a confirmed alias always outranks a mere suggestion.
func remapAliases(ctx context.Context, tx pgx.Tx, householdID uuid.UUID, mapping map[string]string) error {
	rows, err := tx.Query(ctx, `
		SELECT merchant_key, entity_id, source, confidence
		FROM merchant_aliases WHERE household_id = $1`, householdID)
	if err != nil {
		return err
	}
	var all []aliasRow
	for rows.Next() {
		var a aliasRow
		if err := rows.Scan(&a.key, &a.entityID, &a.source, &a.conf); err != nil {
			rows.Close()
			return err
		}
		all = append(all, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	weight := map[uuid.UUID]int{}
	for _, a := range all {
		weight[a.entityID]++
	}

	for _, g := range regroup(all, func(a aliasRow) string { return a.key }, mapping) {
		winner := g.members[0]
		conflicting := false
		for _, a := range g.members[1:] {
			if a.entityID != winner.entityID {
				conflicting = true
			}
			if aliasBeats(a, winner, weight) {
				winner = a
			}
		}
		if conflicting {
			fmt.Printf("  ! %q: descriptors were filed under different merchants; keeping the larger grouping\n", g.target)
		}
		if err := replaceGroup(ctx, tx, householdID, "merchant_aliases", g,
			func(a aliasRow) string { return a.key },
			`INSERT INTO merchant_aliases (household_id, entity_id, merchant_key, source, confidence)
			 VALUES ($1, $2, $3, $4, $5)`,
			winner.entityID, g.target, winner.source, winner.conf); err != nil {
			return err
		}
	}
	return nil
}

// aliasBeats prefers a confirmed alias over a suggestion, then the entity with
// more aliases behind it.
func aliasBeats(a, b aliasRow, weight map[uuid.UUID]int) bool {
	aSuggested, bSuggested := a.source == "suggested", b.source == "suggested"
	if aSuggested != bSuggested {
		return bSuggested
	}
	return weight[a.entityID] > weight[b.entityID]
}

// remapRecurringOverrides moves subscription suppressions onto the new keys. A
// suppression carries no state beyond its presence, so any surviving row is as
// good as any other; the label from the row with the longest one is kept because
// it is the most descriptive.
func remapRecurringOverrides(ctx context.Context, tx pgx.Tx, householdID uuid.UUID, mapping map[string]string) error {
	type overrideRow struct {
		key   string
		label string
	}
	rows, err := tx.Query(ctx, `
		SELECT merchant_key, merchant_label FROM recurring_overrides WHERE household_id = $1`, householdID)
	if err != nil {
		return err
	}
	var all []overrideRow
	for rows.Next() {
		var o overrideRow
		if err := rows.Scan(&o.key, &o.label); err != nil {
			rows.Close()
			return err
		}
		all = append(all, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, g := range regroup(all, func(o overrideRow) string { return o.key }, mapping) {
		winner := g.members[0]
		for _, o := range g.members[1:] {
			if len(o.label) > len(winner.label) {
				winner = o
			}
		}
		if err := replaceGroup(ctx, tx, householdID, "recurring_overrides", g,
			func(o overrideRow) string { return o.key },
			`INSERT INTO recurring_overrides (household_id, merchant_key, merchant_label) VALUES ($1, $2, $3)`,
			g.target, winner.label); err != nil {
			return err
		}
	}
	return nil
}

// replaceGroup deletes every row a group came from and writes the winner on the
// destination key.
//
// Delete-then-insert rather than an UPDATE because several source rows converge on
// one destination: updating them in place would trip the
// UNIQUE (household_id, merchant_key) constraint partway through, and which row
// tripped it would depend on the order they happened to come back in.
func replaceGroup[T any](
	ctx context.Context,
	tx pgx.Tx,
	householdID uuid.UUID,
	table string,
	g group[T],
	keyOf func(T) string,
	insertSQL string,
	insertArgs ...any,
) error {
	keys := make([]string, 0, len(g.members))
	for _, m := range g.members {
		keys = append(keys, keyOf(m))
	}

	if _, err := tx.Exec(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE household_id = $1 AND merchant_key = ANY($2)`, table),
		householdID, keys); err != nil {
		return err
	}

	args := append([]any{householdID}, insertArgs...)
	if _, err := tx.Exec(ctx, insertSQL, args...); err != nil {
		return err
	}
	return nil
}

// updateTransactions writes the new keys, batched by destination so one statement
// covers every row moving to the same merchant.
func updateTransactions(ctx context.Context, tx pgx.Tx, rows []txnRow) (int64, error) {
	byKey := map[string][]uuid.UUID{}
	for _, r := range rows {
		byKey[r.NewKey] = append(byKey[r.NewKey], r.ID)
	}

	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var total int64
	for _, k := range keys {
		tag, err := tx.Exec(ctx,
			`UPDATE transactions SET merchant_key = $1 WHERE id = ANY($2)`, k, byKey[k])
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
	}
	return total, nil
}
