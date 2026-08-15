package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/auth"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/db/dbgen"
	"github.com/madeofpendletonwool/ledgermancy/backend/internal/obligations"
)

// maxToolIterations bounds the model↔tool loop. The cap stops a misbehaving
// model from spinning up unbounded queries and cost.
//
// Raised from 6 to 8 by doc 31, and the reason is specific rather than a hunch:
// THE ADVISOR GENUINELY CHAINS MORE LOOKUPS THAN THE SPENDING ASSISTANT DID.
// "What should I do with $30k" plausibly needs advisor_briefing →
// safe_to_spend → debt_summary → contribution_room → the allocator → the
// likelihood check. At six that is the whole budget with no room for a
// correction; at eight there is slack for one wrong turn and a recovery, which
// is what the cap should leave and what six no longer did.
//
// Raised from 8 to 9 for the escape hatch. A find_tools call costs one whole
// iteration — the model asks, the loop answers, and only the NEXT round-trip
// can call what it loaded — so a turn that escalates had 7 left for work under
// the old cap, below the 8 the paragraph above argues for. Nine restores it.
// aiRouteTimeout moved with it; TestAIRouteTimeoutFitsToolLoop is what makes
// changing one of them without the other a build failure.
const maxToolIterations = 9

// chatMaxTokens is the output budget for ONE turn of the chat loop.
//
// It is 6000 rather than the package default of 1024 because THINKING TOKENS
// COME OUT OF THIS BUDGET, and the configured model is a reasoning model. The
// numbers are measured against this deployment (GLM-5.2 via api.z.ai), on a
// question of the kind this app is for — "how much liquid do we need to hold the
// emergency fund at all times":
//
//	max_tokens=3000  -> 3000 tokens of thinking, NO answer at all, max_tokens
//	max_tokens=10000 -> ~3900 thinking + ~550 answer = 4768 total, end_turn
//
// So the answer costs ~550 tokens and the reasoning in front of it costs seven
// times that. At 1024 the model could not reach a single word of answer, which
// is why the advisor appeared to die on that question: not an error, not a
// timeout — a complete response containing only scratch work.
//
// 6000 clears the measured 4768 with headroom, and continuation (below) covers
// an answer that still runs long. It cannot be raised freely: ai.RequestTimeout
// must cover it at the endpoint's throughput, and aiRouteTimeout must cover
// maxToolIterations of THAT. All three moved together here, and
// TestAIRouteTimeoutFitsToolLoop fails if a future change moves only one.
//
// Turning thinking off would be better than budgeting for it, and is not
// available: `"thinking":{"type":"disabled"}` is accepted and IGNORED by this
// endpoint — the block comes back regardless. Verified, not assumed.
const chatMaxTokens = 6000

// continuePrompt asks a model that was cut off at MaxTokens to finish.
//
// It insists on no repetition and no restart because both are what an
// un-steered continuation does, and both are worse than the truncation: the
// user is watching this stream in, so a restart rewrites an answer in front of
// them and a repeated paragraph reads as a stutter. It also forbids re-deriving
// the figures already written, for the same reason the system prompt does — a
// number that changes between halves of one answer destroys the whole thing,
// and the tool results are still in the transcript to be quoted from.
const continuePrompt = "Your previous message was cut off at the output limit. Continue it from exactly " +
	"where it stopped, mid-sentence if that is where it ended. Do not repeat any text you have already " +
	"written, do not start over, and do not add a preamble. Do not recompute or restate any figure you " +
	"already gave — quote the same tool results. If the answer was in fact complete, reply with nothing."

// maxChatMessages caps the transcript a client may send, so a runaway history
// cannot blow up the prompt.
const maxChatMessages = 40

// chatSystemPrompt keeps the assistant grounded: it must answer from tool
// results, never from guessed figures. This is the auditable-over-plausible
// stance the design calls for.
const chatSystemPrompt = `You are the assistant for Ledgermancy, a household finance app.
Answer questions about the household's own money using the provided tools.

Numbers:
- For any figure, category, budget, balance, count, or total, CALL A TOOL. Never invent or estimate numbers.
- NEVER do arithmetic yourself — do not add up a list of transactions, average them, or compute a difference. Every number you state must come verbatim from a tool result. Tools return exact counts and totals; quote those.
- For "how many times" or "how much did I spend on X", use spend_by_category (it returns a count and total per category) or list_transactions (an exact count and total for a category/merchant).
- For a breakdown or "list every…", call list_transactions and present its transactions. Its count and total are computed over ALL matches; the list may be truncated (see the "truncated" flag) — say so if it is, and still quote the full count/total.
- To filter by category, first learn the exact category names from spend_by_category, then pass one to list_transactions.
- For "vs last month", "trend", or "on average", use monthly_trend or category_averages. monthly_trend returns avg_income, avg_spending and avg_leftover over exactly the months it returned — quote those verbatim for any "average month" question (e.g. "what do we spend per month on average" is avg_spending); never average or sum the months yourself.
- query_transactions and breakdown are the flexible tools — reach for them for any detailed, unusual, or income question the narrow tools can't answer. They span income, spending, and transfers (set "flow"), filter by month or a start/end date range plus category, merchant, amount, or source, and breakdown groups by category, merchant, account, month, day, or Plaid category ("pfc").
- Income is NOT covered by the spending tools (spend_by_category, top_merchants, list_transactions all exclude it). To see where income came from, list individual paychecks/deposits, or compare income across months, use breakdown with flow:"income" (e.g. group_by:"merchant") or query_transactions with flow:"income".
- query_transactions and breakdown return totals split into total_in (money in) and total_out (money out), both positive and computed in SQL. Quote those directly — for income use total_in, for spending use total_out — and never sum the rows yourself.

Conventions:
- Amounts are US dollars; months are "YYYY-MM". If no month is given, assume the current month.
- Spending is money out; income and transfers are excluded from spending totals.
- You can only see this household's data. Do not claim to access anything else.

Style:
- Be concise and concrete. If a tool returns nothing, say so plainly.
- Format lists and comparisons as GitHub-flavored Markdown tables, and bold the key figure in a sentence. Your replies are rendered as Markdown.
- When you call a tool, do not narrate that you are doing so — only produce prose in your final answer.

Advice:
- The planning tools (safe_to_spend, project_balance, upcoming_obligations, debt_summary, debt_payoff, goal_status, goal_solve, retirement_projection, retirement_solve, investment_performance, asset_allocation, fees_summary, contribution_room) each wrap the app's own engines. Their figures are already final — quote them, never recompute them.
- advisor_briefing is the fastest way to answer any broad "how are we doing" question; call it first when the question is about the household's overall position. It carries the household's assumed_real_return, assumed_inflation and the APR hurdle (apr_hurdle + apr_hurdle_basis) — these are DIFFERENT numbers. When you state the household's assumed return, quote assumed_real_return verbatim and never substitute the hurdle floor; the two diverge whenever the assumed return is below 6%.
- advisor_briefing's "college" list is the household's college goals with the beneficiary's age, years to enrollment, percent funded and required monthly ALREADY resolved. Never ask whether college is on the horizon, or hedge with "if" — when that list is non-empty the goal exists, the horizon is a known number and so is the shortfall, so say them. annual_cost_today is ONE year in today's dollars and total_cost is all the years; never quote the first as the cost of college. A goal with projectable=false carries no funding figures at all — report its "note" and never describe it as 0% funded. college_projection returns these same numbers plus the per-year breakdown; call it for that, not to check them.
- advisor_briefing's emergency_fund carries TWO dollar targets. target_amount is the app's official bar — target_months of typical FIXED costs. target_amount_full_spending is the stricter bar — target_months of typical TOTAL spending. When the household asks how much liquid the emergency fund needs, give BOTH, labeled; and when they say full, total, or actual spending ("9 months of full average cost"), lead with target_amount_full_spending. Both are finished figures — quote them, never recompute them from the monthly fields.
- A RATE IS A PROPERTY OF AN ACCOUNT, NOT ONLY OF THE HOUSEHOLD. Each projectable college goal carries account_real_return_pct — the real return its own drawdown actually compounded at — and return_rate_source saying where that rate came from. Asked what return a specific goal, account or 529 is projected at, quote those two and never the top-level assumed_real_return, which is the household DEFAULT and applies only to accounts that have not been given their own rate. The two differ routinely and the difference is deliberate: a 529 on a seventeen-year horizon is not the household's conservative default. If the user says a rate is set higher than what you just quoted, they are probably reading the account's own setting — check account_real_return_pct before contradicting them, and never explain the gap by pointing at apr_hurdle or by claiming inflation was subtracted from a real return.
- When the user asks "what should I do with $X", CALL A TOOL rather than inventing options: the ranked options and the allocation plan are computed, not advised. Do not offer a course of action the tools did not produce.
- Never present a projection as a forecast. Retirement, payoff and balance projections carry a "basis" field saying what they assume and what they omit — repeat the relevant caveat rather than dropping it.
- A null figure means "not known", never zero. An unknown APR, an unreached FI age, and an uncomputable return are all real answers; say so plainly instead of substituting a number.
- You cannot move money. Never offer to make a transfer, a payment or a contribution — you describe and compute, the household acts.

Your tool list is a SUBSET, and find_tools is how you widen it:
- The tools you were given this turn were selected automatically from the wording of the question. They are NOT the full set of things this app can do, and a capability missing from your list usually still exists.
- Before you say a lookup is unavailable, that you lack a tool, or that you cannot produce a breakdown — CALL find_tools with a plain-language description of what you need. "I don't have a tool for that" is only ever true after find_tools has come back without one.
- Before you work a figure out yourself from numbers already in the conversation, CALL find_tools. A missing tool is never a licence to do the arithmetic.
- Never substitute a DIFFERENT figure for the one asked for and present it as the closest available answer. A median is not an average and a projection is not a total. Load the right tool, or say plainly which part you could not compute.
- If find_tools returns nothing that fits, say so plainly and stop. That is a real answer; an invented number is not.` + chatGuardrailPrompt

