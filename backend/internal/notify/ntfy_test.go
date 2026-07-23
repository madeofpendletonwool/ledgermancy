package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/apex42group/ledgermancy/backend/internal/config"
	"github.com/apex42group/ledgermancy/backend/internal/db"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// TestNtfySend drives the notifier against a real Postgres (for the preference
// lookups) and a stub ntfy server: a configured user must produce one POST to
// {base}/{topic} carrying the right headers and body, and an unconfigured user
// (or "none" channel) must be a silent no-op.
//
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test ./internal/notify/
func TestNtfySend(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	q := dbgen.New(pool)

	householdID := uuid.New()
	sender := uuid.New() // channel=ntfy, has a topic
	silent := uuid.New() // channel=none
	unset := uuid.New()  // no preferences at all

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO households (id, name) VALUES ($1, 'Notify Test')`, householdID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM households WHERE id = $1`, householdID)
	})
	for _, u := range []uuid.UUID{sender, silent, unset} {
		exec(`INSERT INTO users (id, household_id, email, password_hash, display_name)
		      VALUES ($1, $2, $3, 'x', 'Tester')`, u, householdID, u.String()+"@example.test")
	}
	setPref := func(u uuid.UUID, key, jsonValue string) {
		exec(`INSERT INTO preferences (scope, user_id, key, value) VALUES ('user', $1, $2, $3::jsonb)`, u, key, jsonValue)
	}
	setPref(sender, "notify.channel", `"ntfy"`)
	setPref(sender, "notify.ntfy_topic", `"my-topic"`)
	setPref(silent, "notify.channel", `"none"`)

	// Stub ntfy server captures the last request it received.
	var got *http.Request
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(config.NTFYConfig{BaseURL: srv.URL, Token: "sekret"}, q)

	msg := Notification{
		Title:    "Large purchase",
		Body:     "Acme — $412.00 on 2026-07-20",
		Priority: 4,
		Tags:     []string{"dollar", "warning"},
		ClickURL: "https://app.example/alerts",
	}

	// 1) Configured user → exactly one POST with the right shape.
	if err := n.Send(ctx, sender, msg); err != nil {
		t.Fatalf("send to configured user: %v", err)
	}
	if got == nil {
		t.Fatal("configured user: expected a request to ntfy, got none")
	}
	if got.Method != http.MethodPost || got.URL.Path != "/my-topic" {
		t.Errorf("request = %s %s, want POST /my-topic", got.Method, got.URL.Path)
	}
	if h := got.Header.Get("Title"); h != msg.Title {
		t.Errorf("Title = %q, want %q", h, msg.Title)
	}
	if h := got.Header.Get("Priority"); h != "4" {
		t.Errorf("Priority = %q, want 4", h)
	}
	if h := got.Header.Get("Tags"); h != "dollar,warning" {
		t.Errorf("Tags = %q, want dollar,warning", h)
	}
	if h := got.Header.Get("Click"); h != msg.ClickURL {
		t.Errorf("Click = %q, want %q", h, msg.ClickURL)
	}
	if h := got.Header.Get("Authorization"); h != "Bearer sekret" {
		t.Errorf("Authorization = %q, want Bearer sekret", h)
	}
	if gotBody != msg.Body {
		t.Errorf("body = %q, want %q", gotBody, msg.Body)
	}

	// 2) channel="none" → no-op, no request.
	got = nil
	if err := n.Send(ctx, silent, msg); err != nil {
		t.Fatalf("send to silent user: %v", err)
	}
	if got != nil {
		t.Error("channel=none user: expected no request, got one")
	}

	// 3) No preferences at all → no-op, no request.
	got = nil
	if err := n.Send(ctx, unset, msg); err != nil {
		t.Fatalf("send to unset user: %v", err)
	}
	if got != nil {
		t.Error("unconfigured user: expected no request, got one")
	}
}
