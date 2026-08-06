package api

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// decodeToolInput reads a model-supplied tool_use input into dst, and — unlike
// the `_ = json.Unmarshal(...)` it replaces — REFUSES TO CONTINUE ON A DECODE
// FAILURE.
//
// The reason is that json.Unmarshal is all-or-nothing. One type-mismatched
// field — a string where an int is declared, which is the most common way a
// model gets a tool call wrong — fails the whole decode and leaves EVERY field
// at its zero value. The tool then runs honestly against a question nobody
// asked: an empty month resolves to the current month, a zero limit falls back
// to its default, and the model narrates a correct answer to the wrong query.
// Nothing in the reply marks it as wrong, which is precisely the failure this
// app's "tools compute, the model narrates" rule exists to prevent. A silently
// wrong number is worse than no number.
//
// The error travels back as a tool_result with is_error set (see the loop in
// handleChat), so the model reads the mismatch and re-issues the call with
// corrected arguments. maxToolIterations leaves room for that recovery, and
// self-correction is what the tool-use wire format is built around.
//
// An absent or empty input is NOT an error: optional fields are load-bearing
// here — omitting "month" to mean "the current month" is intended, documented
// in the tool InputSchema descriptions, and left untouched. Only a genuine
// decode failure is reported.
func decodeToolInput(input json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(input)) == 0 {
		return nil
	}
	if err := json.Unmarshal(input, dst); err != nil {
		return fmt.Errorf("invalid tool input: %w; re-send this tool call with the argument types the schema declares", err)
	}
	return nil
}
