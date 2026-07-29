# Households

![Household](../screenshots/household.png)
<em>Settings → Household: people, members, and invites.</em>

Ledgermancy is built around a **household**: the first account you register
creates it, and everyone after joins by invitation. This keeps the app a private
ledger rather than an open sign-up form on the public internet.

The Household tab lives under **Settings** (old `/household` links redirect
there).

## People and logins are different things

This is the distinction the household model is built on, and it is worth getting
straight because everything else follows from it:

- A **person** is someone in the household that money can be *about*. They have a
  name and a birthdate. A newborn with a 529 is a person. They need no
  credentials and may never sign in.
- A **login** is a set of credentials with a permission level. Adults have one.
  A teenager may be given one — deliberately, by a parent. A toddler will not be.

Every login belongs to a person. **Not every person has a login.** That is why a
child can hold a 529 without fabricated credentials sitting in the auth table.

The **People** list holds everyone in the household — adults, kids, and anyone
money is for. Members with logins appear alongside people who have none.

## Roles

A login has one of three roles, enforced **server-side on every route** (a
client-side check is decoration):

| Role | Can |
| --- | --- |
| **Owner** | Everything a member can do, plus household administration and invites |
| **Member** | Full adult access — the behaviour the app has always had |
| **Child** | Sign in, see their own allowance balance and ledger, record their own spending, see their own goals, and read the balance of accounts where they are the beneficiary |

A child **cannot** see household net worth, anyone else's transactions, accounts,
goals or allowance, the household split ledger, or any projection over household
assets; cannot link or unlink institutions, invite, or change settings. The role
check is mounted on route groups and asserted for every route, so adding one
means deciding which group it belongs to — there is no third option.

## Invites

To add an adult (or enable a login for an existing person):

1. **Create an invite** — returns a one-time token **once**.
2. Send the link to the person.
3. They register with it and join the household.

- An invite is **bound to the address it was issued for**, so an intercepted link
  cannot be redeemed under a different email.
- An invite now carries the **role** it grants, and optionally the **person** it
  attaches to — that is how "enable a login for Ellie" works without creating a
  second Ellie.
- Pending invites are listed and can be **revoked** before they're used.

A child login still needs a **real, deliverable email** — use a plus-address you
control (`parent+ellie@gmail.com`), not a synthetic one. Password reset and every
security notification would fail silently against an address that does not exist,
and that failure surfaces at the worst possible moment. A child is not pushed
toward MFA enrolment.

## Whose money is it

An account or manual asset can name the **person it is for** — a 529's
beneficiary, the minor on a UTMA, the child whose custodial Roth it is. This is
the *529* sense of beneficiary (whose money this is), **not** payable-on-death;
the two mean opposite things about who controls the account today, and the label
says so.

This is deliberately **not joint ownership**. A joint checking account has two
adult owners and this field cannot express that; adult accounts keep working
exactly as they always have, shared through the institution toggle below. Leave
it blank for anything that is not held for a specific person.

**Custodial accounts are segregated from the retirement nest egg.** A 529,
UTMA/UGMA, Coverdell, custodial Roth, and the Trump account fund a dependent
rather than the household's retirement — a UTMA is irrevocably the child's
property from the moment it is funded — so they are excluded from the
[Retirement](retirement.md#account-treatment) projection rather than counted in
it.

## Visibility & sharing

What each member sees follows one rule: **your own items ∪ household items where
`is_shared`**. Every query in the app uses the same shape.

Sharing is controlled **per institution** on the [Accounts](accounts.md) page —
the **Shared with household** toggle on each institution card. This lets one
spouse keep an account private while everything else rolls up to the household.

| Setting | Scope | Effect |
| --- | --- | --- |
| Shared institution | Institution | All its accounts visible to the household |
| Personal goal / bill | Goal / obligation | Tracks for one person only, not the household |

## Shared goals

A goal's scope is **household**, **personal** (`user`), or **for a person** — the
last is how a child with no login can still have a bike fund. Progress for an
account-linked goal still derives from the balance; a **contribution** records
*who funded what* and when, so a shared household goal can show each member's
share without conflating attribution with the balance. See [Goals](goals.md).

## Shared expenses and bill split

The **Shared** page (`/shared`) is the household ledger: who paid for what, and
who owes whom.

- **Splitting a transaction is an attribution overlay.** The charge still
  happened once, and household spending is unchanged by splitting it — the copy
  says so, because a user who thinks splitting reduces their spend will misread
  every report. Shares are stored as exact amounts, so they always sum to the
  transaction with no rounding drift.
- The **household ledger** reduces "who paid for what this month" to a glance:
  each member's running balance, with **settle** actions that record a
  reconciliation without moving any money. The app never pays anyone.

You can split a charge from the [Transactions](transactions.md) row or from the
Shared page.

## Kid accounts and allowance

A child's view is a teaching surface, not a second copy of the household
finances. An **allowance** is a schedule (amount + cadence, optional monthly
limit) plus a **ledger** of entries — allowance, chore, gift, spend, correction —
that a child watches go up when they earn and down when they spend.

- The balance is **derived** (a sum of entries), never stored, and is **play
  money** — it is not reconciled against any real account, and the UI does not
  let a child think it is.
- Scheduled credits are **not** auto-posted by default: money appearing without a
  parent's action is the wrong default for a tool whose point is teaching where
  money comes from. A parent posts them, or turns auto-post on deliberately.

## Your profile and your age

A **Profile** tab lets you set your own name and birthdate without opening the
Household page (`PUT /api/me/person`). The birthdate matters beyond kids: ages
throughout the app used to be integers typed once and wrong every year after.
They are now **derived from a birthdate** where one exists — driving the
[529 horizon](retirement.md), age-gated catch-up contribution limits, and
"when you're 65" rather than "in 30 years."

Resolution order, everywhere an age is needed: the linked person's birthdate aged
to today → the stored integer → *unknown*. An existing install is unaffected
until somebody enters a birthdate: the backfill leaves every current person's
birthdate NULL, a NULL birthdate falls through to the stored integer, and the
projections come out byte-identical until then.

## Removing a member

Revoke a member's sessions from the [Security](../security.md) page if needed;
household membership and roles are managed here. Revoking a login does not delete
the person their 529 and their goals point at. Because an invite is address-bound
and sessions are server-side, there's no client-side loophole for leftover
access.
