package api

import "strings"

// THE TOOL BUDGET IS A REAL CONSTRAINT AND WAVE 6 BLOWS THROUGH IT.
//
// The chat shipped with 12 tools. This doc adds 13, doc 24 adds 2, and docs 32
// and 33 add 7 between them — about 34 definitions on every single request if
// they all travel together. Two things break at that size and neither is
// hypothetical:
//
//   - Tool selection degrades. Thirty-four similarly-named finance tools is a
//     harder retrieval problem than twelve, and the failure mode is a plausible
//     WRONG tool rather than an error, which is the expensive kind.
//   - Six iterations stops being enough. "What should I do with $30k" plausibly
//     needs advisor_briefing → safe_to_spend → debt_summary → contribution_room
//     → allocation_plan → plan_likelihood. That is exactly the old cap, leaving
//     no room for a correction.
//
// The answer is to GROUP the tools and send one group per request. The set is
// chosen by a cheap deterministic classifier over the user's message — never by
// the model — and the chosen set is named in the response, so a wrong pick is
// visible rather than mysterious.

// Tool sets. These names travel to the client in the SSE `tool_set` frame, so
// they are a wire format: renaming one silently changes what a UI is matching
// on.
const (
	// ToolSetSpending is the original assistant: past-tense questions about
	// where the money went. It is also the DEFAULT, because an unrecognised
	// question is far more often "how much did I spend on X" than a planning
	// question, and defaulting to the set the app has always sent is the
	// behaviour-preserving choice.
	ToolSetSpending = "spending"
	// ToolSetPlanning is the advisor's engines: debts, goals, retirement,
	// obligations, investments, contribution room.
	ToolSetPlanning = "planning"
	// ToolSetModelling is for "what should I do with $X": doc 32's allocator
	// and the engines its questions need as inputs.
	ToolSetModelling = "modelling"
	// ToolSetLikelihood is doc 33's: "will this work", "compare these plans",
	// "am I still on track".
	//
	// THIS IS THE FOURTH SET, AND IT EXISTS BECAUSE THE CAP FORCED IT. Doc 31
	// predicted exactly this — "the next wave hits a build failure and picks a
	// fourth set" — and doc 33's four tools are what pushed modelling from 13
	// definitions to 17. Splitting is the right answer rather than raising the
	// cap: a likelihood question and an allocation question need genuinely
	// different inputs, and the split keeps both sets small enough that the
	// model picks correctly within them.
	ToolSetLikelihood = "likelihood"
)

// maxToolsPerSet is the ceiling a single set may reach.
//
// It is enforced by a test rather than at runtime, on purpose: exceeding it is
// not a condition to degrade gracefully from, it is a design decision somebody
// has to make. The next wave hits a build failure and picks a fourth set, which
// is much better than shipping a quality cliff nobody measured.
//
// Raised from 15 to 16 for planning only: monthly_trend had to join that set
// (see ToolSetPlanning) so a cashflow follow-up that names a planning cue —
// "redo the average without July, its numbers were skewed by a loan payoff" —
// does not lose the only tool that answers it. The route is right ("loan
// payoff" is genuinely a planning cue), so the fix is the tool's membership,
// and 16 distinct planning tools is still well inside the budget the cap exists
// to protect.
//
// It counts DOMAIN tools only — see metaTools for why the escape hatch is not
// charged against it.
const maxToolsPerSet = 16

// commonTools are in EVERY set, because every advisor conversation starts from
// the same two places: what is the household's position, and how much room does
// it have this month.
var commonTools = []string{"advisor_briefing", "safe_to_spend"}

