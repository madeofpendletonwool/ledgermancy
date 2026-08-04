package api

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/documents"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/payroll"
)

// The paystub importer.
//
// READ THIS BEFORE ADDING AN AI PATH HERE. Doc 23 anticipated that a paystub
// would be uploaded to a model for OCR and spends a section arguing it should
// not be, and doc 18's `ocrEligibleTypes` already refuses to send anything but
// a receipt to a provider. Neither endpoint below sends anything anywhere:
// extraction is a local parse of the text layer a payroll provider already put
// in the PDF (internal/payroll/pdftext.go). A scanned stub with no text layer
// is reported as such and typed in by hand.
//
// If an AI path is ever built, doc 23 is specific about the terms: its own
// config flag, NOT inherited from DOCUMENTS_OCR_ENABLED, and consent on the
// individual document at the point of upload rather than a deployment-wide
// switch. Adding "paystub" to the vault's OCR allowlist would be one line and
// exactly the wrong one.
//
// Both endpoints return a PROPOSAL and write nothing. The user reviews the
// fields and posts a paystub; nothing here creates a row.

// maxPaystubUploadBytes bounds the direct-upload path. A generated paystub PDF
// is tens of kilobytes; this is generous enough for a scanned one that will
// turn out to have no text layer anyway.
const maxPaystubUploadBytes = 8 << 20

type proposedLineResponse struct {
	Category      string  `json:"category"`
	CategoryLabel string  `json:"category_label"`
	Group         string  `json:"group"`
	Label         string  `json:"label"`
	Amount        string  `json:"amount"`
	YTDAmount     *string `json:"ytd_amount"`
	PreTax        bool    `json:"pre_tax"`
	IsEmployer    bool    `json:"is_employer"`
}

type paystubProposalResponse struct {
	// EmployerNameHint is a guess off the top of the page, labelled as one. The
	// user still picks or creates the employer.
	EmployerNameHint string  `json:"employer_name_hint"`
	PayDate          *string `json:"pay_date"`
	PeriodStart      *string `json:"period_start"`
	PeriodEnd        *string `json:"period_end"`

	Gross    *string `json:"gross"`
	Net      *string `json:"net"`
	YTDGross *string `json:"ytd_gross"`
	YTDNet   *string `json:"ytd_net"`

	Lines []proposedLineResponse `json:"lines"`
	// Unmatched are money-bearing lines whose label this parser does not know.
	// Shown so the user can classify them — an unrecognised deduction is
	// precisely the one that makes the stub fail to balance.
	Unmatched []string `json:"unmatched"`
	Warnings  []string `json:"warnings"`

	Balances bool   `json:"balances"`
	Residual string `json:"residual"`
	// Source is what the created paystub should record. Always "pdf" from these
	// endpoints; carried in the response so the client does not hard-code it.
	Source string `json:"source"`
	// DocumentID is set only by the vault path, so a stub created from the
	// proposal can link back to the file it was read from.
	DocumentID *uuid.UUID `json:"document_id"`
}

// handleParsePaystubUpload reads a PDF posted directly and returns a proposal.
//
// It deliberately does NOT store the file. Path 1 in doc 23's ingestion list
// does not need the vault, and not storing it means a household can import the
// figures off a stub without a PDF containing their SSN and bank details coming
// to rest in the database's blob store at all. Anybody who does want it kept
// uploads it to the vault and uses the endpoint below.
func (s *Server) handleParsePaystubUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPaystubUploadBytes)

	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest,
			"a PDF is required in the 'file' field, and it must be under "+
				humanBytes(maxPaystubUploadBytes))
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxPaystubUploadBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "the upload could not be read")
		return
	}
	if int64(len(data)) > maxPaystubUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge,
			"that PDF is larger than "+humanBytes(maxPaystubUploadBytes))
		return
	}

	s.respondWithProposal(w, data, nil)
}

type parseDocumentRequest struct {
	DocumentID uuid.UUID `json:"document_id"`
}

// handleParsePaystubDocument reads a PDF already in the vault.
//
// No OCR gate and no provider check, because nothing leaves the host: the bytes
// are decrypted, parsed in memory, and dropped. The vault's `ocrEligibleTypes`
// allowlist exists to decide what may be UPLOADED to a third party, and this is
// not that decision.
func (s *Server) handleParsePaystubDocument(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}

	var req parseDocumentRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.DocumentID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "document_id is required")
		return
	}

	doc, ok := s.loadDocumentByID(w, r, req.DocumentID)
	if !ok {
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
		s.internalError(w, "fetch document for paystub parse", err)
		return
	}

	// Sniffed from the bytes, never taken from the stored MIME claim — the same
	// rule the vault's download path follows, and for the same reason.
	if documents.ServedContentType(plaintext) != "application/pdf" {
		writeError(w, http.StatusUnsupportedMediaType,
			"only a PDF paystub can be read. An image or a scan has no text layer to read; enter it by hand.")
		return
	}

	id := doc.ID
	s.respondWithProposal(w, plaintext, &id)
}

// respondWithProposal runs the local extraction and renders the result.
func (s *Server) respondWithProposal(w http.ResponseWriter, data []byte, documentID *uuid.UUID) {
	lines, err := payroll.ExtractPDFText(data)
	switch {
	case errors.Is(err, payroll.ErrNotPDF):
		writeError(w, http.StatusUnsupportedMediaType, "that file is not a PDF")
		return
	case errors.Is(err, payroll.ErrNoTextLayer):
		// 422, and the message says what to do instead. This is the expected
		// outcome for a scanned or photographed stub and is not a failure of
		// the request.
		writeError(w, http.StatusUnprocessableEntity,
			"this PDF has no text to read — it is probably a scan or a photo. "+
				"Nothing is sent anywhere to read it, so enter the stub by hand instead; "+
				"the form has every line type on it.")
		return
	case err != nil:
		s.internalError(w, "extract paystub text", err)
		return
	}

	proposal := payroll.ParseProposal(lines)

	out := paystubProposalResponse{
		EmployerNameHint: proposal.EmployerName,
		PayDate:          formatDatePtr(proposal.PayDate),
		PeriodStart:      formatDatePtr(proposal.PeriodStart),
		PeriodEnd:        formatDatePtr(proposal.PeriodEnd),
		Gross:            moneyOrNil(proposal.Gross),
		Net:              moneyOrNil(proposal.Net),
		YTDGross:         moneyOrNil(proposal.YTDGross),
		YTDNet:           moneyOrNil(proposal.YTDNet),
		Unmatched:        proposal.Unmatched,
		Warnings:         proposal.Warnings,
		Balances:         proposal.Balanced(),
		Source:           "pdf",
		DocumentID:       documentID,
		Lines:            make([]proposedLineResponse, 0, len(proposal.Lines)),
	}
	if out.Unmatched == nil {
		out.Unmatched = []string{}
	}
	if out.Warnings == nil {
		out.Warnings = []string{}
	}

	out.Residual = proposal.Stub().Residual().StringFixed(2)

	for _, l := range proposal.Lines {
		spec := payroll.SpecFor(l.Category)
		out.Lines = append(out.Lines, proposedLineResponse{
			Category:      string(l.Category),
			CategoryLabel: spec.Label,
			Group:         spec.Group,
			Label:         l.Label,
			Amount:        l.Amount.StringFixed(2),
			YTDAmount:     moneyOrNil(l.YTDAmount),
			PreTax:        l.PreTax,
			IsEmployer:    l.IsEmployer,
		})
	}

	writeJSON(w, http.StatusOK, out)
}

func formatDatePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.DateOnly)
	return &s
}
