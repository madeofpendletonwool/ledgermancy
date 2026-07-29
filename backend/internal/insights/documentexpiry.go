package insights

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// document_expiry — a warranty, policy or contract in the vault is about to run
// out.
//
// This is the payoff for storing expiry dates at all. A warranty is worth
// nothing if you discover it lapsed two weeks after the appliance broke, and
// nobody opens a document vault to browse. The insight feed already reaches a
// phone, so filing the paperwork once buys the reminder for free.
//
// Detection is a date comparison — no heuristic, no model. The feed row says
// what expires and when; the vault holds the document that answers what to do
// about it.

const (
	// documentExpiryHorizonDays is how far ahead to look. A month is enough to
	// renew a policy or make a warranty claim without becoming a standing
	// fixture in the feed.
	documentExpiryHorizonDays = 30

	// documentExpiryUrgentDays is when it stops being a note and becomes a
	// deadline. The push threshold in the jobs layer is priority 4, so this is
	// what decides whether an expiry reaches a phone.
	documentExpiryUrgentDays = 7

	// A household filing diligently can have several policies renewing in the
	// same month. Beyond a handful the Documents page, filtered by expiring
	// soon, is the right surface — not one feed row each.
	maxDocumentExpiryCandidates = 5
)

type documentExpiryProducer struct{}

func (documentExpiryProducer) Kind() string { return "document_expiry" }

func (documentExpiryProducer) Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	rows, err := q.ListExpiringDocuments(ctx, dbgen.ListExpiringDocumentsParams{
		HouseholdID: householdID,
		FromDate:    today,
		ToDate:      today.AddDate(0, 0, documentExpiryHorizonDays),
	})
	if err != nil {
		return nil, err
	}

	out := make([]Candidate, 0, len(rows))
	for i, row := range rows {
		if i >= maxDocumentExpiryCandidates {
			break
		}
		// The query filters on a non-null expires_at, so this is belt and braces
		// against a schema change rather than an expected case.
		if row.ExpiresAt == nil {
			continue
		}

		days := int(row.ExpiresAt.Sub(today).Hours() / 24)
		when := fmt.Sprintf("in %d days", days)
		switch days {
		case 0:
			when = "today"
		case 1:
			when = "tomorrow"
		}

		priority := 3
		if days <= documentExpiryUrgentDays {
			priority = 4
		}

		out = append(out, Candidate{
			Kind:     "document_expiry",
			Priority: priority,
			Title:    fmt.Sprintf("%s expires %s", row.Title, when),
			Body: fmt.Sprintf(
				"Your %s \"%s\" expires on %s. The document is in your vault if you need to renew it or make a claim.",
				documentTypeLabel(row.DocType), row.Title,
				row.ExpiresAt.Format("Monday 2 January 2006")),
			Data: map[string]any{
				"document_id":  row.ID.String(),
				"title":        row.Title,
				"doc_type":     row.DocType,
				"expires_at":   row.ExpiresAt.Format(time.DateOnly),
				"days_until":   days,
				"horizon_days": documentExpiryHorizonDays,
			},
			// The expiry date is part of the identity. A renewed policy gets a
			// new expiry and is therefore a new insight, free to push again —
			// while the same unchanged document refreshes one row rather than
			// adding one a day for a month.
			DedupeKey: fmt.Sprintf("document_expiry:%s:%s",
				row.ID, row.ExpiresAt.Format(time.DateOnly)),
		})
	}
	return out, nil
}

// documentTypeLabel renders a doc_type in a sentence. The stored values are
// terse enough to read as jargon ("your warranty" is fine, "your other" is not).
func documentTypeLabel(docType string) string {
	switch docType {
	case "warranty":
		return "warranty"
	case "insurance":
		return "insurance policy"
	case "contract":
		return "contract"
	case "tax":
		return "tax document"
	case "receipt":
		return "receipt"
	case "statement":
		return "statement"
	default:
		return "document"
	}
}