// metaTools are in every set and are NOT charged against maxToolsPerSet.
//
// The exemption is not an accounting dodge, it follows from what the cap is
// for. The cap bounds a RETRIEVAL problem: N similarly-named finance tools, all
// plausible answers to the same money question, one of which is right.
// find_tools is not in that contest — it takes a description of a lookup and
// returns tools, it computes nothing, and no question has it and a domain tool
// as competing answers. Charging it would force a real engine out of a set to
// make room for the mechanism whose entire job is to put engines back.
//
// The exemption is narrow on purpose: this list is asserted to hold exactly one
// name, so "meta" cannot quietly become the drawer that lets sets grow.
var metaTools = []string{toolFinderName}

// toolSetMembers is the membership table. Names only — the definitions live in
// chatToolDefs and chatAdvisorToolDefs, and toolSetDefs filters those by this.
var toolSetMembers = map[string][]string{
	ToolSetSpending: {
		"spending_summary", "spend_by_category", "top_merchants",
		"list_transactions", "query_transactions", "breakdown",
		"monthly_trend", "category_averages", "spending_by_day",
		"budget_status", "net_worth", "recurring_charges",
	},
	ToolSetPlanning: {
		"project_balance", "upcoming_obligations",
		"debt_summary", "debt_payoff",
		"goal_status", "goal_solve",
		// A college-savings question is a planning question too: "how do I save
		// for my kid's education" routes here when it carries none of the
		// modelling keywords (529/college/tuition). Without the tool in this
		// set the model would be asked to plan college funding with none of the
		// engines that compute it — the same hole that produced a fabricated
		// 529 figure, just on the first turn instead of a follow-up.
		"college_projection",
		"retirement_projection", "retirement_solve",
		"investment_performance", "asset_allocation", "fees_summary",
		"contribution_room",
		// A cashflow follow-up can land here through its OWN keyword, not just
		// inheritance: "redo the average monthly leftover without July — that
		// month was skewed by a big loan payoff" routes to planning on "loan",
		// even though the question is monthly_trend's. Without the tool in this
		// set the advisor truthfully reports it has no per-month income/spending
		// tool, exactly the bug that was reported the day after monthly_trend
		// shipped its custom-range + exclude support. Same fix shape as
		// college_projection above: the tool follows the misroute.
		"monthly_trend",
	},
	// A "what should I do with $X" question needs the same position inputs a
	// planning question does — what is owed, what room is left, where the
	// portfolio sits — and then the allocator on top.
	//
	// Doc 33's likelihood tools were originally planned to join this list. They
	// did not fit: four more definitions took it to 17, over the cap, so they
	// became ToolSetLikelihood instead. See that set's note.
	ToolSetModelling: {
		"debt_summary", "contribution_room", "retirement_projection",
		"goal_status", "asset_allocation", "net_worth",
		// Doc 32's allocator. allocation_buckets is listed FIRST of the five
		// because it is the one the model has to call before allocation_plan
		// can name an account, and the order tools arrive in is the order they
		// are read in.
		"allocation_buckets", "allocation_plan",
		"idle_cash", "asset_location", "college_projection",
	},
	// Doc 33. A likelihood question is asked ABOUT A SAVED PLAN, so
	// allocation_plans leads the list for the same reason allocation_buckets
	// leads doc 32's: it is the one the model must call before any of the other
	// three can name a plan, and the order tools arrive in is the order they are
	// read in.
	//
	// The allocator itself is here too, because "what would make this more
	// likely" is answered by changing the split and re-running — and goals, net
	// worth and the retirement projection are the position a likelihood answer
	// is measured against.
	ToolSetLikelihood: {
		"allocation_plans", "plan_likelihood", "compare_plans", "plan_tracker",
		"allocation_buckets", "allocation_plan",
		"goal_status", "net_worth", "retirement_projection",
	},
}

// ToolSets returns every declared set name, in a stable order.
func ToolSets() []string {
	return []string{ToolSetSpending, ToolSetPlanning, ToolSetModelling, ToolSetLikelihood}
}

