package documents

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/crypto"
)

// These tests need no database. They cover the parts of the vault where a
// mistake is silent: bytes that turn out not to be encrypted, a filename that
// escapes its directory, and an HTML file that gets served as a document.

func testVault(t *testing.T, cfg config.DocumentsConfig) (*Vault, string) {
	t.Helper()

	root := t.TempDir()
	store, err := NewLocalStorage(root)
	if err != nil {
		t.Fatalf("new local storage: %v", err)
	}

	key := bytes.Repeat([]byte{0x42}, 32)
	cipher, err := crypto.New(key)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}

	if cfg.MaxFileBytes == 0 {
		cfg.MaxFileBytes = 1 << 20
	}
	return New(cfg, cipher, store), root
}

// The round trip the plan names first: what lands on disk is not the plaintext,
// and what comes back is byte-identical to what went in.
func TestStoreRoundTripIsEncryptedOnDisk(t *testing.T) {
	vault, root := testVault(t, config.DocumentsConfig{})
	ctx := context.Background()

	plaintext := []byte("SSN 123-45-6789, salary 91,000, and other things worth encrypting")

	stored, err := vault.Store(ctx, plaintext)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	onDisk, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(stored.StorageKey)))
	if err != nil {
		t.Fatalf("read stored blob: %v", err)
	}

	// Asserted directly rather than inferred from "it decrypts": a Store that
	// forgot to seal would still round-trip perfectly.
	if bytes.Contains(onDisk, plaintext) {
		t.Fatal("the plaintext is present in the stored file; it was not encrypted")
	}
	if bytes.Contains(onDisk, []byte("123-45-6789")) {
		t.Fatal("part of the plaintext survives in the stored file")
	}

	got, err := vault.Fetch(ctx, stored.StorageKey, stored.ContentHash)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("fetched bytes differ from what was stored")
	}
}

