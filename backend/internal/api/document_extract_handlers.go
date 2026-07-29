package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/documents"
)

// Receipt OCR: the one AI feature in the vault, and the only place in the app
// where an image of a user's paperwork leaves the host.
//
// What this endpoint returns is a *suggestion*. It writes nothing — no
// transaction, no category, no link. The response is fields for a form the user
// confirms, plus any existing transactions the amount and date could match, and
// the frontend's only affordance is "use these to fill in a manual
// transaction". That constraint is the whole design: a model misreading 84.20
// as 8420 should cost one correction, not one wrong row in a ledger the user
// relies on. See docs/plans/18-document-vault.md.

// maxOCRImageBytes bounds what is sent to the provider. The Messages API caps
// an inline image around 5 MB, and a photograph larger than this is a
// full-resolution camera dump whose extra pixels buy no accuracy on a receipt.
const maxOCRImageBytes = 5 << 20

type receiptMatchResponse struct {
	TransactionID uuid.UUID `json:"transaction_id"`
	Date          string    `json:"date"`
	// AuthorizedDate is when the card was swiped, where the institution
	// supplies it. It is what a receipt's printed date actually corresponds to,
	// so the UI can show why a charge posted days later is still the match.
	AuthorizedDate *string `json:"authorized_date"`
	Amount         string  `json:"amount"`
	Label          string  `json:"label"`
}

type extractResponse struct {
	Merchant   string  `json:"merchant"`
	Total      string  `json:"total"`
	Date       string  `json:"date"`
	Currency   string  `json:"currency"`
	Confidence float64 `json:"confidence"`
	Notes      string  `json:"notes"`
	// Matches are existing transactions this receipt could belong to, offered
	// so the common case is "link it to that one" rather than "type it in
	// again". Empty when the total was unreadable — there is nothing to match on.
	Matches []receiptMatchResponse `json:"matches"`
}

