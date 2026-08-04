package payroll

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"strings"
	"testing"
)

// The PDF fixtures are built here rather than checked in as binaries. A
// generated paystub is a handful of text-showing operators at known positions,
// so constructing one is both shorter than a fixture file and far more
// legible about what is actually being tested.

// run is one text-showing operation: a string drawn at a page position.
type run struct {
	x, y float64
	text string
}

// buildContentStream renders runs as a PDF content stream.
func buildContentStream(runs []run) string {
	var b strings.Builder
	for _, r := range runs {
		fmt.Fprintf(&b, "BT /F1 10 Tf 1 0 0 1 %g %g Tm (%s) Tj ET\n", r.x, r.y, r.text)
	}
	return b.String()
}

// buildPDF wraps a content stream in just enough PDF structure for the scanner:
// a header, and an object whose dictionary is immediately followed by the
// stream. Deliberately no xref table — the reader does not walk one, and a
// fixture that carried one would be testing something the code never reads.
func buildPDF(content string, compress bool) []byte {
	var body []byte
	filter := ""
	if compress {
		var buf bytes.Buffer
		zw := zlib.NewWriter(&buf)
		_, _ = zw.Write([]byte(content))
		_ = zw.Close()
		body = buf.Bytes()
		filter = " /Filter /FlateDecode"
	} else {
		body = []byte(content)
	}

	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	fmt.Fprintf(&out, "1 0 obj\n<< /Length %d%s >>\nstream\n", len(body), filter)
	out.Write(body)
	out.WriteString("\nendstream\nendobj\n%%EOF\n")
	return out.Bytes()
}

// paystubRuns is a stub laid out the way a payroll provider lays one out: a
// label column and two number columns, each drawn as its own text run, in an
// order that has nothing to do with reading order.
func paystubRuns() []run {
	return []run{
		{72, 740, "ACME MANUFACTURING INC"},
		{72, 720, "Earnings Statement"},
		{72, 700, "Pay Date: 06/12/2026"},
		{72, 686, "Period Beginning: 05/30/2026 Period Ending: 06/12/2026"},
		// Deliberately out of order: amounts before their labels, and the
		// deductions block before the earnings block.
		{300, 640, "330.00"},
		{400, 640, "3960.00"},
		{72, 640, "Federal Income Tax"},
		{300, 626, "179.80"},
		{72, 626, "Social Security"},
		{400, 626, "2157.60"},
		{72, 612, "Medicare"},
		{300, 612, "42.05"},
		{72, 660, "Gross Pay"},
		{300, 660, "3,000.00"},
		{72, 590, "Net Pay"},
		{300, 590, "2,448.15"},
	}
}

func TestExtractPDFTextGroupsRowsByPosition(t *testing.T) {
	lines, err := ExtractPDFText(buildPDF(buildContentStream(paystubRuns()), false))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	// The label and its amounts were three separate runs at the same vertical
	// position, drawn in the wrong order. Grouping by y and sorting by x is what
	// makes them one line — and one line is what makes label matching mean
	// anything at all.
	want := "Federal Income Tax 330.00 3960.00"
	if !containsLine(lines, want) {
		t.Errorf("expected a line %q, got:\n%s", want, strings.Join(lines, "\n"))
	}
	if !containsLine(lines, "Gross Pay 3,000.00") {
		t.Errorf("expected the gross line, got:\n%s", strings.Join(lines, "\n"))
	}

	// Rows come back top of page first: PDF's origin is the bottom-left, so a
	// larger y is earlier in reading order.
	grossAt, netAt := indexOfLine(lines, "Gross Pay 3,000.00"), indexOfLine(lines, "Net Pay 2,448.15")
	if grossAt < 0 || netAt < 0 || grossAt > netAt {
		t.Errorf("rows are not in reading order (gross at %d, net at %d):\n%s",
			grossAt, netAt, strings.Join(lines, "\n"))
	}
}

