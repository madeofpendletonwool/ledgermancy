# Documents

The encrypted vault: receipts, tax returns, warranties, insurance policies and
contracts, stored next to the financial records they belong to.

Everything else in Ledgermancy is about *transactions*. The paperwork that
explains them normally lives somewhere else entirely — a NAS, a cloud drive, an
email inbox — and none of it is next to the charge it justifies. This is the
part of self-hosting a cloud product structurally cannot match: your documents
are encrypted with your own key on your own disk, and they are one click from
the transaction they belong to.

## What it stores

Any file, up to the per-file limit your deployment sets (25 MB by default),
filed under one of seven types:

| Type | For | Kept until |
| --- | --- | --- |
| **Receipt** | Purchases, returns, warranty claims | 3 years from the document's date |
| **Tax** | Returns, W-2s, 1099s | 7 years |
| **Warranty** | Appliance and vehicle cover | 3 months past expiry |
| **Insurance** | Policies and renewals | 12 months past expiry |
| **Contract** | Leases, agreements, closing paperwork | 7 years |
| **Statement** | Anything a bank sends on paper | 7 years |
| **Other** | Everything else | 7 years |

The "kept until" date is **advice, and only advice**. Nothing is ever deleted
for you, on any schedule, for any reason. The date exists so the app can say
"you can probably let this go", which is a decision you make.

## Filing something

Add a document from the **Documents** page, or straight from the record it
belongs to — every transaction row, manual asset and goal has a paperclip that
uploads and attaches in one step. That second path is the common one for a
receipt, which exists only because of the charge it explains.

A document can be attached to any number of records, or to none. A tax return
belongs to a year, not to a transaction, and standalone is a perfectly normal
state.

Give a warranty or a policy an **expiry date** and the app will raise it in your
[insight feed](insights.md) as it approaches — a month out, and again more
urgently inside a week. A warranty you discover has lapsed is worth nothing;
this is the payoff for having filed it at all.

## Privacy inside a household

Documents are shared with the household by default, matching how linked
institutions work. Tick **Keep this private** and the document becomes invisible
to every other member — it does not appear in their listing, their attachment
counts, or their downloads. A divorce decree or a medical receipt is exactly the
kind of thing that lands in a vault.

Private documents are also kept out of the insight feed, which is a household
surface.

## Encryption, plainly

Documents are sealed with AES-256-GCM under your `ENCRYPTION_KEY` — the same key
that protects your bank connections — before anything is written to disk. The
storage backend never holds readable content.

Two consequences to be clear about:

**Losing `ENCRYPTION_KEY` loses the vault.** Not "makes it inconvenient" —
loses it. Bank connections can be relinked; a tax return whose key is gone is
gone. Back the key up somewhere other than the server.

**The documents are not in your database dump.** `pg_dump` captures every
title, type and expiry date and none of the contents. The bytes live on a
separate volume that needs backing up alongside it. See
[DEPLOYING.md](https://github.com/madeofpendletonwool/ledgermancy/blob/main/DEPLOYING.md)
for the runbook.

## Reading a receipt (optional)

If your deployment sets `DOCUMENTS_OCR_ENABLED=true` and has an AI provider
configured, a document **filed as a receipt** gains an **Extract fields** action
that reads the merchant, total and date off it, then offers the existing
transactions that amount and date could belong to. Attach it to the right one in
a click.

### Receipts, and only receipts

Nothing else in the vault can be sent. A tax document, an insurance policy, a
contract, a statement, a warranty or anything sitting in `other` is refused by
the API — before the file is even decrypted — regardless of what format it is
in. A W-2 you scanned to a PNG is exactly as ineligible as a PDF of one.

That line is drawn where it is because the two cases are not comparable. A
receipt is a merchant, a total and a date, all of which already exist in the
transaction it belongs to; there is very little on it your ledger does not
already know. A tax return is your name, your address, your SSN and a complete
picture of your finances. Those should never be governed by the same switch, and
here they are not.

If a document genuinely is a receipt, refiling it as one opts it in. That is the
intended escape hatch, and it is a decision you make rather than a default you
inherit.

### Scan first, match later

The obvious flow — photograph the receipt at the register, before the charge has
posted — is the one that has to work, so it does.

The reading is stored on the document, so the match is re-checked every time you
open the receipt and again by a background pass as new transactions arrive. When
the charge finally posts, it shows up in your insight feed: *"Your receipt for
$84.20 looks like the Costco charge that has now posted."* Attach it in a click.

Matching compares the amount (within a couple of cents) and the date, within a
few days either way. It uses the **card authorisation date** where your bank
reports it — the day you actually swiped, which is what is printed on the
receipt — so a charge that posts three days late still lines up exactly rather
than relying on the window.

Pending charges are deliberately excluded. A pending row is replaced by a
different row when it posts, and a receipt attached to the pending one would
have that link silently disappear a few days later.

The feed only speaks when there is exactly one candidate. Two charges for the
same amount in the same week is genuinely ambiguous — and quietly guessing is
how a receipt ends up filed against the wrong charge. The Documents page shows
you the full list in that case.

### It only ever suggests

There is no button that turns a model's reading into a transaction, and that is
deliberate: a model can misread 84.20 as 8420, and the cost of that should be
one correction rather than one wrong row in a ledger you rely on. A field it
could not read comes back empty rather than guessed. The only thing you can act
on is attaching the document to a transaction *you* recognised in the list.

Extraction happens only when you press the button on one specific document, and
only once — the stored reading means re-matching later costs nothing and sends
nothing. No background job uploads anything, ever.

This is the one feature that sends your data off the host, which is why it is
off by default and has its own switch rather than riding along with
`AI_API_KEY`. See
[Security](../security.md#receipt-ocr-sends-images-off-the-host).

## Storage limits

The Documents page shows what the household has stored against its quota, and
states the per-file limit up front rather than making you discover it. Both are
set by whoever runs the deployment; see
[Configuration](../configuration.md#document-vault).

## Not included

- **Search inside documents.** Titles and filenames are searchable; contents
  are not. Full-text search over a vault wants an index and OCR over
  everything, which is separate work.
- **Version history.** One document, one file. A document that needs different
  contents is a new document.
- **Sharing outside the household.** There is no external sharing model, and
  the vault is not the place to invent one.
