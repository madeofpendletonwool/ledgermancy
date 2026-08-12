package api

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/madeofpendletonwool/ledgermancy/backend/internal/ai"
)

// THE TOOL SETS WERE A ONE-WAY DOOR, AND THAT IS THE BUG UNDERNEATH THE BUGS.
//
// chat_toolsets.go sends ONE set per turn, chosen by a deterministic keyword
// classifier. That is the right architecture — thirty-five similarly-named
// finance tools is a worse retrieval problem than fourteen, and a plausible
// WRONG tool is the expensive failure. But it shipped with no recovery path: a
// tool absent from the chosen set was absent for the entire turn, and the model
// had no way to ask for it. When the classifier picked a set that did not
// contain the answer, the model's only remaining moves were both bad:
//
//   - Say it has no such capability. Truthful about its own tool list, FALSE
//     about the app. A user who watched the feature ship the day before is told
//     it does not exist.
//   - Compute the number itself. This is the catastrophic one, and it has
//     already happened here twice: the fabricated 529 figure, and the advisor
//     quoting median-based "monthly slack" as a stand-in for an average it could
//     not compute.
//
// Both of those are the same defect wearing different clothes, and both were
// patched the same way: find the misroute, duplicate the missing tool into the
// set it landed in (college_projection into planning, then monthly_trend into
// planning). That works exactly once per report. It is whack-a-mole against an
// unbounded space of phrasings, and the space is unbounded because the
// classifier reads keywords while the user writes English.
//
// THIS FILE CLOSES THE CLASS. find_tools is in every set, and it lets the model
// pull any tool in the catalogue into the turn it is already in. The classifier
// keeps doing its job — pick a small, well-fitted set, which is what keeps
// retrieval sharp — but a wrong pick is now a one-round-trip detour instead of a
// dead end. Nothing has to be predicted in advance, so nothing can be missed.
//
// Execution needed no change at all, which is the tell that this is the right
// seam: executeChatTool already dispatches over the WHOLE catalogue by name and
// never consulted the set. The set only ever bounded what the model could SEE.
const toolFinderName = "find_tools"

// maxGrantedTools bounds how many extra tools one turn may pull in.
//
// A cap is needed for the same reason the sets exist: a model that escalated
// its way to all thirty-five definitions would have rebuilt, inside one turn,
// precisely the retrieval problem the sets were introduced to prevent. Six is
// enough for any real cross-set question — the deepest one observed needs a
// trend tool and two engines — and small enough that the working set stays
// inside the budget the cap in chat_toolsets.go protects.
//
// It bounds the TURN, not the call: two find_tools calls of three tools each
// hit the same ceiling as one of six.
const maxGrantedTools = 6

// chatToolCatalogue is every tool the assistant can execute, in a stable order.
//
// Stable matters twice: it is the order definitions reach the model in, and it
// is the tie-break in the search below, so an identical query returns an
// identical grant. The order is the historical one — the spending assistant's
// tools, then each doc's in the order they shipped.
func chatToolCatalogue() []ai.Tool {
	out := make([]ai.Tool, 0, 40)
	seen := map[string]bool{}
	add := func(defs []ai.Tool) {
		for _, d := range defs {
			if seen[d.Name] {
				continue
			}
			seen[d.Name] = true
			out = append(out, d)
		}
	}
	add(chatBaseToolDefs())
	add(chatAllocationToolDefs())
	add(chatLikelihoodToolDefs())
	add(chatAdvisorToolDefs())
	return out
}

// chatToolFinderDef is the escape hatch's definition.
//
// The description is written AT THE FAILURE, not at the feature: the model's
// observed behaviour when a tool is missing is to apologise and stop, so the
// text has to make clear that stopping is not the move. Both arguments are
// optional and either alone is enough — `need` for "I don't know what it's
// called", `names` for "I read the catalogue and want that one".
func chatToolFinderDef() ai.Tool {
	return ai.Tool{
		Name: toolFinderName,
		Description: "Load a tool that is not in your current list. Your tools are a SUBSET chosen automatically from the question's wording, so a capability missing from that list usually still exists. " +
			"Call this BEFORE telling the user something cannot be done, and before working a figure out yourself. " +
			"It returns the tools it loaded — call them normally on your next step — plus a catalogue of everything else available.",
		InputSchema: json.RawMessage(`{"type":"object","properties":` +
			`{"need":{"type":"string","description":"Plain-language description of the lookup you need, e.g. \"per-month income and spending totals over a custom date range\""},` +
			`"names":{"type":"array","items":{"type":"string"},"description":"Exact tool names to load, when you already know them (e.g. from a previous catalogue)"}},` +
			`"additionalProperties":false}`),
	}
}

