package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/crypto"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/documents"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/testdb"
)

// The document vault's HTTP surface, end to end against a real Postgres and a
// real filesystem backend. The cases are the ones the plan calls out: an
// encrypted round trip, ownership that holds on the download path and not just
// the listing, a traversing filename, an HTML file filed as a receipt, the size
// cap, and the quota.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/api/
func TestDocumentEndpoints(t *testing.T) {
	url := testdb.URL(t)

	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	root := t.TempDir()
	store, err := documents.NewLocalStorage(root)
	if err != nil {
		t.Fatalf("local storage: %v", err)
	}
	cipher, err := crypto.New(bytes.Repeat([]byte{0x11}, 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}

	vaultCfg := config.DocumentsConfig{
		Enabled:      true,
		Backend:      "local",
		LocalRoot:    root,
		MaxFileBytes: 4096,
		QuotaBytes:   8192,
	}

	srv := &Server{
		Pool:      pool,
		Queries:   dbgen.New(pool),
		Cipher:    cipher,
		Documents: documents.New(vaultCfg, cipher, store),
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}

	callerHousehold, callerUser := uuid.New(), uuid.New()
	mateUser := uuid.New()
	otherHousehold, otherUser := uuid.New(), uuid.New()

	exec(`INSERT INTO households (id, name) VALUES ($1, 'Vault Caller'), ($2, 'Vault Other')`,
		callerHousehold, otherHousehold)
	for _, u := range []struct {
		id uuid.UUID
		hh uuid.UUID
	}{{callerUser, callerHousehold}, {mateUser, callerHousehold}, {otherUser, otherHousehold}} {
		exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
		      VALUES ($1, $2, $3, 'x', 'Tester')`, u.id, u.hh, u.id.String()+"@example.test")
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = ANY($1)`,
			[]uuid.UUID{callerHousehold, otherHousehold})
	})

	// A goal in each household, for the link-ownership case.
	callerGoal, foreignGoal := uuid.New(), uuid.New()
	exec(`INSERT INTO goals (id, household_id, scope, kind, name, target_amount)
	      VALUES ($1, $2, 'household', 'savings', 'New roof', '9000.00'),
	             ($3, $4, 'household', 'savings', 'Not yours', '1000.00')`,
		callerGoal, callerHousehold, foreignGoal, otherHousehold)

	identity := func(user, household uuid.UUID) auth.Identity {
		return auth.Identity{UserID: user, HouseholdID: household, DisplayName: "Tester"}
	}
	caller := identity(callerUser, callerHousehold)
	mate := identity(mateUser, callerHousehold)
	stranger := identity(otherUser, otherHousehold)

	as := func(who auth.Identity, r *http.Request) *http.Request {
		return r.WithContext(auth.ContextWithIdentity(ctx, who))
	}
	// Stands in for the router, which normally resolves {documentID}.
	withDoc := func(who auth.Identity, r *http.Request, id uuid.UUID) *http.Request {
		routeCtx := chi.NewRouteContext()
		routeCtx.URLParams.Add("documentID", id.String())
		return r.WithContext(context.WithValue(
			auth.ContextWithIdentity(ctx, who), chi.RouteCtxKey, routeCtx))
	}

	// upload posts a multipart form the way a browser would.
	upload := func(who auth.Identity, filename string, content []byte, fields map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, err := form.CreateFormFile("file", filename)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatalf("write form file: %v", err)
		}
		for k, v := range fields {
			_ = form.WriteField(k, v)
		}
		form.Close()

		req := httptest.NewRequest(http.MethodPost, "/api/documents/", &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		rec := httptest.NewRecorder()
		srv.handleUploadDocument(rec, as(who, req))
		return rec
	}

	receipt := []byte("RECEIPT\nCostco\nTotal 84.20\n")
	var stored documentResponse

	t.Run("upload encrypts the bytes on disk and downloads them intact", func(t *testing.T) {
		rec := upload(caller, "costco receipt.pdf", receipt, map[string]string{
			"title":         "Costco run",
			"doc_type":      "receipt",
			"document_date": "2026-07-04",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if stored.SizeBytes != int64(len(receipt)) {
			t.Errorf("size_bytes = %d, want %d", stored.SizeBytes, len(receipt))
		}
		// Retention is computed on write, not left for a later read.
		if stored.RetainUntil == nil {
			t.Error("retain_until was not computed on upload")
		}

		// The blob on disk must not be the plaintext. Asserted against the
		// filesystem rather than inferred from a successful download, because a
		// vault that forgot to encrypt would download perfectly too.
		var key string
		if err := pool.QueryRow(ctx,
			`SELECT storage_key FROM documents WHERE id = $1`, stored.ID).Scan(&key); err != nil {
			t.Fatalf("read storage key: %v", err)
		}
		onDisk, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(key)))
		if err != nil {
			t.Fatalf("read blob: %v", err)
		}
		if bytes.Contains(onDisk, receipt) || bytes.Contains(onDisk, []byte("Costco")) {
			t.Fatal("the plaintext is readable in the stored file")
		}

		rec = httptest.NewRecorder()
		srv.handleDownloadDocument(rec, withDoc(caller, httptest.NewRequest(http.MethodGet, "/d", nil), stored.ID))
		if rec.Code != http.StatusOK {
			t.Fatalf("download status = %d; body: %s", rec.Code, rec.Body.String())
		}
		if !bytes.Equal(rec.Body.Bytes(), receipt) {
			t.Error("downloaded bytes differ from what was uploaded")
		}
	})

	t.Run("a filename that tries to traverse is stored and served safely", func(t *testing.T) {
		rec := upload(caller, "../../etc/passwd", []byte("root:x:0:0:"), map[string]string{
			"doc_type": "other",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
		}
		var doc documentResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if doc.Filename != "passwd" {
			t.Errorf("filename = %q, want %q", doc.Filename, "passwd")
		}
		// Nothing was written outside the storage root.
		if _, err := os.Stat(filepath.Join(filepath.Dir(root), "etc")); err == nil {
			t.Error("a directory was created outside the storage root")
		}

		rec = httptest.NewRecorder()
		srv.handleDownloadDocument(rec, withDoc(caller, httptest.NewRequest(http.MethodGet, "/d", nil), doc.ID))
		if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, `filename="passwd"`) {
			t.Errorf("Content-Disposition = %q, want a sanitised filename", got)
		}
	})

	t.Run("an HTML file filed as a receipt downloads rather than rendering", func(t *testing.T) {
		html := []byte(`<!doctype html><script>alert(document.cookie)</script>`)
		rec := upload(caller, "receipt.html", html, map[string]string{"doc_type": "receipt"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
		}
		var doc documentResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if doc.PreviewType != "" {
			t.Errorf("preview_type = %q, want empty; HTML must never be offered for inline preview", doc.PreviewType)
		}

		rec = httptest.NewRecorder()
		srv.handleDownloadDocument(rec, withDoc(caller, httptest.NewRequest(http.MethodGet, "/d", nil), doc.ID))

		if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
			t.Errorf("Content-Type = %q, want application/octet-stream", got)
		}
		if got := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment;") {
			t.Errorf("Content-Disposition = %q, want an attachment", got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
		}
	})

	t.Run("another household cannot read the document, even with its id", func(t *testing.T) {
		// The listing.
		rec := httptest.NewRecorder()
		srv.handleListDocuments(rec, as(stranger, httptest.NewRequest(http.MethodGet, "/api/documents/", nil)))
		var theirs []documentResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &theirs); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(theirs) != 0 {
			t.Errorf("another household sees %d documents, want 0", len(theirs))
		}

		// And the download, which is the one that matters: a document id must
		// never be sufficient on its own.
		for _, tc := range []struct {
			name    string
			handler func(http.ResponseWriter, *http.Request)
		}{
			{"download", srv.handleDownloadDocument},
			{"metadata", srv.handleGetDocument},
			{"delete", srv.handleDeleteDocument},
		} {
			rec := httptest.NewRecorder()
			tc.handler(rec, withDoc(stranger, httptest.NewRequest(http.MethodGet, "/d", nil), stored.ID))
			// 404 and not 403: a 403 would confirm the id exists elsewhere.
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s: status = %d, want 404", tc.name, rec.Code)
			}
			if strings.Contains(rec.Body.String(), "Costco") {
				t.Errorf("%s: the response leaked document content", tc.name)
			}
		}
	})

	t.Run("a private document is invisible to another household member", func(t *testing.T) {
		rec := upload(caller, "divorce.pdf", []byte("private paperwork"), map[string]string{
			"doc_type":  "contract",
			"title":     "Private",
			"is_shared": "false",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
		}
		var private documentResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &private); err != nil {
			t.Fatalf("decode: %v", err)
		}

		rec = httptest.NewRecorder()
		srv.handleListDocuments(rec, as(mate, httptest.NewRequest(http.MethodGet, "/api/documents/", nil)))
		var visible []documentResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &visible); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, d := range visible {
			if d.ID == private.ID {
				t.Fatal("a private document appears in another member's listing")
			}
		}

		rec = httptest.NewRecorder()
		srv.handleDownloadDocument(rec, withDoc(mate, httptest.NewRequest(http.MethodGet, "/d", nil), private.ID))
		if rec.Code != http.StatusNotFound {
			t.Errorf("household member downloading a private document: status = %d, want 404", rec.Code)
		}

		// The owner still reaches it.
		rec = httptest.NewRecorder()
		srv.handleDownloadDocument(rec, withDoc(caller, httptest.NewRequest(http.MethodGet, "/d", nil), private.ID))
		if rec.Code != http.StatusOK {
			t.Errorf("owner downloading their own private document: status = %d, want 200", rec.Code)
		}
	})

	t.Run("the size cap rejects cleanly with no row written", func(t *testing.T) {
		var before int64
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM documents WHERE household_id = $1`, callerHousehold).Scan(&before); err != nil {
			t.Fatalf("count: %v", err)
		}

		rec := upload(caller, "big.bin", bytes.Repeat([]byte("x"), 5000), map[string]string{
			"doc_type": "other",
		})
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413; body: %s", rec.Code, rec.Body.String())
		}

		var after int64
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM documents WHERE household_id = $1`, callerHousehold).Scan(&after); err != nil {
			t.Fatalf("count: %v", err)
		}
		if after != before {
			t.Errorf("a rejected upload wrote %d row(s)", after-before)
		}
	})

	t.Run("an empty file is refused", func(t *testing.T) {
		rec := upload(caller, "nothing.pdf", nil, map[string]string{"doc_type": "other"})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an unknown doc_type is refused before it reaches the constraint", func(t *testing.T) {
		rec := upload(caller, "x.pdf", []byte("x"), map[string]string{"doc_type": "invoice"})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("the quota stops an upload at the boundary", func(t *testing.T) {
		// Fill the household to just under the 8192-byte quota, then try to
		// cross it. Sizes are chosen so the last upload is refused for the quota
		// rather than the per-file cap.
		for i := range 2 {
			rec := upload(caller, fmt.Sprintf("filler-%d.bin", i), bytes.Repeat([]byte("f"), 3000),
				map[string]string{"doc_type": "other"})
			if rec.Code != http.StatusCreated {
				t.Fatalf("filler %d: status = %d; body: %s", i, rec.Code, rec.Body.String())
			}
		}

		rec := upload(caller, "over.bin", bytes.Repeat([]byte("o"), 3000), map[string]string{
			"doc_type": "other",
		})
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413; body: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "limit") {
			t.Errorf("the quota error does not say what the limit is: %s", rec.Body.String())
		}

		usage, err := srv.Queries.HouseholdStorageUsed(ctx, callerHousehold)
		if err != nil {
			t.Fatalf("usage: %v", err)
		}
		if usage.BytesUsed > vaultCfg.QuotaBytes {
			t.Errorf("stored %d bytes against a %d quota", usage.BytesUsed, vaultCfg.QuotaBytes)
		}
	})

	t.Run("a link to another household's record is refused", func(t *testing.T) {
		body := fmt.Sprintf(`{"target_kind":"goal","target_id":%q}`, foreignGoal)
		rec := httptest.NewRecorder()
		srv.handleLinkDocument(rec,
			withDoc(caller, httptest.NewRequest(http.MethodPost, "/l", strings.NewReader(body)), stored.ID))

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
		}
		// The refusal must not distinguish "does not exist" from "not yours",
		// or it becomes a probe for another household's records.
		if strings.Contains(strings.ToLower(rec.Body.String()), "household") {
			t.Errorf("the refusal reveals why: %s", rec.Body.String())
		}

		var links int64
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM document_links WHERE goal_id = $1`, foreignGoal).Scan(&links); err != nil {
			t.Fatalf("count links: %v", err)
		}
		if links != 0 {
			t.Error("a link to another household's goal was written")
		}
	})

	t.Run("a link to the caller's own goal is accepted and listed", func(t *testing.T) {
		body := fmt.Sprintf(`{"target_kind":"goal","target_id":%q}`, callerGoal)
		rec := httptest.NewRecorder()
		srv.handleLinkDocument(rec,
			withDoc(caller, httptest.NewRequest(http.MethodPost, "/l", strings.NewReader(body)), stored.ID))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
		}

		rec = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/documents/attached?goal_id="+callerGoal.String(), nil)
		srv.handleAttachedDocuments(rec, as(caller, req))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
		}
		var attached []documentResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &attached); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(attached) != 1 || attached[0].ID != stored.ID {
			t.Errorf("attached = %d document(s), want the one just linked", len(attached))
		}

		// And the other household sees nothing on its own goal.
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/api/documents/attached?goal_id="+callerGoal.String(), nil)
		srv.handleAttachedDocuments(rec, as(stranger, req))
		var strangersView []documentResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &strangersView); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(strangersView) != 0 {
			t.Errorf("another household sees %d attachments, want 0", len(strangersView))
		}
	})

	t.Run("delete removes the row and the blob", func(t *testing.T) {
		rec := upload(caller, "temp.txt", []byte("delete me"), map[string]string{"doc_type": "other"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
		}
		var doc documentResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode: %v", err)
		}

		var key string
		if err := pool.QueryRow(ctx,
			`SELECT storage_key FROM documents WHERE id = $1`, doc.ID).Scan(&key); err != nil {
			t.Fatalf("read storage key: %v", err)
		}

		rec = httptest.NewRecorder()
		srv.handleDeleteDocument(rec, withDoc(caller, httptest.NewRequest(http.MethodDelete, "/d", nil), doc.ID))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body: %s", rec.Code, rec.Body.String())
		}

		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(key))); !os.IsNotExist(err) {
			t.Error("the blob outlived its row")
		}
	})

	// The extraction endpoint refuses a non-receipt before it decrypts anything.
	// Asserted at the HTTP layer as well as in the documents package, because
	// this is the gate that decides what leaves the host and the UI hiding a
	// button is not a control.
	t.Run("a tax document is never eligible for extraction", func(t *testing.T) {
		// A real PNG, filed as a tax document — so the only thing standing
		// between it and an upload is the doc_type check.
		png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 64))
		rec := upload(caller, "w2-scan.png", png, map[string]string{
			"doc_type": "tax",
			"title":    "2025 W-2",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
		}
		var taxDoc documentResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &taxDoc); err != nil {
			t.Fatalf("decode: %v", err)
		}

		// A vault with OCR fully switched on, so the refusal is the doc_type
		// gate and not an incidentally disabled feature.
		ocrOn := *srv
		ocrCfg := vaultCfg
		ocrCfg.OCREnabled = true
		ocrOn.Documents = documents.New(ocrCfg, cipher, store)
		ocrOn.AI = ai.New(config.AIConfig{APIKey: "test-key", BaseURL: "http://127.0.0.1:1", Model: "test"})
		if !ocrOn.Documents.OCREnabled() || !ocrOn.AI.Enabled() {
			t.Fatal("the test setup did not actually enable OCR")
		}

		rec = httptest.NewRecorder()
		ocrOn.handleExtractDocument(rec,
			withDoc(caller, httptest.NewRequest(http.MethodPost, "/x", nil), taxDoc.ID))

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; a tax document must never be sent for extraction", rec.Code)
		}

		// And a receipt with OCR on gets past this gate — proving the 403 above
		// is the type check rather than something failing earlier. The call then
		// fails at the (unreachable) provider, which is a 500, not a 403.
		rec = httptest.NewRecorder()
		ocrOn.handleExtractDocument(rec,
			withDoc(caller, httptest.NewRequest(http.MethodPost, "/x", nil), stored.ID))
		if rec.Code == http.StatusForbidden {
			t.Error("a receipt was refused by the eligibility gate")
		}
	})

	t.Run("a corrupted blob fails closed with a clear error", func(t *testing.T) {
		rec := upload(caller, "fragile.txt", []byte("original contents"), map[string]string{"doc_type": "other"})
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
		}
		var doc documentResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode: %v", err)
		}

		var key string
		if err := pool.QueryRow(ctx,
			`SELECT storage_key FROM documents WHERE id = $1`, doc.ID).Scan(&key); err != nil {
			t.Fatalf("read storage key: %v", err)
		}
		path := filepath.Join(root, filepath.FromSlash(key))
		sealed, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read blob: %v", err)
		}
		sealed[len(sealed)-1] ^= 0xff
		if err := os.WriteFile(path, sealed, 0o600); err != nil {
			t.Fatalf("write blob: %v", err)
		}

		rec = httptest.NewRecorder()
		srv.handleDownloadDocument(rec, withDoc(caller, httptest.NewRequest(http.MethodGet, "/d", nil), doc.ID))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body: %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "original contents") {
			t.Error("partial content was served alongside the error")
		}
		// The message has to point an operator at the likely cause.
		if !strings.Contains(rec.Body.String(), "ENCRYPTION_KEY") {
			t.Errorf("the error does not mention the likely cause: %s", rec.Body.String())
		}
	})
}

// With no vault configured, every route reports 503 rather than panicking on a
// nil Documents — the same contract the Plaid handlers have.
func TestDocumentEndpointsWithoutAVault(t *testing.T) {
	srv := &Server{}
	identity := auth.Identity{UserID: uuid.New(), HouseholdID: uuid.New()}

	for name, handler := range map[string]http.HandlerFunc{
		"list":     srv.handleListDocuments,
		"upload":   srv.handleUploadDocument,
		"storage":  srv.handleDocumentStorage,
		"attached": srv.handleAttachedDocuments,
		"counts":   srv.handleDocumentCounts,
		"get":      srv.handleGetDocument,
		"download": srv.handleDownloadDocument,
		"delete":   srv.handleDeleteDocument,
		"update":   srv.handleUpdateDocument,
		"link":     srv.handleLinkDocument,
		"unlink":   srv.handleUnlinkDocument,
		"extract":  srv.handleExtractDocument,
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/documents/", io.LimitReader(strings.NewReader("{}"), 2))
		req = req.WithContext(auth.ContextWithIdentity(context.Background(), identity))
		rec := httptest.NewRecorder()

		handler(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503", name, rec.Code)
		}
	}
}