type chatRequestBody struct {
	Messages []chatMessage `json:"messages"`
	// ThreadID persists this turn to a saved conversation. Absent means today's
	// stateless behaviour, unchanged.
	ThreadID *uuid.UUID `json:"thread_id"`
	// Model selects which model answers this turn. Empty means the primary.
	// Only ids the operator listed in AI_ADDITIONAL_MODELS (or the primary
	// itself) are accepted — see validateChatModel.
	Model string `json:"model"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// validateChatModel resolves a request's model selection to the id the turn
// runs on, or an error the handler returns as a 400.
//
// The allowlist is the whole design: the Advisor chat is the only surface that
// may steer the model, and only across ids the operator explicitly opened up.
// Anything else — a typo, a stale pick from a config that changed, an id
// probed by hand — is rejected here rather than passed to the endpoint, so a
// misconfigured deployment answers "no" at the front door instead of paying
// for a request the operator never sanctioned.
func (s *Server) validateChatModel(model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" || model == s.Config.AI.Model {
		return "", nil
	}
	for _, m := range s.Config.AI.AdditionalModels {
		if model == m {
			return model, nil
		}
	}
	return "", fmt.Errorf("model %q is not available; ask your operator to add it to AI_ADDITIONAL_MODELS", model)
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustFromContext(r.Context())

	if !s.AI.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "AI features are not configured")
		return
	}

	var req chatRequestBody
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "at least one message is required")
		return
	}
	if len(req.Messages) > maxChatMessages {
		writeError(w, http.StatusBadRequest, "conversation is too long")
		return
	}
	model, err := s.validateChatModel(req.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// A thread id is validated BEFORE the stream opens, so a bad one is an
	// ordinary 404 rather than an error frame the client has to unpick. It is
	// also the scope check: a thread from another household, or a spouse's
	// private thread, does not resolve here and the turn is never persisted to
	// it.
	var thread *dbgen.AdvisorThread
	if req.ThreadID != nil {
		row, err := s.Queries.GetAdvisorThread(r.Context(), dbgen.GetAdvisorThreadParams{
			ID: *req.ThreadID, HouseholdID: identity.HouseholdID, UserID: &identity.UserID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "conversation not found")
			return
		} else if err != nil {
			s.internalError(w, "load advisor thread", err)
			return
		}
		thread = &row
	}

	messages := make([]ai.Message, 0, len(req.Messages)+1)
	// FIGURES IN HISTORY ARE CONTEXT, NEVER CURRENT.
	//
	// A reloaded thread is full of money the model itself stated weeks ago. Left
	// unqualified it will re-read its own six-week-old "$4,120 safe to spend" as
	// though it were sourced and current. One system line naming the thread's
	// last activity is far cheaper and far more robust than trying to redact the
	// transcript.
	if thread != nil && len(req.Messages) > 1 {
		messages = append(messages, ai.Message{
			Role: ai.RoleUser,
			Content: []ai.Block{ai.TextBlock(
				"Context: this is a saved conversation last active on " +
					thread.UpdatedAt.UTC().Format("2 January 2006") +
					". Any figure appearing earlier in it is AS OF THAT DATE and may be stale. " +
					"Re-fetch with a tool before restating any of them.")},
		})
	}
	for _, m := range req.Messages {
		role := ai.RoleUser
		if m.Role == "assistant" {
			role = ai.RoleAssistant
		}
		messages = append(messages, ai.Message{Role: role, Content: []ai.Block{ai.TextBlock(m.Content)}})
	}

	// The tool set is chosen HERE, deterministically, from the transcript —
	// never by the model. See chat_toolsets.go. A turn inherits the thread's
	// established set when the last message is an ambiguous follow-up, so a
	// "now recalculate the 529" reply keeps college_projection instead of
	// dropping to the spending tools and losing the only engine that computes
	// the figure.
	question := lastUserMessage(req.Messages)
	set := classifyFromMessages(req.Messages)

	// Everything below streams over Server-Sent Events: one `{"delta":...}`
	// frame per chunk of answer, a `{"tool":…,"result":…}` frame per tool
	// result, a terminal `{"done":true}`, or `{"error":...}` if the turn fails.
	// Validation above still returns a normal JSON error, because nothing has
	// been written yet.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Tell any nginx in front of us not to buffer this response, so tokens reach
	// the browser as they are written rather than in one batch at the end.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	sendSSE := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	onDelta := func(delta string) { sendSSE(map[string]string{"delta": delta}) }

	// The chosen set is announced so a wrong pick is VISIBLE rather than
	// mysterious. A user who asks a debt question and gets a spending answer can
	// see which set was sent; without this the only symptom is a worse answer.
	sendSSE(map[string]string{"tool_set": set})

	// Tool results are forwarded to the client as they land, so a chart can be
	// rendered from the SAME value the model was handed. This is the whole
	// mechanism behind inline charts: the client maps tool name → chart
	// component deterministically, and the model never picks a chart, shapes
	// data, or labels an axis.
	trace := []chatToolTrace{}
	onTool := func(name string, result string) {
		trace = append(trace, chatToolTrace{Tool: name, Result: json.RawMessage(result)})
		sendSSE(map[string]any{"tool": name, "result": json.RawMessage(result)})
	}

	// An escalation is announced for the same reason the set is: it makes the
	// classifier's mistakes MEASURABLE. A set that is escalated out of on most
	// turns is a membership bug with a visible symptom, where before the only
	// symptom was an answer that quietly declined to compute something.
	onGrant := func(names []string) {
		sendSSE(map[string]any{"tools_added": names})
	}

	answer, err := s.runChat(r.Context(), identity, set, model, messages, onDelta, onTool, onGrant)
	if err != nil {
		slog.Error("chat", "error", err)
		sendSSE(map[string]string{"error": "Something went wrong answering that."})
		return
	}

	// Persistence happens AFTER the stream completes, never mid-stream: a turn
	// half-written to the database because the client hung up is worse than one
	// not written at all.
	if thread != nil {
		s.persistTurn(r.Context(), thread.ID, identity.HouseholdID, question, answer, trace)
	}

	sendSSE(map[string]bool{"done": true})
}

// chatToolTrace is one tool call's result, kept so an assistant turn can be
// persisted WITH the provenance of every figure in it.
//
// Storing only role and prose honours the "every number comes from a tool"
// rule for a fresh turn and quietly breaks it on reload. See the migration's
// note on advisor_messages.tool_trace.
type chatToolTrace struct {
	Tool   string          `json:"tool"`
	Result json.RawMessage `json:"result"`
}

// decodeToolTrace reads a persisted trace back out of its opened plaintext.
func decodeToolTrace(raw []byte, out *[]chatToolTrace) error {
	return json.Unmarshal(raw, out)
}

// persistTurn writes the user and assistant halves of one completed turn.
//
// Best-effort and never fatal: the answer has already been streamed, so failing
// the request now would tell the user their working answer was an error. A
// failed write is logged and the conversation simply does not gain the turn.
func (s *Server) persistTurn(
	ctx context.Context, threadID, householdID uuid.UUID,
	question, answer string, trace []chatToolTrace,
) {
	save := func(role, content string, tr []chatToolTrace) {
		if content == "" {
			return
		}
		sealed, err := s.Cipher.SealString(content)
		if err != nil {
			slog.Error("seal advisor message", "error", err, "thread_id", threadID)
			return
		}
		var sealedTrace []byte
		if len(tr) > 0 {
			raw, err := json.Marshal(tr)
			if err == nil {
				sealedTrace, err = s.Cipher.Seal(raw)
			}
			if err != nil {
				slog.Error("seal advisor tool trace", "error", err, "thread_id", threadID)
				return
			}
		}
		if _, err := s.Queries.InsertAdvisorMessage(ctx, dbgen.InsertAdvisorMessageParams{
			ThreadID: threadID, Role: role, Content: sealed, ToolTrace: sealedTrace,
		}); err != nil {
			slog.Error("persist advisor message", "error", err, "thread_id", threadID)
		}
	}

	save("user", question, nil)
	save("assistant", answer, trace)

	if err := s.Queries.TouchAdvisorThread(ctx, dbgen.TouchAdvisorThreadParams{
		ID: threadID, HouseholdID: householdID,
	}); err != nil {
		slog.Error("touch advisor thread", "error", err, "thread_id", threadID)
	}
}

// runChat drives the tool-calling loop: the model may ask to run scoped queries,
// whose results are fed back until it produces a final text answer. The final
// answer's text is streamed to onText as it is generated; the full text is also
// returned. Tool-calling turns produce no user-visible text (the system prompt
// forbids it), so nothing leaks between lookups.
// onTool, when non-nil, receives every tool result as it is produced — the SAME
// JSON string that becomes the model's ToolResultBlock. Forwarding it is what
// lets the client render a deterministic chart from a turn's own data; it is
// never a second computation.
//
// model is the Advisor chat's per-turn model selection: empty runs on the
// primary, anything else is one the route already validated against
// AI_ADDITIONAL_MODELS.
func (s *Server) runChat(
	ctx context.Context, identity auth.Identity, set string, model string,
	messages []ai.Message, onText func(string), onTool func(name, result string),
	onGrant func(names []string),
) (string, error) {
	// The chosen set is the turn's STARTING tool list, not its final one. A
	// find_tools call appends to this slice mid-loop, so the next iteration is
	// sent the definitions the model asked for — see chat_tool_escalation.go for
	// why a set had to stop being a one-way door.
	tools := toolSetDefs(set)
	available := make(map[string]bool, len(tools))
	for _, t := range tools {
		available[t.Name] = true
	}
	granted := 0

	// Text already streamed to the user across CONTINUATION turns.
	//
	// A model that hits MaxTokens mid-answer has not failed and has not
	// finished: it has written a prefix. The loop below asks it to carry on, so
	// the final answer is assembled from several turns and this is where the
	// earlier pieces live. It is not a buffer for the streaming path — that has
	// already sent these bytes — it is what the handler saves to the thread, and
	// dropping it would persist only the last fragment of an answer the user
	// watched arrive in full.
	var answer strings.Builder

	// The model has no clock of its own, so it cannot resolve "July" or "last
	// month" without being told today's date. Inject it into the system prompt.
	system := chatSystemPrompt + "\n\nToday's date is " +
		time.Now().Format("Monday, 2 January 2006") +
		". Use it to resolve months like \"July\" (the most recent one) and phrases like \"last month\"."

	for i := 0; i < maxToolIterations; i++ {
		resp, streamed, err := s.chatComplete(ctx, ai.Request{
			System:    system,
			Messages:  messages,
			Tools:     tools,
			MaxTokens: chatMaxTokens,
			Model:     model,
		}, onText)
		if err != nil {
			return "", err
		}

		uses := resp.ToolUses()
		if len(uses) == 0 {
			text := resp.Text()
			// The streaming path already forwarded this text token by token;
			// the fallback path did not, so emit it once here.
			if !streamed && onText != nil {
				onText(text)
			}
			answer.WriteString(text)

			// A completion that stopped at the token ceiling is a PREFIX, not an
			// answer, and nothing downstream can tell the difference: the text
			// is populated and the error is nil, so this used to return a
			// sentence that stopped mid-word as though the model had chosen to
			// end there. That is how "how much liquid do we need to hold the
			// emergency fund at all times" came back dead — three tool calls,
			// a long answer cut at 1024 tokens, and HTTP 200.
			//
			// So ask for the rest. Continuing costs one more iteration out of a
			// budget that already exists, and it keeps every per-call invariant
			// intact — MaxTokens, ai.RequestTimeout and the route budget that is
			// sized from their product (server.go, TestAIRouteTimeoutFitsToolLoop)
			// are all per REQUEST. Raising MaxTokens instead would break that
			// sizing: at the ~22 tokens/second this deployment actually gets,
			// 1024 tokens already takes 47 of the 60 seconds one request is
			// allowed, so a bigger ceiling buys a timeout rather than a longer
			// answer.
			// Truncated with NOTHING written is not a continuation case: there is
			// no prefix to carry on from, and appending the turn anyway sends
			// `content: null` (thinking is not echoed) which the endpoint rejects
			// with a 422 naming only the message index. That is what turned this
			// bug from a blank answer into a failed request.
			//
			// It happens when a reasoning model spends its whole output budget
			// thinking. The honest thing is to say so: the budget below is sized
			// so this is rare, and when it does happen the user needs to know the
			// model ran out of room rather than that their question had no answer.
			if resp.Truncated() && resp.OnlyThinking() {
				slog.Warn("model exhausted its output budget before writing an answer",
					"max_tokens", chatMaxTokens, "iteration", i)
				break
			}
			if resp.Truncated() {
				messages = append(messages, resp.AsMessage(), ai.UserText(continuePrompt))
				continue
			}
			return answer.String(), nil
		}

		// Echo the assistant's tool_use turn back, then answer each call.
		messages = append(messages, resp.AsMessage())
		results := make([]ai.Block, 0, len(uses))
		for _, use := range uses {
			// find_tools is answered HERE rather than in executeChatTool, because
			// it is the one tool whose effect is on the request itself: it widens
			// the tool list the next iteration is sent. Handling it beside the
			// slice it mutates is what keeps the grant and the definitions the
			// model actually receives from drifting apart.
			if use.Name == toolFinderName {
				out, added, err := grantTools(use.Input, available, granted)
				if err != nil {
					results = append(results, ai.ToolResultBlock(use.ID, err.Error(), true))
					continue
				}
				for _, t := range added {
					tools = append(tools, t)
					available[t.Name] = true
				}
				granted += len(added)
				results = append(results, ai.ToolResultBlock(use.ID, out, false))
				// Not forwarded to onTool: the trace is the PROVENANCE of the
				// figures in an answer, and a tool lookup produced none of them.
				// It gets its own frame instead, so an escalation is visible
				// without being mistaken for a source.
				if onGrant != nil && len(added) > 0 {
					names := make([]string, 0, len(added))
					for _, t := range added {
						names = append(names, t.Name)
					}
					onGrant(names)
				}
				continue
			}

			out, err := s.executeChatTool(ctx, identity, use.Name, use.Input)
			if err != nil {
				// Hand the model the error so it can recover or apologise,
				// rather than failing the whole request.
				results = append(results, ai.ToolResultBlock(use.ID, err.Error(), true))
				continue
			}
			results = append(results, ai.ToolResultBlock(use.ID, out, false))
			if onTool != nil {
				onTool(use.Name, out)
			}
		}
		messages = append(messages, ai.Message{Role: ai.RoleUser, Content: results})
	}

	// Ran out of iterations without a final answer.
	//
	// If the model was mid-ANSWER when the budget ran out, the user has already
	// watched real text arrive and replacing it with the generic apology below
	// would be both a lie and a loss. Keep what was written and say plainly that
	// it stops early: an answer the reader knows is unfinished is usable, and an
	// unfinished one they believe is complete is the thing to never ship.
	if answer.Len() > 0 {
		const note = "\n\n---\n\n**This answer is cut off — it ran out of room before finishing.** " +
			"Everything above is computed from your data and stands as written. " +
			"Ask for the rest, or ask for one part of it, and I can finish."
		if onText != nil {
			onText(note)
		}
		answer.WriteString(note)
		return answer.String(), nil
	}

	msg := "I wasn't able to work that out — try asking in a simpler way."
	if onText != nil {
		onText(msg)
	}
	return msg, nil
}

// chatComplete runs one model turn, streaming assistant text to onText. It
// prefers the streaming endpoint but falls back to a single non-streaming call
// if streaming fails before any text was emitted (e.g. an endpoint that does
// not support SSE) — so the assistant keeps working regardless. The returned
// bool reports whether the text was streamed, so the caller knows whether it
// still needs to emit the final answer itself.
func (s *Server) chatComplete(ctx context.Context, req ai.Request, onText func(string)) (*ai.Response, bool, error) {
	emitted := false
	resp, err := s.AI.CompleteStream(ctx, req, func(delta string) {
		emitted = true
		if onText != nil {
			onText(delta)
		}
	})
	if err == nil {
		return resp, true, nil
	}
	// If text was already streamed, falling back would duplicate it — surface
	// the error instead.
	if emitted {
		return nil, false, err
	}
	slog.Warn("ai stream failed; falling back to non-streaming", "error", err)
	resp, err = s.AI.Complete(ctx, req)
	return resp, false, err
}

// chatBaseToolDefs are the original spending tools. Each maps to an existing
// reporting query; execution scopes every one to the caller.
//
// Nothing sends this whole list plus the advisor's — see chat_toolsets.go for
// why, and toolSetDefs for how a request's set is assembled.
func chatBaseToolDefs() []ai.Tool {
	monthSchema := json.RawMessage(`{"type":"object","properties":{"month":{"type":"string","description":"Month as YYYY-MM; omit for the current month"}}}`)
	return []ai.Tool{
		{
			Name:        "spending_summary",
			Description: "Income, total spending, fixed vs discretionary, and leftover for a month.",
			InputSchema: monthSchema,
		},
		{
			Name:        "spend_by_category",
			Description: "Spending by category for a month, largest first. Each category includes the number of transactions (count) — use it to answer \"how many times\" questions.",
			InputSchema: monthSchema,
		},
		{
			Name:        "top_merchants",
			Description: "The merchants you spent the most at in a month, with the number of transactions at each.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"month":{"type":"string"},"limit":{"type":"integer","description":"How many, 1-20"}}}`),
		},
		{
			Name:        "list_transactions",
			Description: "Individual transactions for a month, optionally filtered to a category and/or merchant. Returns an exact count and total plus the matching transactions — use this for breakdowns and \"list every…\" questions. The count and total are computed over ALL matches; the transactions list may be capped by limit.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"month":{"type":"string","description":"Month as YYYY-MM; omit for the current month"},"category":{"type":"string","description":"Category name or slug to filter by, e.g. \"Food & Drink\". Learn exact names from spend_by_category first."},"merchant":{"type":"string","description":"Merchant name substring to filter by"},"limit":{"type":"integer","description":"Max transactions to list, 1-100 (default 50)"}}}`),
		},
		{
			Name:        "query_transactions",
			Description: "The flexible transaction lister — covers INCOME, spending, and transfers (unlike list_transactions, which is spending only). Lists individual transactions matching any combination of filters, and returns an exact count plus totals split into total_in (money in) and total_out (money out) over ALL matches; the listed rows may be capped by limit. Use flow:\"income\" to list paychecks/deposits. Each row has amount (a positive magnitude), direction (\"in\"/\"out\"), category, account, and source.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"flow":{"type":"string","enum":["spending","income","transfers","all"],"description":"Which money to include: spending (money out, the default), income, transfers, or all"},"month":{"type":"string","description":"Month as YYYY-MM. Omit for the current month."},"start":{"type":"string","description":"Range start date YYYY-MM-DD (use start+end instead of month for a custom range)"},"end":{"type":"string","description":"Range end date YYYY-MM-DD"},"category":{"type":"string","description":"Category name or slug to filter by"},"merchant":{"type":"string","description":"Merchant/payee name substring to filter by"},"source":{"type":"string","enum":["plaid","csv","manual"],"description":"Only transactions imported from this source"},"min_amount":{"type":"number","description":"Only transactions at least this large (absolute dollars)"},"max_amount":{"type":"number","description":"Only transactions at most this large (absolute dollars)"},"limit":{"type":"integer","description":"Max transactions to list, 1-100 (default 50)"}}}`),
		},
		{
			Name:        "breakdown",
			Description: "The flexible aggregator — group transactions along one dimension and get per-group totals. Covers income, spending, and transfers (set flow). Answers \"where did my income come from\" (flow:\"income\", group_by:\"merchant\"), \"spending by category\", \"income by month\", etc. Returns per group: count, total_in (money in) and total_out (money out), largest first, all computed in SQL.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"group_by":{"type":"string","enum":["category","merchant","account","month","day","pfc"],"description":"Dimension to group by (pfc = Plaid's detailed category). Required."},"flow":{"type":"string","enum":["spending","income","transfers","all"],"description":"Which money to include (default spending). Use \"income\" to see where income came from."},"month":{"type":"string","description":"A single month YYYY-MM"},"months":{"type":"integer","description":"Trailing N months, 1-24. Pair with group_by:\"month\" for a trend."},"start":{"type":"string","description":"Range start date YYYY-MM-DD"},"end":{"type":"string","description":"Range end date YYYY-MM-DD"},"category":{"type":"string","description":"Category name or slug to filter by"},"merchant":{"type":"string","description":"Merchant/payee name substring to filter by"},"source":{"type":"string","enum":["plaid","csv","manual"],"description":"Only transactions imported from this source"},"min_amount":{"type":"number","description":"Only transactions at least this large (absolute dollars)"},"max_amount":{"type":"number","description":"Only transactions at most this large (absolute dollars)"}},"required":["group_by"]}`),
		},
		{
			Name: "monthly_trend",
			Description: "Income, spending, and leftover per calendar month, oldest first, in \"months\". Also returns " +
				"\"avg_income\", \"avg_spending\" and \"avg_leftover\" — the exact means across the months ACTUALLY " +
				"RETURNED — quote those figures verbatim for \"average monthly spend/income/leftover\" questions " +
				"rather than averaging the months yourself. Defaults to the last 12 " +
				"months. Give start+end (YYYY-MM-DD) for a custom window, e.g. a past year that excludes the current " +
				"partial month; the window is snapped to whole months. Use exclude ([\"YYYY-MM\", ...]) to drop one-time " +
				"anomalies like a loan payoff or a month whose income has not arrived yet — excluded months leave both the " +
				"series and every average, so the averages always match the months the user sees.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"months":{"type":"integer","description":"How many recent months, 1-24 (default 12). Ignored when start+end are given."},"start":{"type":"string","description":"Range start as YYYY-MM-DD. Give both start and end for a custom window instead of trailing months; the window is snapped to whole months."},"end":{"type":"string","description":"Range end as YYYY-MM-DD, paired with start."},"exclude":{"type":"array","items":{"type":"string"},"description":"Months as YYYY-MM to drop from the series AND the averages, e.g. [\"2026-07\"] to omit a one-time anomaly."}}}`),
		},
		{
			Name:        "category_averages",
			Description: "Average monthly spend per category over the last N months (default 12). Use for \"typical\" or \"on average\" questions.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"months":{"type":"integer","description":"How many recent months, 1-24 (default 12)"}}}`),
		},
		{
			Name:        "spending_by_day",
			Description: "Total spending for each day of a month (days with spending only).",
			InputSchema: monthSchema,
		},
		{
			Name:        "budget_status",
			Description: "Each budget for a month with how much has been spent against it.",
			InputSchema: monthSchema,
		},
		{
			Name:        "net_worth",
			Description: "The household's current net worth: cash, investments, assets and debts.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		{
			Name:        "recurring_charges",
			Description: "Detected subscriptions and regular bills with their monthly cost.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
	}
}

