package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
Rules:
- For any figure, category, budget, or balance, CALL A TOOL. Never invent or estimate numbers.
- Amounts are US dollars; months are "YYYY-MM". If no month is given, assume the current month.
- Spending is money out; income and transfers are excluded from spending totals.
- Be concise and concrete. Quote the amounts the tools return. If a tool returns nothing, say so plainly.
- You can only see this household's data. Do not claim to access anything else.`

type chatRequestBody struct {
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponseBody struct {
	Reply string `json:"reply"`
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

	reply, err := s.runChat(r.Context(), identity, messages)
	if err != nil {
		s.internalError(w, "chat", err)
		return
	}
	writeJSON(w, http.StatusOK, chatResponseBody{Reply: reply})
}

// runChat drives the tool-calling loop: the model may ask to run scoped queries,
// whose results are fed back until it produces a final text answer.
func (s *Server) runChat(ctx context.Context, identity auth.Identity, messages []ai.Message) (string, error) {
	tools := chatToolDefs()

	for i := 0; i < maxToolIterations; i++ {
		resp, err := s.AI.Complete(ctx, ai.Request{
			System:    chatSystemPrompt,
			Messages:  messages,
			Tools:     tools,
			MaxTokens: 1024,
		})
		if err != nil {
			return "", err
		}

		uses := resp.ToolUses()
		if len(uses) == 0 {
			return resp.Text(), nil
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
	return "I wasn't able to work that out — try asking in a simpler way.", nil
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
			Description: "Spending broken down by category for a month, largest first.",
			InputSchema: monthSchema,
		},
		{
			Name:        "top_merchants",
			Description: "The merchants you spent the most at in a month.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"month":{"type":"string"},"limit":{"type":"integer","description":"How many, 1-20"}}}`),
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
		out := make([]map[string]string, 0, len(rows))
		for _, c := range rows {
			out = append(out, map[string]string{"category": c.CategoryName, "spent": c.Total.StringFixed(2)})
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
		out := make([]map[string]string, 0, len(rows))
		for _, m := range rows {
			out = append(out, map[string]string{"merchant": m.Merchant, "spent": m.Total.StringFixed(2)})
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

func marshalTool(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
