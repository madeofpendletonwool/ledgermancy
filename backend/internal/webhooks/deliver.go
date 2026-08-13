package webhooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// RequestTimeout bounds one delivery.
//
// Short, for the same reason internal/notify's is: a delivery is fire-and-
// forget from the app's point of view, the message row is already durable, and
// the queue will retry — so holding a worker on a receiver that has stopped
// answering buys nothing and costs the other households in the queue. A
// receiver that genuinely needs longer than this should answer 202 and do its
// work afterwards, which is what every webhook guide already tells it to.
const RequestTimeout = 10 * time.Second

// maxStoredBodyBytes caps what a single attempt keeps of the receiver's
// response.
//
// The response body is stored to answer "what did it say?", and the useful
// answer is a short error string or a fragment of an HTML error page. A
// misconfigured receiver that returns its whole 40 KB login page would otherwise
// write that much per retry, five times per message, forever. Two kilobytes is
// enough to read the beginning of any error worth reading.
const maxStoredBodyBytes = 2 << 10

// Delivery is one HTTP request to make.
type Delivery struct {
	URL    string
	Secret string
	// MessageID is the webhook_messages id. It goes out as X-Ledgermancy-Delivery
	// and is stable across retries, so a receiver can dedupe on it.
	MessageID uuid.UUID
	Trigger   string
	// Attempt is 1-based and appears in the headers, so a receiver can tell a
	// retry from a first delivery.
	Attempt int
	// Body is sent verbatim and is what gets signed. It must be the bytes read
	// back from the payload column, not a re-marshalled struct — see Sign.
	Body []byte
}

// Attempt is the record of one delivery, ready to be written to
// webhook_attempts. Exactly one of Response* or Err is populated: a request that
// never completed has no response to describe, and a completed one has no
// transport error to explain.
type Attempt struct {
	RequestHeaders map[string]string
	RequestBody    string

	// ResponseStatus is nil when the request never completed.
	ResponseStatus  *int32
	ResponseHeaders map[string][]string
	ResponseBody    string

	// Err is the transport failure (DNS, refused, timeout) or the rejection
	// reason for a non-2xx answer. Empty on success.
	Err string

	DurationMS int32

	// OK is the single fact the worker branches on: the receiver answered 2xx.
	OK bool
}

// NewClient builds the HTTP client deliveries are made with.
//
// Redirects are NOT followed, and that is a security decision rather than a
// simplification. A signed payload carrying a household's finances is addressed
// to one host the user named; a 302 is that host asking us to send the same
// signed body somewhere else, and honouring it would turn any compromised or
// merely careless receiver into a way to forward the household's events to a
// third party. Returning the 3xx instead means it is recorded as a failed
// attempt the user can see and fix.
func NewClient() *http.Client {
	return &http.Client{
		Timeout: RequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Send performs one delivery and describes what happened.
//
// It returns an Attempt for EVERY outcome, including the ones where no request
// was made at all, and only returns a non-nil error when it could not even build
// the request. The caller always has something to record: an attempt row that
// exists but says "connection refused" is the answer to a support question,
// where a dropped error is the absence of one.
//
// now is passed rather than read so the signature timestamp is the one the test
// pins.
func Send(ctx context.Context, client *http.Client, d Delivery, now time.Time) (Attempt, error) {
	signature := Sign(d.Secret, now, d.Body)

	headers := map[string]string{
		"Content-Type":  "application/json",
		"User-Agent":    "Ledgermancy-Webhooks/1",
		DeliveryHeader:  d.MessageID.String(),
		TriggerHeader:   d.Trigger,
		AttemptHeader:   strconv.Itoa(d.Attempt),
		SignatureHeader: signature,
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(d.Body))
	if err != nil {
		return Attempt{}, fmt.Errorf("build webhook request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	attempt := Attempt{
		RequestHeaders: headers,
		RequestBody:    string(d.Body),
	}

	start := time.Now()
	resp, err := client.Do(req)
	attempt.DurationMS = int32(time.Since(start).Milliseconds())
	if err != nil {
		// The URL is deliberately not repeated here: it is already on the
		// webhook row the attempt hangs off, and Go's *url.Error stringifies to
		// include it, which would put a user-supplied string into the middle of
		// a message the UI renders.
		attempt.Err = transportError(err)
		return attempt, nil
	}
	defer resp.Body.Close()

	status := int32(resp.StatusCode)
	attempt.ResponseStatus = &status
	attempt.ResponseHeaders = resp.Header.Clone()

	// Read past the cap by one byte so a truncated body can say so, and drain
	// nothing further — the connection is not worth reusing to a receiver that
	// answers with a novel.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxStoredBodyBytes+1))
	if len(body) > maxStoredBodyBytes {
		body = append(body[:maxStoredBodyBytes], "… (truncated)"...)
	}
	attempt.ResponseBody = string(body)

	// 2xx and nothing else. A 3xx reaches here only because redirects are not
	// followed (see NewClient), and it is a misconfiguration worth surfacing
	// rather than a success.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		attempt.OK = true
		return attempt, nil
	}
	attempt.Err = fmt.Sprintf("receiver responded %d", resp.StatusCode)
	return attempt, nil
}

// transportError renders a client.Do failure without the URL.
//
// http.Client wraps everything in *url.Error, whose Error() embeds the request
// URL. That string is shown in the delivery inspector, and echoing a
// user-supplied URL back into rendered text is a habit worth not having. The
// timeout gets its own wording because "context deadline exceeded" tells a user
// nothing about which deadline or how long it was.
func transportError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("no response within %s", RequestTimeout)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err.Error()
	}
	return err.Error()
}
