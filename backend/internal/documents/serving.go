package documents

import (
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"unicode"
)

// This file holds the rules for handing a stored document back to a browser.
//
// The threat is small, specific and easy to get wrong: a user uploads an HTML
// file, calls it a receipt, and the vault serves it back with a
// user-controlled Content-Type. The browser renders it as a document on the
// app's own origin, and stored XSS reads every balance in the account. Nothing
// below is defence in depth — each piece closes that path, and they are only
// all here because any one of them alone can be worked around.

// safeContentTypes is the allowlist of types the vault will ever name in a
// response. Everything else is served as an opaque download.
//
// It is short on purpose. These are the formats a browser renders without
// script and that a document vault genuinely needs to preview; every addition
// is a new renderer given user-supplied bytes on our origin. Note the absence
// of image/svg+xml — SVG is a script-bearing document format wearing an image's
// clothes, and is exactly the kind of thing that looks safe on this list.
var safeContentTypes = map[string]bool{
	"image/png":       true,
	"image/jpeg":      true,
	"image/gif":       true,
	"image/webp":      true,
	"application/pdf": true,
}

// fallbackContentType is what everything not on the allowlist is served as. It
// makes the browser download rather than interpret.
const fallbackContentType = "application/octet-stream"

// ServedContentType decides the Content-Type for a download by sniffing the
// decrypted bytes — never by echoing the MIME type the uploader claimed.
//
// Sniffing rather than trusting stored metadata is the stronger choice in both
// directions: an HTML file uploaded as "image/png" is served as a download, and
// a real PNG uploaded as "text/html" is still served as a PNG.
func ServedContentType(plaintext []byte) string {
	// DetectContentType looks at the first 512 bytes and always returns
	// something, defaulting to application/octet-stream.
	sniffed := http.DetectContentType(plaintext)
	// It appends parameters for text types ("text/html; charset=utf-8"), which
	// never match the allowlist anyway — but compare on the bare type so the
	// allowlist stays a list of types rather than of exact header values.
	if base, _, err := mime.ParseMediaType(sniffed); err == nil {
		sniffed = base
	}
	if safeContentTypes[sniffed] {
		return sniffed
	}
	return fallbackContentType
}

// PreviewType is the client-facing hint for whether a document can be shown
// inline, derived from the *claimed* MIME type.
//
// It is only a hint, and it is safe for it to be wrong: the download response
// decides the real Content-Type from the actual bytes, so a document that lies
// about being a PNG simply fails to render. Empty string means "offer download
// only".
func PreviewType(claimedMIME string) string {
	base, _, err := mime.ParseMediaType(claimedMIME)
	if err != nil {
		return ""
	}
	if safeContentTypes[strings.ToLower(base)] {
		return strings.ToLower(base)
	}
	return ""
}

// NormaliseMIME cleans a client-supplied MIME type for storage. An unparseable
// or absurd value becomes the generic type rather than being rejected — the
// value is display metadata and is never used to decide how bytes are served,
// so there is nothing to gain by failing an upload over it.
func NormaliseMIME(claimed string) string {
	base, _, err := mime.ParseMediaType(strings.TrimSpace(claimed))
	if err != nil || base == "" || len(base) > 128 {
		return fallbackContentType
	}
	return strings.ToLower(base)
}

// maxFilenameLength keeps a stored name to something a filesystem and a header
// can both carry comfortably.
const maxFilenameLength = 200

// SanitiseFilename reduces an uploaded name to something safe to store and to
// echo in a Content-Disposition header.
//
// Note what this is NOT for: it never decides where bytes are written. Storage
// keys are generated UUIDs (see NewStorageKey), so a filename of
// "../../etc/passwd" was already harmless before this function saw it. What is
// left to defend is the download header, where a newline would let an uploader
// inject response headers, and the user's own filesystem, where a leading
// "../" would be honoured by some download managers.
func SanitiseFilename(name string) string {
	// Take the base under both separator conventions: a Windows client sends
	// "C:\Users\me\receipt.pdf" and filepath.Base on Linux keeps all of it.
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)

	var b strings.Builder
	for _, r := range name {
		switch {
		// Control characters, CR and LF above all: a filename is interpolated
		// into a response header, and a newline there is header injection.
		case r < 0x20 || r == 0x7f:
			continue
		// Reserved on Windows, and quote/backslash would break out of the
		// quoted-string in Content-Disposition.
		case strings.ContainsRune(`<>:"/\|?*`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}

	cleaned := strings.TrimSpace(b.String())
	// "." and ".." survive everything above and are not names.
	cleaned = strings.Trim(cleaned, ".")
	cleaned = strings.TrimSpace(cleaned)

	if len(cleaned) > maxFilenameLength {
		// Truncate the stem, keep the extension: a name that loses its ".pdf"
		// is materially less useful to the person who downloaded it.
		ext := filepath.Ext(cleaned)
		if len(ext) > 16 {
			ext = "" // not really an extension
		}
		keep := maxFilenameLength - len(ext)
		cleaned = strings.TrimSpace(cleaned[:keep]) + ext
	}

	if cleaned == "" {
		return "document"
	}
	return cleaned
}

// ContentDisposition builds the download header for a filename.
//
// Both forms are emitted per RFC 6266: a plain ASCII `filename=` that every
// client understands, and a `filename*=` in UTF-8 for names with accents or
// non-Latin scripts. It is always "attachment" — the vault has no inline
// download path, because "inline" is the whole vulnerability this file exists
// to close.
func ContentDisposition(filename string) string {
	safe := SanitiseFilename(filename)

	// The ASCII fallback: anything outside printable ASCII becomes an
	// underscore, so the quoted-string is unambiguous for old clients.
	var ascii strings.Builder
	for _, r := range safe {
		if r > unicode.MaxASCII || r < 0x20 || r == '"' || r == '\\' {
			ascii.WriteRune('_')
			continue
		}
		ascii.WriteRune(r)
	}
	fallback := ascii.String()
	if strings.Trim(fallback, "_ ") == "" {
		fallback = "document"
	}

	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`,
		fallback, escapeExtValue(safe))
}

// escapeExtValue percent-encodes for the RFC 5987 ext-value used by
// `filename*=`, where only attr-char survives unescaped.
func escapeExtValue(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			strings.IndexByte("!#$&+-.^_`|~", c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
