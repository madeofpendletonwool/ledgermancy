package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// Receipt field extraction for the document vault.
//
// This is the only place in the app that sends an image off the host, and the
// only place a model is asked to read a number rather than describe one. Both
// of those make it exceptional, and both are why the result is a *suggestion*
// and nothing more: ExtractReceipt returns fields for a person to confirm, and
// no caller is permitted to write a transaction from them directly. A model
// that misreads 84.20 as 8420 must cost a user one correction, not one wrong
// entry in their ledger.
//
// The extraction is also gated twice — on an API key existing at all, and on
// DOCUMENTS_OCR_ENABLED — so a deployment with AI turned on for categorisation
// does not thereby start uploading photographs of paperwork.

// ReceiptFields is what a model believes it read off a receipt. Every field is
// optional: a blurred total is better returned as absent than as a guess.
type ReceiptFields struct {
	// Merchant is the business name as printed.
	Merchant string `json:"merchant"`
	// Total is the amount paid, as a decimal STRING. A string and not a float
	// deliberately: the value is carried to a form field and then parsed by
	// shopspring/decimal, and a float would round on the way through.
	Total string `json:"total"`
	// Date is the transaction date in YYYY-MM-DD, if one is legible.
	Date string `json:"date"`
	// Currency is the ISO code if printed; often absent on a receipt.
	Currency string `json:"currency"`
	// Confidence is the model's own 0–1 rating of the read. Shown to the user
	// alongside the fields; it is never a threshold for skipping confirmation.
	Confidence float64 `json:"confidence"`
	// Notes is one short line about anything ambiguous — a smudged total, two
	// candidate dates — so the person confirming knows where to look.
	Notes string `json:"notes"`
}

const extractReceiptSystemPrompt = `You read the fields off a photographed or scanned receipt.

You are transcribing, not calculating. Copy what is printed:
- total: the FINAL amount paid, after tax and tip, as a plain decimal string like "84.20". No currency symbol, no thousands separators. If several totals appear (subtotal, tax, total, amount due), take the one actually charged.
- merchant: the business name as printed at the top, tidied to ordinary capitalisation.
- date: the transaction date as YYYY-MM-DD. Resolve two-digit years to the current century. If the receipt shows only a time, or no date, leave it empty.
- currency: the ISO code (USD, EUR, GBP) only if the receipt makes it explicit. Otherwise leave it empty.

Rules that matter more than completeness:
- NEVER guess a digit you cannot read. An empty field is correct; an invented number is not. The user checks every field before anything is saved, and a plausible wrong total is far more likely to slip past them than a blank one.
- Do not add up line items to produce a total the receipt does not print.
- If the image is not a receipt or invoice at all, return empty fields with confidence 0 and say so in notes.
- confidence is your honest 0-1 rating of the whole read.
- notes is one short clause naming anything ambiguous, or empty if nothing is.

Answer only by calling the extract_receipt tool.`

var extractReceiptTool = Tool{
	Name:        "extract_receipt",
	Description: "Return the fields printed on the receipt, leaving anything illegible empty.",
	InputSchema: json.RawMessage(`{
		"type": "object",
		"properties": {
			"merchant": {"type": "string", "description": "Business name as printed, tidied. Empty if unreadable."},
			"total": {"type": "string", "description": "Final amount charged as a plain decimal string, e.g. \"84.20\". Empty if unreadable."},
			"date": {"type": "string", "description": "Transaction date as YYYY-MM-DD. Empty if not printed."},
			"currency": {"type": "string", "description": "ISO code if explicit, otherwise empty."},
			"confidence": {"type": "number", "description": "0-1 honest confidence in the whole read"},
			"notes": {"type": "string", "description": "One short clause about anything ambiguous, or empty"}
		},
		"required": ["merchant", "total", "date", "confidence"]
	}`),
}

// SupportedReceiptImage reports whether a media type can be sent for
// extraction. The Messages API accepts these four; a PDF statement is not an
// image and is refused rather than silently sent as bytes the model cannot read.
func SupportedReceiptImage(mediaType string) bool {
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// ExtractReceipt asks the model to transcribe a receipt image.
//
// The caller is responsible for having decrypted the image and for confirming
// the user opted into OCR. What comes back is unvalidated model output destined
// for a confirmation form — treat every field as untrusted text.
//
// Returns ErrDisabled when no key is configured.
func (c *Client) ExtractReceipt(ctx context.Context, mediaType string, image []byte) (ReceiptFields, error) {
	if !c.Enabled() {
		return ReceiptFields{}, ErrDisabled
	}
	if !SupportedReceiptImage(mediaType) {
		return ReceiptFields{}, fmt.Errorf("ai: %s cannot be read as a receipt image", mediaType)
	}
	if len(image) == 0 {
		return ReceiptFields{}, fmt.Errorf("ai: receipt image is empty")
	}

	resp, err := c.Complete(ctx, Request{
		System: extractReceiptSystemPrompt,
		Messages: []Message{{
			Role: RoleUser,
			Content: []Block{
				ImageBlock(mediaType, base64.StdEncoding.EncodeToString(image)),
				TextBlock("Read the fields off this receipt."),
			},
		}},
		Tools:      []Tool{extractReceiptTool},
		ToolChoice: map[string]string{"type": "tool", "name": extractReceiptTool.Name},
		MaxTokens:  1024,
	})
	if err != nil {
		return ReceiptFields{}, err
	}

	uses := resp.ToolUses()
	if len(uses) == 0 {
		return ReceiptFields{}, fmt.Errorf("ai: model did not call extract_receipt")
	}
	var fields ReceiptFields
	if err := json.Unmarshal(uses[0].Input, &fields); err != nil {
		return ReceiptFields{}, fmt.Errorf("ai: decode receipt fields: %w", err)
	}
	return fields, nil
}
