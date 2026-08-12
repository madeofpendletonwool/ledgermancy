package categorize

// Structural transfer pairing: match a debit on one of the household's accounts
// to an equal credit on another, within a date window, and treat the two as one
// internal transfer. This is name-independent, which is the whole point — the
// payee-name heuristics in detectTransfer and Plaid's own category both miss
// transfers whose two legs carry different counterparty names (a transfer out
// labelled "ACH CAPITAL ONE - TRANSFER" met by a transfer in labelled "ALTRA
// FEDERAL CREDIT UNION" a day later), and the merchant cache can poison a leg
// with the wrong category so the heuristic never runs. The structural match
// finds the pair regardless of what either side called it.
//
// The rule and its safeguards live in MatchPairs (pure, testable). PairTransfers
// is the DB-backed pass that runs after CategoriseHousehold on each sync.

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// PairCandidate is one transaction the pairer may match.
type PairCandidate struct {
	ID        uuid.UUID
	AccountID uuid.UUID
	Amount    decimal.Decimal // positive = debit (money out), negative = credit (money in)
	Date      time.Time
}

// Pair is one matched transfer: Out is the debit leg, In the credit leg.
// |Out.Amount| == |In.Amount|, on different accounts, within the window.
type Pair struct {
	Out, In PairCandidate
	Amount  decimal.Decimal // the magnitude, positive
}

// PairWindowDays is how far apart the two legs may be to count as one transfer.
// Transfers between a person's own accounts usually clear in a day or two; a
// week covers weekends and the odd ACH lag without opening the window so wide
// that unrelated same-sized transactions pair by coincidence.
const PairWindowDays = 5

// MatchPairs returns the transfer pairs implied by candidates under the
// structural rule. It is deterministic and pure: same candidates in, same pairs
// out. The rule:
//
//   - one debit (Amount > 0) and one credit (Amount < 0)
//   - on DIFFERENT accounts (a same-account in/out is a refund or fee, not a
//     transfer between accounts)
//   - of EXACTLY equal magnitude (transfers between own accounts are exact; a
//     tolerance here is what lets a $699.99 expense pair with a $700 deposit)
//   - dated within PairWindowDays of each other
//
// Income, manual, already-paired, and non-depository/investment legs are
// excluded upstream by ListTransferPairCandidates, so this function only encodes
// the matching itself. A credit is consumed by the closest-dated unmatched
// debit, so two same-day debits split a same-day pair deterministically rather
// than at random.
//
// Every ordering decision falls through to the leg's ID, never to the order the
// caller happened to supply: debits are walked by (date, ID), and a credit tie
// at the same date gap is broken by (ID). That makes the guarantee hold on this
// function's own terms rather than resting on the candidate query's ORDER BY —
// the query is totally ordered too (date, id), but a set of candidates fed in
// any order still yields the same pairs, so a restore converges.
func MatchPairs(candidates []PairCandidate) []Pair {
	type leg = PairCandidate
	debits := make([]leg, 0)
	credits := make([]leg, 0)
	for _, c := range candidates {
		switch {
		case c.Amount.IsPositive():
			debits = append(debits, c)
		case c.Amount.IsNegative():
			credits = append(credits, c)
		}
	}

	window := time.Duration(PairWindowDays) * 24 * time.Hour
	used := make(map[uuid.UUID]bool, len(credits))
	pairs := make([]Pair, 0, len(debits))

	// Match each debit to its closest-dated equal-magnitude credit on a
	// different account. Walking debits in date order and credits by closeness
	// keeps the result stable across input ordering — the same ledger yields the
	// same pairs every run, which is what makes a re-run safe.
	debits = sortByDateThenID(debits)
	for _, d := range debits {
		best := -1
		var bestGap time.Duration
		for i, c := range credits {
			if used[c.ID] {
				continue
			}
			if c.AccountID == d.AccountID {
				continue // a transfer is between two accounts, not within one
			}
			if !c.Amount.Neg().Equal(d.Amount) {
				continue // exact magnitude; a tolerance would let coincidences pair
			}
			gap := absDuration(d.Date.Sub(c.Date))
			if gap > window {
				continue
			}
			// Strictly-closer wins; an exact tie goes to the lower ID. Without
			// the ID tie-break this would resolve by slice position, i.e. by
			// whatever order the caller supplied.
			if best == -1 || gap < bestGap || (gap == bestGap && lessID(c.ID, credits[best].ID)) {
				best, bestGap = i, gap
			}
		}
		if best == -1 {
			continue
		}
		c := credits[best]
		used[c.ID] = true
		pairs = append(pairs, Pair{Out: d, In: c, Amount: d.Amount})
	}
	return pairs
}

// SyncPairLookback is how far back the post-sync pass scans. It only needs to
// reach across a sync boundary — a transfer whose two legs arrived in different
// pages — not the household's whole history. The one-time backfill
// (PairAllHouseholds) uses a much longer window to reach historical transfers.
const SyncPairLookback = 60

