package mailer

import (
	"context"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
)

// An unconfigured mailer must be a silent no-op rather than an error. This is
// the default state of every deployment, and the README's "sends no email"
// promise rests on it: callers do not branch, so nothing else in the app has to
// remember that mail might not be set up.
func TestUnconfiguredIsNoOp(t *testing.T) {
	m := New(config.SMTPConfig{})
	if m.Enabled() {
		t.Fatal("a mailer with no host reports Enabled()")
	}
	// Deliberately a host that would fail instantly if it were ever dialled.
	if err := m.Send(context.Background(), Message{
		To: "someone@example.test", Subject: "hi", Body: "there",
	}); err != nil {
		t.Errorf("Send on an unconfigured mailer = %v, want nil", err)
	}
}

// Enabled needs BOTH a host and a sender. A host without a From would produce
// messages most servers reject, so it must not read as configured — config.Load
// refuses to start in that state, and this pins the same rule at the type.
func TestEnabledNeedsHostAndFrom(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.SMTPConfig
		want bool
	}{
		{"nothing set", config.SMTPConfig{}, false},
		{"host only", config.SMTPConfig{Host: "mail.example.test"}, false},
		{"from only", config.SMTPConfig{From: "a@example.test"}, false},
		{"both", config.SMTPConfig{Host: "mail.example.test", From: "a@example.test"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := New(c.cfg).Enabled(); got != c.want {
				t.Errorf("Enabled() = %v, want %v", got, c.want)
			}
		})
	}
}

// A configured mailer must still refuse a message with nowhere to go, rather
// than opening a connection to find out.
func TestSendRequiresRecipient(t *testing.T) {
	m := New(config.SMTPConfig{Host: "mail.example.test", From: "a@example.test", Port: 587})
	if err := m.Send(context.Background(), Message{Subject: "hi", Body: "there"}); err == nil {
		t.Error("Send with no recipient returned nil")
	}
}

// The wire format. Headers present, UTF-8 declared, and — the one that actually
// bites — CRLF line endings with dot-stuffing, so a body line consisting of a
// single "." cannot terminate the DATA section early and truncate the digest.
func TestRenderMessage(t *testing.T) {
	out := render("ledger@example.test", Message{
		To:      "you@example.test",
		Subject: "Your July 2026 recap",
		Body:    "line one\n.\nline two",
	})

	for _, want := range []string{
		"From: ledger@example.test\r\n",
		"To: you@example.test\r\n",
		"MIME-Version: 1.0\r\n",
		"Content-Type: text/plain; charset=utf-8\r\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered message is missing %q", want)
		}
	}

	if !strings.Contains(out, "\r\n..\r\n") {
		t.Errorf("a lone \".\" line was not dot-stuffed:\n%q", out)
	}
	// Every LF must be part of a CRLF: a bare LF on the wire is not legal, and
	// some servers silently mangle the message rather than rejecting it.
	if strings.Contains(strings.ReplaceAll(out, "\r\n", ""), "\n") {
		t.Errorf("rendered message contains a bare LF:\n%q", out)
	}

	headers, body, found := strings.Cut(out, "\r\n\r\n")
	if !found {
		t.Fatal("no blank line separating headers from body")
	}
	if strings.Contains(headers, "line one") {
		t.Error("body content leaked into the headers")
	}
	if !strings.Contains(body, "line two") {
		t.Error("body is missing its content")
	}
}

// A non-ASCII subject must be encoded rather than sent raw, or it arrives
// mojibake in most clients. Period labels are user-visible prose, so this is
// reachable in practice.
func TestSubjectIsEncoded(t *testing.T) {
	out := render("a@example.test", Message{
		To: "b@example.test", Subject: "Your recap — July", Body: "x",
	})
	subject := ""
	for _, line := range strings.Split(out, "\r\n") {
		if strings.HasPrefix(line, "Subject: ") {
			subject = line
			break
		}
	}
	if subject == "" {
		t.Fatal("no Subject header")
	}
	if strings.Contains(subject, "—") {
		t.Errorf("Subject carries a raw non-ASCII rune: %q", subject)
	}
	if !strings.Contains(subject, "=?utf-8?") {
		t.Errorf("Subject was not Q-encoded: %q", subject)
	}
}
