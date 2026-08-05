package payroll

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"errors"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Local PDF text-layer extraction.
//
// WHY THIS EXISTS AT ALL, given the app already has an AI provider wired up and
// a receipt OCR endpoint sitting next door.
//
// A paystub from ADP, Gusto, Paychex or UKG is GENERATED, not scanned. The text
// is already in the file, as text, in a fixed layout that does not change
// between pay periods. Reading it is a parsing problem, not a perception one —
// and the parsing answer is better on both axes that matter here:
//
//   - Accuracy. Transcribing a known field out of a text layer cannot misread a
//     digit. A vision model can, and doc 23 is explicit that a misread YTD
//     figure flowing into a tax summary is the expensive failure. The review
//     queue exists because of that risk; not taking the risk is better than
//     catching it.
//   - Disclosure. Nothing leaves this machine. Doc 18's OCR allowlist already
//     refuses to send a tax document to a provider, and a paystub — gross
//     salary, an employer, usually an SSN — is strictly more sensitive than the
//     tax documents that allowlist protects. Widening `ocrEligibleTypes` to
//     include paystubs would have been one line and exactly the wrong line.
//
// The deliberate limits. This reads the text layer and nothing else: a scanned
// stub has no text layer and is reported as such so the user types it in, which
// is the fallback that must work anyway. It assumes single-byte, ASCII-
// compatible font encodings, which is what every US payroll provider emits;
// anything else is detected as unreadable rather than guessed at. It ignores
// the graphics CTM, so text positioned by `cm` rather than `Tm` may group into
// the wrong row — that too surfaces as a failed parse rather than as a wrong
// number, because the label/amount pairing simply will not match.
//
// Everything this produces is a PROPOSAL. Nothing here writes a paystub.

// Errors the API maps onto status codes.
var (
	// ErrNotPDF means the bytes are not a PDF at all.
	ErrNotPDF = errors.New("this file is not a PDF")
	// ErrNoTextLayer means the PDF carries no readable text — a scan, or a
	// stub whose fonts use an encoding this reader will not guess at. The
	// answer is manual entry, and the message says so.
	ErrNoTextLayer = errors.New("this PDF has no readable text layer")
)

// maxPDFBytes bounds what will be parsed. A paystub is one or two pages; a file
// larger than this is not one, and inflating an arbitrary compressed stream
// from an untrusted file is not something to do without a ceiling.
const maxPDFBytes = 8 << 20

// maxInflatedBytes bounds the DECOMPRESSED size of any one stream, which is the
// limit that actually matters: a few kilobytes of flate can expand to gigabytes,
// and the file size check above does nothing about that.
const maxInflatedBytes = 32 << 20

// ExtractPDFText returns the PDF's text, one line per visual row of the page,
// rows in reading order.
//
// Rows rather than a flat string because the whole label-matching strategy
// depends on it: a paystub prints "Federal Income Tax" and "412.55" as two
// separate text runs at the same vertical position, and a reader that
// concatenates in stream order pairs the wrong number with the wrong label. See
// groupIntoLines.
func ExtractPDFText(data []byte) ([]string, error) {
	if len(data) > maxPDFBytes {
		return nil, ErrNotPDF
	}
	// The header may legally be preceded by junk, so this is a search rather
	// than a prefix test — but only within the first bytes, because a "%PDF-"
	// found a megabyte in is a string inside something else.
	if idx := bytes.Index(data, []byte("%PDF-")); idx < 0 || idx > 1024 {
		return nil, ErrNotPDF
	}

	// Grouped PER STREAM, then concatenated in stream order — never pooled into
	// one coordinate space.
	//
	// A text matrix is page-local, so page 2's "Medical PPO" and page 1's "Gross
	// Pay" both sit at y=700 and pooling them merges two unrelated rows into
	// "Gross PayMedical PPO 3,000.00100.00". The label pattern then matches the
	// wrong figure and the importer proposes a gross of 100.00 off a 3,000.00
	// stub — silently, and confidently. Multi-page stubs are ordinary (earnings
	// on one page, year-to-date detail on the next), so this is the common case
	// rather than an edge.
	//
	// The cost is that a single page whose content is split across several
	// streams has its rows split at those boundaries. That trade is deliberate:
	// a split row loses the label/amount pairing, so the line goes unmatched and
	// the balance check reports the gap — a loud, visible failure. Pooling fails
	// silently with a wrong number, which is the one outcome this whole package
	// is built to avoid.
	var lines []string
	sawFragment := false
	for _, content := range contentStreams(data) {
		fragments := parseContentStream(content)
		if len(fragments) == 0 {
			continue
		}
		sawFragment = true
		lines = append(lines, groupIntoLines(fragments)...)
	}
	if !sawFragment {
		return nil, ErrNoTextLayer
	}

	if !looksLikeText(lines) {
		return nil, ErrNoTextLayer
	}
	return lines, nil
}

