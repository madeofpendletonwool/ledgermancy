# Assistant

![Assistant](../screenshots/assistant.png)
<em>Ask about your money; every figure comes from your own data.</em>

The Assistant is a chatbot over your own financial data. It only appears when a
provider is configured (`AI_API_KEY`). Ask about spending, subscriptions, or
trends; answers stream back as Markdown.

## How it answers

The Assistant uses **tool-calling over read-only, household-scoped queries —
not RAG.** The model gets tools like `spend_by_category(month, scope)`,
`transactions_search(filters)`, and `budget_status(month)`, each backed by an
existing query scoped to your household visibility. It composes an answer from
real query results.

This is materially more accurate and auditable than embedding retrieval for
financial questions, and it reuses the reporting layer that is already correct.
The model never sees raw rows it didn't ask for, and every figure is one a
report could reproduce.

!!! info "Visibility is always scoped"
    Every tool inherits the same `WHERE u.household_id = $1 AND (i.user_id = $2
    OR i.is_shared)` shape the rest of the app uses. A tool that forgot this
    would leak a spouse's private account — so the scoping is enforced in the
    tool layer, not left to each caller.

## Suggestions

The empty state offers high-value prompts that show what the assistant is
*actually* good at — dynamic, one-off questions the dashboard can't answer at a
glance:

- "How many times did I eat out in July? Give me a breakdown"
- "Is my dining spending up vs last month?"
- "List my subscriptions and what they cost"
- "What are my biggest merchants this month?"

(Deliberately not "what's my net worth" — the app already shows that.)

## Disabling

If `AI_API_KEY` is blank, the Assistant page explains that a provider is needed
and the nav item is hidden. Every other feature works unchanged. See
[Configuration → AI](../configuration.md#ai-provider).

## Provider

The provider is any **Anthropic Messages API-compatible endpoint** — GLM,
Claude, or anything else speaking that wire format — selected via a configurable
base URL. Nothing is hardcoded to `api.anthropic.com`.
