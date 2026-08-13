package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/crypto"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/webhooks"
)

// With WEBHOOKS_ENABLED unset, every route reports 503 rather than quietly
// working. Hiding the settings section would not be enough: the guarantee the
// switch makes is that this instance cannot make an outbound webhook request,
// and a route that still created subscriptions would break it the moment
// somebody found the endpoint.
func TestWebhookEndpointsAreOffWithoutTheSwitch(t *testing.T) {
	srv := &Server{Config: config.Config{}}
	identity := auth.Identity{UserID: uuid.New(), HouseholdID: uuid.New()}

	for name, handler := range map[string]http.HandlerFunc{
		"list":     srv.handleListWebhooks,
		"create":   srv.handleCreateWebhook,
		"triggers": srv.handleListWebhookTriggers,
		"update":   srv.handleUpdateWebhook,
		"delete":   srv.handleDeleteWebhook,
		"rotate":   srv.handleRotateWebhookSecret,
		"test":     srv.handleTestWebhook,
		"messages": srv.handleListWebhookMessages,
		"attempts": srv.handleListWebhookAttempts,
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/webhooks/", strings.NewReader("{}"))
		req = req.WithContext(auth.ContextWithIdentity(context.Background(), identity))
		rec := httptest.NewRecorder()

		handler(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503", name, rec.Code)
		}
		// The message has to name the variable — the next question an operator
		// asks is always "how do I turn it on".
		if !strings.Contains(rec.Body.String(), "WEBHOOKS_ENABLED") {
			t.Errorf("%s: the error does not name the switch: %s", name, rec.Body.String())
		}
	}
}

// Enabled but with no cipher is a wiring mistake, not a configuration. It must
// refuse rather than nil-panic on the way to sealing a secret.
func TestWebhookEndpointsRefuseWithoutACipher(t *testing.T) {
	srv := &Server{Config: config.Config{Webhooks: config.WebhooksConfig{Enabled: true}}}

	req := httptest.NewRequest(http.MethodGet, "/api/webhooks/", nil)
	req = req.WithContext(auth.ContextWithIdentity(context.Background(),
		auth.Identity{UserID: uuid.New(), HouseholdID: uuid.New()}))
	rec := httptest.NewRecorder()

	srv.handleListWebhooks(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// Everything a create body can be wrong about, refused with a message that says
// which field. Validation is shared between create and update, so a webhook can
// never be created under one set of rules and edited under another.
func TestWebhookCreateValidation(t *testing.T) {
	cipher, err := crypto.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	srv := &Server{
		Config: config.Config{Webhooks: config.WebhooksConfig{Enabled: true}},
		Cipher: cipher,
	}
	identity := auth.Identity{UserID: uuid.New(), HouseholdID: uuid.New()}

	cases := []struct {
		name, body, wantIn string
	}{
		{"no name", `{"name":"  ","url":"https://a.test/h","triggers":["alert.fired"]}`, "name"},
		{"no url", `{"name":"n","url":"","triggers":["alert.fired"]}`, "url"},
		{"not http", `{"name":"n","url":"ftp://a.test/h","triggers":["alert.fired"]}`, "http"},
		{"no triggers", `{"name":"n","url":"https://a.test/h","triggers":[]}`, "triggers"},
		{"unknown trigger", `{"name":"n","url":"https://a.test/h","triggers":["transaction.created"]}`, "triggers"},
		{"name too long", `{"name":"` + strings.Repeat("x", maxWebhookNameLen+1) +
			`","url":"https://a.test/h","triggers":["alert.fired"]}`, "too long"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/webhooks/", strings.NewReader(tc.body))
			req = req.WithContext(auth.ContextWithIdentity(context.Background(), identity))
			rec := httptest.NewRecorder()

			srv.handleCreateWebhook(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantIn) {
				t.Errorf("message %q does not mention %q", rec.Body.String(), tc.wantIn)
			}
		})
	}
}

// The trigger list the UI builds its checkboxes from must be the one the
// validator accepts. A vocabulary that drifted would offer a subscription the
// backend then refuses.
func TestPublishedTriggersAreTheOnesAccepted(t *testing.T) {
	cipher, err := crypto.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	srv := &Server{
		Config: config.Config{Webhooks: config.WebhooksConfig{Enabled: true}},
		Cipher: cipher,
	}

	req := httptest.NewRequest(http.MethodGet, "/api/webhooks/triggers", nil)
	rec := httptest.NewRecorder()
	srv.handleListWebhookTriggers(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	for _, trigger := range webhooks.Triggers {
		if !strings.Contains(rec.Body.String(), trigger) {
			t.Errorf("the published list is missing %q: %s", trigger, rec.Body.String())
		}
		if _, ok := webhooks.NormalizeTriggers([]string{trigger}); !ok {
			t.Errorf("published trigger %q is refused by the validator", trigger)
		}
	}
	// And the test trigger is not offered, or a press of "send test" would fan
	// out to every subscriber instead of the one webhook it is addressed to.
	if strings.Contains(rec.Body.String(), webhooks.TriggerTest) {
		t.Errorf("the test trigger is subscribable: %s", rec.Body.String())
	}
}