// --------------------------------------------------------------------------
// Locating content streams
// --------------------------------------------------------------------------

// contentStreams returns every decompressed stream in the file that plausibly
// holds page content.
//
// It scans for `stream` keywords rather than walking the cross-reference table,
// and that is a deliberate trade. A proper xref walk means implementing xref
// tables, xref streams, incremental updates and object streams — four formats,
// each with its own edge cases, to find something a linear scan finds anyway.
// The scan's failure mode is also the safe one: it may pick up a stream that is
// not page content, which the operator parser then yields nothing from.
func contentStreams(data []byte) [][]byte {
	var out [][]byte

	for pos := 0; ; {
		rel := bytes.Index(data[pos:], []byte("stream"))
		if rel < 0 {
			break
		}
		at := pos + rel
		pos = at + len("stream")

		// "endstream" also contains "stream"; skip those rather than treating
		// the tail of one as the head of another.
		if at >= 3 && bytes.Equal(data[at-3:at], []byte("end")) {
			continue
		}
		// The keyword must be preceded by the stream's dictionary.
		dict, ok := dictBefore(data, at)
		if !ok {
			continue
		}
		// Images and object streams are never page content. Skipping them is
		// not just an optimisation: an image's bytes would be inflated in full
		// only to be discarded.
		if bytes.Contains(dict, []byte("/Image")) || bytes.Contains(dict, []byte("/ObjStm")) {
			continue
		}

		// The data begins after the keyword and a single EOL, per the spec.
		body := data[pos:]
		switch {
		case bytes.HasPrefix(body, []byte("\r\n")):
			body = body[2:]
		case bytes.HasPrefix(body, []byte("\n")), bytes.HasPrefix(body, []byte("\r")):
			body = body[1:]
		}

		// Bounded by the `endstream` keyword rather than by the dictionary's
		// /Length, because /Length is frequently an indirect reference — which
		// is precisely the xref resolution this scan exists to avoid.
		end := bytes.Index(body, []byte("endstream"))
		if end < 0 {
			break
		}
		raw := bytes.TrimRight(body[:end], "\r\n")
		pos += end

		content, ok := decodeStream(dict, raw)
		if !ok {
			continue
		}
		out = append(out, content)
	}
	return out
}