// executeChatTool runs one tool and returns its result as a JSON string. Every
// query is scoped to the caller's household and visibility — a tool that
// forgot the scope would leak a spouse's private accounts.
func (s *Server) executeChatTool(ctx context.Context, identity auth.Identity, name string, input json.RawMessage) (string, error) {
	// The advisor's engines dispatch first and report whether they owned the
	// name, so this switch stays the spending assistant's and doc 31's tools
	// live beside the engines they wrap.
	if out, owned, err := s.executeAllocationTool(ctx, identity, name, input); owned {
		return out, err
	}
	if out, owned, err := s.executeLikelihoodTool(ctx, identity, name, input); owned {
		return out, err
	}
	if out, owned, err := s.executeAdvisorTool(ctx, identity, name, input); owned {
		return out, err
	}

	switch name {
	case "spending_summary":
		from, to, err := toolMonth(input)
		if err != nil {
			return "", err
		}
		row, err := s.Queries.GetSpendingSummary(ctx, dbgen.GetSpendingSummaryParams{
			HouseholdID: identity.HouseholdID, UserID: identity.UserID, Date: from, Date_2: to,
		})
		if err != nil {
			return "", err
		}
		return marshalTool(map[string]string{
			"income":                 row.Income.StringFixed(2),
			"spending":               row.Spending.StringFixed(2),
			"fixed_spending":         row.FixedSpending.StringFixed(2),
			"discretionary_spending": row.DiscretionarySpending.StringFixed(2),
			"leftover":               row.Income.Sub(row.Spending).StringFixed(2),
		})

	case "spend_by_category":
		from, to, err := toolMonth(input)
		if err != nil {
			return "", err
		}
		rows, err := s.Queries.GetSpendingByCategory(ctx, dbgen.GetSpendingByCategoryParams{
			HouseholdID: identity.HouseholdID, UserID: identity.UserID, Date: from, Date_2: to,
		})
		if err != nil {
			return "", err
		}
		out := make([]map[string]any, 0, len(rows))
		for _, c := range rows {
			out = append(out, map[string]any{
				"category": c.CategoryName,
				"spent":    c.Total.StringFixed(2),
				"count":    c.TransactionCount,
			})
		}
		return marshalTool(out)

	case "top_merchants":
		var in struct {
			Month string `json:"month"`
			Limit int    `json:"limit"`
		}
		if err := decodeToolInput(input, &in); err != nil {
			return "", err
		}
		from, to, err := monthRange(in.Month)
		if err != nil {
			return "", err
		}
		limit := in.Limit
		if limit < 1 || limit > 20 {
			limit = 10
		}
		rows, err := s.Queries.GetTopMerchants(ctx, dbgen.GetTopMerchantsParams{
			HouseholdID: identity.HouseholdID, UserID: identity.UserID,
			Date: from, Date_2: to, Limit: int32(limit),
		})
		if err != nil {
			return "", err
		}
		out := make([]map[string]any, 0, len(rows))
		for _, m := range rows {
			out = append(out, map[string]any{
				"merchant": m.Merchant,
				"spent":    m.Total.StringFixed(2),
				"count":    m.TransactionCount,
			})
		}
		return marshalTool(out)

	case "budget_status":
		from, to, err := toolMonth(input)
		if err != nil {
			return "", err
		}
		rows, err := s.Queries.GetBudgetProgress(ctx, dbgen.GetBudgetProgressParams{
			HouseholdID: identity.HouseholdID, UserID: identity.UserID,
			WindowStart: from, WindowEnd: to, Ref: time.Now().UTC(),
		})
		if err != nil {
			return "", err
		}
		out := make([]map[string]string, 0, len(rows))
		for _, b := range rows {
			out = append(out, map[string]string{
				"category":  b.CategoryName,
				"budgeted":  b.Budgeted.StringFixed(2),
				"spent":     b.Spent.StringFixed(2),
				"remaining": b.Budgeted.Sub(b.Spent).StringFixed(2),
			})
		}
		return marshalTool(out)

	case "net_worth":
		row, err := s.Queries.ComputeNetWorth(ctx, identity.HouseholdID)
		if err != nil {
			return "", err
		}
		assets := row.Cash.Add(row.Investments).Add(row.OtherAssets).Add(row.ManualAssets)
		debts := row.CreditDebt.Add(row.LoanDebt).Add(row.ManualDebt)
		return marshalTool(map[string]string{
			"cash":         row.Cash.StringFixed(2),
			"investments":  row.Investments.StringFixed(2),
			"other_assets": row.OtherAssets.Add(row.ManualAssets).StringFixed(2),
			"debts":        debts.StringFixed(2),
			"net_worth":    assets.Sub(debts).StringFixed(2),
		})

	case "recurring_charges":
		now := time.Now()
		since := now.AddDate(0, -obligations.RecurringLookbackMonths, 0)
		rows, err := s.Queries.GetRecurringMerchants(ctx, dbgen.GetRecurringMerchantsParams{
			HouseholdID: identity.HouseholdID, UserID: identity.UserID,
			Date: since, Column4: now,
		})
		if err != nil {
			return "", err
		}
		out := make([]map[string]string, 0, len(rows))
		for _, m := range rows {
			var monthly decimal.Decimal
			if m.AvgGapDays.IsPositive() {
				monthly = m.TypicalAmount.Mul(daysPerMonth).Div(m.AvgGapDays).Round(2)
			}
			out = append(out, map[string]string{
				"merchant":         m.Merchant,
				"cadence":          obligations.CadenceLabel(m.AvgGapDays),
				"typical_amount":   m.TypicalAmount.StringFixed(2),
				"monthly_estimate": monthly.StringFixed(2),
			})
		}
		return marshalTool(out)

	case "list_transactions":
		var in struct {
			Month    string `json:"month"`
			Category string `json:"category"`
			Merchant string `json:"merchant"`
			Limit    int    `json:"limit"`
		}
		if err := decodeToolInput(input, &in); err != nil {
			return "", err
		}
		from, to, err := monthRange(in.Month)
		if err != nil {
			return "", err
		}
		limit := in.Limit
		if limit < 1 || limit > 100 {
			limit = 50
		}
		var category, merchant *string
		if v := strings.TrimSpace(in.Category); v != "" {
			category = &v
		}
		if v := strings.TrimSpace(in.Merchant); v != "" {
			merchant = &v
		}

		// The sum is exact over every match; the list is capped by limit. Both
		// share the same filters, so the count here reconciles with the count
		// spend_by_category reports for the same category and month.
		sum, err := s.Queries.SumFilteredTransactions(ctx, dbgen.SumFilteredTransactionsParams{
			HouseholdID: identity.HouseholdID, UserID: identity.UserID,
			Date: from, Date_2: to, Category: category, Merchant: merchant,
		})
		if err != nil {
			return "", err
		}
		rows, err := s.Queries.ListFilteredTransactions(ctx, dbgen.ListFilteredTransactionsParams{
			HouseholdID: identity.HouseholdID, UserID: identity.UserID,
			Date: from, Date_2: to, Category: category, Merchant: merchant, Lim: int32(limit),
		})
		if err != nil {
			return "", err
		}
		txns := make([]map[string]string, 0, len(rows))
		matched := map[string]struct{}{}
		for _, r := range rows {
			txns = append(txns, map[string]string{
				"date":     r.Date.Format("2006-01-02"),
				"merchant": r.Merchant,
				"amount":   r.Amount.StringFixed(2),
				"category": r.CategoryName,
			})
			matched[r.CategoryName] = struct{}{}
		}
		result := map[string]any{
			"count":        sum.TransactionCount,
			"total":        sum.Total.StringFixed(2),
			"listed":       len(txns),
			"truncated":    int64(len(txns)) < sum.TransactionCount,
			"transactions": txns,
		}
		if category != nil {
			names := make([]string, 0, len(matched))
			for n := range matched {
				names = append(names, n)
			}
			result["matched_categories"] = names
		}
		return marshalTool(result)

	case "query_transactions":
		var in struct {
			Flow      string      `json:"flow"`
			Month     string      `json:"month"`
			Start     string      `json:"start"`
			End       string      `json:"end"`
			Category  string      `json:"category"`
			Merchant  string      `json:"merchant"`
			Source    string      `json:"source"`
			MinAmount json.Number `json:"min_amount"`
			MaxAmount json.Number `json:"max_amount"`
			Limit     int         `json:"limit"`
		}
		if err := decodeToolInput(input, &in); err != nil {
			return "", err
		}
		flow, err := normalizeFlow(in.Flow)
		if err != nil {
			return "", err
		}
		from, to, err := toolDateRange(in.Month, in.Start, in.End, 0)
		if err != nil {
			return "", err
		}
		minAmt, err := toolDecimal(in.MinAmount)
		if err != nil {
			return "", err
		}
		maxAmt, err := toolDecimal(in.MaxAmount)
		if err != nil {
			return "", err
		}
		limit := in.Limit
		if limit < 1 || limit > 100 {
			limit = 50
		}

		// The sum is exact over every match; the list is capped by limit. Both
		// share the same filters, so their figures reconcile.
		sum, err := s.Queries.SumQueriedTransactions(ctx, dbgen.SumQueriedTransactionsParams{
			HouseholdID: identity.HouseholdID, UserID: identity.UserID,
			Date: from, Date_2: to, Flow: flow,
			Category: optStr(in.Category), Merchant: optStr(in.Merchant), Source: optStr(in.Source),
			MinAmount: minAmt, MaxAmount: maxAmt,
		})
		if err != nil {
			return "", err
		}
		rows, err := s.Queries.ListQueriedTransactions(ctx, dbgen.ListQueriedTransactionsParams{
			HouseholdID: identity.HouseholdID, UserID: identity.UserID,
			Date: from, Date_2: to, Flow: flow,
			Category: optStr(in.Category), Merchant: optStr(in.Merchant), Source: optStr(in.Source),
			MinAmount: minAmt, MaxAmount: maxAmt, Lim: int32(limit),
		})
		if err != nil {
			return "", err
		}
		txns := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			t := map[string]any{
				"date":      r.Date.Format("2006-01-02"),
				"merchant":  r.Merchant,
				"amount":    r.Amount.StringFixed(2),
				"direction": r.Direction,
				"category":  r.CategoryName,
				"account":   r.AccountName,
				"source":    r.Source,
			}
			if r.IsIncome {
				t["is_income"] = true
			}
			if r.IsTransfer {
				t["is_transfer"] = true
			}
			if r.PlaidPfcDetailed != nil && *r.PlaidPfcDetailed != "" {
				t["plaid_category"] = *r.PlaidPfcDetailed
			}
			txns = append(txns, t)
		}
		return marshalTool(map[string]any{
			"count":        sum.TransactionCount,
			"total_in":     sum.TotalIn.StringFixed(2),
			"total_out":    sum.TotalOut.StringFixed(2),
			"listed":       len(txns),
			"truncated":    int64(len(txns)) < sum.TransactionCount,
			"transactions": txns,
		})

	case "breakdown":
		var in struct {
			GroupBy   string      `json:"group_by"`
			Flow      string      `json:"flow"`
			Month     string      `json:"month"`
			Months    int         `json:"months"`
			Start     string      `json:"start"`
			End       string      `json:"end"`
			Category  string      `json:"category"`
			Merchant  string      `json:"merchant"`
			Source    string      `json:"source"`
			MinAmount json.Number `json:"min_amount"`
			MaxAmount json.Number `json:"max_amount"`
		}
		if err := decodeToolInput(input, &in); err != nil {
			return "", err
		}
		groupBy, err := normalizeGroupBy(in.GroupBy)
		if err != nil {
			return "", err
		}
		flow, err := normalizeFlow(in.Flow)
		if err != nil {
			return "", err
		}
		from, to, err := toolDateRange(in.Month, in.Start, in.End, in.Months)
		if err != nil {
			return "", err
		}
		minAmt, err := toolDecimal(in.MinAmount)
		if err != nil {
			return "", err
		}
		maxAmt, err := toolDecimal(in.MaxAmount)
		if err != nil {
			return "", err
		}
		rows, err := s.Queries.BreakdownTransactions(ctx, dbgen.BreakdownTransactionsParams{
			HouseholdID: identity.HouseholdID, UserID: identity.UserID,
			Date: from, Date_2: to, GroupBy: groupBy, Flow: flow,
			Category: optStr(in.Category), Merchant: optStr(in.Merchant), Source: optStr(in.Source),
			MinAmount: minAmt, MaxAmount: maxAmt,
		})
		if err != nil {
			return "", err
		}
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, map[string]any{
				"group":     labelString(r.GroupLabel),
				"count":     r.TransactionCount,
				"total_in":  r.TotalIn.StringFixed(2),
				"total_out": r.TotalOut.StringFixed(2),
			})
		}
		return marshalTool(out)

	case "monthly_trend":
		from, to, exclude, err := trendWindow(input)
		if err != nil {
			return "", err
		}
		rows, err := s.Queries.GetMonthlyTrend(ctx, dbgen.GetMonthlyTrendParams{
			HouseholdID: identity.HouseholdID, UserID: identity.UserID, Date: from, Date_2: to,
		})
		if err != nil {
			return "", err
		}
		// The averages are computed HERE, in exact decimal, over EXACTLY the
		// months returned (after any exclude), because each is a finished figure
		// like every other money figure this app returns. avg_leftover is the
		// reference line the chat's trend chart draws, and avg_spending /
		// avg_income are the answers to "what's our average spent per month"
		// and its income twin — the model quotes them verbatim when asked for
		// an average, and neither the client nor the model may average the
		// series itself; a client-derived average is a second answer to a
		// question the server already answered. Excluding a month has to flow
		// through every average or the chart's reference line and the months it
		// spans would disagree.
		months, avgs := monthlyTrendSeries(rows, exclude)
		result := map[string]any{"months": months}
		if avgs != nil {
			result["avg_income"] = avgs.Income
			result["avg_spending"] = avgs.Spending
			result["avg_leftover"] = avgs.Leftover
		}
		return marshalTool(result)

	case "category_averages":
		months, err := toolMonths(input)
		if err != nil {
			return "", err
		}
		from, to := trailingMonthsRange(months)
		rows, err := s.Queries.GetCategoryAverages(ctx, dbgen.GetCategoryAveragesParams{
			HouseholdID: identity.HouseholdID, UserID: identity.UserID, Date: from, Date_2: to,
		})
		if err != nil {
			return "", err
		}
		out := make([]map[string]any, 0, len(rows))
		for _, c := range rows {
			out = append(out, map[string]any{
				"category":        c.CategoryName,
				"total":           c.Total.StringFixed(2),
				"monthly_average": c.MonthlyAverage.StringFixed(2),
				"count":           c.TransactionCount,
			})
		}
		return marshalTool(out)

	case "spending_by_day":
		from, to, err := toolMonth(input)
		if err != nil {
			return "", err
		}
		rows, err := s.Queries.GetSpendingByDay(ctx, dbgen.GetSpendingByDayParams{
			HouseholdID: identity.HouseholdID, UserID: identity.UserID, Date: from, Date_2: to,
		})
		if err != nil {
			return "", err
		}
		out := make([]map[string]string, 0, len(rows))
		for _, d := range rows {
			out = append(out, map[string]string{
				"day":      d.Day.Format("2006-01-02"),
				"spending": d.Spending.StringFixed(2),
			})
		}
		return marshalTool(out)

	default:
		// A bare "unknown tool" is a DEAD END, and to the model it reads exactly
		// like the capability not existing — which is the failure the escape
		// hatch exists to end. Naming the closest real tools turns a hallucinated
		// name into a correction it can act on in the next iteration.
		if near := nearestToolNames(name, 3); len(near) > 0 {
			return "", fmt.Errorf(
				"unknown tool %q. The closest real tools are: %s. Call %s with their names to load them, or with a description of what you need. Do not tell the user this cannot be done",
				name, strings.Join(near, ", "), toolFinderName)
		}
		return "", fmt.Errorf(
			"unknown tool %q. Call %s with a plain-language description of the lookup you need; it returns the real tool names. Do not tell the user this cannot be done without calling it first",
			name, toolFinderName)
	}
}

