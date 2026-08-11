package categorize

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// dec is already defined in the allocation package; this package has its own
// helpers because the categorize tests stand alone.
func pdec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return ts
}

// THE CASE THAT MOTIVATED THE FEATURE: a debit and a credit of equal magnitude
// on two different accounts, a day apart, with unrelated payee names. The
// structural match must link them regardless of what either side called it.
func TestMatchPairsCapitalOneCase(t *testing.T) {
	checking := uuid.New()
	savings := uuid.New()
	candidates := []PairCandidate{
		{ID: uuid.New(), AccountID: checking, Amount: pdec("700.00"), Date: mustTime(t, "2026-08-06")}, // ACH CAPITAL ONE - TRANSFER
		{ID: uuid.New(), AccountID: savings, Amount: pdec("-700.00"), Date: mustTime(t, "2026-08-05")}, // ALTRA FEDERAL CREDIT UNION
	}
	pairs := MatchPairs(candidates)
	if len(pairs) != 1 {
		t.Fatalf("pairs = %d, want 1", len(pairs))
	}
	if !pairs[0].Amount.Equal(pdec("700.00")) {
		t.Errorf("amount = %s, want 700", pairs[0].Amount)
	}
	if pairs[0].Out.AccountID != checking || pairs[0].In.AccountID != savings {
		t.Error("out/in legs assigned to the wrong accounts")
	}
}

// A same-account debit and credit is a refund or a fee pair, NOT a transfer
// between accounts, and must not pair.
func TestMatchPairsIgnoresSameAccount(t *testing.T) {
	acct := uuid.New()
	candidates := []PairCandidate{
		{ID: uuid.New(), AccountID: acct, Amount: pdec("50.00"), Date: mustTime(t, "2026-08-06")},
		{ID: uuid.New(), AccountID: acct, Amount: pdec("-50.00"), Date: mustTime(t, "2026-08-06")},
	}
	if got := MatchPairs(candidates); len(got) != 0 {
		t.Errorf("same-account legs paired: %d", len(got))
	}
}

// A magnitude mismatch must not pair. Allowing a tolerance is exactly what would
// let a $699.99 expense pair with a $700 deposit and hide the expense.
func TestMatchPairsRequiresExactMagnitude(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	candidates := []PairCandidate{
		{ID: uuid.New(), AccountID: a, Amount: pdec("700.00"), Date: mustTime(t, "2026-08-06")},
		{ID: uuid.New(), AccountID: b, Amount: pdec("-699.99"), Date: mustTime(t, "2026-08-06")},
	}
	if got := MatchPairs(candidates); len(got) != 0 {
		t.Errorf("mismatched magnitudes paired: %d", len(got))
	}
}

// Legs outside the date window are not the same transfer.
func TestMatchPairsRequiresDateWindow(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	candidates := []PairCandidate{
		{ID: uuid.New(), AccountID: a, Amount: pdec("700.00"), Date: mustTime(t, "2026-08-06")},
		{ID: uuid.New(), AccountID: b, Amount: pdec("-700.00"), Date: mustTime(t, "2026-08-20")}, // 14 days later
	}
	if got := MatchPairs(candidates); len(got) != 0 {
		t.Errorf("out-of-window legs paired: %d", len(got))
	}
}

// When two debits could match one credit, the CLOSEST-dated debit wins and the
// result is stable across input ordering — a re-run must produce the same pair.
func TestMatchPairsClosestDateWinsDeterministically(t *testing.T) {
	credit := uuid.New()
	a := uuid.New()
	b := uuid.New()
	far := PairCandidate{ID: uuid.New(), AccountID: a, Amount: pdec("700.00"), Date: mustTime(t, "2026-08-10")}
	near := PairCandidate{ID: uuid.New(), AccountID: b, Amount: pdec("700.00"), Date: mustTime(t, "2026-08-05")}
	c := PairCandidate{ID: uuid.New(), AccountID: credit, Amount: pdec("-700.00"), Date: mustTime(t, "2026-08-06")}

	for _, order := range [][]PairCandidate{{far, near, c}, {c, near, far}, {near, c, far}} {
		pairs := MatchPairs(order)
		if len(pairs) != 1 {
			t.Fatalf("pairs = %d, want 1 (one credit can only match one debit)", len(pairs))
		}
		if pairs[0].Out.ID != near.ID {
			t.Errorf("matched the far debit (%s) instead of the near one — not deterministic by closest date",
				pairs[0].Out.Date.Format("2006-01-02"))
		}
	}
}

// Two genuine same-sized transfers in the same window both pair, each credit to
// its own debit. This is the "two $700 transfers a week apart" reality of the
// motivating case, and a greedy matcher that consumed the first credit globally
// would get it wrong.
func TestMatchPairsMultipleTransfers(t *testing.T) {
	checking := uuid.New()
	savings := uuid.New()
	candidates := []PairCandidate{
		{ID: uuid.New(), AccountID: checking, Amount: pdec("700.00"), Date: mustTime(t, "2026-07-07")},
		{ID: uuid.New(), AccountID: savings, Amount: pdec("-700.00"), Date: mustTime(t, "2026-07-06")},
		{ID: uuid.New(), AccountID: checking, Amount: pdec("700.00"), Date: mustTime(t, "2026-08-06")},
		{ID: uuid.New(), AccountID: savings, Amount: pdec("-700.00"), Date: mustTime(t, "2026-08-05")},
	}
	if got := len(MatchPairs(candidates)); got != 2 {
		t.Errorf("pairs = %d, want 2", got)
	}
}