// dictBefore returns the dictionary immediately preceding a `stream` keyword.
//
// It walks backwards from the keyword counting `>>`/`<<` pairs, so a stream
// dictionary containing nested dictionaries (a /DecodeParms, say) yields the
// whole outer dictionary rather than stopping at the inner one's opener.
func dictBefore(data []byte, streamAt int) ([]byte, bool) {
	end := streamAt
	for end > 0 && isPDFSpace(data[end-1]) {
		end--
	}
	if end < 2 || !bytes.Equal(data[end-2:end], []byte(">>")) {
		return nil, false
	}

	// Starts at end-1, not end-2. The pair is matched on its SECOND byte —
	// data[i] and data[i-1] — so the closing `>>` that `end` was just proved to
	// sit behind is matched at index end-1. Starting one earlier skips it, depth
	// never reaches 1, and the first `<<` drives it to -1 so nothing ever
	// returns: every stream in every PDF is silently skipped and the whole
	// importer reports "no readable text layer".
	depth := 0
	for i := end - 1; i >= 1; i-- {
		switch {
		case data[i] == '>' && data[i-1] == '>':
			depth++
			i--
		case data[i] == '<' && data[i-1] == '<':
			depth--
			if depth == 0 {
				return data[i-1 : end], true
			}
			i--
		}
	}
	return nil, false
}

// decodeStream applies the stream's filter. Only Flate is supported, which
// covers everything a payroll provider emits; an LZW- or DCT-encoded stream is
// skipped rather than half-read.
func decodeStream(dict, raw []byte) ([]byte, bool) {
	if !bytes.Contains(dict, []byte("/Filter")) {
		// Uncompressed content streams are legal and do occur.
		return raw, true
	}
	if !bytes.Contains(dict, []byte("/FlateDecode")) {
		return nil, false
	}

	// zlib first (the spec's framing), then raw deflate, because a handful of
	// producers omit the two-byte zlib header and a reader that only tries one
	// silently loses those files.
	if out, ok := inflate(raw, true); ok {
		return out, true
	}
	return inflate(raw, false)
}

func inflate(raw []byte, zlibFraming bool) ([]byte, bool) {
	var r io.ReadCloser
	if zlibFraming {
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, false
		}
		r = zr
	} else {
		r = flate.NewReader(bytes.NewReader(raw))
	}
	defer r.Close()

	// LimitReader, not ReadAll: this is attacker-influenced compressed data
	// reached through a file upload, and a decompression bomb is the obvious
	// thing to point at it.
	out, err := io.ReadAll(io.LimitReader(r, maxInflatedBytes))
	// A truncated stream still yields usable text — some producers pad the
	// tail — so partial output with an error is kept rather than discarded.
	if len(out) == 0 && err != nil {
		return nil, false
	}
	return out, true
}

func isPDFSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n' || b == '\f' || b == 0
}

// --------------------------------------------------------------------------
// The content stream operator parser
// --------------------------------------------------------------------------

// textFragment is one run of shown text and where on the page it started.
type textFragment struct {
	x, y float64
	text string
}

