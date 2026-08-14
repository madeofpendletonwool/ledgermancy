package webhooks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// End to end against a real HTTP server that behaves like a receiver: it reads
// the body, reads the signature header, and verifies. This is the test that says
// the wire format works, as opposed to TestSignatureMatchesTheDocumentedRecipe,
// which says the recipe is right.
func TestSendSignsWhatTheReceiverReads(t *testing.T) {
	const secret = "whsec_receiver"
	messageID := uuid.New()
	body := []byte(`{"trigger":"alert.fired","data":{"amount":"412.55"}}`)

	var verified bool
	var gotDelivery, gotTrigger, gotAttempt, gotContentType string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received := make([]byte, r.ContentLength)
		if _, err := r.Body.Read(received); err != nil && len(received) == 0 {
			t.Errorf("read body: %v", err)
		}
		if err := Verify(secret, r.Header.Get(SignatureHeader), received, time.Now(), time.Minute); err != nil {
			t.Errorf("receiver could not verify the delivery: %v", err)
		} else {
			verified = true
		}
		gotDelivery = r.Header.Get(DeliveryHeader)
		gotTrigger = r.Header.Get(TriggerHeader)
		gotAttempt = r.Header.Get(AttemptHeader)
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	attempt, err := Send(context.Background(), srv.Client(), Delivery{
		URL: srv.URL, Secret: secret, MessageID: messageID,
		Trigger: TriggerAlertFired, Attempt: 1, Body: body,
	}, time.Now())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if !attempt.OK {
		t.Fatalf("a 204 is a success; got err=%q status=%v", attempt.Err, attempt.ResponseStatus)
	}
	if !verified {
		t.Fatal("the receiver never verified the signature")
	}
	if gotDelivery != messageID.String() {
		t.Errorf("%s = %q, want the message id %q", DeliveryHeader, gotDelivery, messageID)
	}
	if gotTrigger != TriggerAlertFired {
		t.Errorf("%s = %q, want %q", TriggerHeader, gotTrigger, TriggerAlertFired)
	}
	if gotAttempt != "1" {
		t.Errorf("%s = %q, want %q", AttemptHeader, gotAttempt, "1")
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
}

// A receiver that answers with an error is a failure the user has to be able to
// read afterwards, so the status and the body both have to survive into the
// attempt record.
func TestSendRecordsAFailedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream exploded"))
	}))
	defer srv.Close()

	attempt, err := Send(context.Background(), srv.Client(), Delivery{
		URL: srv.URL, Secret: "whsec_x", MessageID: uuid.New(),
		Trigger: TriggerTest, Attempt: 2, Body: []byte(`{}`),
	}, time.Now())
	if err != nil {
		t.Fatalf("Send returned an error for a completed request: %v", err)
	}

	if attempt.OK {
		t.Fatal("a 500 must not be treated as delivered")
	}
	if attempt.ResponseStatus == nil || *attempt.ResponseStatus != 500 {
		t.Errorf("ResponseStatus = %v, want 500", attempt.ResponseStatus)
	}
	if !strings.Contains(attempt.ResponseBody, "upstream exploded") {
		t.Errorf("ResponseBody = %q, want the receiver's message", attempt.ResponseBody)
	}
	if attempt.Err == "" {
		t.Error("a failed delivery must carry a reason")
	}
}

// A receiver nobody is listening on must produce an attempt record, not a
// dropped error. "Connection refused" recorded against the message is the answer
// to a support question; a silent failure is the absence of one.
func TestSendRecordsATransportFailure(t *testing.T) {
	// Start and immediately stop a server, so the port is real and closed.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	attempt, err := Send(context.Background(), NewClient(), Delivery{
		URL: url, Secret: "whsec_x", MessageID: uuid.New(),
		Trigger: TriggerTest, Attempt: 1, Body: []byte(`{}`),
	}, time.Now())
	if err != nil {
		t.Fatalf("Send should describe a transport failure, not return it: %v", err)
	}

	if attempt.OK {
		t.Fatal("a refused connection is not a delivery")
	}
	if attempt.ResponseStatus != nil {
		t.Errorf("ResponseStatus = %v, want nil when nothing answered", *attempt.ResponseStatus)
	}
	if attempt.Err == "" {
		t.Error("a transport failure must carry a reason")
	}
	// The URL must not be echoed into the recorded reason — see transportError.
	if strings.Contains(attempt.Err, url) {
		t.Errorf("the recorded error repeats the destination URL: %q", attempt.Err)
	}
}

// Redirects are not followed: a 302 is a receiver asking us to send a household's
// signed events somewhere else, and honouring it would make any careless
// receiver a forwarding service.
func TestSendDoesNotFollowRedirects(t *testing.T) {
	var elsewhereHit bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	defer redirector.Close()

	attempt, err := Send(context.Background(), NewClient(), Delivery{
		URL: redirector.URL, Secret: "whsec_x", MessageID: uuid.New(),
		Trigger: TriggerTest, Attempt: 1, Body: []byte(`{}`),
	}, time.Now())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if elsewhereHit {
		t.Fatal("the signed payload was forwarded to the redirect target")
	}
	if attempt.OK {
		t.Fatal("a 302 must be recorded as a failure, not a delivery")
	}
	if attempt.ResponseStatus == nil || *attempt.ResponseStatus != http.StatusFound {
		t.Errorf("ResponseStatus = %v, want 302 recorded verbatim", attempt.ResponseStatus)
	}
}

// A receiver that answers with a wall of HTML must not write a wall of HTML per
// retry, forever.
func TestSendTruncatesAHugeResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("x", 100_000)))
	}))
	defer srv.Close()

	attempt, err := Send(context.Background(), srv.Client(), Delivery{
		URL: srv.URL, Secret: "whsec_x", MessageID: uuid.New(),
		Trigger: TriggerTest, Attempt: 1, Body: []byte(`{}`),
	}, time.Now())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	// The cap plus the truncation marker, and nowhere near what was sent.
	if len(attempt.ResponseBody) > maxStoredBodyBytes+64 {
		t.Errorf("stored %d bytes of response body, cap is %d", len(attempt.ResponseBody), maxStoredBodyBytes)
	}
	if !strings.Contains(attempt.ResponseBody, "truncated") {
		t.Error("a truncated body should say so")
	}
}