// toolBrief is one catalogue entry as the model sees it.
type toolBrief struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
}

// toolFinderResult is find_tools' output.
//
// `catalogue` is present on EVERY call, and it is the floor that makes this
// mechanism total rather than best-effort. The search below is crude lexical
// matching and will sometimes miss; the catalogue means a miss costs one more
// round trip (read the names, ask again by name) instead of resurrecting the
// dead end. It is also what makes "there is no tool for that" a CHECKED claim —
// the model can only say it after seeing the list.
type toolFinderResult struct {
	Loaded    []toolBrief `json:"loaded"`
	Catalogue []toolBrief `json:"catalogue"`
	Note      string      `json:"note"`
}

// toolFinderQuery is find_tools' input.
type toolFinderQuery struct {
	Need  string   `json:"need"`
	Names []string `json:"names"`
}

// grantTools runs one find_tools call.
//
// available is the turn's live tool list and is READ ONLY here — runChat owns
// the mutation, so a granted tool and the definition actually sent to the model
// cannot drift apart. granted is how many this turn has already taken, so the
// cap bounds the turn rather than the call.
func grantTools(input json.RawMessage, available map[string]bool, granted int) (string, []ai.Tool, error) {
	var q toolFinderQuery
	if err := decodeToolInput(input, &q); err != nil {
		return "", nil, err
	}

	catalogue := chatToolCatalogue()
	room := maxGrantedTools - granted

	picked := make([]ai.Tool, 0, maxGrantedTools)
	take := func(t ai.Tool) {
		if len(picked) >= room || available[t.Name] {
			return
		}
		for _, p := range picked {
			if p.Name == t.Name {
				return
			}
		}
		picked = append(picked, t)
	}

	// Explicit names first and in the order asked. A model that names a tool has
	// already decided; ranking its choice against a fuzzy score would be the
	// search second-guessing an exact request.
	for _, want := range q.Names {
		for _, t := range catalogue {
			if strings.EqualFold(strings.TrimSpace(want), t.Name) {
				take(t)
				break
			}
		}
	}
	for _, t := range rankToolsByNeed(q.Need, catalogue) {
		take(t)
	}

	res := toolFinderResult{
		Loaded:    briefs(picked),
		Catalogue: remainingBriefs(catalogue, available, picked),
		Note:      finderNote(picked, room),
	}
	out, err := marshalTool(res)
	if err != nil {
		return "", nil, err
	}
	return out, picked, nil
}

// finderNote tells the model what to do next, in the three states it can be in.
// A tool result that only reports data leaves the next move to inference, and
// the inference this model made unprompted was to give up.
func finderNote(picked []ai.Tool, room int) string {
	switch {
	case room <= 0:
		return "No room left: this turn has already loaded the maximum number of extra tools. Answer with what you have, and say plainly which part you could not compute."
	case len(picked) == 0:
		return "Nothing matched that description. Read \"catalogue\" — if one of those tools fits, call find_tools again with its exact name in \"names\". If none of them fits, the app genuinely cannot answer this; say so plainly and do not work the figure out yourself."
	default:
		return "The tools in \"loaded\" are now available — call them on your next step. Do not tell the user a figure is unavailable while an unused tool in \"catalogue\" could produce it."
	}
}

// rankToolsByNeed scores the catalogue against a plain-language description.
//
// Deliberately crude, for the same reason the classifier is: deterministic and
// inspectable beats clever. There is no embedding here and there should not be
// — a lexical score that a human can reproduce by eye is debuggable, and the
// catalogue returned alongside it means the cost of a miss is one round trip
// rather than a wrong answer. Ties break on catalogue order, so the same query
// always returns the same tools in the same order.
func rankToolsByNeed(need string, catalogue []ai.Tool) []ai.Tool {
	terms := searchTerms(need)
	if len(terms) == 0 {
		return nil
	}

	type scored struct {
		tool  ai.Tool
		score int
		order int
	}
	ranked := make([]scored, 0, len(catalogue))
	for i, t := range catalogue {
		if s := scoreTool(t, terms); s > 0 {
			ranked = append(ranked, scored{tool: t, score: s, order: i})
		}
	}
	sort.SliceStable(ranked, func(a, b int) bool {
		if ranked[a].score != ranked[b].score {
			return ranked[a].score > ranked[b].score
		}
		return ranked[a].order < ranked[b].order
	})

	out := make([]ai.Tool, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, r.tool)
	}
	return out
}