// parseContentStream walks the text-showing operators and records what was
// drawn where.
//
// Only the text sub-language is interpreted. Paths, colours, clipping and the
// graphics CTM are skipped — a paystub's figures are all set with Tm/Td inside
// BT/ET blocks, and interpreting the full graphics model to place them would be
// a renderer rather than a reader.
func parseContentStream(content []byte) []textFragment {
	var (
		out      []textFragment
		operands []token
		// tm is the text matrix, tlm the text line matrix. Only the translation
		// components are used, which is all that "where did this land" needs.
		tm, tlm [6]float64
		leading float64
		inText  bool

		// The run being accumulated at the current position. Consecutive show
		// operators with no repositioning between them are one visual word, and
		// flushing per operator would shred "412.55" into "4", "1", "2"…
		pending  strings.Builder
		pendingX float64
		pendingY float64
	)

	identity := [6]float64{1, 0, 0, 1, 0, 0}
	tm, tlm = identity, identity

	flush := func() {
		if pending.Len() == 0 {
			return
		}
		out = append(out, textFragment{x: pendingX, y: pendingY, text: pending.String()})
		pending.Reset()
	}
	show := func(s string) {
		if s == "" {
			return
		}
		if pending.Len() == 0 {
			pendingX, pendingY = tm[4], tm[5]
		}
		pending.WriteString(s)
	}
	// translate applies a relative move to the text line matrix, which is what
	// Td and its derivatives do.
	translate := func(tx, ty float64) {
		tlm = [6]float64{
			tlm[0], tlm[1], tlm[2], tlm[3],
			tx*tlm[0] + ty*tlm[2] + tlm[4],
			tx*tlm[1] + ty*tlm[3] + tlm[5],
		}
		tm = tlm
	}

	lex := newLexer(content)
	for {
		tok, ok := lex.next()
		if !ok {
			break
		}
		if tok.kind != tokOperator {
			operands = append(operands, tok)
			// A malformed stream could otherwise accumulate operands forever.
			if len(operands) > 64 {
				operands = operands[len(operands)-64:]
			}
			continue
		}

		switch tok.text {
		case "BT":
			flush()
			tm, tlm = identity, identity
			inText = true
		case "ET":
			flush()
			inText = false
		case "Tm":
			if len(operands) >= 6 {
				flush()
				for i := 0; i < 6; i++ {
					tm[i] = operands[len(operands)-6+i].num
				}
				tlm = tm
			}
		case "Td":
			if len(operands) >= 2 {
				flush()
				translate(operands[len(operands)-2].num, operands[len(operands)-1].num)
			}
		case "TD":
			if len(operands) >= 2 {
				flush()
				ty := operands[len(operands)-1].num
				leading = -ty
				translate(operands[len(operands)-2].num, ty)
			}
		case "TL":
			if len(operands) >= 1 {
				leading = operands[len(operands)-1].num
			}
		case "T*":
			flush()
			translate(0, -leading)
		case "Tj":
			if inText && len(operands) >= 1 {
				show(operands[len(operands)-1].str)
			}
		case "'":
			if inText && len(operands) >= 1 {
				flush()
				translate(0, -leading)
				show(operands[len(operands)-1].str)
			}
		case "\"":
			if inText && len(operands) >= 3 {
				flush()
				translate(0, -leading)
				show(operands[len(operands)-1].str)
			}
		case "TJ":
			if inText {
				show(showArray(operands))
			}
		}
		operands = operands[:0]
	}
	flush()
	return out
}

// tjSpaceThreshold is the kerning adjustment, in thousandths of an em, past
// which a gap in a TJ array is treated as a space.
//
// A TJ array interleaves strings with kerning numbers, and a word space is
// often encoded as a large negative adjustment rather than as a space
// character. 200 (a fifth of an em) is comfortably above the tightening applied
// between digits and letters within a word, and comfortably below a real
// inter-word gap. Getting this wrong in the tight direction inserts a space
// into "1,234.56"; the amount pattern would then fail to match and the field
// would come back empty rather than wrong, which is the right way to fail.
const tjSpaceThreshold = 200

// showArray renders a TJ operand array — strings interleaved with kerning
// adjustments.
func showArray(operands []token) string {
	// Find the array that ended immediately before the operator.
	end := len(operands)
	depth := 0
	start := -1
	for i := end - 1; i >= 0; i-- {
		switch operands[i].kind {
		case tokArrayEnd:
			depth++
		case tokArrayStart:
			depth--
			if depth == 0 {
				start = i
			}
		}
		if start >= 0 {
			break
		}
	}
	if start < 0 {
		return ""
	}

	var b strings.Builder
	for _, t := range operands[start+1 : end] {
		switch t.kind {
		case tokString:
			b.WriteString(t.str)
		case tokNumber:
			if t.num <= -tjSpaceThreshold {
				b.WriteByte(' ')
			}
		}
	}
	return b.String()
}

// --------------------------------------------------------------------------
// Grouping fragments back into visual lines
// --------------------------------------------------------------------------

// rowTolerance is how far apart, in PDF units (1/72 inch), two fragments may be
// vertically and still count as the same row. Two points is under a third of a
// typical stub's line height and above the sub-point drift a producer
// introduces between runs on one line.
const rowTolerance = 2.0

