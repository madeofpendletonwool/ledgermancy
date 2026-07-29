package documents

import "time"

// Document types. These mirror the CHECK constraint on documents.doc_type —
// adding one means a migration, which is deliberate: the retention table below
// has to gain a row at the same time or the new type silently gets the default.
const (
	TypeReceipt   = "receipt"
	TypeTax       = "tax"
	TypeWarranty  = "warranty"
	TypeInsurance = "insurance"
	TypeContract  = "contract"
	TypeStatement = "statement"
	TypeOther     = "other"
)

// ValidTypes is the set an upload is checked against, so a bad doc_type is a
// 400 with a readable message rather than a constraint violation surfacing as
// a 500.
var ValidTypes = map[string]bool{
	TypeReceipt:   true,
	TypeTax:       true,
	TypeWarranty:  true,
	TypeInsurance: true,
	TypeContract:  true,
	TypeStatement: true,
	TypeOther:     true,
}

// ocrEligibleTypes is the allowlist of document types that may be sent to an
// AI provider for field extraction.
//
// This is the narrowest gate in the vault and it is deliberately an allowlist
// of exactly one entry, not a blocklist of the scary ones. The difference
// matters: a blocklist means any doc_type added by a future migration is
// sendable until somebody remembers to add it, and "fails open" is not an
// acceptable default for the single feature that moves a user's paperwork off
// their own machine.
//
// A receipt is a purchase record — a merchant, a total and a date, all of which
// already exist in the transaction it belongs to. A tax document is a name, an
// address, an SSN and a full financial picture. Those are not the same
// exposure, and the app must not treat them as one because both happen to be
// JPEGs. `other` is excluded for the same reason: it is the bucket unsorted
// scans land in, so eligibility there would be eligibility for anything the
// user had not got round to filing.
//
// Refiling a document as a receipt is the deliberate act that opts it in. That
// is the intended escape hatch, and it is a decision rather than an accident.
var ocrEligibleTypes = map[string]bool{
	TypeReceipt: true,
}

// OCREligible reports whether a document type may be sent for extraction.
//
// Callers must check this *before* decrypting the bytes: an ineligible
// document should never be read into memory for a purpose it is not allowed to
// be used for.
func OCREligible(docType string) bool { return ocrEligibleTypes[docType] }

// MatchWindowDays is how far either side of a receipt's date to look for the
// transaction it belongs to.
//
// It covers date *skew*, not sync latency: a card authorised on the 4th can
// post on the 7th, and different institutions disagree about which of those
// `transactions.date` holds. It is emphatically not what makes a receipt
// scanned before its charge posted eventually find it — that is the re-match
// pass, which runs the same comparison again later.
//
// Defined here rather than in either caller because both the API's matcher and
// the insight producer must use the same number. Two windows that drifted apart
// would mean the feed proposing a match the page does not show.
const MatchWindowDays = 5

// retentionYears is how long each kind of document is worth keeping, measured
// from the date on the document.
//
// These are advisory and only ever advisory. Nothing in this app deletes a
// document when retain_until passes, and nothing ever should: a finance app
// that silently discards a user's tax return has failed at the one thing it was
// trusted with. The date exists so the UI can say "you can probably let this
// go", which is a suggestion a person acts on.
//
// The tax figure is the one with an actual basis — the IRS assessment period
// runs to six years for a substantial understatement, so seven is the
// conventional safe margin. The rest are ordinary practice, not law.
var retentionYears = map[string]int{
	TypeTax:       7,
	TypeContract:  7,
	TypeStatement: 7,
	TypeReceipt:   3,
	TypeInsurance: 3,
	TypeWarranty:  3,
	TypeOther:     7, // unknown means keep it; the cost of storage is lower than the cost of a wrong guess
}

// insuranceGraceMonths is how long an insurance policy stays worth keeping past
// its renewal date. A claim can be filed against a policy period after the
// policy itself has lapsed, so expiry is not the end of its usefulness.
const insuranceGraceMonths = 12

// warrantyGraceMonths is the equivalent for a warranty: a claim raised just
// before expiry can still be in progress after it.
const warrantyGraceMonths = 3

// RetainUntil computes the advisory keep-until date for a document.
//
// Types with a stated expiry are measured from that expiry plus a grace period,
// because a policy or warranty is about a period of cover and the paperwork
// outlives it. Everything else is measured from the date on the document,
// falling back to when it was uploaded.
func RetainUntil(docType string, documentDate, expiresAt *time.Time, uploadedAt time.Time) time.Time {
	switch docType {
	case TypeWarranty:
		if expiresAt != nil {
			return expiresAt.AddDate(0, warrantyGraceMonths, 0)
		}
	case TypeInsurance:
		if expiresAt != nil {
			return expiresAt.AddDate(0, insuranceGraceMonths, 0)
		}
	}

	from := uploadedAt
	if documentDate != nil {
		from = *documentDate
	}

	years, ok := retentionYears[docType]
	if !ok {
		years = retentionYears[TypeOther]
	}
	return from.AddDate(years, 0, 0)
}