// PairTransfers runs the structural pairer over a household's recent
// transactions and persists the matches: each leg is re-categorised to the
// canonical transfer category (transfer-out for the debit, transfer-in for the
// credit) and the pair is recorded. It returns the number of new pairs.
// lookbackDays is how far back to scan (see SyncPairLookback).
//
// Safe to re-run: legs already in transaction_pairs are excluded by the
// candidate query, and a leg in a manual category is excluded too (so a manual
// choice is never overridden).
func PairTransfers(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time, lookbackDays int) (int, error) {
	// Resolve the household's transfer categories by slug. Missing categories
	// (a stripped seed) disable pairing for that household rather than erroring,
	// the same way NewResolver treats a missing transfer-out slug.
	outCat, err := q.GetCategoryBySlug(ctx, dbgen.GetCategoryBySlugParams{
		Slug: "transfer-out", HouseholdID: &householdID,
	})
	if err != nil {
		return 0, nil // no transfer-out category -> nothing to pair into
	}
	inCat, err := q.GetCategoryBySlug(ctx, dbgen.GetCategoryBySlugParams{
		Slug: "transfer-in", HouseholdID: &householdID,
	})
	if err != nil {
		return 0, nil
	}

	// Look back far enough to catch a transfer whose two legs straddle a sync
	// boundary.
	since := now.AddDate(0, 0, -lookbackDays)
	rows, err := q.ListTransferPairCandidates(ctx, dbgen.ListTransferPairCandidatesParams{
		HouseholdID: householdID, Date: since,
	})
	if err != nil {
		return 0, fmt.Errorf("list transfer candidates: %w", err)
	}

	candidates := make([]PairCandidate, 0, len(rows))
	for _, r := range rows {
		candidates = append(candidates, PairCandidate{
			ID: r.ID, AccountID: r.AccountID, Amount: r.Amount, Date: r.Date,
		})
	}
	pairs := MatchPairs(candidates)

	for _, p := range pairs {
		// Re-categorise both legs BEFORE recording the pair. ApplyCategory skips
		// manual rows, so a manual leg keeps its category; the pair is still
		// recorded, which is correct — they are two halves of one transfer
		// regardless of how one leg is labelled. Doing this before the insert is
		// what keeps a failed insert self-healing: the legs are already
		// categorised and remain candidates until the pair is recorded.
		outSource := string(SourcePairing)
		inSource := string(SourcePairing)
		if err := q.ApplyCategory(ctx, dbgen.ApplyCategoryParams{
			ID: p.Out.ID, CategoryID: &outCat.ID, CategorySource: &outSource,
		}); err != nil {
			return len(pairs), fmt.Errorf("apply out-leg category: %w", err)
		}
		if err := q.ApplyCategory(ctx, dbgen.ApplyCategoryParams{
			ID: p.In.ID, CategoryID: &inCat.ID, CategorySource: &inSource,
		}); err != nil {
			return len(pairs), fmt.Errorf("apply in-leg category: %w", err)
		}
		if _, err := q.CreateTransferPair(ctx, dbgen.CreateTransferPairParams{
			HouseholdID: householdID, OutTxnID: p.Out.ID, InTxnID: p.In.ID, Amount: p.Amount,
		}); err != nil {
			return len(pairs), fmt.Errorf("create transfer pair: %w", err)
		}
	}
	return len(pairs), nil
}

// BackfillLookback is how far back the one-time PairAllHouseholds backfill
// scans. It is deliberately long — two years — so that a household's existing
// transfers (which arrived before the pairer shipped) get linked in one pass
// rather than only the last 60 days. Re-running it is cheap and safe: already-
// paired legs are excluded, so it converges to no-ops.
const BackfillLookback = 365 * 2

// PairAllHouseholds runs PairTransfers over every household with a long
// lookback. It is the one-time backfill for transfers that arrived before the
// pairer shipped, and the function to call after deploying the feature. Returns
// the total pairs created across all households.
func PairAllHouseholds(ctx context.Context, q *dbgen.Queries, now time.Time) (int, error) {
	households, err := q.ListHouseholdIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("list households: %w", err)
	}
	total := 0
	for _, hid := range households {
		n, err := PairTransfers(ctx, q, hid, now, BackfillLookback)
		if err != nil {
			return total, fmt.Errorf("pair household %s: %w", hid, err)
		}
		total += n
	}
	return total, nil
}

// SourcePairing marks a category assignment made by the structural transfer
// pairer. It is rewritable (a manual choice, a rule, or the cache can override
// it), which is why ApplyCategory — not the sticky manual write — applies it.
const SourcePairing Source = "pairing"

// sortByDateThenID orders candidates by date, breaking ties on ID so the result
// is a total order. Date alone would leave same-dated legs in input order, which
// is the caller's accident, not a property of the ledger.
func sortByDateThenID(xs []PairCandidate) []PairCandidate {
	// Insertion sort: candidate lists are small (a household's 60-day window)
	// and determinism matters more than big-O here.
	out := make([]PairCandidate, len(xs))
	copy(out, xs)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && lessCandidate(out[j], out[j-1]); j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func lessCandidate(a, b PairCandidate) bool {
	if !a.Date.Equal(b.Date) {
		return a.Date.Before(b.Date)
	}
	return lessID(a.ID, b.ID)
}

// lessID orders two UUIDs by their raw bytes. Any total order will do; this one
// is stable across processes and restores, which is the point.
func lessID(a, b uuid.UUID) bool { return bytes.Compare(a[:], b[:]) < 0 }

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
