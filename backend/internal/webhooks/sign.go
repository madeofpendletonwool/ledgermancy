package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Delivery headers. Every one of these is part of the public contract with a
// receiver, so they are constants rather than string literals at the call site
// and are documented in docs/features/webhooks.md.
const (
	// SignatureHeader carries the HMAC. See Sign for the format.
	SignatureHeader = "X-Ledgermancy-Signature"

	// DeliveryHeader carries the webhook_messages id: unique per (event,
	// subscriber) and STABLE across retries, so it is the value a receiver
	// should dedupe on if it wants at-most-once processing.
	DeliveryHeader = "X-Ledgermancy-Delivery"

	// TriggerHeader repeats the envelope's `trigger` so a receiver can route on
	// it without parsing the body — useful for the reverse proxies and
	// automation tools that filter on headers.
	TriggerHeader = "X-Ledgermancy-Trigger"

	// AttemptHeader is 1-based and increases on every retry. A receiver seeing
	// attempt > 1 is being told "you may have seen this already".
	AttemptHeader = "X-Ledgermancy-Attempt"
)

// signatureVersion labels the scheme inside the header value, so a future change
// to the derivation can be introduced alongside the current one rather than
// breaking every receiver on a deploy.
const signatureVersion = "v1"

// Sign produces the X-Ledgermancy-Signature value for one delivery.
//
// Format:
//
//	t=<unix seconds>,v1=<hex HMAC-SHA256>
//
// where the MAC is taken over the bytes `<unix seconds>.<body>` with the
// webhook's secret string as the key.
//
// # Why the timestamp is inside the MAC
//
// A bare HMAC over the body proves the body came from us, but it is replayable
// forever: anyone who captures one delivery — trivially, if the receiver is
// plain HTTP on a LAN — can resend it verbatim to the same receiver a month
// later and it will verify. Mixing the timestamp into the signed material means
// a captured delivery can only be replayed inside whatever freshness window the
// receiver enforces, and the receiver gets to choose that window rather than
// having no option at all. It is Stripe's scheme, and it is worth copying
// precisely because receiver authors already know it.
//
// # What the body is
//
// The exact bytes that go on the wire — which is the payload as Postgres hands
// it back, not as Go marshalled it. JSONB normalises whitespace and key order on
// the way in, so signing anything reconstructed in the application would produce
// a MAC over bytes the receiver never sees. The delivery path reads once and
// uses that one slice for both the signature and the request body.
func Sign(secret string, ts time.Time, body []byte) string {
	unix := strconv.FormatInt(ts.Unix(), 10)
	return "t=" + unix + "," + signatureVersion + "=" + hex.EncodeToString(mac(secret, unix, body))
}

// mac is the derivation both Sign and Verify go through, so the two can never
// drift into disagreeing about what is signed.
func mac(secret, unix string, body []byte) []byte {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(unix))
	h.Write([]byte("."))
	h.Write(body)
	return h.Sum(nil)
}

// Verify checks a signature header against a body, the way a receiver would.
//
// It exists so the receiver side of the contract is exercised by this repo's own
// tests rather than described in prose and hoped for: if Sign and Verify ever
// disagree, or a header format change breaks verification, the test fails here
// instead of in somebody's Home Assistant. It is not used by the delivery path.
//
// tolerance bounds how old a delivery may be; pass 0 to skip the freshness check
// (which is what a receiver replaying a captured request for debugging wants,
// and nothing else should).
func Verify(secret, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var tsPart, macPart string
	for _, field := range strings.Split(header, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok {
			continue
		}
		switch name {
		case "t":
			tsPart = value
		case signatureVersion:
			macPart = value
		}
	}
	if tsPart == "" || macPart == "" {
		return fmt.Errorf("signature header is missing t= or %s=", signatureVersion)
	}

	unix, err := strconv.ParseInt(tsPart, 10, 64)
	if err != nil {
		return fmt.Errorf("signature timestamp is not a unix time")
	}
	if tolerance > 0 {
		age := now.Sub(time.Unix(unix, 0))
		if age < -tolerance || age > tolerance {
			return fmt.Errorf("signature timestamp is outside the tolerance window")
		}
	}

	got, err := hex.DecodeString(macPart)
	if err != nil {
		return fmt.Errorf("signature is not hex")
	}
	// Compare the MACs, not the rendered header: field order and whitespace are
	// a receiver's business, and a scheme that failed on a re-serialised header
	// would be one every proxy could break. hmac.Equal is the constant-time
	// comparison the timing attack on a naive `==` calls for.
	if !hmac.Equal(got, mac(secret, tsPart, body)) {
		return fmt.Errorf("signature does not match")
	}
	return nil
}