// toolMonth reads an optional {"month":"YYYY-MM"} input into a date range.
func toolMonth(input json.RawMessage) (from, to time.Time, err error) {
	var in struct {
		Month string `json:"month"`
	}
	if err := decodeToolInput(input, &in); err != nil {
		return time.Time{}, time.Time{}, err
	}
	return monthRange(in.Month)
}

// monthRange resolves a "YYYY-MM" (or empty = current month) to its day range.
func monthRange(month string) (from, to time.Time, err error) {
	_, from, to, _, err = monthPeriod(month)
	return from, to, err
}

// toolMonths reads an optional {"months":N} input, clamped to 1-24 and
// defaulting to 12 — the window the trend and averages tools look back over.
//
// The clamp still absorbs an out-of-range N silently, because that is a bounds
// decision the tool is entitled to make. A DECODE failure is a different thing
// and is reported: it means the model asked about a window we could not read,
// and answering over the default twelve months would put a year's figures
// behind a question about, say, three.
func toolMonths(input json.RawMessage) (int, error) {
	var in struct {
		Months int `json:"months"`
	}
	if err := decodeToolInput(input, &in); err != nil {
		return 0, err
	}
	if in.Months >= 1 && in.Months <= 24 {
		return in.Months, nil
	}
	return 12, nil
}