// toolSetNames returns one set's full membership, common and meta tools
// included.
//
// The meta tools go LAST. Tool order is read order, and the escape hatch should
// be the thing the model reaches for after the domain tools have failed it, not
// the first entry it sees.
func toolSetNames(set string) []string {
	members, ok := toolSetMembers[set]
	if !ok {
		members = toolSetMembers[ToolSetSpending]
	}
	out := make([]string, 0, len(commonTools)+len(members)+len(metaTools))
	out = append(out, commonTools...)
	out = append(out, members...)
	out = append(out, metaTools...)
	return out
}

// isMetaTool reports whether a name is infrastructure rather than an engine.
func isMetaTool(name string) bool {
	for _, m := range metaTools {
		if m == name {
			return true
		}
	}
	return false
}

// setKeywords maps a set to the phrases that select it.
//
// Checked MOST SPECIFIC FIRST — likelihood, then modelling, then planning, then
// spending — because the narrower phrases subsume the broader ones: "what should
// I do with $30k" contains "invest", which is also a planning keyword, and the
// allocator is the right answer to it; "what are the odds this works" is a
// likelihood question even though it is asked about an allocation.
//
// Substring matching over a lowercased message, which is crude and deliberately
// so. The classifier's job is to be DETERMINISTIC and inspectable, not clever:
// the same message must pick the same set every time, and a human reading this
// list must be able to predict which one. A model-chosen set would reintroduce
// exactly the wrong-tool failure the sets exist to bound.
//
// Spending carries its OWN keywords (not just the fallback) so that a topic
// change INTO spending is detectable: "and what did I spend at Costco" must
// route to the transaction tools even mid-way through a planning thread. The
// keywords are high-precision on purpose and none of them is a substring of a
// planning/modelling/likelihood keyword, so the more-specific sets still win
// when a message carries cues from more than one.
var setKeywords = map[string][]string{
	// Doc 33's, checked FIRST because they are the most specific of all. "What
	// are the odds I hit my number" contains no allocator keyword, and the tools
	// that answer it live only here — routing it to modelling would hand the
	// model an allocator and no simulation.
	//
	// "likelihood", "odds", "probability", "chance of" and "simulate" MOVED HERE
	// from modelling, where doc 32 parked them because doc 33's tools did not
	// exist yet. They now select the set that actually contains them.
	ToolSetLikelihood: {
		"likelihood", "odds", "probability", "chance of", "how likely",
		"success rate", "will it work", "will this work", "will that work",
		"compare plan", "compare my plan", "compare these plan", "which plan",
		"top pick", "monte carlo", "simulate", "simulation", "simulated",
		"on track with", "drift", "fan chart", "worst case", "drawdown",
	},
	ToolSetModelling: {
		"what should i do with", "what do i do with", "where should i put",
		"allocate", "allocation plan", "split it", "split this",
		"scenario", "what if",
		// Doc 32's own questions. "college" and "529" are here rather than in
		// planning because the answer is the drawdown the allocator computes,
		// and "idle cash" / "asset location" name their tools directly.
		"college", "529", "tuition",
		"idle cash", "cash drag", "asset location", "which account should",
	},
	ToolSetPlanning: {
		"debt", "payoff", "pay off", "paying off", "loan", "mortgage",
		"credit card", "apr", "interest rate", "minimum payment", "utilisation",
		"utilization", "balance transfer",
		"goal", "on track", "save up", "saving for",
		"retire", "retirement", "401k", "401(k)", "ira", "iras", "roth", "hsa",
		"contribute", "contribution", "nest egg", "financial independence",
		"invest", "portfolio", "holdings", "asset allocation", "expense ratio",
		"fees", "fee drag", "return", "performance",
		"overdraft", "overdraw", "run out of money", "project my balance",
		"upcoming bill", "bills due", "obligation",
		"safe to spend", "emergency fund", "runway", "briefing", "brief me",
		"headroom", "how much room", "contribution room",
	},
	// Spending keywords make a topic change INTO spending detectable, which is
	// the other half of the follow-up problem: once a thread can inherit a
	// deeper set (see classifyFromMessages), "now show me my Costco spending"
	// must still route to the transaction tools rather than staying pinned to
	// planning.
	//
	// Spending is checked LAST, which is what resolves the one deliberate
	// overlap: "spend" sits inside the planning keyword "safe to spend", and
	// "am I safe to spend $500" is a planning question. Order decides it, so a
	// spending keyword may overlap a deeper set's — it must simply never be the
	// only reading of a message that belongs to the deeper set.
	ToolSetSpending: {
		// "spend" rather than only "spending": the bare verb is how the question
		// is most often asked ("how much did I spend on gas last month") and it
		// matched NOTHING here, so that message carried no topic keyword at all.
		// On a first turn it fell to the spending default and looked fine; once
		// follow-ups began inheriting the thread's set (classifyFromMessages),
		// the same question asked inside a planning thread inherited planning and
		// lost every transaction tool. "spend" subsumes "spending"/"my spending";
		// "spent" is a separate stem and stays.
		"spend", "spent",
		"transactions", "transaction",
		"merchant", "merchants",
		"costco", "amazon", "groceries", "grocery",
		"dining", "restaurant",
		"subscription", "subscriptions",
		// CASHFLOW vocabulary, and the reason it is here is the reported bug.
		// "Average money leftover at the end of each month over the last year"
		// is monthly_trend's question and it matched NOTHING — it only reached
		// the right tools by falling through to the spending default, which is
		// luck, not routing. Luck runs out the moment such a message is asked
		// inside a planning thread, where the ambiguous-follow-up rule inherits
		// planning instead.
		//
		// Every phrase here is DISTINCTIVE on purpose. The generic ones that
		// would also fit — "each month", "per month", "income", "average" —
		// are deliberately absent: spending is checked last, so a keyword here
		// binds a message that matched nothing deeper, and a generic phrase
		// would start yanking genuine planning follow-ups ("how much should I
		// put in each month?") out of the set that answers them. That is the
		// 529-fabrication bug run backwards.
		"leftover", "left over", "cash flow", "cashflow",
		"month by month", "month-by-month", "monthly trend", "monthly breakdown",
		"income vs", "income versus", "paycheck", "paychecks",
	},
}

