package api

import (
	"testing"
	"time"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
)

// The AI request path is governed by constants that are NOT independent, and
// the bug this file exists to prevent is precisely the one where two of them
// were edited as if they were.
//
// A chat turn drives a model↔tool loop that makes up to maxToolIterations
// sequential calls, each bounded by ai.RequestTimeout. The iterations cannot be
// parallelised — iteration N+1 depends on iteration N's tool result — so the
// worst case the route has to survive is the SUM, not the max. Add
// aiToolBudget of our own work between model calls, and the route budget
// (aiRouteTimeout) has to cover the lot. If it ever does not, a multi-tool turn
// hits the route's context cancel mid-loop and the user gets a failed request
// instead of an answer — which is the original bug this whole change fixed.
//
// Editing any one of these numbers without re-checking the others is exactly how
// they drifted apart before. This test fails loudly in that case rather than
// silently reintroducing a budget the loop below it can exceed. The constants
// are documented together in server.go and internal/ai/client.go; this test is
// the machine-checked version of that comment.
func TestAIRouteTimeoutFitsToolLoop(t *testing.T) {
	t.Run("chat route budget covers the worst-case tool loop", func(t *testing.T) {
		modelTime := time.Duration(maxToolIterations) * ai.RequestTimeout
		need := modelTime + aiToolBudget
		if need > aiRouteTimeout {
			t.Fatalf(
				"AI route budget no longer fits the tool loop:\n"+
					"  maxToolIterations     = %d\n"+
					"  ai.RequestTimeout     = %s\n"+
					"  model time (iters×to) = %s\n"+
					"  aiToolBudget          = %s\n"+
					"  needed                = %s\n"+
					"  aiRouteTimeout        = %s (short by %s)\n"+
					"Raise aiRouteTimeout, or lower maxToolIterations / ai.RequestTimeout. "+
					"All three are documented together in server.go.",
				maxToolIterations, ai.RequestTimeout, modelTime, aiToolBudget,
				need, aiRouteTimeout, need-aiRouteTimeout,
			)
		}
	})

	t.Run("default route budget covers a single narration call", func(t *testing.T) {
		// narrationBudget decorates a deterministic read with one model call and
		// lives inside defaultRouteTimeout. A narration budget wider than the
		// route would take the computed answer down with the prose — the same
		// class of bug as the tool loop, one level shallower.
		if narrationBudget >= defaultRouteTimeout {
			t.Fatalf(
				"narration budget no longer fits its route:\n"+
					"  narrationBudget     = %s\n"+
					"  defaultRouteTimeout = %s\n"+
					"Lower narrationBudget or raise defaultRouteTimeout.",
				narrationBudget, defaultRouteTimeout,
			)
		}
	})

	t.Run("each model round-trip fits inside the route budget", func(t *testing.T) {
		// A weaker but useful sanity bound: even a single ai.RequestTimeout must
		// clear aiRouteTimeout, or the math above is meaningless and the loop
		// cannot complete even one iteration.
		if ai.RequestTimeout >= aiRouteTimeout {
			t.Fatalf(
				"a single model round-trip exceeds the route budget:\n"+
					"  ai.RequestTimeout = %s\n"+
					"  aiRouteTimeout    = %s\n",
				ai.RequestTimeout, aiRouteTimeout,
			)
		}
	})

	t.Run("http server write timeout does not undercut the chat route", func(t *testing.T) {
		// net/http's http.Server.WriteTimeout is enforced BELOW the chi router,
		// so a handler's context.WithTimeout can only shorten it, never widen
		// it. If WriteTimeout is positive and smaller than aiRouteTimeout, the
		// streaming chat route is torn down mid-stream no matter what the route
		// budget says — the response writer's context cancels, the in-flight
		// model stream read returns context.Canceled, and the browser is left
		// with a half-finished HTTP/2 stream it reports as
		// ERR_HTTP2_PROTOCOL_ERROR. That is the exact regression this constant
		// exists to prevent, so this test fails loudly if anyone re-introduces
		// a positive WriteTimeout below the route budget.
		//
		// Zero (disabled) is allowed and is the current value: the chat route
		// is SSE and a server-level write deadline is the wrong layer for it.
		// middleware.Timeout(aiRouteTimeout) on the route enforces the budget
		// instead, and cooperates with the stream rather than cutting it.
		if HTTPServerWriteTimeout < 0 {
			t.Fatalf("HTTPServerWriteTimeout is negative: %s", HTTPServerWriteTimeout)
		}
		if HTTPServerWriteTimeout == 0 {
			return
		}
		if HTTPServerWriteTimeout < aiRouteTimeout {
			t.Fatalf(
				"http.Server.WriteTimeout undercuts the chat route:\n"+
					"  HTTPServerWriteTimeout = %s\n"+
					"  aiRouteTimeout        = %s (short by %s)\n"+
					"Raise HTTPServerWriteTimeout to at least aiRouteTimeout, or set it to 0 to\n"+
					"disable it (the streaming chat route is bounded by middleware.Timeout\n"+
					"on the route, not by the server). See the comment on\n"+
					"HTTPServerWriteTimeout in server.go.",
				HTTPServerWriteTimeout, aiRouteTimeout, aiRouteTimeout-HTTPServerWriteTimeout,
			)
		}
	})
}