// trailingMonthsRange returns the first day of the month n-1 months ago through
// the last day of the current month, so a request for 12 months spans the
// current month plus the eleven before it.
func trailingMonthsRange(months int) (from, to time.Time) {
	now := time.Now()
	firstOfThis := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	from = firstOfThis.AddDate(0, -(months - 1), 0)
	to = firstOfThis.AddDate(0, 1, -1)
	return from, to
}

// trendWindow resolves the monthly_trend tool's window and optional month
// exclusions from the model's input.
//
// Precedence mirrors toolDateRange: an explicit start+end range wins, then a
// trailing N-month window (clamped to 1-24, default 12). start/end are YYYY-MM-DD
// snapped to whole-month boundaries — the tool reports per calendar month, so a
// window that began mid-month would either drop partial data or fold a month the
// caller did not name into the series, and both read exactly like a real answer.
//
// exclude drops named YYYY-MM months from BOTH the series and the average. That
// is the case a custom range alone cannot express: "redo the last year without
// July, which had a one-time loan payoff" is not a contiguous window. Averaging
// happens in monthlyTrendSeries over exactly the surviving months, so the
// average always reconciles with the rows the user sees.
//
// A decode failure is reported rather than defaulted, for the same reason as in
// toolMonths: silently substituting the trailing twelve answers a question
// nobody asked and leaves no trace.
func trendWindow(input json.RawMessage) (from, to time.Time, exclude map[string]bool, err error) {
	var in struct {
		Months  int      `json:"months"`
		Start   string   `json:"start"`
		End     string   `json:"end"`
		Exclude []string `json:"exclude"`
	}
	if err = decodeToolInput(input, &in); err != nil {
		return from, to, nil, err
	}

	start := strings.TrimSpace(in.Start)
	end := strings.TrimSpace(in.End)
	if start != "" || end != "" {
		if start == "" || end == "" {
			return from, to, nil, fmt.Errorf("both start and end are required for a custom monthly range")
		}
		from, to, err = monthBoundRange(start, end)
		if err != nil {
			return from, to, nil, err
		}
	} else {
		months := in.Months
		if months < 1 || months > 24 {
			months = 12
		}
		from, to = trailingMonthsRange(months)
	}

	for _, e := range in.Exclude {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if _, perr := time.Parse("2006-01", e); perr != nil {
			return from, to, nil, fmt.Errorf("invalid exclude month %q (use YYYY-MM)", e)
		}
		if exclude == nil {
			exclude = make(map[string]bool)
		}
		exclude[e] = true
	}
	return from, to, exclude, nil
}

