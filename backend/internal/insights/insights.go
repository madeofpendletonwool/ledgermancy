// Package insights is the spine of Ledgermancy's proactive feed: deterministic
// detectors ("producers") find facts in SQL, the engine stores them as feed
// rows, and later AI features plug in as additional producers rather than
// touching storage or delivery.
//
// The one rule: AI never computes. Every number a producer raises is finished
// in SQL / shopspring/decimal and stashed in Candidate.Data as a StringFixed(2)
// string; the model, when enabled, only rephrases the template text. Detection
// is always deterministic, so the feed is fully populated with no API key.
package insights

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
)

// Candidate is one insight a producer wants to raise. Title/Body are a plain
// template the engine may pass through AI for warmer phrasing; the numbers in
// Data are authoritative and never recomputed by the model.
type Candidate struct {
	Kind      string
	Priority  int
	Title     string         // template headline
	Body      string         // template narrative
	Data      map[string]any // deterministic facts; money as StringFixed(2)
	Period    *time.Time     // month/period the insight is about (nullable)
	DedupeKey string         // stable identity, e.g. 'spending_spike:dining:2026-07'
}

// Producer detects candidates for one household using deterministic SQL only.
// Later docs (subscription intelligence, forecast, alert explanation, …) add a
// Producer and register it in DefaultProducers; the engine needs no changes.
type Producer interface {
	Kind() string
	Detect(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]Candidate, error)
}

// Classifier is an optional capability a Producer may implement to enrich its
// candidates with AI *labels* — never numbers — during the engine's AI-gated
// phase. Detection in Detect stays deterministic and the numbers in Data stay
// authoritative; Classify only fills in soft fields (e.g. a subscription's
// category) and is best-effort, so the feed is unchanged without a key. The
// engine calls it, after Detect and only when AI is enabled, mutating the
// candidates' Data maps in place before phrasing and upsert.
type Classifier interface {
	Classify(ctx context.Context, client *ai.Client, candidates []Candidate) error
}

// Retractor is an optional capability a Producer may implement to declare that
// its insights are RE-DERIVABLE: the claim they make can stop being true, and
// when it does the feed row must be withdrawn rather than left standing.
//
// The engine only upserts, which means "no longer detected" and "never
// detected" are indistinguishable to storage. That is correct for most
// producers — large_transaction reports that a $2,400 charge happened on the
// 3rd, and no later event makes that false — but wrong for the bill matcher,
// where "we can't find a payment for this" is a statement about the present that
// a sync can falsify an hour later. Without retraction an overdue_bill raised at
// 6am outlives the payment landing at noon and sits in the feed until somebody
// dismisses it by hand.
//
// LiveKeys returns every dedupe key the producer still considers true, and the
// engine retracts every live insight of that Kind whose key is absent.
//
// It must return the producer's FULL detection, not the candidate slice Detect
// returned. Producers cap how many rows are worth a feed entry
// (overdueMaxCandidates and friends); an occurrence dropped by that cap is still
// true, and reporting only the survivors would retract it on the very next pass.
// Implementations share one detection helper with Detect so the two cannot
// drift — at the cost of running the detection query twice per pass, which is a
// cheap indexed read against a job that runs hourly.
type Retractor interface {
	LiveKeys(ctx context.Context, q *dbgen.Queries, householdID uuid.UUID, now time.Time) ([]string, error)
}

// DefaultProducers is the registry the generation job iterates. Keeping
// registration in one place means a new producer is a one-line append.
func DefaultProducers() []Producer {
	return []Producer{
		spendingSpikeProducer{},
		newRecurringProducer{},
		budgetPaceProducer{},
		lowLeftoverProducer{},
		subscriptionProducer{},
		forecastProducer{},
		alertExplanationProducer{},
		goalProducer{},
		// Expansion producers (expansion.go).
		monthEndProjectionProducer{},
		largeTransactionProducer{},
		incomeChangeProducer{},
		savingsMilestoneProducer{},
		// Budget-vs-actual trend (budgettrend.go).
		budgetTrendProducer{},
		// Bill calendar (upcomingbill.go).
		upcomingBillProducer{},
		// Overdue bills (reminders.go, MAD-85). The backward-looking companion
		// to upcoming_bill: where that says "due in N days", this says "was due
		// and we can't find a payment". Registered beside it so the two halves
		// of the bill reminder sit together.
		overdueBillProducer{},
		// Bills paid outside their stated amount range (billrange.go, MAD-120).
		// The third answer beside upcoming_bill ("it's coming") and overdue_bill
		// ("nothing paid it"): a payment landed, for the wrong amount. Registered
		// here because it shares the overdue matcher's candidate predicate —
		// exactly one of the two claims any given occurrence.
		billOutOfRangeProducer{},
		// Debt-payoff coaching (reminders.go, MAD-85). goalProducer above
		// deliberately skips kind='debt_payoff'; this producer owns it, coaching
		// a payoff goal whose current payment won't hit its target date.
		payoffProgressProducer{},
		// Document vault (documentexpiry.go, receiptmatch.go).
		documentExpiryProducer{},
		receiptMatchProducer{},
		// Anomaly detection (anomaly.go). merchantOutlierProducer and
		// largeTransactionProducer above are two halves of one behaviour: this
		// one claims any merchant with a usable baseline, that one covers
		// everything else. Neither is complete alone.
		merchantOutlierProducer{},
		duplicateChargeProducer{},
		// Asset revaluation nudges (assetstale.go). Bonds are excluded — they
		// revalue themselves from published rates every month, so a nudge would
		// be asking the user to confirm arithmetic the app already did.
		assetStaleProducer{},
		// Proactive cash-flow advisor (advisor.go). ONE insight per run,
		// carrying the whole ranked list — the ranking itself is entirely
		// deterministic and lives in internal/advisor. Registered last because
		// it is the only producer that proposes rather than reports.
		advisorProducer{},
	}
}
