# Schedule

<em>What's due, when, and what it leaves in the bank.</em>

Schedule is the forward-looking view: every bill the app knows about, on a
calendar, with today's cash balance carried forward through them. It is the
answer to "can I make rent on the 10th" before the 10th arrives — the most
common personal-finance failure mode, and the one this app already held every
input to prevent.

Everything here is derived from **known obligations**. It is not a forecast of
discretionary spending, and it says so plainly rather than dressing a guess up as
a number.

## Where the bills come from

Two sources, unified into one list:

- **Detected, from your transactions.** The recurring detector already finds
  subscriptions and repeating charges; those are promoted into tracked bills
  (`source: detected`). Re-detection updates a detected bill in place rather than
  duplicating it, and **edits you make are kept** — a cadence you corrected is
  not clobbered the next time charges are scanned. A merchant you marked *not
  recurring* is never promoted.
- **Entered by hand.** This is the only path for the bills a bank feed cannot
  show: an annual insurance premium, biennial dues, anything paid by cheque or
  an undifferentiated ACH. It is a first-class section, not buried behind a menu,
  because detection will never find these.

## The list and the calendar

Pick a horizon — **next 30 / 60 / 90 days** — and the page shows every due
occurrence in that window with its total. Each bill carries its label, due date,
cadence, and amount, alongside a month-grid calendar with each bill on its due
day.

Dates are derived, never stored. A bill is an anchor date plus a cadence
("every N days / weeks / months / years"); every later date is counted from it.
That is why a monthly bill anchored on the 31st lands on the last day of shorter
months — month-end clamping happens in Postgres, where interval addition gets it
right, rather than in Go's `time.AddDate` which does not.

## Projected balance

Today's cash balance, minus each bill as it falls due, drawn forward across the
horizon. A **Lowest point** tile names the day the line bottoms out, and if any
account is projected to run out, a banner says so outright — *and notes that it
is before any day-to-day spending*.

Three things this deliberately does not do:

- **Only depository accounts are projected** (checking, savings). Running the
  projection over a credit card would subtract the card's own bills from the
  balance they make up, which is nonsense.
- **Known obligations only.** The line is the floor, not a prediction of what you
  will actually spend. A discretionary forecast would be a guess wearing a
  number's clothes.
- **Bills that name no account stay out of a single account's line** (they are
  still in the *All cash accounts* total), and the page says so — quietly
  understating the drop is worse than naming the gap.

!!! info "Assign an account where you can"
    A bill only lands on an account's projected line when it names that account.
    Unassigned bills still count in the combined total, but a per-account line
    without them understates the real drop — so naming the account a bill is paid
    from is worth the second it takes.

## Add a bill

| Field | What it does |
| --- | --- |
| What is it | A label — "Car insurance" |
| Amount | One occurrence, as a decimal |
| First (or most recent) due date | The **anchor**. Every later date is counted from this. |
| Repeats every | A count and a unit — days / weeks / months / years |
| Category *(optional)* | Keeps "safe to spend" from counting this bill twice — see below |
| Paid from *(optional)* | The account whose projected line this bill lands on |
| Stops after *(optional)* | An end date; leave blank for open-ended |
| Just for me | A personal bill, kept out of the household view |

A detected bill offers **Stop tracking** rather than delete; a bill you entered
offers **Delete**. Either way the cadence is editable when the detection got it
wrong.

## How it connects to the rest of the app

One set of rows backs the calendar, the list, the projection, the insight, and
the budget figure — so they can never disagree about a due date.

- **[Budgets → Safe to spend](budgets.md#safe-to-spend).** A known bill is
  subtracted from this month's free cash **once**. The rule that makes that safe:
  a fixed cost counts once, either as its remaining unpaid obligation (where one
  is tracked) or as the trailing typical figure for a category with no obligation
  coverage — never both. **That is why the category field matters:** it is how the
  page knows a bill and a budgeted category are the same money.
- **[Insights](insights.md).** A bill due within a week raises an `upcoming_bill`
  insight. Only the largest few become feed rows each — a dozen small bills is
  what the calendar is for — and a big one (a mortgage) is high enough priority to
  push to your phone.
- **[Alerts](alerts.md).** A **predicted-low-balance** alert fires when a
  projected balance is set to cross a floor you choose within a number of days —
  the forward-looking counterpart to the *Low leftover* rule.

## Not included

- **Paying bills.** The app never moves money.
- **Predicting discretionary spending.** Known obligations only.
- **Raising Plaid's history window.** A detected cadence is only as deep as the
  transaction history behind it; for institutions that cap history, the
  [CSV importer](transactions.md#importing-a-csv) is how you backfill further.