// monthBoundRange parses two YYYY-MM-DD dates and snaps them to the first day of
// the start month and the last day of the end month, normalising a reversed
// pair. The snap happens AFTER the swap so a reversed range still lands on
// whole-month boundaries rather than last-day-of-month as "from".
func monthBoundRange(start, end string) (from, to time.Time, err error) {
	s, err := time.Parse("2006-01-02", start)
	if err != nil {
		return from, to, fmt.Errorf("invalid start date %q (use YYYY-MM-DD)", start)
	}
	e, err := time.Parse("2006-01-02", end)
	if err != nil {
		return from, to, fmt.Errorf("invalid end date %q (use YYYY-MM-DD)", end)
	}
	if e.Before(s) {
		s, e = e, s
	}
	from = time.Date(s.Year(), s.Month(), 1, 0, 0, 0, 0, time.UTC)
	to = time.Date(e.Year(), e.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, -1)
	return from, to, nil
}

// trendAverages is the three monthly means monthly_trend hands the model,
// finished and formatted. avg_leftover existed from the start; avg_income and
// avg_spending joined it because "what's our average spent per month" is the
// same class of question with the same constraint — the model may not average
// the series itself, so the mean has to arrive precomputed or the question gets
// a table and an apology instead of a number.
type trendAverages struct {
	Income   string
	Spending string
	Leftover string
}

