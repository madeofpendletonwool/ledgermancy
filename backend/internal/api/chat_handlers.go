package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/apex42group/ledgermancy/backend/internal/ai"
	"github.com/apex42group/ledgermancy/backend/internal/auth"
	"github.com/apex42group/ledgermancy/backend/internal/db/dbgen"
)

// maxToolIterations bounds the model↔tool loop. A finance question rarely needs
// more than a couple of lookups; the cap stops a misbehaving model from
// spinning up unbounded queries and cost.
const maxToolIterations = 6

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
- For "vs last month", "trend", or "on average", use monthly_trend or category_averages.

Conventions:
- Amounts are US dollars; months are "YYYY-MM". If no month is given, assume the current month.
- Spending is money out; income and transfers are excluded from spending totals.
- You can only see this household's data. Do not claim to access anything else.

Style:
- Be concise and concrete. If a tool returns nothing, say so plainly.
- Format lists and comparisons as GitHub-flavored Markdown tables, and bold the key figure in a sentence. Your replies are rendered as Markdown.
- When you call a tool, do not narrate that you are doing so — only produce prose in your final answer.`

type chatRequestBody struct {
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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

	messages := make([]ai.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := ai.RoleUser
		if m.Role == "assistant" {
			role = ai.RoleAssistant
		}
		messages = append(messages, ai.Message{Role: role, Content: []ai.Block{ai.TextBlock(m.Content)}})
	}

	// Everything below streams over Server-Sent Events: one `{"delta":...}`
	// frame per chunk of answer, a terminal `{"done":true}`, or `{"error":...}`
	// if the turn fails. Validation above still returns a normal JSON error,
	// because nothing has been written yet.
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

	if _, err := s.runChat(r.Context(), identity, messages, onDelta); err != nil {
		slog.Error("chat", "error", err)
		sendSSE(map[string]string{"error": "Something went wrong answering that."})
		return
	}
	sendSSE(map[string]bool{"done": true})
}

// runChat drives the tool-calling loop: the model may ask to run scoped queries,
// whose results are fed back until it produces a final text answer. The final
// answer's text is streamed to onText as it is generated; the full text is also
// returned. Tool-calling turns produce no user-visible text (the system prompt
// forbids it), so nothing leaks between lookups.
func (s *Server) runChat(ctx context.Context, identity auth.Identity, messages []ai.Message, onText func(string)) (string, error) {
	tools := chatToolDefs()

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
			MaxTokens: 1024,
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
			return text, nil
		}

		// Echo the assistant's tool_use turn back, then answer each call.
		messages = append(messages, resp.AsMessage())
		results := make([]ai.Block, 0, len(uses))
		for _, use := range uses {
			out, err := s.executeChatTool(ctx, identity, use.Name, use.Input)
			if err != nil {
				// Hand the model the error so it can recover or apologise,
				// rather than failing the whole request.
				results = append(results, ai.ToolResultBlock(use.ID, err.Error(), true))
				continue
			}
			results = append(results, ai.ToolResultBlock(use.ID, out, false))
		}
		messages = append(messages, ai.Message{Role: ai.RoleUser, Content: results})
	}

	// Ran out of iterations without a final answer.
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

// chatToolDefs are the read-only tools the assistant may call. Each maps to an
// existing reporting query; execution scopes every one to the caller.
func chatToolDefs() []ai.Tool {
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
			Name:        "monthly_trend",
			Description: "Income and spending per calendar month over the last N months (default 12), oldest first. Use for month-over-month comparisons and trends.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"months":{"type":"integer","description":"How many recent months, 1-24 (default 12)"}}}`),
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
		_ = json.Unmarshal(input, &in)
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
			HouseholdID: identity.HouseholdID, UserID: identity.UserID, Date: from, Date_2: to,
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
		since := time.Now().AddDate(0, -recurringLookbackMonths, 0)
		rows, err := s.Queries.GetRecurringMerchants(ctx, dbgen.GetRecurringMerchantsParams{
			HouseholdID: identity.HouseholdID, UserID: identity.UserID, Date: since,
		})
		if err != nil {
			return "", err
		}
		out := make([]map[string]string, 0, len(rows))
		for _, m := range rows {
			var monthly decimal.Decimal
			if m.AvgGapDays.IsPositive() {
				monthly = m.AverageAmount.Mul(daysPerMonth).Div(m.AvgGapDays).Round(2)
			}
			out = append(out, map[string]string{
				"merchant":         m.Merchant,
				"cadence":          cadenceLabel(m.AvgGapDays),
				"typical_amount":   m.AverageAmount.StringFixed(2),
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
		_ = json.Unmarshal(input, &in)
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

	case "monthly_trend":
		from, to := trailingMonthsRange(toolMonths(input))
		rows, err := s.Queries.GetMonthlyTrend(ctx, dbgen.GetMonthlyTrendParams{
			HouseholdID: identity.HouseholdID, UserID: identity.UserID, Date: from, Date_2: to,
		})
		if err != nil {
			return "", err
		}
		out := make([]map[string]string, 0, len(rows))
		for _, m := range rows {
			out = append(out, map[string]string{
				"month":    m.Month.Format("2006-01"),
				"income":   m.Income.StringFixed(2),
				"spending": m.Spending.StringFixed(2),
				"leftover": m.Income.Sub(m.Spending).StringFixed(2),
			})
		}
		return marshalTool(out)

	case "category_averages":
		from, to := trailingMonthsRange(toolMonths(input))
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
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

// toolMonth reads an optional {"month":"YYYY-MM"} input into a date range.
func toolMonth(input json.RawMessage) (from, to time.Time, err error) {
	var in struct {
		Month string `json:"month"`
	}
	_ = json.Unmarshal(input, &in)
	return monthRange(in.Month)
}

// monthRange resolves a "YYYY-MM" (or empty = current month) to its day range.
func monthRange(month string) (from, to time.Time, err error) {
	_, from, to, _, err = monthPeriod(month)
	return from, to, err
}

// toolMonths reads an optional {"months":N} input, clamped to 1-24 and
// defaulting to 12 — the window the trend and averages tools look back over.
func toolMonths(input json.RawMessage) int {
	var in struct {
		Months int `json:"months"`
	}
	_ = json.Unmarshal(input, &in)
	if in.Months >= 1 && in.Months <= 24 {
		return in.Months
	}
	return 12
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

func marshalTool(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
