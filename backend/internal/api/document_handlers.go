package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/documents"
)

// The document vault's HTTP surface.
//
// One rule governs every handler here and is worth stating before the code: a
// document id is not an authorisation. Every read, including the download,
// goes through GetDocument with the caller's household AND user id, and a miss
// is a 404 rather than a 403 — a 403 would confirm the id exists somewhere
// else. There is no code path in this file that fetches bytes from a key
// without having first resolved the row that way.

// documentListLimit caps one listing. A vault is browsed, not paged through
// exhaustively, and the filters do the narrowing.
const documentListLimit = 500

// maxPaperclipIDs bounds the batched attachment-count lookup, which takes ids
// from a query string.
const maxPaperclipIDs = 200

// multipartOverhead is the slack allowed above the file cap for the metadata
// fields and MIME framing that share the upload body.
const multipartOverhead = 1 << 20

// documentLinkResponse is one attachment, carrying enough of the target to
// render it ("Costco, 12 March, 84.20") without a second lookup per link.
type documentLinkResponse struct {
	ID         uuid.UUID `json:"id"`
	DocumentID uuid.UUID `json:"document_id"`
	TargetKind string    `json:"target_kind"`
	TargetID   uuid.UUID `json:"target_id"`
	Label      string    `json:"label"`
	Date       *string   `json:"date"`
	Amount     *string   `json:"amount"`
}