// monthlyTrendSeries turns GetMonthlyTrend rows into the tool's output, dropping
// any month in exclude and computing the averages over EXACTLY the months it
// returns. The model cannot average or subtract — it is told to quote the
// avg_* figures verbatim — so each average has to come from here, and each has
// to agree with the rows the user sees. An excluded month leaves the series and
// all three averages together, which is what keeps every reference line honest.
func monthlyTrendSeries(rows []dbgen.GetMonthlyTrendRow, exclude map[string]bool) (months []map[string]string, avg *trendAverages) {
	months = make([]map[string]string, 0, len(rows))
	totalIncome, totalSpending := decimal.Zero, decimal.Zero
	for _, m := range rows {
		if exclude[m.Month.Format("2006-01")] {
			continue
		}
		totalIncome = totalIncome.Add(m.Income)
		totalSpending = totalSpending.Add(m.Spending)
		months = append(months, map[string]string{
			"month":    m.Month.Format("2006-01"),
			"income":   m.Income.StringFixed(2),
			"spending": m.Spending.StringFixed(2),
			"leftover": m.Income.Sub(m.Spending).StringFixed(2),
		})
	}
	if len(months) > 0 {
		n := decimal.NewFromInt(int64(len(months)))
		avg = &trendAverages{
			Income:   totalIncome.Div(n).StringFixed(2),
			Spending: totalSpending.Div(n).StringFixed(2),
			Leftover: totalIncome.Sub(totalSpending).Div(n).StringFixed(2),
		}
	}
	return months, avg
}

