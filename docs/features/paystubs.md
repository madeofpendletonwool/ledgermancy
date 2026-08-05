# Paystubs

What you earned before anything came out of it.

Everything else in Ledgermancy starts from a bank transaction, and a bank
transaction is what *survived*. By the time your pay reaches an account, federal
and state tax, Social Security, Medicare, your 401(k) contribution, your health
premium and your HSA have already been taken out of it. For a typical W-2 earner
that is **30–45% of gross income** the rest of the app has never been able to
see.

That gap makes three questions unanswerable, and they are not small ones:

- Your savings rate is measured against what landed in the bank, not against
  what you earned. That is a flattering number and a different one.
- The [retirement projection](retirement.md) cannot see your 401(k)
  contributions or your employer's match — usually the largest wealth-building
  flows in the household.
- "What is my effective tax rate" and "am I on track to max my 401(k)" have
  every input except the one that matters.

This page closes that. It is a **companion** to tax filing, not a filer: the
output is a report over your own records, and nothing here is filed with anyone.

## Adding a paystub

Two ways, and the second always works.

### Import a PDF

If your employer gives you a PDF stub — ADP, Gusto, Paychex, UKG and most others
do — drop it in and the figures are read off it.

**Nothing is sent anywhere.** A generated paystub already contains its text, in
a fixed layout, so the app pulls it out on your own machine with no network call
and no AI model. The file is not stored either; it is read in memory and
dropped. If you *want* the PDF kept, upload it to the [document
vault](documents.md) first and parse it from there — same local read, and the
stub links back to the file.

A **scanned or photographed** stub has no text to read. The app says so and asks
you to type it in rather than sending an image off the host. That is deliberate:
a paystub carries your salary, your employer and usually your Social Security
number, and it is more sensitive than the tax documents the receipt reader
already refuses to send anywhere.

What comes back is a **draft**. Every figure is a suggestion until you have
checked it against the stub.

### Type it in

Every line type is on the form — six kinds of tax, retirement in four flavours,
HSA and FSA, health, dental, vision, life, disability, commuter, dependent care,
garnishments and a catch-all. A paper stub, an unusual employer or a payroll
system nobody has heard of is fully capturable.

## Confirming, and why the app is fussy about it

A paystub counts for **nothing** until you confirm it. Not the savings rate, not
the tax summary, not your contribution totals. Until then it sits in a review
queue at the top of the page.

Confirming also requires the stub to **balance**:

> gross − everything deducted = net

within a cent. The form shows the gap live while you type, so a missing line is
obvious before you try to save. A stub that does not reconcile can be kept as a
draft indefinitely — but it cannot be confirmed, because a mis-entry stored
silently puts its gap into every figure derived from it.

Your employer's contributions — the 401(k) match, employer-paid premiums — are
recorded separately and are correctly *not* part of that equation. They are
money added on top of gross, not taken out of it.

## What you get

**Where your paycheck went.** Gross flowing left to right through taxes,
retirement and benefits into what actually landed. Most people have never seen
their own pay drawn this way, and it is the reason to use this feature at all.

**Effective tax rate.** Income tax and FICA over gross, per period and for the
year. Only genuine taxes count towards it — a health premium is money gone, but
calling it tax would produce a number that reads far worse than reality.

**Contribution room.** How much of your 401(k), IRA and HSA limits you have used
for the year, and what to defer per remaining paycheque to land exactly on the
cap. Two things this gets right that are commonly got wrong:

- A traditional and a Roth 401(k) share **one** limit, and so do a traditional
  and a Roth IRA. They are pooled, not counted separately.
- **Two jobs in one year share one limit too.** Each employer's year-to-date
  figures restart at zero on the new job, so adding them up naively reports
  roughly twice the room you have. They are pooled against a single cap, and
  going over is shown loudly — an excess deferral has to be withdrawn before the
  filing deadline or it is taxed twice.

If the app does not have the IRS limits for a year yet, it says so rather than
measuring you against another year's numbers.

**Total compensation.** Gross plus everything your employer paid on top of it —
usually 10–25% above the salary you would quote.

**Your real savings rate.** Measured against gross, shown next to the existing
figure rather than replacing it. The existing one is not wrong; it answers a
different question, and both are worth seeing.

## Matching the deposit

Link a stub to the bank deposit it produced and the pre-tax record and the
post-tax transaction stop being able to drift apart.

The app **proposes**, ranked by how close each deposit is to the stub's net pay,
and you pick. It never links one for you. Two earners in a household with the
same take-home is common, and a wrong match corrupts both records at once. A
deposit already claimed by another stub is not offered again.

A direct deposit split between checking and savings shows up as a partial match
with the gap displayed, rather than as no match at all.

## Privacy inside a household

**Paystubs are private by default.** This is the one place the app inverts its
usual sharing rule: linked institutions and vault documents are shared with the
household unless you say otherwise, and a salary is the opposite. The other
member of your household learning what you make should be a decision you made,
not a default you never saw.

Sharing a stub makes it *visible*. It does not make it editable — confirming,
editing and deleting stay with the person whose pay it is.

An employer's EIN is encrypted with the same key as everything else sensitive
here, shown as `**-***6789` everywhere except the tax summary, and anything that
looks like a Social Security number is stripped out of imported text **before**
it is stored, not merely before it is shown.

## Tax summary

At the end of a year, your confirmed stubs are added up and laid out the way a
W-2 is: wages, federal withheld, Social Security and Medicare wages and tax, and
box 12 codes for 401(k), Roth and HSA. Together with your [document
vault](documents.md), that is most of what an accountant asks for, already
assembled.

**It is not a W-2**, it has not been filed with anyone, and figures may differ
from the form your employer issues. The disclaimer travels with the data, not
just on screen, because this gets printed and emailed away from the page that
framed it.

One detail worth knowing if you compare it to a real W-2: box 1 and boxes 3/5
will differ if you contribute to a 401(k), and that is correct. A 401(k)
deferral comes out before income tax but **not** before Social Security and
Medicare.

## Out of scope

Ledgermancy does not file anything, generate forms, or give tax advice. It does
not model state-specific tax rules beyond recording what your stub says, and it
does not predict future withholding. It records what happened and adds it up
exactly.