func (s *Server) handleExtractDocument(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}
	identity := auth.MustFromContext(r.Context())

	// Two independent gates. A deployment that configured a model for
	// categorisation has not thereby agreed to send photographs of its
	// paperwork anywhere, so OCR needs its own switch as well as a key.
	if !s.Documents.OCREnabled() {
		writeError(w, http.StatusForbidden,
			"receipt extraction is switched off on this deployment (DOCUMENTS_OCR_ENABLED)")
		return
	}
	if !s.AI.Enabled() {
		writeError(w, http.StatusServiceUnavailable,
			"receipt extraction needs an AI provider to be configured")
		return
	}

	doc, ok := s.loadDocument(w, r)
	if !ok {
		return
	}

	// The third gate, and the one that decides what a switched-on deployment is
	// actually willing to send: only receipts are eligible.
	//
	// Checked here, before the bytes are decrypted, so an ineligible document is
	// never even read into memory for a purpose it is not allowed to be used
	// for. Enforced server-side rather than only by hiding the button, because
	// "the UI does not offer it" is not a control.
	if !documents.OCREligible(doc.DocType) {
		writeError(w, http.StatusForbidden,
			"only receipts can be sent for extraction. A tax document, policy or contract is never uploaded to an AI provider — refile it as a receipt if that is genuinely what it is.")
		return
	}

	plaintext, err := s.Documents.Fetch(r.Context(), doc.StorageKey, doc.ContentHash)
	switch {
	case errors.Is(err, documents.ErrNotFound):
		writeError(w, http.StatusGone, "this document's contents are missing from storage")
		return
	case errors.Is(err, documents.ErrCorrupt):
		writeError(w, http.StatusUnprocessableEntity, "this document could not be decrypted")
		return
	case err != nil:
		s.internalError(w, "fetch document for extraction", err)
		return
	}

	// The media type is sniffed from the bytes, never taken from the stored
	// claim: this decides what gets uploaded to a third party, and "trust the
	// uploader" is not a basis for that decision.
	mediaType := documents.ServedContentType(plaintext)
	if !ai.SupportedReceiptImage(mediaType) {
		writeError(w, http.StatusUnsupportedMediaType,
			"only PNG, JPEG, GIF and WebP images can be read. Scanned PDFs need to be exported as images first.")
		return
	}
	if len(plaintext) > maxOCRImageBytes {
		writeError(w, http.StatusRequestEntityTooLarge,
			"this image is too large to read; "+humanBytes(maxOCRImageBytes)+" is the limit for extraction")
		return
	}

	fields, err := s.AI.ExtractReceipt(r.Context(), mediaType, plaintext)
	if err != nil {
		s.internalError(w, "extract receipt", err)
		return
	}

	out := extractResponse{
		// Model output is untrusted text bound for a form. Trim and cap it here;
		// the frontend renders it as text, never as markup.
		Merchant:   clip(fields.Merchant, 120),
		Currency:   clip(fields.Currency, 8),
		Notes:      clip(fields.Notes, 300),
		Confidence: fields.Confidence,
		Matches:    make([]receiptMatchResponse, 0),
	}

	// Both of these are validated rather than echoed: a model asked for
	// YYYY-MM-DD can still answer "last Tuesday", and a total can come back as
	// "$84.20". A field that does not parse is dropped, which shows the user an
	// empty box to fill rather than a plausible-looking wrong value.
	total, haveTotal := parseReceiptAmount(fields.Total)
	if haveTotal {
		out.Total = total.StringFixed(2)
	}
	receiptDate, haveDate := parseReceiptDate(fields.Date)
	if haveDate {
		out.Date = receiptDate.Format(time.DateOnly)
	}

	// Cache the reading before matching.
	//
	// This is what makes the ordinary case work. A receipt is scanned at the
	// register and the card charge posts days later, so the match that matters
	// is the one run *after* this request, not during it. Stored fields let that
	// happen in SQL, for free, with the image never leaving the host a second
	// time — and let a re-open show what was read rather than paying for
	// another upload to re-derive it.
	//
	// A failure here is logged, not surfaced: the user has their fields, and the
	// cost of the failure is a later re-run rather than a broken response.
	saved, err := s.Queries.SaveDocumentExtraction(r.Context(), dbgen.SaveDocumentExtractionParams{
		ID:                  doc.ID,
		HouseholdID:         identity.HouseholdID,
		UserID:              &identity.UserID,
		ExtractedMerchant:   nilIfEmpty(out.Merchant),
		ExtractedAmount:     decimalOrNull(total, haveTotal),
		ExtractedDate:       dateOrNil(receiptDate, haveDate),
		ExtractedConfidence: decimalOrNull(decimal.NewFromFloat(fields.Confidence).Round(4), true),
		ExtractedNotes:      nilIfEmpty(out.Notes),
	})
	if err != nil {
		slog.Error("could not cache receipt extraction",
			"document_id", doc.ID, "error", err)
	} else {
		doc = saved
	}

	if haveTotal {
		matches, err := s.matchReceipt(r, doc)
		if err != nil {
			s.internalError(w, "match receipt to transactions", err)
			return
		}
		out.Matches = matches
	}

	writeJSON(w, http.StatusOK, out)
}

// handleDocumentMatches re-runs the transaction match against a receipt's
// already-extracted fields.
//
// It costs nothing: no decryption, no upload, no model call — just the same
// deterministic query the extraction endpoint runs. That is the whole point of
// caching the reading. The UI calls this every time a read receipt is opened,
// so a charge that posted since the scan simply appears in the list.
func (s *Server) handleDocumentMatches(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}

	doc, ok := s.loadDocument(w, r)
	if !ok {
		return
	}

	// No stored amount means nothing to match on — an empty list, not an error:
	// the caller asked a reasonable question about a receipt that has not been
	// read yet.
	if !doc.ExtractedAmount.Valid || doc.ExtractedAmount.Decimal.IsZero() {
		writeJSON(w, http.StatusOK, []receiptMatchResponse{})
		return
	}

	matches, err := s.matchReceipt(r, doc)
	if err != nil {
		s.internalError(w, "match receipt to transactions", err)
		return
	}
	writeJSON(w, http.StatusOK, matches)
}