// columnGap is the horizontal distance, in the same units, past which two
// fragments on one row are separated by a space rather than butted together.
// Roughly one character at 10pt; below it the two runs are almost always one
// word split across show operators.
const columnGap = 4.0

// groupIntoLines turns positioned fragments into rows of text.
//
// This is the step that makes label matching possible. A stub prints its label
// column and its amount column as separate runs, frequently in an order that
// has nothing to do with reading order, and often across more than one content
// stream. Grouping by vertical position and then sorting by horizontal position
// reconstructs "Federal Income Tax 412.55 3,300.40" as one line — at which
// point a label pattern and an amount pattern on the same line mean what they
// look like they mean.
func groupIntoLines(fragments []textFragment) []string {
	sorted := make([]textFragment, len(fragments))
	copy(sorted, fragments)
	// Descending y: PDF's origin is the bottom-left corner, so a larger y is
	// further UP the page and therefore earlier in reading order.
	sort.SliceStable(sorted, func(i, j int) bool {
		if math.Abs(sorted[i].y-sorted[j].y) > rowTolerance {
			return sorted[i].y > sorted[j].y
		}
		return sorted[i].x < sorted[j].x
	})

	var (
		lines   []string
		row     []textFragment
		flushed = func() {
			if len(row) == 0 {
				return
			}
			sort.SliceStable(row, func(i, j int) bool { return row[i].x < row[j].x })
			var b strings.Builder
			prevEnd := math.Inf(-1)
			for _, f := range row {
				if b.Len() > 0 && f.x-prevEnd > columnGap {
					b.WriteByte(' ')
				}
				b.WriteString(f.text)
				prevEnd = f.x
			}
			if line := strings.TrimSpace(collapseSpaces(b.String())); line != "" {
				lines = append(lines, line)
			}
			row = row[:0]
		}
	)

	for _, f := range sorted {
		if len(row) > 0 && math.Abs(row[0].y-f.y) > rowTolerance {
			flushed()
		}
		row = append(row, f)
	}
	flushed()
	return lines
}

func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// looksLikeText rejects output that decoded into nonsense.
//
// A font with a custom or multi-byte encoding produces bytes that are not the
// characters they appear to be, and this reader deliberately does not implement
// CMap lookup. Detecting the result and refusing it is what keeps that
// limitation from turning into wrong figures: the user is told the stub could
// not be read and types it in, which is a good outcome. Silently proposing a
// gross salary derived from mis-decoded glyphs is not.
func looksLikeText(lines []string) bool {
	printable, total := 0, 0
	for _, l := range lines {
		for _, r := range l {
			total++
			if r == ' ' || (r >= '!' && r <= '~') {
				printable++
			}
		}
	}
	if total < 32 {
		return false
	}
	return float64(printable)/float64(total) >= 0.85
}

// --------------------------------------------------------------------------
// Tokenizer
// --------------------------------------------------------------------------

type tokenKind int

const (
	tokNumber tokenKind = iota
	tokString
	tokName
	tokArrayStart
	tokArrayEnd
	tokDictStart
	tokDictEnd
	tokOperator
)

type token struct {
	kind tokenKind
	num  float64
	str  string
	text string
}

type lexer struct {
	data []byte
	pos  int
}

func newLexer(data []byte) *lexer { return &lexer{data: data} }

