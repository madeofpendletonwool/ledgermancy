package insights

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/documents"
)

// receipt_match — a receipt in the vault now matches a charge that has posted.
//
// This exists because of a timing problem that makes the obvious flow the
// broken one. You scan a receipt at the register; the card charge posts two or
// three days later. At the moment of scanning there is nothing to match, so a
// match run only at scan time finds nothing and the receipt sits unattached
// forever. The fix is to look again once the charge arrives, which is exactly
// what an hourly per-household producer is for.
//
// It is entirely deterministic. The AI read the receipt *once*, at upload, and
// its answer was cached on the row; this producer only compares an amount and a
// date in SQL. No image is re-decrypted and nothing is re-sent anywhere — the
// nudge is free, in every sense that matters.

const (
	// How far back to keep looking. Long enough to cover a slow institution and
	// a holiday weekend; short enough that a receipt whose charge never arrived
	// stops being mentioned rather than becoming furniture in the feed.
	receiptMatchLookbackDays = 45

	// Only a single unambiguous candidate is raised. Two transactions for the
	// same amount within a few days is a genuinely ambiguous case — a coffee
	// bought twice, a split payment — and guessing between them is how a receipt
	// ends up filed against the wrong charge. The Documents page shows the full
	// candidate list; the feed only speaks when there is one answer.
	receiptMatchRequiredCandidates = 1

	// A handful per pass. This is a convenience nudge, not something to fill a
	// feed with.
	maxReceiptMatchCandidates = 3
)

type receiptMatchProducer struct{}

func (receiptMatchProducer) Kind() string { return "receipt_match" }

func (receiptMatchProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	// Shared receipts only, read but never attached to a transaction. Attaching
	// one to anything removes it from this set for good, so a matched receipt is
	// never reconsidered.
	pending, err := q.ListReceiptsAwaitingMatch(ctx, dbgen.ListReceiptsAwaitingMatchParams{
		HouseholdID: householdID,
		Since:       today.AddDate(0, 0, -receiptMatchLookbackDays),
	})
	if err != nil {
		return nil, err
	}

	out := make([]Candidate, 0, len(pending))
	for _, receipt := range pending {
		if len(out) >= maxReceiptMatchCandidates {
			break
		}
		if !receipt.ExtractedAmount.Valid {
			continue // the query filters these out; belt and braces
		}

		// Anchor on the date printed on the receipt, falling back to when it was
		// filed — the same order the API's matcher uses.
		anchor := receipt.CreatedAt
		if receipt.ExtractedDate != nil {
			anchor = *receipt.ExtractedDate
		}

		// sharedUser: the feed is a household surface, so this must only see
		// transactions from shared institutions. See producers.go.
		matches, err := q.MatchTransactionsForReceipt(ctx, dbgen.MatchTransactionsForReceiptParams{
			HouseholdID: householdID,
			UserID:      sharedUser,
			FromDate:    anchor.AddDate(0, 0, -documents.MatchWindowDays),
			ToDate:      anchor.AddDate(0, 0, documents.MatchWindowDays),
			Amount:      receipt.ExtractedAmount.Decimal.Abs(),
			OnDate:      anchor,
		})
		if err != nil {
			return nil, err
		}
		if len(matches) != receiptMatchRequiredCandidates {
			continue
		}

		match := matches[0]
		amount := match.Amount.Round(2)

		merchant := match.Label
		if receipt.ExtractedMerchant != nil && *receipt.ExtractedMerchant != "" {
			merchant = *receipt.ExtractedMerchant
		}

		out = append(out, Candidate{
			Kind: "receipt_match",
			// Deliberately below the push threshold (4). Filing a receipt is
			// housekeeping — useful to find in the feed, not worth a phone
			// buzzing in someone's pocket.
			Priority: 2,
			Title:    fmt.Sprintf("Receipt matches a %s charge", money(amount)),
			Body: fmt.Sprintf(
				"Your receipt \"%s\" for %s looks like the %s charge from %s that has now posted. Open it to attach the two.",
				receipt.Title, money(amount), merchant, match.Date.Format("2 January")),
			Data: map[string]any{
				"document_id":    receipt.ID.String(),
				"document_title": receipt.Title,
				"transaction_id": match.ID.String(),
				"amount":         amount.StringFixed(2),
				"merchant":       merchant,
				"posted_date":    match.Date.Format(time.DateOnly),
			},
			// Keyed on the document alone, not the transaction. A receipt should
			// raise this at most once: if the user dismisses it, they have
			// decided not to file it, and re-proposing the same pairing next hour
			// under a new key would be nagging.
			DedupeKey: fmt.Sprintf("receipt_match:%s", receipt.ID),
		})
	}
	return out, nil
}
