package continuity

import (
	"fmt"
	"testing"
	"time"
)

// Retention is measured in months, so the only practical way to check it is
// against fabricated timestamps. That is why Prune takes no clock and touches
// no disk — see the comment on it.

// daily builds n artefacts one day apart, newest first.
func daily(n int, end time.Time) []Artefact {
	out := make([]Artefact, 0, n)
	for i := 0; i < n; i++ {
		taken := end.AddDate(0, 0, -i)
		out = append(out, Artefact{
			Kind:  KindDBDump,
			Path:  fmt.Sprintf("/backups/db_dump/d-%s.dump", taken.Format(stampLayout)),
			Taken: taken,
		})
	}
	return out
}

func paths(arts []Artefact) map[string]bool {
	out := make(map[string]bool, len(arts))
	for _, a := range arts {
		out[a.Path] = true
	}
	return out
}

func TestPruneKeepsTheGenerationalSet(t *testing.T) {
	// A year of nightly backups. 7/4/6 should keep the last seven days, one per
	// week for four weeks, and one per month for six months.
	end := time.Date(2026, 7, 29, 3, 0, 0, 0, time.UTC)
	arts := daily(365, end)

	keep, remove := Prune(arts, Policy{Daily: 7, Weekly: 4, Monthly: 6})

	if len(keep)+len(remove) != len(arts) {
		t.Fatalf("prune lost artefacts: %d kept + %d removed != %d", len(keep), len(remove), len(arts))
	}

	// The seven most recent are non-negotiable: this is the tier that answers
	// "I deleted something yesterday".
	kept := paths(keep)
	for i := 0; i < 7; i++ {
		if !kept[arts[i].Path] {
			t.Errorf("daily tier dropped %s (%d days old)", arts[i].Path, i)
		}
	}

	// Weekly and monthly tiers overlap the daily one, so the total is bounded
	// rather than exact. What matters is that it is small and that it spans the
	// year — a policy that kept 300 of 365 would technically pass the tier
	// checks and defeat the purpose.
	if len(keep) > 7+4+6 {
		t.Errorf("kept %d artefacts, more than the 17 the policy allows", len(keep))
	}
	if len(keep) < 10 {
		t.Errorf("kept only %d artefacts from a year; the weekly and monthly tiers are not working", len(keep))
	}

	oldest := keep[len(keep)-1]
	if end.Sub(oldest.Taken) < 150*24*time.Hour {
		t.Errorf("oldest kept artefact is only %v old; six monthly slots should reach back ~6 months",
			end.Sub(oldest.Taken).Round(24*time.Hour))
	}
}

// TestPruneNeverEmptiesTheSet is the property that matters most. Every other
// behaviour here is a preference; deleting the last backup is a catastrophe.
func TestPruneNeverEmptiesTheSet(t *testing.T) {
	end := time.Date(2026, 7, 29, 3, 0, 0, 0, time.UTC)
	for _, count := range []int{1, 2, 5, 30, 400} {
		keep, _ := Prune(daily(count, end), Policy{Daily: 7, Weekly: 4, Monthly: 6})
		if len(keep) == 0 {
			t.Errorf("prune kept nothing from a set of %d", count)
		}
	}
}

// TestPruneIsSetRelativeNotClockRelative is the self-hosted case the policy was
// designed around: a home server that was powered off for months comes back and
// runs a sweep. A rule expressed against wall time would find every backup it
// has is outside the window and delete all of them. This asserts it does not.
func TestPruneKeepsEverythingOnAHostThatWasAsleep(t *testing.T) {
	// Backups stop a year ago; the host has been off since.
	end := time.Date(2025, 7, 29, 3, 0, 0, 0, time.UTC)
	arts := daily(10, end)

	keep, remove := Prune(arts, Policy{Daily: 7, Weekly: 4, Monthly: 6})

	if len(keep) < 7 {
		t.Errorf("a host that was asleep for a year kept only %d of its %d backups; "+
			"the policy has become clock-relative", len(keep), len(arts))
	}
	for _, a := range remove {
		if a.Taken.Equal(arts[0].Taken) {
			t.Error("the newest backup was pruned")
		}
	}
}

func TestPruneKeepsUnparseableFilesOutOfItsHands(t *testing.T) {
	// List() never returns files it cannot parse, so Prune never sees an
	// operator's own ledgermancy-2024-manual.dump. This pins the parsing half.
	if _, ok := parseStamp(KindDBDump, "ledgermancy-2024-manual.dump"); ok {
		t.Error("a hand-named file parsed as an artefact; retention could delete it")
	}
	if _, ok := parseStamp(KindDBDump, "ledgermancy-db_dump-20260729T030000Z.dump"); !ok {
		t.Error("a file this package wrote did not parse back")
	}
	// A dump must not be mistaken for an export, or one kind's retention would
	// prune the other's.
	if _, ok := parseStamp(KindExport, "ledgermancy-db_dump-20260729T030000Z.dump"); ok {
		t.Error("a dump parsed as an export")
	}
}

func TestNameAndParseStampRoundTrip(t *testing.T) {
	at := time.Date(2026, 7, 29, 3, 4, 5, 0, time.UTC)
	for _, kind := range []string{KindDBDump, KindDocumentsArchive, KindExport} {
		name, err := Name(kind, at)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		got, ok := parseStamp(kind, name)
		if !ok {
			t.Fatalf("%s: %q did not parse back", kind, name)
		}
		if !got.Equal(at) {
			t.Errorf("%s: round-tripped %v, want %v", kind, got, at)
		}
	}

	// Kinds that produce no file must refuse to invent one rather than
	// returning a plausible name nothing will ever write.
	if _, err := Name(KindRestoreTest, at); err == nil {
		t.Error("Name() invented a filename for a kind that produces no artefact")
	}
}

// TestGuardScratchNameRefusesAnythingButAScratchDB pins the check that stands
// between a bug in scratch-name construction and DROP DATABASE against the live
// database.
func TestGuardScratchNameRefusesAnythingButAScratchDB(t *testing.T) {
	bad := []string{
		"ledgermancy",
		"postgres",
		"",
		"ledgermancy_restoretest_", // prefix alone, no suffix
		`ledgermancy_restoretest_"; DROP DATABASE ledgermancy; --`,
		"ledgermancy_restore_test_123", // near miss
	}
	for _, name := range bad {
		if err := guardScratchName(name); err == nil {
			t.Errorf("guardScratchName(%q) allowed a database it must refuse", name)
		}
	}
	if err := guardScratchName("ledgermancy_restoretest_1753800000000000000"); err != nil {
		t.Errorf("guardScratchName rejected a legitimate scratch name: %v", err)
	}
}