type documentResponse struct {
	ID          uuid.UUID  `json:"id"`
	Title       string     `json:"title"`
	DocType     string     `json:"doc_type"`
	Filename    string     `json:"filename"`
	MimeType    string     `json:"mime_type"`
	SizeBytes   int64      `json:"size_bytes"`
	IsShared    bool       `json:"is_shared"`
	UploadedBy  *uuid.UUID `json:"uploaded_by"`
	IsOwn       bool       `json:"is_own"`
	ContentHash string     `json:"content_hash"`

	DocumentDate *string `json:"document_date"`
	ExpiresAt    *string `json:"expires_at"`
	RetainUntil  *string `json:"retain_until"`
	Notes        *string `json:"notes"`

	// PreviewType is non-empty when the client may render the bytes inline. It
	// is derived from the claimed MIME type and is only a hint — the download
	// response decides the real Content-Type by sniffing, so a file that lies
	// about its type simply fails to render.
	PreviewType string `json:"preview_type"`

	LinkCount int64                  `json:"link_count"`
	Links     []documentLinkResponse `json:"links"`

	// Extraction is what OCR last read off this receipt, cached so re-opening it
	// costs no second upload to an AI provider. Null when it has never been run.
	Extraction *storedExtraction `json:"extraction"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// storedExtraction is a cached reading, never a source of truth. Nothing in the
// app computes from these values: they populate a form the user confirms and
// give the matcher an amount to compare against.
type storedExtraction struct {
	ExtractedAt time.Time `json:"extracted_at"`
	Merchant    string    `json:"merchant"`
	Total       string    `json:"total"`
	Date        string    `json:"date"`
	Confidence  float64   `json:"confidence"`
	Notes       string    `json:"notes"`
}

func extractionFromRow(d dbgen.Document) *storedExtraction {
	if d.ExtractedAt == nil {
		return nil
	}
	out := &storedExtraction{ExtractedAt: *d.ExtractedAt}
	if d.ExtractedMerchant != nil {
		out.Merchant = *d.ExtractedMerchant
	}
	if d.ExtractedNotes != nil {
		out.Notes = *d.ExtractedNotes
	}
	if d.ExtractedAmount.Valid {
		out.Total = d.ExtractedAmount.Decimal.Round(2).StringFixed(2)
	}
	if d.ExtractedDate != nil {
		out.Date = d.ExtractedDate.Format(time.DateOnly)
	}
	if d.ExtractedConfidence.Valid {
		c, _ := d.ExtractedConfidence.Decimal.Float64()
		out.Confidence = c
	}
	return out
}

func dateOnly(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.DateOnly)
	return &s
}

func documentFromRow(d dbgen.Document, callerID uuid.UUID) documentResponse {
	return documentResponse{
		ID:           d.ID,
		Title:        d.Title,
		DocType:      d.DocType,
		Filename:     d.Filename,
		MimeType:     d.MimeType,
		SizeBytes:    d.SizeBytes,
		IsShared:     d.IsShared,
		UploadedBy:   d.UploadedBy,
		IsOwn:        d.UploadedBy != nil && *d.UploadedBy == callerID,
		ContentHash:  d.ContentHash,
		DocumentDate: dateOnly(d.DocumentDate),
		ExpiresAt:    dateOnly(d.ExpiresAt),
		RetainUntil:  dateOnly(d.RetainUntil),
		Notes:        d.Notes,
		PreviewType:  documents.PreviewType(d.MimeType),
		Links:        make([]documentLinkResponse, 0),
		Extraction:   extractionFromRow(d),
		CreatedAt:    d.CreatedAt,
		UpdatedAt:    d.UpdatedAt,
	}
}

// vaultReady guards every handler. The vault is optional in the same way Plaid
// is: a deployment with no writable document volume runs perfectly well without
// one, and says so instead of panicking.
func (s *Server) vaultReady(w http.ResponseWriter) bool {
	if s.Documents == nil {
		writeError(w, http.StatusServiceUnavailable,
			"the document vault is not configured on this deployment")
		return false
	}
	return true
}

// --------------------------------------------------------------------------
// Listing
// --------------------------------------------------------------------------

func (s *Server) handleListDocuments(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}
	identity := auth.MustFromContext(r.Context())
	q := r.URL.Query()

	params := dbgen.ListDocumentsParams{
		HouseholdID: identity.HouseholdID,
		UserID:      &identity.UserID,
		RowLimit:    documentListLimit,
	}

	if v := strings.TrimSpace(q.Get("doc_type")); v != "" {
		if !documents.ValidTypes[v] {
			writeError(w, http.StatusBadRequest, "unknown document type")
			return
		}
		params.DocType = &v
	}
	if v := strings.TrimSpace(q.Get("search")); v != "" {
		params.Search = &v
	}
	for _, f := range []struct {
		key string
		dst **time.Time
	}{
		{"from", &params.FromDate},
		{"to", &params.ToDate},
		{"expiring_before", &params.ExpiringBefore},
	} {
		raw := strings.TrimSpace(q.Get(f.key))
		if raw == "" {
			continue
		}
		parsed, err := time.Parse(time.DateOnly, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be YYYY-MM-DD", f.key))
			return
		}
		*f.dst = &parsed
	}
	// Tri-state: absent means "everything", which is why this is not a plain
	// bool with a false default.
	if raw := strings.TrimSpace(q.Get("linked")); raw != "" {
		linked, err := strconv.ParseBool(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "linked must be true or false")
			return
		}
		params.Linked = &linked
	}

	rows, err := s.Queries.ListDocuments(r.Context(), params)
	if err != nil {
		s.internalError(w, "list documents", err)
		return
	}

	out := make([]documentResponse, 0, len(rows))
	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		doc := documentFromRow(dbgen.Document{
			ID: row.ID, HouseholdID: row.HouseholdID, UploadedBy: row.UploadedBy,
			IsShared: row.IsShared, Title: row.Title, DocType: row.DocType,
			Filename: row.Filename, MimeType: row.MimeType, SizeBytes: row.SizeBytes,
			StorageKey: row.StorageKey, ContentHash: row.ContentHash,
			DocumentDate: row.DocumentDate, ExpiresAt: row.ExpiresAt,
			RetainUntil: row.RetainUntil, Notes: row.Notes,
			ExtractedAt: row.ExtractedAt, ExtractedMerchant: row.ExtractedMerchant,
			ExtractedAmount: row.ExtractedAmount, ExtractedDate: row.ExtractedDate,
			ExtractedConfidence: row.ExtractedConfidence, ExtractedNotes: row.ExtractedNotes,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		}, identity.UserID)
		doc.LinkCount = row.LinkCount
		out = append(out, doc)
		ids = append(ids, row.ID)
	}

	// One query for every link on the page rather than one per document, so the
	// listing stays a fixed two round trips regardless of vault size.
	if err := s.attachLinks(r, out, ids); err != nil {
		s.internalError(w, "list document links", err)
		return
	}

	writeJSON(w, http.StatusOK, out)
}

// attachLinks fills in the Links slice on each response in place.
func (s *Server) attachLinks(r *http.Request, docs []documentResponse, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	links, err := s.Queries.ListDocumentLinks(r.Context(), ids)
	if err != nil {
		return err
	}

	byDoc := make(map[uuid.UUID][]documentLinkResponse, len(ids))
	for _, l := range links {
		var targetID uuid.UUID
		switch {
		case l.TransactionID != nil:
			targetID = *l.TransactionID
		case l.ManualAssetID != nil:
			targetID = *l.ManualAssetID
		case l.AccountID != nil:
			targetID = *l.AccountID
		case l.GoalID != nil:
			targetID = *l.GoalID
		}

		entry := documentLinkResponse{
			ID:         l.ID,
			DocumentID: l.DocumentID,
			TargetKind: l.TargetKind,
			TargetID:   targetID,
			Label:      l.TargetLabel,
			Date:       dateOnly(l.TransactionDate),
		}
		if l.TransactionAmount.Valid {
			amount := l.TransactionAmount.Decimal.Round(2).StringFixed(2)
			entry.Amount = &amount
		}
		byDoc[l.DocumentID] = append(byDoc[l.DocumentID], entry)
	}

	for i := range docs {
		if found, ok := byDoc[docs[i].ID]; ok {
			docs[i].Links = found
		}
	}
	return nil
}

// handleGetDocument returns one document's metadata and its links.
func (s *Server) handleGetDocument(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}
	identity := auth.MustFromContext(r.Context())

	doc, ok := s.loadDocument(w, r)
	if !ok {
		return
	}

	out := []documentResponse{documentFromRow(doc, identity.UserID)}
	if err := s.attachLinks(r, out, []uuid.UUID{doc.ID}); err != nil {
		s.internalError(w, "list document links", err)
		return
	}
	out[0].LinkCount = int64(len(out[0].Links))
	writeJSON(w, http.StatusOK, out[0])
}

// loadDocument resolves the {documentID} path parameter under the caller's
// visibility. Every handler that touches a specific document starts here; a
// miss is written as 404 and the bool is false.
func (s *Server) loadDocument(w http.ResponseWriter, r *http.Request) (dbgen.Document, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "documentID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "document not found")
		return dbgen.Document{}, false
	}
	return s.loadDocumentByID(w, r, id)
}

// loadDocumentByID is loadDocument for a caller that already has the id — the
// paystub importer, which takes it from a request body rather than a path.
// Split out rather than duplicated so there is exactly one place the vault's
// visibility predicate lives.
func (s *Server) loadDocumentByID(w http.ResponseWriter, r *http.Request, id uuid.UUID) (dbgen.Document, bool) {
	identity := auth.MustFromContext(r.Context())

	doc, err := s.Queries.GetDocument(r.Context(), dbgen.GetDocumentParams{
		ID:          id,
		HouseholdID: identity.HouseholdID,
		UserID:      &identity.UserID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// 404, never 403: another household's document, and a private document
		// belonging to another member, must be indistinguishable from one that
		// does not exist.
		writeError(w, http.StatusNotFound, "document not found")
		return dbgen.Document{}, false
	}
	if err != nil {
		s.internalError(w, "get document", err)
		return dbgen.Document{}, false
	}
	return doc, true
}

// --------------------------------------------------------------------------
// Upload
// --------------------------------------------------------------------------

func (s *Server) handleUploadDocument(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}
	identity := auth.MustFromContext(r.Context())
	vault := s.Documents

	// Bound the body before parsing so an oversized upload is refused at the
	// socket rather than after it has been buffered.
	r.Body = http.MaxBytesReader(w, r.Body, vault.MaxFileBytes()+multipartOverhead)

	// The memory budget is the whole file cap on purpose. Above it, Go spills
	// multipart file parts to temporary files — which would write the plaintext
	// of an encrypted vault to disk, unencrypted, as a side effect of an upload.
	// MaxBytesReader above already bounds what this can allocate.
	if err := r.ParseMultipartForm(vault.MaxFileBytes()); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeSizeError(w, vault.MaxFileBytes())
			return
		}
		writeError(w, http.StatusBadRequest, "expected a multipart form upload")
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "no file was included in the upload")
		return
	}
	defer file.Close()

	docType := strings.TrimSpace(r.FormValue("doc_type"))
	if docType == "" {
		docType = documents.TypeOther
	}
	if !documents.ValidTypes[docType] {
		writeError(w, http.StatusBadRequest, "unknown document type")
		return
	}

	filename := documents.SanitiseFilename(header.Filename)
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		title = filename
	}
	if len(title) > 300 {
		title = title[:300]
	}

	documentDate, err := optionalFormDate(r, "document_date")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	expiresAt, err := optionalFormDate(r, "expires_at")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Shared by default, matching the table default and plaid_items. A member
	// who wants privacy opts into it; the alternative default would silently
	// hide household paperwork from the household.
	isShared := true
	if raw := strings.TrimSpace(r.FormValue("is_shared")); raw != "" {
		if isShared, err = strconv.ParseBool(raw); err != nil {
			writeError(w, http.StatusBadRequest, "is_shared must be true or false")
			return
		}
	}

	var notes *string
	if v := strings.TrimSpace(r.FormValue("notes")); v != "" {
		notes = &v
	}

	plaintext, err := vault.ReadLimited(file)
	switch {
	case errors.Is(err, documents.ErrTooLarge):
		writeSizeError(w, vault.MaxFileBytes())
		return
	case errors.Is(err, documents.ErrEmpty):
		writeError(w, http.StatusBadRequest, "the uploaded file is empty")
		return
	case err != nil:
		s.internalError(w, "read upload", err)
		return
	}

	if !s.quotaAllows(w, r, identity.HouseholdID, int64(len(plaintext))) {
		return
	}

	// Bytes first, row second. A failed store leaves no row, whereas a row
	// written before the bytes would be a download that 500s forever.
	stored, err := vault.Store(r.Context(), plaintext)
	if err != nil {
		s.internalError(w, "store document", err)
		return
	}

	uploadedAt := time.Now().UTC()
	retainUntil := documents.RetainUntil(docType, documentDate, expiresAt, uploadedAt)

	doc, err := s.Queries.CreateDocument(r.Context(), dbgen.CreateDocumentParams{
		HouseholdID:  identity.HouseholdID,
		UploadedBy:   &identity.UserID,
		IsShared:     isShared,
		Title:        title,
		DocType:      docType,
		Filename:     filename,
		MimeType:     documents.NormaliseMIME(header.Header.Get("Content-Type")),
		SizeBytes:    stored.SizeBytes,
		StorageKey:   stored.StorageKey,
		ContentHash:  stored.ContentHash,
		DocumentDate: documentDate,
		ExpiresAt:    expiresAt,
		RetainUntil:  &retainUntil,
		Notes:        notes,
	})
	if err != nil {
		// The row is what makes the blob reachable, so without it the blob is
		// unreferenced garbage. Clean it up rather than leaving it to consume
		// quota nothing can ever see.
		if rmErr := vault.Remove(r.Context(), stored.StorageKey); rmErr != nil {
			slog.Error("orphaned document blob after failed insert",
				"storage_key", stored.StorageKey, "error", rmErr)
		}
		s.internalError(w, "create document", err)
		return
	}

	out := documentFromRow(doc, identity.UserID)

	// An optional link supplied with the upload — "attach a receipt to this
	// transaction" in one step. A bad target is reported but does not undo the
	// upload: the document is stored and the user can link it by hand.
	if kind, target := r.FormValue("link_kind"), r.FormValue("link_id"); kind != "" && target != "" {
		targetID, parseErr := uuid.Parse(target)
		if parseErr == nil {
			if link, linkErr := s.createLink(r, doc.ID, kind, targetID); linkErr == nil {
				out.Links = append(out.Links, link)
				out.LinkCount = 1
			} else {
				slog.Warn("upload link target rejected",
					"document_id", doc.ID, "kind", kind, "error", linkErr)
			}
		}
	}

	writeJSON(w, http.StatusCreated, out)
}

// writeSizeError reports an over-cap upload as 413 with the actual limit, so
// the user learns the number rather than guessing at it.
func writeSizeError(w http.ResponseWriter, limit int64) {
	writeError(w, http.StatusRequestEntityTooLarge,
		fmt.Sprintf("file is larger than the %s limit for a single document",
			humanBytes(limit)))
}

// quotaAllows enforces the per-household ceiling.
//
// The check is read-then-write and therefore racy: two simultaneous uploads can
// both pass it and land the household marginally over. That is accepted
// deliberately — the overshoot is bounded by one file's size, and the
// alternative is serialising every upload behind a lock to defend a number that
// exists to protect the operator's disk, not to bill anyone.
func (s *Server) quotaAllows(w http.ResponseWriter, r *http.Request, householdID uuid.UUID, incoming int64) bool {
	quota := s.Documents.QuotaBytes()
	if quota <= 0 {
		return true
	}

	usage, err := s.Queries.HouseholdStorageUsed(r.Context(), householdID)
	if err != nil {
		s.internalError(w, "household storage used", err)
		return false
	}
	if usage.BytesUsed+incoming > quota {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"this upload would put the vault over its %s limit (%s already stored). Delete something first, or raise DOCUMENTS_QUOTA_BYTES.",
			humanBytes(quota), humanBytes(usage.BytesUsed)))
		return false
	}
	return true
}

func optionalFormDate(r *http.Request, field string) (*time.Time, error) {
	raw := strings.TrimSpace(r.FormValue(field))
	if raw == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.DateOnly, raw)
	if err != nil {
		return nil, fmt.Errorf("%s must be YYYY-MM-DD", field)
	}
	return &parsed, nil
}

// humanBytes renders a size the way the UI states limits.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// --------------------------------------------------------------------------
// Download
// --------------------------------------------------------------------------

// handleDownloadDocument decrypts and returns the bytes.
//
// Three headers carry the entire safety of this endpoint, and all three are set
// here rather than inherited: an attachment disposition with a sanitised name,
// a Content-Type sniffed from the plaintext against a short allowlist (never
// the uploader's claim), and nosniff so a browser cannot second-guess either.
// Together they mean an HTML file uploaded as a "receipt" downloads instead of
// executing on this origin.
func (s *Server) handleDownloadDocument(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}

	doc, ok := s.loadDocument(w, r)
	if !ok {
		return
	}

	plaintext, err := s.Documents.Fetch(r.Context(), doc.StorageKey, doc.ContentHash)
	switch {
	case errors.Is(err, documents.ErrNotFound):
		slog.Error("document row has no blob; storage and database disagree",
			"document_id", doc.ID, "storage_key", doc.StorageKey)
		writeError(w, http.StatusGone,
			"this document's contents are missing from storage. The record is intact; the file is not.")
		return
	case errors.Is(err, documents.ErrCorrupt):
		// Fail closed and say so plainly. The likeliest cause is a changed
		// ENCRYPTION_KEY, which an operator can act on and a retry cannot fix.
		writeError(w, http.StatusUnprocessableEntity,
			"this document could not be decrypted. If ENCRYPTION_KEY changed, the original key is required to read it.")
		return
	case err != nil:
		s.internalError(w, "fetch document", err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", documents.ServedContentType(plaintext))
	h.Set("Content-Disposition", documents.ContentDisposition(doc.Filename))
	h.Set("Content-Length", strconv.Itoa(len(plaintext)))
	// The middleware sets this globally; repeated here because losing it on this
	// route specifically is what turns an uploaded HTML file into stored XSS.
	h.Set("X-Content-Type-Options", "nosniff")

	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(plaintext); err != nil {
		slog.Warn("document download interrupted", "document_id", doc.ID, "error", err)
	}
}

// --------------------------------------------------------------------------
// Metadata + delete
// --------------------------------------------------------------------------

type updateDocumentRequest struct {
	Title        string  `json:"title"`
	DocType      string  `json:"doc_type"`
	IsShared     *bool   `json:"is_shared"`
	DocumentDate *string `json:"document_date"`
	ExpiresAt    *string `json:"expires_at"`
	Notes        *string `json:"notes"`
}

func (s *Server) handleUpdateDocument(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}
	identity := auth.MustFromContext(r.Context())

	doc, ok := s.loadDocument(w, r)
	if !ok {
		return
	}

	var req updateDocumentRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.Title = strings.TrimSpace(req.Title); req.Title == "" {
		writeError(w, http.StatusBadRequest, "a document needs a title")
		return
	}
	if !documents.ValidTypes[req.DocType] {
		writeError(w, http.StatusBadRequest, "unknown document type")
		return
	}

	documentDate, err := optionalDate(req.DocumentDate, "document_date")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	expiresAt, err := optionalDate(req.ExpiresAt, "expires_at")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	isShared := doc.IsShared
	if req.IsShared != nil {
		isShared = *req.IsShared
	}

	// Recomputed rather than edited: retain_until is derived, and letting a
	// client set it would make it a second, drifting source of truth alongside
	// the policy.
	retainUntil := documents.RetainUntil(req.DocType, documentDate, expiresAt, doc.CreatedAt)

	updated, err := s.Queries.UpdateDocumentMetadata(r.Context(), dbgen.UpdateDocumentMetadataParams{
		ID:           doc.ID,
		HouseholdID:  identity.HouseholdID,
		UserID:       &identity.UserID,
		Title:        req.Title,
		DocType:      req.DocType,
		IsShared:     isShared,
		DocumentDate: documentDate,
		ExpiresAt:    expiresAt,
		RetainUntil:  &retainUntil,
		Notes:        trimmedOrNil(req.Notes),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		s.internalError(w, "update document", err)
		return
	}

	out := []documentResponse{documentFromRow(updated, identity.UserID)}
	if err := s.attachLinks(r, out, []uuid.UUID{updated.ID}); err != nil {
		s.internalError(w, "list document links", err)
		return
	}
	out[0].LinkCount = int64(len(out[0].Links))
	writeJSON(w, http.StatusOK, out[0])
}

func optionalDate(raw *string, field string) (*time.Time, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.DateOnly, strings.TrimSpace(*raw))
	if err != nil {
		return nil, fmt.Errorf("%s must be YYYY-MM-DD", field)
	}
	return &parsed, nil
}

func trimmedOrNil(raw *string) *string {
	if raw == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*raw)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// handleDeleteDocument removes the row, then the blob.
//
// That order is deliberate. Row-then-blob can leave an unreferenced blob if the
// second step fails, which costs disk. Blob-then-row can leave a row whose
// download is permanently broken, which costs the user's trust in the vault.
func (s *Server) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}
	identity := auth.MustFromContext(r.Context())

	id, err := uuid.Parse(chi.URLParam(r, "documentID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}

	storageKey, err := s.Queries.DeleteDocument(r.Context(), dbgen.DeleteDocumentParams{
		ID:          id,
		HouseholdID: identity.HouseholdID,
		UserID:      &identity.UserID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		s.internalError(w, "delete document", err)
		return
	}

	if err := s.Documents.Remove(r.Context(), storageKey); err != nil {
		// The user's delete succeeded; what is left is an orphaned blob for an
		// operator to notice, not an error to report back.
		slog.Error("document row deleted but blob remains",
			"storage_key", storageKey, "error", err)
	}

	w.WriteHeader(http.StatusNoContent)
}

// --------------------------------------------------------------------------
// Links
// --------------------------------------------------------------------------

type createLinkRequest struct {
	TargetKind string    `json:"target_kind"`
	TargetID   uuid.UUID `json:"target_id"`
}

func (s *Server) handleLinkDocument(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}

	doc, ok := s.loadDocument(w, r)
	if !ok {
		return
	}

	var req createLinkRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	link, err := s.createLink(r, doc.ID, req.TargetKind, req.TargetID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, link)
}

// createLink verifies the target is one the caller may see, then writes the
// link.
//
// The ownership check is not optional politeness. Without it "attach this
// document to transaction X" is a probe that tells the caller whether X exists
// in someone else's household, and a link whose label is resolved in SQL would
// then hand back that transaction's merchant and amount.
func (s *Server) createLink(r *http.Request, documentID uuid.UUID, kind string, targetID uuid.UUID) (documentLinkResponse, error) {
	identity := auth.MustFromContext(r.Context())
	ctx := r.Context()

	params := dbgen.CreateDocumentLinkParams{DocumentID: documentID}

	var err error
	switch kind {
	case "transaction":
		_, err = s.Queries.TargetTransactionInHousehold(ctx, dbgen.TargetTransactionInHouseholdParams{
			ID: targetID, HouseholdID: identity.HouseholdID, UserID: identity.UserID,
		})
		params.TransactionID = &targetID
	case "manual_asset":
		_, err = s.Queries.TargetManualAssetInHousehold(ctx, dbgen.TargetManualAssetInHouseholdParams{
			ID: targetID, HouseholdID: identity.HouseholdID,
		})
		params.ManualAssetID = &targetID
	case "account":
		_, err = s.Queries.TargetAccountInHousehold(ctx, dbgen.TargetAccountInHouseholdParams{
			ID: targetID, HouseholdID: identity.HouseholdID, UserID: identity.UserID,
		})
		params.AccountID = &targetID
	case "goal":
		_, err = s.Queries.TargetGoalInHousehold(ctx, dbgen.TargetGoalInHouseholdParams{
			ID: targetID, HouseholdID: identity.HouseholdID, UserID: &identity.UserID,
		})
		params.GoalID = &targetID
	default:
		return documentLinkResponse{}, errors.New("target_kind must be transaction, manual_asset, account or goal")
	}

	if errors.Is(err, pgx.ErrNoRows) {
		// Same wording whether the target is absent or belongs to someone else.
		return documentLinkResponse{}, errors.New("that record could not be found")
	}
	if err != nil {
		return documentLinkResponse{}, fmt.Errorf("verify link target: %w", err)
	}

	link, err := s.Queries.CreateDocumentLink(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT DO NOTHING returned nothing: the link already exists,
		// which is the state the caller asked for.
		return documentLinkResponse{
			DocumentID: documentID, TargetKind: kind, TargetID: targetID,
		}, nil
	}
	if err != nil {
		return documentLinkResponse{}, fmt.Errorf("create document link: %w", err)
	}

	return documentLinkResponse{
		ID:         link.ID,
		DocumentID: documentID,
		TargetKind: kind,
		TargetID:   targetID,
	}, nil
}

func (s *Server) handleUnlinkDocument(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}
	identity := auth.MustFromContext(r.Context())

	linkID, err := uuid.Parse(chi.URLParam(r, "linkID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "link not found")
		return
	}

	if err := s.Queries.DeleteDocumentLink(r.Context(), dbgen.DeleteDocumentLinkParams{
		ID:          linkID,
		HouseholdID: identity.HouseholdID,
		UserID:      &identity.UserID,
	}); err != nil {
		s.internalError(w, "delete document link", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAttachedDocuments lists the documents on one ledger record — what the
// paperclip on a transaction row, a manual asset or a goal expands into.
func (s *Server) handleAttachedDocuments(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}
	identity := auth.MustFromContext(r.Context())
	q := r.URL.Query()

	params := dbgen.ListDocumentsForTargetParams{
		HouseholdID: identity.HouseholdID,
		UserID:      &identity.UserID,
	}

	targets := 0
	for key, dst := range map[string]**uuid.UUID{
		"transaction_id":  &params.TransactionID,
		"manual_asset_id": &params.ManualAssetID,
		"account_id":      &params.AccountID,
		"goal_id":         &params.GoalID,
	} {
		raw := strings.TrimSpace(q.Get(key))
		if raw == "" {
			continue
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid "+key)
			return
		}
		*dst = &id
		targets++
	}
	if targets != 1 {
		writeError(w, http.StatusBadRequest,
			"give exactly one of transaction_id, manual_asset_id, account_id or goal_id")
		return
	}

	rows, err := s.Queries.ListDocumentsForTarget(r.Context(), params)
	if err != nil {
		s.internalError(w, "list attached documents", err)
		return
	}

	out := make([]documentResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, documentFromRow(row, identity.UserID))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDocumentCounts powers the paperclip indicators on a page of
// transactions: one round trip for the whole page rather than one per row.
func (s *Server) handleDocumentCounts(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}
	identity := auth.MustFromContext(r.Context())

	raw := r.URL.Query()["transaction_id"]
	if len(raw) > maxPaperclipIDs {
		raw = raw[:maxPaperclipIDs]
	}

	ids := make([]uuid.UUID, 0, len(raw))
	for _, v := range raw {
		id, err := uuid.Parse(v)
		if err != nil {
			continue // a malformed id is simply not a transaction with documents
		}
		ids = append(ids, id)
	}

	counts := make(map[string]int64, len(ids))
	if len(ids) > 0 {
		rows, err := s.Queries.CountDocumentsForTransactions(r.Context(), dbgen.CountDocumentsForTransactionsParams{
			TransactionIds: ids,
			HouseholdID:    identity.HouseholdID,
			UserID:         &identity.UserID,
		})
		if err != nil {
			s.internalError(w, "count attached documents", err)
			return
		}
		for _, row := range rows {
			if row.TransactionID != nil {
				counts[row.TransactionID.String()] = row.DocumentCount
			}
		}
	}
	writeJSON(w, http.StatusOK, counts)
}

// --------------------------------------------------------------------------
// Storage usage
// --------------------------------------------------------------------------

type storageResponse struct {
	BytesUsed     int64  `json:"bytes_used"`
	QuotaBytes    int64  `json:"quota_bytes"` // 0 = unlimited
	MaxFileBytes  int64  `json:"max_file_bytes"`
	DocumentCount int64  `json:"document_count"`
	Backend       string `json:"backend"`
	OCREnabled    bool   `json:"ocr_enabled"`
}

func (s *Server) handleDocumentStorage(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}
	identity := auth.MustFromContext(r.Context())

	usage, err := s.Queries.HouseholdStorageUsed(r.Context(), identity.HouseholdID)
	if err != nil {
		s.internalError(w, "household storage used", err)
		return
	}

	writeJSON(w, http.StatusOK, storageResponse{
		BytesUsed:     usage.BytesUsed,
		QuotaBytes:    s.Documents.QuotaBytes(),
		MaxFileBytes:  s.Documents.MaxFileBytes(),
		DocumentCount: usage.DocumentCount,
		// The backend string names a local path or an S3 bucket — deployment
		// shape, not a credential. It is what makes "where did my documents go"
		// answerable from the UI.
		Backend:    s.Documents.Backend(),
		OCREnabled: s.Documents.OCREnabled() && s.AI.Enabled(),
	})
}
