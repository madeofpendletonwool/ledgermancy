// Package mailer sends the optional emailed digest.
//
// It mirrors internal/notify and internal/ai in shape: a client is always
// constructed, reports Enabled(), and is safe to call when nothing is
// configured — so callers never branch on configuration.
//
// This is the only place in the app that sends mail, and it exists solely for
// doc 25's digest. Adding a second caller means revisiting the README claim
// about what the app does and does not send.
package mailer

import (
	"context"
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/config"
)

// dialTimeout bounds the whole conversation with the mail server. A digest is
// not interactive and the River job retries, so failing fast beats holding a
// worker on a server that has stopped answering.
const dialTimeout = 20 * time.Second

// Message is one plain-text email. Only the fields the digest needs: there is
// no attachment, HTML alternative or CC path, and adding one should be a
// deliberate change rather than something that grows in.
type Message struct {
	To      string
	Subject string
	Body    string
}

// Sender delivers a Message. The interface exists so the digest job can be
// tested without a mail server.
type Sender interface {
	// Enabled reports whether a server is configured to deliver to at all.
	Enabled() bool
	// Send delivers one message, or returns nil without doing anything when no
	// server is configured.
	Send(ctx context.Context, msg Message) error
}

// SMTP is the production Sender.
type SMTP struct {
	cfg config.SMTPConfig
}

// New builds a Sender from SMTP config. Like notify.New it never fails and
// never returns nil; an unconfigured server yields no-op sends.
func New(cfg config.SMTPConfig) *SMTP { return &SMTP{cfg: cfg} }

// Enabled reports whether a server is configured.
func (m *SMTP) Enabled() bool { return m != nil && m.cfg.Enabled() }

// Send delivers one message. It is a no-op (nil) when no server is configured,
// so callers can always call.
func (m *SMTP) Send(ctx context.Context, msg Message) error {
	if !m.Enabled() {
		return nil
	}
	if strings.TrimSpace(msg.To) == "" {
		return fmt.Errorf("no recipient address")
	}

	client, err := m.dial(ctx)
	if err != nil {
		return err
	}
	// Quit closes the connection cleanly; on an error path the deferred Close is
	// what actually releases the socket, and a double-close is harmless.
	defer func() { _ = client.Close() }()

	if err := m.authenticate(client); err != nil {
		return err
	}

	if err := client.Mail(m.cfg.From); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	if err := client.Rcpt(msg.To); err != nil {
		return fmt.Errorf("smtp RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := w.Write([]byte(render(m.cfg.From, msg))); err != nil {
		return fmt.Errorf("write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finish message: %w", err)
	}
	return client.Quit()
}

// dial opens the connection, honouring the configured transport security.
//
// Both encrypted modes verify the server's certificate against the configured
// host name. There is deliberately no "skip verification" setting: a knob that
// turns an encrypted channel into an unauthenticated one is a footgun, and an
// operator who genuinely has no usable certificate can say so with
// SMTP_SECURITY=none rather than pretending.
func (m *SMTP) dial(ctx context.Context) (*smtp.Client, error) {
	addr := net.JoinHostPort(m.cfg.Host, strconv.Itoa(m.cfg.Port))
	tlsCfg := &tls.Config{ServerName: m.cfg.Host, MinVersion: tls.VersionTLS12}

	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	var conn net.Conn
	var err error
	if m.cfg.Security == "tls" {
		conn, err = (&tls.Dialer{Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("dial smtp %s: %w", addr, err)
	}
	// The deadline covers the whole SMTP conversation, not just the dial —
	// net/smtp has no context support of its own.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	client, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("smtp handshake: %w", err)
	}

	if m.cfg.Security == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			_ = client.Close()
			return nil, fmt.Errorf("smtp server %s does not offer STARTTLS (set SMTP_SECURITY=none if that is intended)", addr)
		}
		if err := client.StartTLS(tlsCfg); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("smtp STARTTLS: %w", err)
		}
	}
	return client, nil
}

// authenticate performs PLAIN auth when a username is configured and the server
// advertises AUTH. net/smtp refuses PLAIN over an unencrypted connection to a
// non-localhost server, and that refusal is left in place on purpose.
func (m *SMTP) authenticate(client *smtp.Client) error {
	if m.cfg.Username == "" {
		return nil
	}
	if ok, _ := client.Extension("AUTH"); !ok {
		return fmt.Errorf("smtp server does not offer AUTH but SMTP_USERNAME is set")
	}
	if err := client.Auth(smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)); err != nil {
		return fmt.Errorf("smtp auth: %w", err)
	}
	return nil
}

// render builds the RFC 5322 message.
//
// The subject is Q-encoded so a "£" or an em dash in a period label survives;
// the body goes out as UTF-8 plain text, which every client has handled for
// twenty years and which cannot execute anything.
func render(from string, msg Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", msg.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", msg.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	// Bare LF is legal in a body but not on the wire; normalise to CRLF, and
	// dot-stuff so a line that is just "." cannot end the DATA section early.
	for _, line := range strings.Split(strings.ReplaceAll(msg.Body, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, ".") {
			b.WriteByte('.')
		}
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	return b.String()
}
