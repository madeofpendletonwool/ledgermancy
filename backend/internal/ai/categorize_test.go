package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apex42group/ledgermancy/backend/internal/config"
)

func TestParseMerchantVerdicts(t *testing.T) {
	cases := map[string]string{
		"bare array":    `[{"merchant_key":"a","slug":"groceries","confidence":0.9}]`,
		"json fence":    "```json\n[{\"merchant_key\":\"a\",\"slug\":\"groceries\",\"confidence\":0.9}]\n```",
		"leading prose": `Sure! Here you go: [{"merchant_key":"a","slug":"groceries","confidence":0.9}] hope that helps`,
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parseMerchantVerdicts(text)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(got) != 1 || got[0].Slug != "groceries" || got[0].MerchantKey != "a" {
				t.Fatalf("got %+v", got)
			}
		})
	}

	if _, err := parseMerchantVerdicts("no array here"); err == nil {
		t.Error("expected error for response with no JSON array")
	}
}

// A slug the model invents despite instructions must be dropped, never applied —
// otherwise a hallucinated category id lookup would silently fail or, worse,
// mislabel. A null slug is a clean abstention and is simply omitted.
func TestCategoriseMerchantsDropsInvalidSlug(t *testing.T) {
	// The model's answer is a JSON array delivered inside a text block; marshal
	// the whole response so the inner string is escaped correctly.
	answer := `[{"merchant_key":"good","slug":"groceries","confidence":0.9},` +
		`{"merchant_key":"made_up","slug":"teleportation","confidence":0.95},` +
		`{"merchant_key":"unsure","slug":null,"confidence":0.1}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(Response{
			ID: "m", Role: RoleAssistant, StopReason: "end_turn",
			Content: []Block{TextBlock(answer)},
		})
	}))
	defer srv.Close()

	c := New(config.AIConfig{BaseURL: srv.URL, APIKey: "k", Model: "glm-4.6"})
	got, err := c.CategoriseMerchants(context.Background(),
		[]MerchantInput{{MerchantKey: "good"}, {MerchantKey: "made_up"}, {MerchantKey: "unsure"}},
		[]CategoryOption{{Slug: "groceries", Name: "Groceries"}, {Slug: "dining", Name: "Dining"}},
	)
	if err != nil {
		t.Fatalf("CategoriseMerchants: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 valid verdict, got %d: %+v", len(got), got)
	}
	if got[0].MerchantKey != "good" || got[0].Slug != "groceries" {
		t.Errorf("got %+v", got[0])
	}
}

func TestCategoriseMerchantsDisabled(t *testing.T) {
	c := New(config.AIConfig{Model: "glm-4.6"})
	if _, err := c.CategoriseMerchants(context.Background(),
		[]MerchantInput{{MerchantKey: "a"}}, []CategoryOption{{Slug: "x"}}); err != ErrDisabled {
		t.Fatalf("want ErrDisabled, got %v", err)
	}
}

// No merchants or no categories is a no-op, not a model call.
func TestCategoriseMerchantsEmpty(t *testing.T) {
	c := New(config.AIConfig{BaseURL: "http://unreachable.invalid", APIKey: "k", Model: "m"})
	got, err := c.CategoriseMerchants(context.Background(), nil, []CategoryOption{{Slug: "x"}})
	if err != nil || got != nil {
		t.Fatalf("want nil,nil got %+v,%v", got, err)
	}
}
