# Financial Summary

![Financial Summary](../screenshots/report.png)
<em>A one-click, print-styled trailing-twelve-month report.</em>

**Report** in the nav produces a one-click **Financial Summary** covering the
trailing twelve months: current position, cash flow, savings rate, per-category
averages and annual totals, month-by-month history, debt with rates, and a
labelled projection.

## Exporting

There is deliberately **no** server-side PDF renderer. Instead, the page is
print-styled so the browser produces a better document than a PDF library would,
with no headless-Chrome dependency in the container, and what is on screen is
exactly what prints.

| Button | What you get |
| --- | --- |
| **Print / Save as PDF** | The report as a PDF via your browser's print dialog |
| **Transactions CSV** | Every transaction in the window |
| **Categories CSV** | Per-category average and total |
| **Net worth CSV** | The daily net-worth history |

### CSV conventions

The transaction amounts are **sign-flipped** from Plaid's convention so negative
means money out — summing the column in a spreadsheet gives the right answer
without a formula. The `is_transfer` and `is_income` flags are included so a
spreadsheet can reproduce the app's totals rather than guess which rows to
exclude.

See [Concepts → CSV exports](../concepts.md#csv-exports) for why.

## What's on the page

- **Position today** — assets, liabilities, net worth, with the cash /
  investments / manual-asset / credit / loan breakdown.
- **Cash flow, trailing 12 months** — income, spending, left to invest, savings
  rate, and the fixed vs. discretionary split.
- **Spending by category** — average per month, total per year, with type.
- **Month by month** — income / spending / leftover for each of the last twelve
  months.
- **Debt** — account, type, rate, balance (if you have liabilities linked).
- **Outlook** — a labelled projection at 1 / 5 / 10 years, with assumptions
  echoed back. An optional AI narrative appears above the numbers when AI is on;
  it **supplements** the numbers and the caveat, never replaces them.

## The footer caveat

Every report carries a footer noting that figures come from linked account data,
that **transfers between own accounts — including credit-card payments — are
excluded** from both income and spending, and that projections are
illustrations, not forecasts. That text travels with the document on the page,
not just in the UI around it.
