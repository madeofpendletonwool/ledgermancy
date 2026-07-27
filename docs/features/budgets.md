# Budgets

![Budgets](../screenshots/budget-goals.png)
<em>What you planned to spend, and how the month is tracking.</em>

Budgets let you set a target per spending category and track the month against
it. Pick a month at the top and the page recalculates.

## Safe to spend

The headline card. It's the household's free-to-spend figure, shown with its
full breakdown so the number is legible rather than magic:

```
Typical income
  − Fixed bills
  − Budgeted discretionary
  − Goal contributions
  = Safe to spend
```

It's household-level and based on **typical** income, so it does not track the
month selector above. With less than three months of income history, a note
calls it a rough estimate.

When safe-to-spend is positive, a hint suggests assigning it to budgets or goals
to reach a zero-based plan where every dollar has a job.

## Category budgets

Each budget shows spent-vs-available with a progress bar, turns red when over,
and shows the over/remaining amount. **Edit** to change the amount, period, or
rollover; **Delete** to remove it.

Summary tiles above total **Budgeted**, **Spent**, and **Remaining** for the
month.

### Periods and rollover

- **Period:** weekly, monthly, or yearly.
- **Rollover** *(monthly only)*: unspent amount rolls into next month. A budget
  with rollover shows a `+ carried` chip and measures against `available`
  (amount + carryover) rather than the raw amount.

## Suggest budgets

Propose a round target for each spending category from your last year of
spending. Click **Suggest budgets**, then review, tweak, and deselect before
applying:

- **Flexible** categories are listed first.
- **Fixed costs** are listed separately — usually budgeted at their actual
  amount.
- A proposal is pre-checked unless that category already has a budget that
  already covers its average.
- Targets are **rule-based** by default, **AI-tailored** when AI is on (anchored
  on your exact averages with a short rationale per row).

Applying runs the same single-write path as manual entry, so validation and
audit are identical.

## Add a budget

Set a weekly, monthly, or yearly target for any spending category. Income and
transfer categories can't be budgeted. A duplicate-category guard means "Add
budget" never offers one that already exists.

**Percentage mode** (monthly only) sizes a budget as a share of typical income
for convenience; the stored value is still the resulting dollar amount — handy
for zero-based / percentage allocation.