func (l *lexer) next() (token, bool) {
	l.skipSpaceAndComments()
	if l.pos >= len(l.data) {
		return token{}, false
	}

	switch c := l.data[l.pos]; {
	case c == '(':
		return token{kind: tokString, str: l.literalString()}, true
	case c == '<':
		if l.pos+1 < len(l.data) && l.data[l.pos+1] == '<' {
			l.pos += 2
			return token{kind: tokDictStart}, true
		}
		return token{kind: tokString, str: l.hexString()}, true
	case c == '>':
		if l.pos+1 < len(l.data) && l.data[l.pos+1] == '>' {
			l.pos += 2
			return token{kind: tokDictEnd}, true
		}
		l.pos++
		return l.next()
	case c == '[':
		l.pos++
		return token{kind: tokArrayStart}, true
	case c == ']':
		l.pos++
		return token{kind: tokArrayEnd}, true
	case c == '/':
		l.pos++
		return token{kind: tokName, text: l.regularRun()}, true
	case c == '{' || c == '}':
		l.pos++
		return l.next()
	case c == '+' || c == '-' || c == '.' || (c >= '0' && c <= '9'):
		raw := l.regularRun()
		n, err := strconv.ParseFloat(strings.TrimSuffix(raw, "."), 64)
		if err != nil {
			// A malformed number is not worth abandoning the stream over; the
			// operator that consumes it simply sees one fewer operand.
			return l.next()
		}
		return token{kind: tokNumber, num: n}, true
	default:
		op := l.regularRun()
		if op == "" {
			l.pos++
			return l.next()
		}
		return token{kind: tokOperator, text: op}, true
	}
}

func (l *lexer) skipSpaceAndComments() {
	for l.pos < len(l.data) {
		c := l.data[l.pos]
		if isPDFSpace(c) {
			l.pos++
			continue
		}
		if c == '%' {
			for l.pos < len(l.data) && l.data[l.pos] != '\n' && l.data[l.pos] != '\r' {
				l.pos++
			}
			continue
		}
		return
	}
}

// regularRun reads until the next delimiter or whitespace — a number, a name or
// an operator.
func (l *lexer) regularRun() string {
	start := l.pos
	for l.pos < len(l.data) {
		c := l.data[l.pos]
		if isPDFSpace(c) || bytes.IndexByte([]byte("()<>[]{}/%"), c) >= 0 {
			break
		}
		l.pos++
	}
	return string(l.data[start:l.pos])
}

// literalString reads a `(...)` string, honouring nesting and the escape
// sequences the spec defines.
func (l *lexer) literalString() string {
	l.pos++ // consume '('
	var b strings.Builder
	depth := 1

	for l.pos < len(l.data) {
		c := l.data[l.pos]
		l.pos++

		switch c {
		case '\\':
			if l.pos >= len(l.data) {
				return b.String()
			}
			e := l.data[l.pos]
			l.pos++
			switch e {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case '(', ')', '\\':
				b.WriteByte(e)
			case '\n':
				// A backslash-newline is a line continuation inside the source
				// and contributes nothing to the string.
			case '\r':
				if l.pos < len(l.data) && l.data[l.pos] == '\n' {
					l.pos++
				}
			default:
				if e >= '0' && e <= '7' {
					val := int(e - '0')
					for range 2 {
						if l.pos < len(l.data) && l.data[l.pos] >= '0' && l.data[l.pos] <= '7' {
							val = val*8 + int(l.data[l.pos]-'0')
							l.pos++
						}
					}
					b.WriteByte(byte(val))
				} else {
					b.WriteByte(e)
				}
			}
		case '(':
			depth++
			b.WriteByte(c)
		case ')':
			depth--
			if depth == 0 {
				return b.String()
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// hexString reads a `<...>` string.
func (l *lexer) hexString() string {
	l.pos++ // consume '<'
	var digits []byte
	for l.pos < len(l.data) && l.data[l.pos] != '>' {
		c := l.data[l.pos]
		l.pos++
		if isHexDigit(c) {
			digits = append(digits, c)
		}
	}
	if l.pos < len(l.data) {
		l.pos++ // consume '>'
	}
	// An odd number of digits is padded with a trailing zero, per the spec.
	if len(digits)%2 == 1 {
		digits = append(digits, '0')
	}

	var b strings.Builder
	for i := 0; i < len(digits); i += 2 {
		v, err := strconv.ParseUint(string(digits[i:i+2]), 16, 8)
		if err != nil {
			continue
		}
		b.WriteByte(byte(v))
	}
	return b.String()
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
