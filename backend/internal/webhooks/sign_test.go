package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The signature has to verify at a RECEIVER, which is a program nobody in this
// repo controls. So the check here is not "Verify accepts what Sign produced" —
// that would pass just as happily if both were wrong together. It recomputes the
// MAC from the documented recipe, by hand, with the standard library, exactly as
// a receiver author reading docs/features/webhooks.md would.
//
// If the derivation ever changes, this fails here rather than in somebody's Home
// Assistant three weeks after a deploy.
func TestSignatureMatchesTheDocumentedRecipe(t *testing.T) {
	const secret = "whsec_0123456789abcdef"
	body := []byte(`{"trigger":"alert.fired","data":{"amount":"412.55"}}`)
	ts := time.Unix(1_800_000_000, 0)

	header := Sign(secret, ts, body)

	// The documented recipe: HMAC-SHA256 over "<unix>.<body>", keyed by the
	// secret string, hex-encoded.
	unix := strconv.FormatInt(ts.Unix(), 10)
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(unix + "." + string(body)))
	want := "t=" + unix + ",v1=" + hex.EncodeToString(h.Sum(nil))

	if header != want {
		t.Fatalf("signature does not match the documented recipe:\n got %s\nwant %s", header, want)
	}
}

// A receiver following the documented steps accepts a real delivery.
func TestVerifyAcceptsARealDelivery(t *testing.T) {
	const secret = "whsec_abc"
	body := []byte(`{"trigger":"insight.created"}`)
	now := time.Unix(1_800_000_000, 0)

	if err := Verify(secret, Sign(secret, now, body), body, now, 5*time.Minute); err != nil {
		t.Fatalf("a freshly signed delivery did not verify: %v", err)
	}
}

// Every way a delivery can be wrong has to be refused. The body edit is the one
// that matters most: a signature that survived it would be decoration.
func TestVerifyRejectsTampering(t *testing.T) {
	const secret = "whsec_abc"
	body := []byte(`{"amount":"10.00"}`)
	now := time.Unix(1_800_000_000, 0)
	header := Sign(secret, now, body)

	// Change the last hex digit of the MAC, whatever it happens to be. A
	// find-and-replace on a fixed digit would silently do nothing on the runs
	// where the MAC does not contain it, and pass for the wrong reason.
	flipped := header[:len(header)-1] + map[bool]string{true: "0", false: "1"}[header[len(header)-1] != '0']

	cases := []struct {
		name   string
		secret string
		header string
		body   []byte
	}{
		{"body edited in flight", secret, header, []byte(`{"amount":"10000.00"}`)},
		{"signed with a different secret", "whsec_someone_else", header, body},
		{"mac flipped", secret, flipped, body},
		{"timestamp moved without resigning", secret,
			strings.Replace(header, "t=1800000000", "t=1800000060", 1), body},
		{"no mac at all", secret, "t=1800000000", body},
		{"empty header", secret, "", body},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Verify(tc.secret, tc.header, tc.body, now, 5*time.Minute); err == nil {
				t.Fatal("accepted a delivery it should have refused")
			}
		})
	}
}

// The timestamp is inside the MAC so that a captured delivery cannot be replayed
// forever. A receiver enforcing a window must be able to refuse an old one even
// though its signature is genuine.
func TestVerifyRejectsAReplayOutsideTheWindow(t *testing.T) {
	const secret = "whsec_abc"
	body := []byte(`{}`)
	signedAt := time.Unix(1_800_000_000, 0)
	header := Sign(secret, signedAt, body)

	replayedAt := signedAt.Add(time.Hour)
	if err := Verify(secret, header, body, replayedAt, 5*time.Minute); err == nil {
		t.Fatal("accepted an hour-old delivery against a five-minute tolerance")
	}

	// ...and the same delivery is still verifiable with the freshness check off,
	// which is what makes the refusal above about age rather than the MAC.
	if err := Verify(secret, header, body, replayedAt, 0); err != nil {
		t.Fatalf("tolerance 0 should skip the freshness check, got: %v", err)
	}
}