// A blob that decrypts to something other than what was recorded must not be
// served. GCM authenticates the bytes, so this is specifically the wrong-blob
// case: intact ciphertext belonging to a different document.
func TestFetchRejectsWrongBlob(t *testing.T) {
	vault, _ := testVault(t, config.DocumentsConfig{})
	ctx := context.Background()

	mine, err := vault.Store(ctx, []byte("my tax return"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	theirs, err := vault.Store(ctx, []byte("somebody else's tax return"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	// Their blob, my recorded hash — a storage-layer mixup.
	if _, err := vault.Fetch(ctx, theirs.StorageKey, mine.ContentHash); err != ErrCorrupt {
		t.Errorf("err = %v, want ErrCorrupt; a mixed-up blob decrypts fine and must be caught by the hash", err)
	}
}

func TestFetchFailsClosedOnCorruptCiphertext(t *testing.T) {
	vault, root := testVault(t, config.DocumentsConfig{})
	ctx := context.Background()

	stored, err := vault.Store(ctx, []byte("a warranty certificate"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	path := filepath.Join(root, filepath.FromSlash(stored.StorageKey))
	sealed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	sealed[len(sealed)-1] ^= 0xff // flip a bit in the GCM tag
	if err := os.WriteFile(path, sealed, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := vault.Fetch(ctx, stored.StorageKey, stored.ContentHash)
	if err != ErrCorrupt {
		t.Errorf("err = %v, want ErrCorrupt", err)
	}
	if got != nil {
		t.Error("partial output was returned alongside the error; a failed decrypt must yield nothing")
	}
}

func TestFetchReportsMissingBlobDistinctly(t *testing.T) {
	vault, _ := testVault(t, config.DocumentsConfig{})

	_, err := vault.Fetch(context.Background(),
		"ab/cd/00000000-0000-0000-0000-000000000000.bin", "whatever")
	if err != ErrNotFound {
		// An operator needs "the file is gone" and "the file is wrong" to be
		// different answers.
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestSizeCapRejectsBeforeBuffering(t *testing.T) {
	vault, _ := testVault(t, config.DocumentsConfig{MaxFileBytes: 64})

	if _, err := vault.ReadLimited(bytes.NewReader(bytes.Repeat([]byte("x"), 65))); err != ErrTooLarge {
		t.Errorf("65 bytes against a 64 byte cap: err = %v, want ErrTooLarge", err)
	}
	// Exactly at the limit is allowed — the +1 read in ReadLimited exists to
	// tell "at" from "over", and an off-by-one here would reject a legal file.
	if _, err := vault.ReadLimited(bytes.NewReader(bytes.Repeat([]byte("x"), 64))); err != nil {
		t.Errorf("exactly at the cap: err = %v, want nil", err)
	}
	if _, err := vault.ReadLimited(bytes.NewReader(nil)); err != ErrEmpty {
		t.Errorf("empty upload: err = %v, want ErrEmpty", err)
	}
}

// Storage keys are generated, never derived from user input. This pins that
// the generated shape is what the backends will accept, so the two cannot
// drift apart.
func TestStorageKeysAreGeneratedAndValidated(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		key := NewStorageKey()
		if err := validateKey(key); err != nil {
			t.Fatalf("generated key %q is rejected by its own validator: %v", key, err)
		}
		if seen[key] {
			t.Fatalf("duplicate storage key %q", key)
		}
		seen[key] = true
	}

	for _, bad := range []string{
		"../../etc/passwd",
		"ab/cd/../../../etc/passwd.bin",
		"/absolute/path.bin",
		"ab/cd/not-a-uuid.bin",
		"",
	} {
		if err := validateKey(bad); err == nil {
			t.Errorf("validateKey(%q) = nil, want an error", bad)
		}
	}
}

func TestLocalStorageRefusesKeysOutsideItsRoot(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocalStorage(root)
	if err != nil {
		t.Fatalf("new local storage: %v", err)
	}

	// Even if a malformed key somehow reached this layer from the database, it
	// must not resolve to a path outside the root.
	if err := store.Put(context.Background(), "../escaped.bin", []byte("x")); err == nil {
		t.Error("Put accepted a traversing key")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escaped.bin")); err == nil {
		t.Error("a file was written outside the storage root")
	}
}

// --------------------------------------------------------------------------
// Serving rules
// --------------------------------------------------------------------------

// The named attack: an HTML file filed as a receipt must never come back with a
// content type a browser will render on this origin.
func TestHTMLUploadedAsReceiptIsServedAsADownload(t *testing.T) {
	html := []byte(`<!doctype html><script>fetch('/api/accounts').then(r=>r.json())</script>`)

	if got := ServedContentType(html); got != "application/octet-stream" {
		t.Errorf("ServedContentType(html) = %q, want application/octet-stream", got)
	}
	// And the claim it was uploaded under buys it nothing.
	if got := PreviewType("text/html"); got != "" {
		t.Errorf("PreviewType(\"text/html\") = %q, want \"\" (no inline preview)", got)
	}
}

func TestServedContentTypeSniffsRatherThanTrusts(t *testing.T) {
	pngHeader := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32))
	pdfHeader := []byte("%PDF-1.7\n" + strings.Repeat("\x00", 32))

	if got := ServedContentType(pngHeader); got != "image/png" {
		t.Errorf("ServedContentType(png) = %q, want image/png", got)
	}
	if got := ServedContentType(pdfHeader); got != "application/pdf" {
		t.Errorf("ServedContentType(pdf) = %q, want application/pdf", got)
	}
	// A .docx and friends are opaque downloads, which is correct: the vault
	// should store them and the browser should not try to interpret them.
	if got := ServedContentType([]byte("PK\x03\x04zip content here")); got != "application/octet-stream" {
		t.Errorf("ServedContentType(zip) = %q, want application/octet-stream", got)
	}
}

// SVG is the entry most likely to be added to the allowlist by someone who
// reasons "it is an image". It is a scriptable document.
func TestSVGIsNotPreviewable(t *testing.T) {
	if got := PreviewType("image/svg+xml"); got != "" {
		t.Errorf("PreviewType(svg) = %q, want \"\": SVG can carry script", got)
	}
}

func TestSanitiseFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		// The traversal case from the plan. Storage keys are generated anyway,
		// so this defends the download header and the user's own filesystem.
		{"../../etc/passwd", "passwd"},
		{"..\\..\\windows\\system32\\config", "config"},
		{"/etc/shadow", "shadow"},
		{"receipt.pdf", "receipt.pdf"},
		{"Costco — 12 March.pdf", "Costco — 12 March.pdf"},
		// Header injection: a newline in a filename is a new response header.
		// The colon goes too, being reserved on Windows.
		{"receipt\r\nSet-Cookie: a=b.pdf", "receiptSet-Cookie_ a=b.pdf"},
		{"a\x00b.pdf", "ab.pdf"},
		{"...", "document"},
		{"", "document"},
		{"   ", "document"},
	}
	for _, tc := range cases {
		if got := SanitiseFilename(tc.in); got != tc.want {
			t.Errorf("SanitiseFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	long := strings.Repeat("a", 400) + ".pdf"
	got := SanitiseFilename(long)
	if len(got) > maxFilenameLength {
		t.Errorf("length %d exceeds the %d cap", len(got), maxFilenameLength)
	}
	if !strings.HasSuffix(got, ".pdf") {
		t.Errorf("truncation dropped the extension: %q", got)
	}
}

func TestContentDispositionIsAlwaysAttachment(t *testing.T) {
	for _, name := range []string{
		"receipt.pdf",
		`evil"; filename="x.html`,
		"../../etc/passwd",
		"réçu.pdf",
		"发票.pdf",
	} {
		got := ContentDisposition(name)

		if !strings.HasPrefix(got, "attachment;") {
			t.Errorf("ContentDisposition(%q) = %q, want an attachment disposition", name, got)
		}
		// A quote or a newline escaping the quoted-string would let a filename
		// forge header content.
		if strings.ContainsAny(got, "\r\n") {
			t.Errorf("ContentDisposition(%q) contains a newline: %q", name, got)
		}
		if strings.Count(got, `"`) != 2 {
			t.Errorf("ContentDisposition(%q) = %q: the quoted-string is not balanced", name, got)
		}
		if !strings.Contains(got, "filename*=UTF-8''") {
			t.Errorf("ContentDisposition(%q) = %q, want an RFC 5987 form too", name, got)
		}
	}
}

func TestNormaliseMIMEFallsBackRatherThanRejecting(t *testing.T) {
	cases := map[string]string{
		"image/png":                     "image/png",
		"IMAGE/PNG":                     "image/png",
		"application/pdf; charset=utf8": "application/pdf",
		"not a mime type":               "application/octet-stream",
		"":                              "application/octet-stream",
	}
	for in, want := range cases {
		if got := NormaliseMIME(in); got != want {
			t.Errorf("NormaliseMIME(%q) = %q, want %q", in, got, want)
		}
	}
}

// --------------------------------------------------------------------------
// Retention
// --------------------------------------------------------------------------

func TestRetainUntil(t *testing.T) {
	date := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	uploaded := date(2026, time.July, 28)

	docDate := date(2025, time.April, 15)
	expiry := date(2027, time.January, 1)

	cases := []struct {
		name    string
		docType string
		docDate *time.Time
		expires *time.Time
		want    time.Time
	}{
		{"tax runs seven years from the document's own date", TypeTax, &docDate, nil, date(2032, time.April, 15)},
		{"no document date falls back to the upload", TypeTax, nil, nil, date(2033, time.July, 28)},
		{"a warranty is kept a little past its expiry", TypeWarranty, nil, &expiry, date(2027, time.April, 1)},
		{"a policy outlives its renewal by a claim window", TypeInsurance, nil, &expiry, date(2028, time.January, 1)},
		{"a warranty with no expiry falls back to the type's term", TypeWarranty, &docDate, nil, date(2028, time.April, 15)},
		{"an unknown type is kept, not discarded", "not-a-type", &docDate, nil, date(2032, time.April, 15)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RetainUntil(tc.docType, tc.docDate, tc.expires, uploaded)
			if !got.Equal(tc.want) {
				t.Errorf("RetainUntil = %s, want %s", got.Format(time.DateOnly), tc.want.Format(time.DateOnly))
			}
		})
	}
}

// Every type the database will accept must have a retention rule, or a new
// doc_type silently inherits a default nobody chose.
func TestEveryValidTypeHasRetention(t *testing.T) {
	for docType := range ValidTypes {
		if _, ok := retentionYears[docType]; !ok {
			t.Errorf("doc_type %q is valid but has no retention rule", docType)
		}
	}
}

// --------------------------------------------------------------------------
// OCR eligibility
// --------------------------------------------------------------------------

// The narrowest gate in the vault, and the one with the worst failure mode: a
// document sent to a third party cannot be unsent. This test exists so that
// widening the allowlist is a deliberate edit to an assertion rather than
// something that happens as a side effect.
func TestOnlyReceiptsAreOCREligible(t *testing.T) {
	if !OCREligible(TypeReceipt) {
		t.Error("receipts must be eligible; the feature does nothing otherwise")
	}

	// Named individually rather than looped over ValidTypes, so adding a
	// doc_type does not quietly satisfy this test.
	for _, docType := range []string{
		TypeTax,       // name, address, SSN, full financial picture
		TypeInsurance, // policy numbers, dependants, addresses
		TypeContract,  // signatures, terms, counterparties
		TypeStatement, // account numbers and a full transaction history
		TypeWarranty,
		TypeOther, // the bucket unsorted scans land in
	} {
		if OCREligible(docType) {
			t.Errorf("doc_type %q is OCR-eligible; only receipts may be sent off the host", docType)
		}
	}

	// An unknown type must be refused, not defaulted into eligibility. This is
	// the property a blocklist would not have.
	if OCREligible("some-future-type") {
		t.Error("an unrecognised doc_type is OCR-eligible; the allowlist must fail closed")
	}
}