// matchReceipt finds the transactions a document's stored extraction could
// belong to. Shared by the extraction endpoint and the standalone match
// endpoint so the two can never drift into disagreeing about what matches.
func (s *Server) matchReceipt(r *http.Request, doc dbgen.Document) ([]receiptMatchResponse, error) {
	identity := auth.MustFromContext(r.Context())

	// Anchor on the date printed on the receipt, then the date the user gave
	// the document, then the day it was filed. A receipt is normally filed close
	// to when it was issued, so each fallback is a wider guess at the same thing.
	anchor := doc.CreatedAt
	if doc.DocumentDate != nil {
		anchor = *doc.DocumentDate
	}
	if doc.ExtractedDate != nil {
		anchor = *doc.ExtractedDate
	}

	// Plaid's sign convention is positive = money out, and a receipt is always
	// money out, so the printed total compares directly as a positive.
	rows, err := s.Queries.MatchTransactionsForReceipt(r.Context(), dbgen.MatchTransactionsForReceiptParams{
		HouseholdID: identity.HouseholdID,
		UserID:      identity.UserID,
		FromDate:    anchor.AddDate(0, 0, -documents.MatchWindowDays),
		ToDate:      anchor.AddDate(0, 0, documents.MatchWindowDays),
		Amount:      doc.ExtractedAmount.Decimal.Abs(),
		OnDate:      anchor,
	})
	if err != nil {
		return nil, err
	}

	out := make([]receiptMatchResponse, 0, len(rows))
	for _, row := range rows {
		match := receiptMatchResponse{
			TransactionID: row.ID,
			Date:          row.Date.Format(time.DateOnly),
			Amount:        row.Amount.Round(2).StringFixed(2),
			Label:         row.Label,
		}
		// Surfaced so the UI can explain a match whose posted date looks days
		// off from the receipt: it lined up on the authorisation instead.
		if row.AuthorizedDate != nil {
			authorized := row.AuthorizedDate.Format(time.DateOnly)
			match.AuthorizedDate = &authorized
		}
		out = append(out, match)
	}
	return out, nil
}

// dateOrNil and decimalOrNull turn a parsed-or-not value into the nullable form
// the extraction columns take. Deliberately not named nullDecimal — that
// already exists in ledger_handlers.go and converts in the opposite direction
// (database value out to JSON), so reusing the name would invite writing one
// where the other was meant.
func dateOrNil(t time.Time, ok bool) *time.Time {
	if !ok {
		return nil
	}
	return &t
}

func decimalOrNull(d decimal.Decimal, ok bool) decimal.NullDecimal {
	return decimal.NullDecimal{Decimal: d, Valid: ok}
}

// parseReceiptAmount reads the model's total, tolerating the currency symbols
// and thousands separators the prompt asked it not to use.
func parseReceiptAmount(raw string) (decimal.Decimal, bool) {
	cleaned := strings.Map(func(r rune) rune {
		if (r >= '0' && r <= '9') || r == '.' || r == '-' {
			return r
		}
		return -1
	}, strings.TrimSpace(raw))

	if cleaned == "" {
		return decimal.Zero, false
	}
	amount, err := decimal.NewFromString(cleaned)
	if err != nil || amount.IsZero() {
		return decimal.Zero, false
	}
	return amount, true
}

// parseReceiptDate accepts only the format that was asked for, and only dates
// that are actually plausible for a receipt. A model that answers "2019-13-45"
// or a date fifty years out has not read anything, and passing that through
// would put a nonsense default in the user's form.
func parseReceiptDate(raw string) (time.Time, bool) {
	parsed, err := time.Parse(time.DateOnly, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, false
	}
	now := time.Now().UTC()
	if parsed.After(now.AddDate(0, 0, 1)) || parsed.Before(now.AddDate(-25, 0, 0)) {
		return time.Time{}, false
	}
	return parsed, true
}

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max]
	}
	return s
}