// scoreTool weights a name hit far above a description hit.
//
// The name is what the tool IS; the description is prose about it, and every
// tool's prose mentions money, month and spending. Without the weighting a
// query for a trend tool ranks a dozen tools that merely say "for a month"
// alongside the one actually called monthly_trend.
func scoreTool(t ai.Tool, terms []string) int {
	nameParts := map[string]bool{}
	for _, p := range strings.Split(strings.ToLower(t.Name), "_") {
		nameParts[p] = true
	}
	lowerName := strings.ToLower(t.Name)
	lowerDesc := strings.ToLower(t.Description)

	score := 0
	for _, term := range terms {
		switch {
		case nameParts[term]:
			score += 8
		case strings.Contains(lowerName, term):
			score += 4
		case containsWord(lowerDesc, term):
			score += 1
		}
	}
	return score
}

// searchTerms lowercases, splits on non-alphanumerics, and drops the words that
// appear in nearly every finance question. A query is mostly filler — "I need a
// tool that shows me the income and spending for each month" carries three
// words worth matching on — and without the stop list the filler outvotes them.
func searchTerms(need string) []string {
	fields := strings.FieldsFunc(strings.ToLower(need), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	out := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, f := range fields {
		if len(f) < 3 || toolSearchStopWords[f] || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
		// Match the singular too, so "totals" finds "total" and "merchants"
		// finds "merchant" without every caller having to guess the stem.
		if stem := strings.TrimSuffix(f, "s"); stem != f && len(stem) >= 3 && !seen[stem] {
			seen[stem] = true
			out = append(out, stem)
		}
	}
	return out
}

// toolSearchStopWords are words that carry no signal HERE — either ordinary
// English filler, or vocabulary so universal across a finance tool catalogue
// that matching on it ranks everything equally.
var toolSearchStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true,
	"this": true, "over": true, "into": true, "from": true, "each": true,
	"per": true, "how": true, "what": true, "need": true, "want": true,
	"tool": true, "tools": true, "get": true, "show": true, "give": true,
	"can": true, "some": true, "any": true, "all": true, "user": true,
	"data": true, "figure": true, "figures": true, "number": true,
	"numbers": true, "money": true, "amount": true, "amounts": true,
	"household": true, "app": true, "call": true, "using": true, "use": true,
	"across": true, "between": true, "their": true, "there": true,
	"about": true, "would": true, "could": true, "should": true,
}

// containsWord is whole-word containment over already-lowercased prose, so a
// description matching "income" is not also matched by "come".
func containsWord(haystack, word string) bool {
	for from := 0; from+len(word) <= len(haystack); {
		i := strings.Index(haystack[from:], word)
		if i < 0 {
			return false
		}
		start := from + i
		if !isWordByte(haystack, start-1) && !isWordByte(haystack, start+len(word)) {
			return true
		}
		from = start + 1
	}
	return false
}

// nearestToolNames is the recovery path for a HALLUCINATED tool name.
//
// The model sometimes invents a plausible name rather than calling find_tools —
// "monthly_breakdown" for monthly_trend. Answering that with a bare `unknown
// tool` is a dead end that reads, to the model, exactly like the capability not
// existing, which is the failure this whole file exists to end. Naming the
// closest real tools turns it into a correction it can act on.
func nearestToolNames(name string, limit int) []string {
	catalogue := chatToolCatalogue()
	ranked := rankToolsByNeed(strings.ReplaceAll(name, "_", " "), catalogue)
	out := make([]string, 0, limit)
	for _, t := range ranked {
		if len(out) >= limit {
			break
		}
		out = append(out, t.Name)
	}
	return out
}

// briefs summarises tools for the model.
func briefs(tools []ai.Tool) []toolBrief {
	out := make([]toolBrief, 0, len(tools))
	for _, t := range tools {
		out = append(out, toolBrief{Name: t.Name, Purpose: firstSentence(t.Description)})
	}
	return out
}

// remainingBriefs is everything the turn does not already have. Listing tools
// the model can already call would invite it to "load" them and burn a round
// trip re-acquiring what is in front of it.
func remainingBriefs(catalogue []ai.Tool, available map[string]bool, picked []ai.Tool) []toolBrief {
	just := map[string]bool{}
	for _, p := range picked {
		just[p.Name] = true
	}
	out := make([]toolBrief, 0, len(catalogue))
	for _, t := range catalogue {
		if available[t.Name] || just[t.Name] {
			continue
		}
		out = append(out, toolBrief{Name: t.Name, Purpose: firstSentence(t.Description)})
	}
	return out
}

// firstSentence keeps the catalogue readable. Several descriptions are a
// paragraph of usage guidance aimed at a model that has already chosen the
// tool; in a list of thirty-odd candidates that is noise, and the opening
// sentence is the part that says what the tool is for.
func firstSentence(desc string) string {
	desc = strings.TrimSpace(desc)
	if i := strings.Index(desc, ". "); i > 0 {
		return desc[:i+1]
	}
	return desc
}
