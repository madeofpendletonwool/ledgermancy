package webhooks

import "time"

// MaxAttempts is how many HTTP requests a message gets before it is dead-
// lettered.
//
// Five, spread by RetryDelay over roughly eighty minutes. The shape matters more
// than the number: the failures worth retrying are a receiver restarting, a
// container coming back up, a home router dropping a packet — all of which
// resolve in minutes. A receiver that has been unreachable for an hour and a
// quarter is not having a blip, it is misconfigured or gone, and continuing to
// retry it forever would only bury the evidence under identical failures.
//
// The message is not lost when this runs out. It is marked `failed` with the
// last error, keeps every attempt behind it, and is visible in the inspector —
// which is the point of dead-lettering rather than dropping.
const MaxAttempts = 5

// retryBase and retryFactor shape the backoff; retryCap keeps the last step from
// running away.
const (
	retryBase   = time.Minute
	retryFactor = 4
	retryCap    = time.Hour
)

// RetryDelay is how long to wait before attempt+1, given that `attempt` (1-based)
// has just failed.
//
// Exponential — 1m, 4m, 16m, then capped at an hour — and deliberately WITHOUT
// jitter, unlike River's default policy. Jitter exists to stop a thundering herd
// of clients retrying in lockstep; the herd here is one household's events
// against one receiver, so the only thing randomness would buy is a backoff
// nobody can predict when they are watching the inspector waiting for the next
// try.
func RetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := retryBase
	for i := 1; i < attempt; i++ {
		delay *= retryFactor
		if delay >= retryCap {
			return retryCap
		}
	}
	return delay
}
