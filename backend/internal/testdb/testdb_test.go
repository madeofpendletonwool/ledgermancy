package testdb

import (
	"strings"
	"testing"
)

// The name derivation is the part that can silently do the wrong thing: get it
// wrong and every package quietly shares one database again, which is exactly
// the bug this package exists to kill.
func TestDatabaseName(t *testing.T) {
	long := strings.Repeat("a", 60)

	cases := []struct {
		name, admin, pkg, want string
	}{
		{
			name:  "the CI and local form",
			admin: "postgres://postgres:test@localhost:55432/lmtest?sslmode=disable",
			pkg:   "api",
			want:  "lmtest_api",
		},
		{
			name:  "a different base database still leads",
			admin: "postgres://u:p@db:5432/scratch",
			pkg:   "insights",
			want:  "scratch_insights",
		},
		{
			name:  "over 63 bytes is truncated the way Postgres would",
			admin: "postgres://u:p@db:5432/" + long,
			pkg:   "api",
			want:  long + "_ap",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := databaseName(tc.admin, tc.pkg)
			if err != nil {
				t.Fatalf("databaseName: %v", err)
			}
			if got != tc.want {
				t.Errorf("databaseName = %q, want %q", got, tc.want)
			}
			if len(got) > 63 {
				t.Errorf("name is %d bytes; Postgres would truncate it", len(got))
			}
		})
	}
}

func TestDatabaseNameRejectsAURLWithNoDatabase(t *testing.T) {
	if _, err := databaseName("postgres://postgres:test@localhost:55432", "api"); err == nil {
		t.Fatal("a URL naming no database was accepted; the derived name would be junk")
	}
}

// The rewrite must keep sslmode and the credentials, or the derived URL fails to
// connect in exactly the environment (CI) where it matters.
func TestWithDatabaseKeepsEverythingButTheName(t *testing.T) {
	got, err := withDatabase("postgres://postgres:test@localhost:55432/lmtest?sslmode=disable", "lmtest_db")
	if err != nil {
		t.Fatalf("withDatabase: %v", err)
	}
	want := "postgres://postgres:test@localhost:55432/lmtest_db?sslmode=disable"
	if got != want {
		t.Errorf("withDatabase = %q, want %q", got, want)
	}
}

// Connection strings reach CI logs through t.Fatalf, so the password must not.
func TestRedactHidesThePassword(t *testing.T) {
	got := redact("postgres://postgres:hunter2@localhost:55432/lmtest?sslmode=disable")
	if strings.Contains(got, "hunter2") {
		t.Errorf("redact leaked the password: %q", got)
	}
	if !strings.Contains(got, "postgres:xxxxx@localhost") {
		t.Errorf("redact should keep the shape of the string, got %q", got)
	}
}