// optStr trims s and returns a pointer to it, or nil when empty — the shape the
// generated queries expect for an absent optional text filter (a nil narg passes
// everything).
func optStr(s string) *string {
	if v := strings.TrimSpace(s); v != "" {
		return &v
	}
	return nil
}

// toolDecimal reads an optional JSON-number amount into a NullDecimal (invalid =
// absent, so the query skips the filter). Parsing from the literal keeps money in
// decimal rather than routing it through a float.
func toolDecimal(n json.Number) (decimal.NullDecimal, error) {
	s := strings.TrimSpace(n.String())
	if s == "" {
		return decimal.NullDecimal{}, nil
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.NullDecimal{}, fmt.Errorf("invalid amount %q", s)
	}
	return decimal.NullDecimal{Decimal: d, Valid: true}, nil
}

// toolDateRange resolves the flexible tools' date window. Precedence: an explicit
// start+end range, then a single month, then a trailing N-month window, then the
// current month as the default. months out of 1-24 is clamped to that band.
func toolDateRange(month, start, end string, months int) (from, to time.Time, err error) {
	start = strings.TrimSpace(start)
	end = strings.TrimSpace(end)
	if start != "" || end != "" {
		if start == "" || end == "" {
			return from, to, fmt.Errorf("both start and end are required for a custom date range")
		}
		if from, err = time.Parse("2006-01-02", start); err != nil {
			return from, to, fmt.Errorf("invalid start date %q (use YYYY-MM-DD)", start)
		}
		if to, err = time.Parse("2006-01-02", end); err != nil {
			return from, to, fmt.Errorf("invalid end date %q (use YYYY-MM-DD)", end)
		}
		if to.Before(from) {
			from, to = to, from
		}
		return from, to, nil
	}
	if strings.TrimSpace(month) != "" {
		return monthRange(month)
	}
	if months >= 1 {
		if months > 24 {
			months = 24
		}
		from, to = trailingMonthsRange(months)
		return from, to, nil
	}
	return monthRange("")
}

// normalizeFlow validates the flow filter and defaults an empty value to
// "spending", matching the app's convention that a bare figure means money out.
func normalizeFlow(flow string) (string, error) {
	switch flow = strings.ToLower(strings.TrimSpace(flow)); flow {
	case "":
		return "spending", nil
	case "spending", "income", "transfers", "all":
		return flow, nil
	default:
		return "", fmt.Errorf("unknown flow %q (use spending, income, transfers, or all)", flow)
	}
}

// normalizeGroupBy validates the breakdown dimension. It is required — there is no
// sensible default grouping.
func normalizeGroupBy(g string) (string, error) {
	switch g = strings.ToLower(strings.TrimSpace(g)); g {
	case "category", "merchant", "account", "month", "day", "pfc":
		return g, nil
	case "":
		return "", fmt.Errorf("group_by is required (category, merchant, account, month, day, or pfc)")
	default:
		return "", fmt.Errorf("unknown group_by %q (use category, merchant, account, month, day, or pfc)", g)
	}
}

// labelString renders a breakdown group label. sqlc types the CASE expression as
// interface{}; pgx decodes the text column as a string, but the []byte and
// fallback arms keep it robust.
func labelString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

func marshalTool(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