// wholeWordKeywords must match as WHOLE WORDS rather than as substrings.
//
// Substring matching is the right default and deliberately crude — it is what
// makes "invest" cover "investing" and "investment" without three entries. But a
// three-letter acronym is a substring of ordinary English, and the failure is
// silent: "apr" matches "April", so EVERY message mentioning that month routed
// to planning (checked before spending) and lost spending_summary,
// list_transactions and spend_by_category. "How much did I spend in April?" was
// answered with no transaction tool in the set for a twelfth of the calendar.
// "ira" is the same shape inside "spiral", "admiral" and "Miracle".
//
// Only acronyms belong here. A keyword that wants to catch its own inflections
// ("debt" -> "debts", "invest" -> "investing") must stay a substring, which is
// why this is an opt-in list and not a change to the matching rule.
var wholeWordKeywords = map[string]bool{
	"apr":  true,
	"ira":  true,
	"iras": true,
}

// matchesKeyword is the matching rule: substring by default, whole word for the
// acronyms above.
//
// A word character is [a-z0-9] over the already-lowercased message. Any other
// byte — punctuation, space, or the lead byte of a multibyte rune — is a
// boundary, which is the behaviour we want: "0% APR." and "my IRA," both match
// and "April" does not.
func matchesKeyword(lower, kw string) bool {
	if !wholeWordKeywords[kw] {
		return strings.Contains(lower, kw)
	}
	for from := 0; from+len(kw) <= len(lower); {
		i := strings.Index(lower[from:], kw)
		if i < 0 {
			return false
		}
		start := from + i
		end := start + len(kw)
		if !isWordByte(lower, start-1) && !isWordByte(lower, end) {
			return true
		}
		// Overlapping occurrences matter: a rejected hit must not hide a real one
		// later in the same message ("April ... 0% apr").
		from = start + 1
	}
	return false
}