func TestExtractPDFTextReadsFlateStreams(t *testing.T) {
	// Every real paystub PDF is compressed; the uncompressed fixture above only
	// exists because it is easier to debug.
	lines, err := ExtractPDFText(buildPDF(buildContentStream(paystubRuns()), true))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !containsLine(lines, "Gross Pay 3,000.00") {
		t.Errorf("compressed stream did not decode, got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestExtractPDFTextRejectsNonPDF(t *testing.T) {
	if _, err := ExtractPDFText([]byte("this is a text file, not a PDF at all")); err != ErrNotPDF {
		t.Errorf("err = %v, want ErrNotPDF", err)
	}
}

// TestExtractPDFTextReportsMissingTextLayer is the scanned-stub path, and the
// message it produces is the whole fallback story: nothing is sent anywhere to
// read the image, so the user types the stub in.
func TestExtractPDFTextReportsMissingTextLayer(t *testing.T) {
	// A PDF whose only stream is an image — no text operators anywhere.
	pdf := []byte("%PDF-1.4\n1 0 obj\n<< /Subtype /Image /Length 4 >>\nstream\n\x01\x02\x03\x04\nendstream\nendobj\n")
	if _, err := ExtractPDFText(pdf); err != ErrNoTextLayer {
		t.Errorf("err = %v, want ErrNoTextLayer", err)
	}
}

// TestExtractPDFTextRefusesGarbledEncodings pins looksLikeText. A font with a
// custom CMap decodes to bytes that are not the characters they appear to be,
// and this reader deliberately does not implement CMap lookup — so the right
// outcome is "could not read it", never a gross salary derived from mis-decoded
// glyphs.
func TestExtractPDFTextRefusesGarbledEncodings(t *testing.T) {
	var b strings.Builder
	for i := range 20 {
		fmt.Fprintf(&b, "BT 1 0 0 1 72 %d Tm <0102030405060708> Tj ET\n", 700-i*12)
	}
	if _, err := ExtractPDFText(buildPDF(b.String(), false)); err != ErrNoTextLayer {
		t.Errorf("err = %v, want ErrNoTextLayer", err)
	}
}

// TestTJArrayKerningBecomesSpaces covers the encoding most providers actually
// emit: strings interleaved with kerning adjustments, where a word space is a
// large negative number rather than a space character.
func TestTJArrayKerningBecomesSpaces(t *testing.T) {
	content := "BT 1 0 0 1 72 700 Tm [(Federal)-400(Income)-400(Tax)] TJ ET\n" +
		"BT 1 0 0 1 300 700 Tm [(3)-20(3)-20(0)-20(.)-20(0)-20(0)] TJ ET\n" +
		"BT 1 0 0 1 72 680 Tm (Social Security Deduction Line Padding) Tj ET\n"

	lines, err := ExtractPDFText(buildPDF(content, false))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	// Word gaps become spaces; the tight kerning inside "330.00" must not, or
	// the amount pattern stops matching it.
	if !containsLine(lines, "Federal Income Tax 330.00") {
		t.Errorf("kerning was not handled, got:\n%s", strings.Join(lines, "\n"))
	}
}

// TestLiteralStringEscapes covers the escape forms a producer may emit inside a
// (...) string: octal, escaped parens, and a backslash-newline continuation.
func TestLiteralStringEscapes(t *testing.T) {
	content := "BT 1 0 0 1 72 700 Tm (Health \\(PPO\\) Premium) Tj ET\n" +
		"BT 1 0 0 1 300 700 Tm (100\\05600) Tj ET\n" +
		"BT 1 0 0 1 72 680 Tm (Dental Vision And Other Benefits) Tj ET\n"

	lines, err := ExtractPDFText(buildPDF(content, false))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	// \056 is octal for '.', so the amount reads back as 100.00.
	if !containsLine(lines, "Health (PPO) Premium 100.00") {
		t.Errorf("string escapes were not decoded, got:\n%s", strings.Join(lines, "\n"))
	}
}

func containsLine(lines []string, want string) bool {
	return indexOfLine(lines, want) >= 0
}

func indexOfLine(lines []string, want string) int {
	for i, l := range lines {
		if l == want {
			return i
		}
	}
	return -1
}
