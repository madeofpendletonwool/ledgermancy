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

// TestExtractPDFTextHandlesRealWorldStreamDictionaries covers the three shapes
// every mainstream payroll PDF has and the simple fixture above does not.
//
// This is a regression test with a specific history. `dictBefore` walks
// backwards from the `stream` keyword counting `>>`/`<<` pairs, and it shipped
// starting one byte too late — so the closing `>>` was never counted, depth
// never reached 1, and EVERY stream in EVERY PDF was skipped. The importer
// reported "no readable text layer" for all input, including the files it was
// built for. The flat-dictionary fixtures caught it; these cover the branch a
// flat dictionary never reaches at all.
//
//	nested dictionary  /DecodeParms << /Predictor 12 >> — the actual nesting
//	                   path, which a flat dict leaves entirely unexecuted
//	indirect /Length   `/Length 7 0 R` — extremely common, and the reason the
//	                   scanner bounds streams with `endstream` rather than
//	                   trusting /Length, which would need xref resolution
//	multiple objects   a real file has many, including non-content ones the
//	                   scan must walk past without tripping over
func TestExtractPDFTextHandlesRealWorldStreamDictionaries(t *testing.T) {
	page1 := buildContentStream([]run{
		{72, 700, "Gross Pay"},
		{300, 700, "3,000.00"},
		{72, 686, "Federal Income Tax"},
		{300, 686, "330.00"},
	})
	page2 := buildContentStream([]run{
		{72, 700, "Medical PPO"},
		{300, 700, "100.00"},
		{72, 686, "Net Pay"},
		{300, 686, "2,570.00"},
	})

	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	_, _ = zw.Write([]byte(page2))
	_ = zw.Close()

	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.5\n")
	// A catalog object first, so the scan has a non-stream object to walk past.
	pdf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	pdf.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 >>\nendobj\n")
	// Page one: an INDIRECT /Length, which cannot be resolved without the xref.
	fmt.Fprintf(&pdf, "5 0 obj\n<< /Length 7 0 R >>\nstream\n%s\nendstream\nendobj\n", page1)
	// Page two: a NESTED dictionary inside the stream dictionary.
	fmt.Fprintf(&pdf,
		"6 0 obj\n<< /Filter /FlateDecode /DecodeParms << /Predictor 12 /Columns 4 >> /Length %d >>\nstream\n",
		compressed.Len())
	pdf.Write(compressed.Bytes())
	pdf.WriteString("\nendstream\nendobj\n")
	// An image stream, which must be skipped rather than inflated and parsed.
	pdf.WriteString("8 0 obj\n<< /Type /XObject /Subtype /Image /Width 2 /Height 2 /Length 4 >>\nstream\n\x01\x02\x03\x04\nendstream\nendobj\n")
	pdf.WriteString("7 0 obj\n" + fmt.Sprint(len(page1)) + "\nendobj\n")
	pdf.WriteString("xref\n0 9\n0000000000 65535 f \ntrailer\n<< /Size 9 /Root 1 0 R >>\nstartxref\n0\n%%EOF\n")

	lines, err := ExtractPDFText(pdf.Bytes())
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	// Both pages have to come through: the indirect-length one and the one
	// behind a nested dictionary.
	for _, want := range []string{
		"Gross Pay 3,000.00",
		"Federal Income Tax 330.00",
		"Medical PPO 100.00",
		"Net Pay 2,570.00",
	} {
		if !containsLine(lines, want) {
			t.Errorf("expected a line %q, got:\n%s", want, strings.Join(lines, "\n"))
		}
	}

	// And the whole thing has to parse into a usable proposal, which is the
	// property that actually matters to a user.
	p := ParseProposal(lines)
	if !p.Gross.Valid || p.Gross.Decimal.String() != "3000" {
		t.Errorf("gross = %v, want 3000", p.Gross)
	}
	if !p.Net.Valid || p.Net.Decimal.String() != "2570" {
		t.Errorf("net = %v, want 2570", p.Net)
	}
	if !p.Balanced() {
		t.Errorf("3000 − 330 − 100 = 2570 should balance, residual %s",
			p.Stub().Residual().StringFixed(2))
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
