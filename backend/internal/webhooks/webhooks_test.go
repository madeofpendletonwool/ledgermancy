package webhooks

import (
	"reflect"
	"testing"
	"time"
)

// An unknown trigger must be REFUSED, not dropped. Dropping it hands back a
// subscription that listens to less than the caller asked for, and the first
// anybody learns of it is an event that never arrived.
func TestNormalizeTriggers(t *testing.T) {
	cases := []struct {
		name   string
		in     []string
		want   []string
		wantOK bool
	}{
		{"canonical order", []string{TriggerAlertFired, TriggerInsightCreated},
			[]string{TriggerInsightCreated, TriggerAlertFired}, true},
		{"duplicates collapse", []string{TriggerAlertFired, TriggerAlertFired},
			[]string{TriggerAlertFired}, true},
		{"case and space", []string{"  ALERT.FIRED "}, []string{TriggerAlertFired}, true},
		{"unknown is refused", []string{TriggerAlertFired, "transaction.created"}, nil, false},
		{"empty is refused", nil, nil, false},
		// webhook.test is emitted by the test button and addressed to one
		// webhook; nothing may subscribe to it, or a press would fan out.
		{"the test trigger is not subscribable", []string{TriggerTest}, nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NormalizeTriggers(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// The URL check is deliberately permissive about WHERE (a LAN address is the
// normal case) and strict about WHAT: it has to be something this app can POST
// to at all.
func TestValidateURL(t *testing.T) {
	ok := []string{
		"https://example.test/hook",
		"http://homeassistant.local:8123/api/webhook/ledgermancy",
		// A private address is explicitly fine — blocking it would block the
		// feature's main use case.
		"http://192.168.1.20:5678/webhook/abc",
	}
	for _, raw := range ok {
		if _, err := ValidateURL(raw); err != nil {
			t.Errorf("ValidateURL(%q) = %v, want accepted", raw, err)
		}
	}

	bad := []string{
		"",
		"example.test/hook",       // no scheme
		"ftp://example.test/hook", // not HTTP
		"file:///etc/passwd",      // not HTTP, and pointedly so
		"https://",                // no host
		"http://" + string(make([]byte, maxURLLen)), // absurd length
	}
	for _, raw := range bad {
		if _, err := ValidateURL(raw); err == nil {
			t.Errorf("ValidateURL(%q) was accepted, want refused", raw)
		}
	}
}

// A secret has to be long, unguessable, and different every time. The prefix is
// part of the key material, so a receiver copies the whole string.
func TestNewSecretIsUniqueAndPrefixed(t *testing.T) {
	seen := make(map[string]bool, 32)
	for range 32 {
		secret, err := NewSecret()
		if err != nil {
			t.Fatalf("NewSecret: %v", err)
		}
		if len(secret) < len(SecretPrefix)+64 {
			t.Fatalf("secret is too short to be a signing key: %q", secret)
		}
		if seen[secret] {
			t.Fatal("NewSecret returned a duplicate")
		}
		seen[secret] = true
	}
}

// The dedupe key is what makes the periodic sweeps free to re-examine the same
// events: it has to be stable for one event and distinct across triggers.
func TestDedupeKeyIsStablePerEventAndTrigger(t *testing.T) {
	if DedupeKey(TriggerInsightCreated, "abc") != DedupeKey(TriggerInsightCreated, "abc") {
		t.Fatal("the same event produced two different dedupe keys")
	}
	if DedupeKey(TriggerInsightCreated, "abc") == DedupeKey(TriggerAlertFired, "abc") {
		t.Fatal("two triggers over one object share a dedupe key")
	}
}

// The backoff has to grow and then stop growing. A policy that kept doubling
// would schedule the last retry days out, long after anybody has stopped caring.
func TestRetryDelayBacksOffAndCaps(t *testing.T) {
	want := []time.Duration{time.Minute, 4 * time.Minute, 16 * time.Minute, time.Hour, time.Hour}
	for i, expected := range want {
		if got := RetryDelay(i + 1); got != expected {
			t.Errorf("RetryDelay(%d) = %s, want %s", i+1, got, expected)
		}
	}

	// Attempt numbers below 1 cannot happen from the worker, but a zero must not
	// mean "retry immediately, forever".
	if RetryDelay(0) != time.Minute {
		t.Errorf("RetryDelay(0) = %s, want the first delay", RetryDelay(0))
	}
}
