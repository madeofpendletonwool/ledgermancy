package config

import "testing"

// validateSMTP is the boot-time guard on a half-configured mail server. The
// failure it prevents does not look like a configuration mistake when it
// happens: Settings shows an "Email it to me" toggle, members tick it, and every
// message is rejected by a server that will not accept a sender-less envelope.
func TestValidateSMTP(t *testing.T) {
	cases := []struct {
		name    string
		cfg     SMTPConfig
		wantErr bool
	}{
		{
			name: "unconfigured is fine — this is every deployment by default",
			cfg:  SMTPConfig{},
		},
		{
			// Inert rather than wrong: with no host nothing is ever dialled, so
			// refusing to boot over it would be officious.
			name: "a sender with no host is tolerated",
			cfg:  SMTPConfig{From: "ledger@example.test"},
		},
		{
			name:    "a host with no sender is refused",
			cfg:     SMTPConfig{Host: "mail.example.test", Port: 587, Security: "starttls"},
			wantErr: true,
		},
		{
			name: "fully configured",
			cfg: SMTPConfig{
				Host: "mail.example.test", From: "ledger@example.test",
				Port: 587, Security: "starttls",
			},
		},
		{
			name: "implicit TLS on 465",
			cfg: SMTPConfig{
				Host: "mail.example.test", From: "ledger@example.test",
				Port: 465, Security: "tls",
			},
		},
		{
			name: "unencrypted local relay is allowed, deliberately",
			cfg: SMTPConfig{
				Host: "localhost", From: "ledger@example.test",
				Port: 25, Security: "none",
			},
		},
		{
			name: "an unknown security mode is refused rather than silently downgraded",
			cfg: SMTPConfig{
				Host: "mail.example.test", From: "ledger@example.test",
				Port: 587, Security: "ssl",
			},
			wantErr: true,
		},
		{
			name: "a nonsense port is refused",
			cfg: SMTPConfig{
				Host: "mail.example.test", From: "ledger@example.test",
				Port: 0, Security: "starttls",
			},
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateSMTP(c.cfg)
			if c.wantErr && err == nil {
				t.Error("expected an error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// Enabled() is what every caller branches on, so it has to mean "a message could
// actually be delivered", not "somebody set a variable".
func TestSMTPEnabled(t *testing.T) {
	if (SMTPConfig{}).Enabled() {
		t.Error("an empty SMTP config reports Enabled()")
	}
	if (SMTPConfig{Host: "mail.example.test"}).Enabled() {
		t.Error("a host with no sender reports Enabled()")
	}
	if !(SMTPConfig{Host: "mail.example.test", From: "a@example.test"}).Enabled() {
		t.Error("a fully configured SMTP config does not report Enabled()")
	}
}
