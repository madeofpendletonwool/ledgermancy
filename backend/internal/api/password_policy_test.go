package api

import "testing"

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name        string
		password    string
		email       string
		displayName string
		wantOK      bool
	}{
		{
			name:     "a long passphrase is accepted",
			password: "correct horse battery staple",
			email:    "ada@example.com", displayName: "Ada",
			wantOK: true,
		},
		{
			// NIST guidance: length is the requirement that matters, and
			// composition rules push people towards predictable patterns.
			name:     "no composition rules are imposed",
			password: "aaabbbcccdddeee",
			email:    "ada@example.com", displayName: "Ada",
			wantOK: true,
		},
		{
			name:     "eleven characters is too short",
			password: "elevenchars",
			email:    "ada@example.com", displayName: "Ada",
			wantOK: false,
		},
		{
			name:     "exactly twelve characters is allowed",
			password: "twelvecharss",
			email:    "ada@example.com", displayName: "Ada",
			wantOK: true,
		},
		{
			name:     "cannot contain the email local part",
			password: "adalovelace-is-me",
			email:    "adalovelace@example.com", displayName: "Ada",
			wantOK: false,
		},
		{
			name:     "email match is case insensitive",
			password: "ADALOVELACE-is-me",
			email:    "adalovelace@example.com", displayName: "Ada",
			wantOK: false,
		},
		{
			name:     "cannot contain the display name",
			password: "xxAda Lovelacexx",
			email:    "someone@example.com", displayName: "Ada Lovelace",
			wantOK: false,
		},
		{
			name:     "a single repeated character is rejected",
			password: "aaaaaaaaaaaaaaaa",
			email:    "ada@example.com", displayName: "Ada",
			wantOK: false,
		},
		{
			// A missing display name must not make every password "contain" it.
			name:     "empty display name does not reject everything",
			password: "a perfectly fine passphrase",
			email:    "ada@example.com", displayName: "",
			wantOK: true,
		},
		{
			// Likewise an address with no @: Cut reports not-found, and the
			// check must be skipped rather than matching the whole string.
			name:     "malformed email does not reject everything",
			password: "a perfectly fine passphrase",
			email:    "not-an-email", displayName: "Ada",
			wantOK: true,
		},
		{
			// Long enough to pass the length check, but empty once trimmed.
			// Indexing the trimmed value without this guard panics, and this
			// input reaches an unauthenticated endpoint.
			name:     "all whitespace is rejected, not a panic",
			password: "            ",
			email:    "ada@example.com", displayName: "Ada",
			wantOK: false,
		},
		{
			name:     "leading and trailing whitespace is otherwise fine",
			password: "   a good passphrase   ",
			email:    "ada@example.com", displayName: "Ada",
			wantOK: true,
		},
		{
			// A one-character name would otherwise match nearly every
			// password that happens to contain that letter.
			name:     "a very short display name is not matched",
			password: "a-good-long-passphrase",
			email:    "someone@example.com", displayName: "L",
			wantOK: true,
		},
		{
			// "Al" appears inside personal, final, normally, casually…
			name:     "a two-letter name is not matched",
			password: "a perfectly normal passphrase",
			email:    "someone@example.com", displayName: "Al",
			wantOK: true,
		},
		{
			name:     "a short email local part is not matched",
			password: "a-good-long-passphrase",
			email:    "jo@example.com", displayName: "Josephine",
			wantOK: true,
		},
		{
			// At four characters the check turns back on: this is real reuse.
			name:     "a four-character name is still matched",
			password: "myname-is-adam-ok",
			email:    "someone@example.com", displayName: "Adam",
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, ok := validatePassword(tc.password, tc.email, tc.displayName)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v (%q), want %v", ok, msg, tc.wantOK)
			}
			if !ok && msg == "" {
				t.Error("rejected without telling the user why")
			}
		})
	}
}