func isWordByte(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// classifyExplicit returns the set a message selects when it carries an
// explicit topic keyword, and matched=false when it carries none (the spending
// default). The matched flag is what lets a follow-up INHERIT the thread's
// established set rather than dropping to spending and losing every computation
// tool the conversation was just using — see classifyFromMessages.
//
// The check order is the same most-specific-first order the keywords are
// declared in: a message carrying cues from more than one set routes to the
// narrower one, which is how "what should I do with $30k" lands in modelling
// even though it also contains the planning keyword "invest".
func classifyExplicit(message string) (set string, matched bool) {
	lower := strings.ToLower(message)
	for _, set := range []string{ToolSetLikelihood, ToolSetModelling, ToolSetPlanning, ToolSetSpending} {
		for _, kw := range setKeywords[set] {
			if matchesKeyword(lower, kw) {
				return set, true
			}
		}
	}
	return ToolSetSpending, false
}

// classifyToolSet picks the tool set for a single message. It is the
// single-message view the tests pin: deterministic, and defaulting to spending
// for a message that carries no topic keyword.
//
// For an actual advisor turn use classifyFromMessages, which inherits the
// thread's set when the last message is an ambiguous follow-up. Keeping this
// function separate preserves the single-message contract the classifier tests
// assert against.
func classifyToolSet(message string) string {
	set, _ := classifyExplicit(message)
	return set
}

// classifyFromMessages picks the tool set for a turn from the WHOLE transcript,
// not just the last message. It is what the chat handler calls.
//
// The single-message rule still decides the turn when the last message carries
// a keyword — including a topic change INTO spending ("and what did I spend at
// Costco" matches "spent"/"costco"), so the user can still change the subject
// exactly as before. What this adds is the follow-up case the single-message
// rule got badly wrong: a message like "now that the birthday's fixed, what's
// the monthly number?" carries no topic keyword at all, and under the old rule
// it dropped to the spending set — which removed college_projection from a
// thread that was using it one turn ago. With the tool gone the model invented
// a 529 figure instead of computing one, and a fabricated number in a money app
// is the failure the whole tool architecture exists to prevent.
//
// So when the last message matches no keyword, the conversation's most recent
// EXPLICIT set is inherited, bounded by recency so a topic raised many turns
// ago does not pin the present. The asymmetry that makes this the right
// default: losing a planning/modelling tool is DANGEROUS (the model fabricates
// numbers), while losing a spending tool is merely degraded (the model says it
// cannot see transactions). A keyword in the last message always wins.
func classifyFromMessages(messages []chatMessage) string {
	last := lastUserMessage(messages)
	if set, matched := classifyExplicit(last); matched {
		return set
	}

	// No explicit keyword in the last turn. Walk the earlier user turns, most
	// recent first, and inherit the first explicit set a message carried. The
	// window keeps a long-ago topic from sticking — a thread that opened on
	// retirement and moved to groceries six turns back should not still be
	// pinned to planning.
	const historyWindow = 8
	turns := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			continue
		}
		turns++
		if turns == 1 {
			continue // the last message — already classified as ambiguous
		}
		if turns > historyWindow {
			break
		}
		if set, matched := classifyExplicit(messages[i].Content); matched {
			return set
		}
	}
	return ToolSetSpending
}

// lastUserMessage is what the classifier reads: the most recent user turn, not
// the whole transcript.
//
// The transcript is the wrong input. A conversation that opened with a debt
// question and moved on to "and what did I spend at Costco" must follow the
// user to the spending set; classifying over the concatenated history would
// pin it to planning for the rest of the session.
func lastUserMessage(messages []chatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "assistant" {
			return messages[i].Content
		}
	}
	return ""
}
