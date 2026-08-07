package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
)

func timeNowMonth() time.Month { return time.Now().UTC().Month() }

// The four cases decodeToolInput exists to separate: a good decode, an absent
// optional field, a type-mismatched field, and outright malformed JSON.
//
// The middle pair is the whole point. Both used to be swallowed, and because
// json.Unmarshal is all-or-nothing, ONE bad field zeroed every OTHER field too
// — so a model that fumbled "limit" also silently lost the month it got right.
func TestDecodeToolInput(t *testing.T) {
	type input struct {
		Month string `json:"month"`
		Limit int    `json:"limit"`
	}

	t.Run("valid input decodes every field", func(t *testing.T) {
		var in input
		if err := decodeToolInput(json.RawMessage(`{"month":"2025-03","limit":5}`), &in); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if in.Month != "2025-03" || in.Limit != 5 {
			t.Errorf("got %+v, want {2025-03 5}", in)
		}
	})

	// Optional fields are load-bearing: an absent month means "current month"
	// and is documented that way in the tool InputSchema. It must stay silent.
	t.Run("absent optional fields are not an error", func(t *testing.T) {
		for _, raw := range []string{`{}`, `{"limit":3}`, ``, `   `, `null`} {
			var in input
			if err := decodeToolInput(json.RawMessage(raw), &in); err != nil {
				t.Errorf("decodeToolInput(%q) = %v, want nil", raw, err)
			}
		}
	})

	t.Run("type-mismatched field is a retryable error", func(t *testing.T) {
		var in input
		err := decodeToolInput(json.RawMessage(`{"month":"2025-03","limit":"five"}`), &in)
		if err == nil {
			t.Fatal("a string in an int field must not decode silently")
		}
		// The message is the model's only instruction for the retry, so it has
		// to name the offending field rather than just saying "bad input".
		if !strings.Contains(err.Error(), "limit") {
			t.Errorf("error should name the field, got %q", err)
		}
	})

	t.Run("malformed json is a retryable error", func(t *testing.T) {
		var in input
		if err := decodeToolInput(json.RawMessage(`{"month":`), &in); err == nil {
			t.Fatal("truncated JSON must not decode silently")
		}
	})

	// The regression this whole change exists to prevent, pinned to what the
	// decoder ACTUALLY does rather than what it is often assumed to do.
	//
	// encoding/json does not discard everything on a type mismatch: it keeps the
	// fields it read and returns the UnmarshalTypeError at the end. So the blast
	// radius is the mismatched field alone — but that is enough, because the
	// mismatched field is left at its ZERO VALUE and every zero value here means
	// something. An unreadable month becomes "", and "" is not "no month", it is
	// THE CURRENT MONTH. Continuing past this error is how "what did I spend in
	// March" gets answered, correctly and confidently, about today.
	t.Run("a mismatched field is left at a zero value that means something", func(t *testing.T) {
		var in input
		if err := decodeToolInput(json.RawMessage(`{"month":123,"limit":5}`), &in); err == nil {
			t.Fatal("a number in a string field must not decode silently")
		}
		if in.Month != "" {
			t.Fatalf("expected the mismatched field to be zeroed, got %q", in.Month)
		}
		// Proof that the zero value is dangerous rather than inert: it resolves,
		// happily and without error, to a real range — the wrong one.
		from, to, err := monthRange(in.Month)
		if err != nil {
			t.Fatalf("monthRange(%q) errored: %v", in.Month, err)
		}
		if from.IsZero() || to.IsZero() {
			t.Fatal("expected monthRange to resolve the empty month to a range")
		}
		if from.Month() != timeNowMonth() {
			t.Errorf("empty month resolved to %s, expected the current month — "+
				"this is the range a swallowed decode error would have queried", from.Month())
		}
	})
}

// A decode failure must reach the model as a tool error it can act on, not as a
// confident answer to a different question. executeChatTool is the boundary
// where that is decided, and the loop in handleChat turns the error it returns
// into an is_error tool_result the model gets to correct.
//
// No database is touched: the decode fails before any query runs, which is
// itself the property under test.
func TestExecuteChatToolRejectsBadInput(t *testing.T) {
	s := &Server{}

	cases := map[string]string{
		"top_merchants":      `{"month":"2025-03","limit":"ten"}`,
		"list_transactions":  `{"month":"2025-03","limit":"fifty"}`,
		"query_transactions": `{"flow":"income","limit":"all"}`,
		"breakdown":          `{"group_by":"merchant","months":"six"}`,
		"monthly_trend":      `{"months":"six"}`,
		"category_averages":  `{"months":"six"}`,
		"spending_summary":   `{"month":123}`,
		"spend_by_category":  `{"month":["2025-03"]}`,
	}

	for name, raw := range cases {
		out, err := s.executeChatTool(context.Background(), auth.Identity{}, name, json.RawMessage(raw))
		if err == nil {
			t.Errorf("%s(%s) returned no error; got output %q", name, raw, out)
			continue
		}
		if !strings.Contains(err.Error(), "invalid tool input") {
			t.Errorf("%s: error should be recognisable as an input problem, got %q", name, err)
		}
	}
}
