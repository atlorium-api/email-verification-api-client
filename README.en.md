# Email verification API — validate an address and clean your mailing list

[Русский](README.md) · **English**

[![Live API tests](https://github.com/atlorium-api/email-verification-api-client/actions/workflows/examples.yml/badge.svg)](https://github.com/atlorium-api/email-verification-api-client/actions/workflows/examples.yml)
[![license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![API](https://img.shields.io/badge/API-Swagger-brightgreen)](https://atlorium.com/emailAPI)

Ready-to-run examples for the **email verification API** in six languages: **Python, TypeScript (Node.js), Go, Java, C#, PHP.**
**Verify an email address exists** without sending anything to it: the service talks to the domain's mail server and finds out whether it would accept a message for that mailbox — **no email is ever sent**. It also reports the MX record, whether the domain is **catch-all**, whether the address is a **disposable email**, a **spamtrap**, a complainer, or a role address (`info@`, `sales@`), and returns a typo suggestion for the domain (`gmial.com` → `gmail.com`).

Every example **runs out of the box — no signup, no key, no card.** A public demo key is baked in.

```bash
git clone https://github.com/atlorium-api/email-verification-api-client
cd email-verification-api-client/python && pip install -r requirements.txt && python main.py
```

> The demo key returns **realistic mock data**, not the result of a real SMTP probe. That is the point: you can write and test the integration before paying. Swap in a live key and the same code returns real verdicts.

---

## What it is for

**Mailing list cleaning** before a send, validating an address in a signup form, blocking disposable inboxes on trial signup, **bounce rate reduction**.

The examples do not just print JSON — they **apply** it. Each ships a `filterMailingList()` function that turns every address into a decision — `SEND`, `SEND_RISKY`, `RETRY_LATER` or `DROP` — and finishes by estimating the bounce rate you would have had if you had mailed the list as-is.

The single most valuable thing here is **spamtrap detection**. A spamtrap is an address nobody ever created and nobody ever subscribed: it is published deliberately to catch senders who mail purchased or scraped lists. The mailbox answers like a live one, so you cannot spot it yourself. A single hit damages your sending domain's reputation — and then **the entire campaign** lands in spam, including the mail to your real customers. That is why in these examples a spamtrap is not merely "one more DROP" but a separate, loud warning.

The decision logic, identical across all six languages:

| Status | Decision | Why |
|--------|----------|-----|
| `valid` | **SEND** | The mailbox exists and the domain accepts mail |
| `catch-all` | **SEND_RISKY** | The domain accepts mail for *any* address, so the existence of this specific mailbox cannot be established from the outside. Send at your own risk, ideally as a separate segment |
| `unknown` | **RETRY_LATER** | Greylisting: the domain's server did not answer. **Not billed** — just retry later |
| `invalid` | **DROP** | The mailbox does not exist; the message will hard-bounce |
| `do_not_mail` | **DROP** | Role address (`info@`, `sales@`) or a disposable inbox |
| `abuse` | **DROP** | Complainer: has previously marked mail as spam |
| `spamtrap` | **DROP + warning** | Destroys sender domain reputation. Remove unconditionally |

If `didYouMean` comes back, treat it as a **rescued contact, not a lost one**: the address has a typo in the domain (`gmial.com`) and the user can be asked to confirm the fix.

### Limits of the service — stated plainly

- It verifies **deliverability**, not whether a human actually reads the mailbox.
- For **catch-all** domains the existence of a specific mailbox cannot be determined at all — that is a limitation of SMTP, not of this service.
- Personal data about the mailbox owner (first name, last name, gender) is **not collected and not returned**.
- **No email is sent** to the address under test — not a test message, not anything.

## Quick start

Try the API without cloning anything:

```bash
curl -H "Authorization: Bearer ak_sandbox_demo_mockdata_v1" \
     "https://atlorium.com/api/EmailValidation?email=user@example.com"
```

| Language | Run | Requires |
|----------|-----|----------|
| [Python](python/) | `pip install -r requirements.txt && python main.py` | Python 3.10+ |
| [TypeScript / Node.js](node/) | `npm install && npm start` | Node.js 20+ |
| [Go](go/) | `go run .` | Go 1.22+ |
| [Java](java/) | `java Main.java` | JDK 17+ (no dependencies) |
| [C#](csharp/) | `dotnet run` | .NET 8+ |
| [PHP](php/) | `php main.php` | PHP 8.1+ |

Pass your own comma-separated list:

```bash
python main.py "anna@example.com,sales@catchall.example.com,x@unknown.example.com"
```

## Authentication

The key goes in the `Authorization` header:

```
Authorization: Bearer YOUR_KEY
```

| Key | Behaviour |
|-----|-----------|
| `ak_sandbox_demo_mockdata_v1` | **Demo key.** Public, shared by everyone. Returns mocks, charges nothing, needs no account. Responses are deterministic, so you can assert on them in tests. |
| Live key | A real SMTP probe. Get one at [atlorium.com](https://atlorium.com) |

Switching to a live key requires **no code changes** — every example reads an environment variable:

```bash
export ATLORIUM_API_KEY="ak_your_live_key"
```

Every sandbox response carries the header `X-Atlorium-Sandbox: true`, so mock data can never be mistaken for real data.

### Sandbox scenario domains

So you can exercise every branch of your code instead of waiting for the right status to turn up by chance, the sandbox returns a **fixed status per domain**:

| Domain in the address | Status | `subStatus` |
|-----------------------|--------|-------------|
| `invalid.example.com` | `invalid` | `mailbox_not_found` |
| `nodns.example.com` | `invalid` | `no_dns_entries` (no MX) |
| `catchall.example.com` | `catch-all` | — |
| `unknown.example.com` | `unknown` | `greylisted` |
| `spamtrap.example.com` | `spamtrap` | `possible_trap` |
| `abuse.example.com` | `abuse` | `global_suppression` |
| `disposable.example.com` | `do_not_mail` | `disposable` |
| `role.example.com` | `do_not_mail` | `role_based` |
| any other domain | `valid` | — |

The domains `gmial.com` and `gmai.com` additionally return `didYouMean`, which exercises the typo-suggestion branch.

## Endpoints

Base URL: `https://atlorium.com`

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/api/EmailValidation` | Verify deliverability of a single email address |

### `GET /api/EmailValidation`

| Parameter | In | Type | Description |
|-----------|----|------|-------------|
| `email` | query | string | The address to verify, e.g. `user@example.com` |

This is the only parameter the method takes.

## Response fields

| Field | Type | Meaning |
|-------|------|---------|
| `email` | string | The verified address, normalised |
| `status` | string | **The verdict.** `Valid`, `Invalid`, `CatchAll`, `Unknown`, `SpamTrap`, `Abuse`, `DoNotMail` |
| `statusRaw` | string | The status exactly as the source returned it: `valid`, `invalid`, `catch-all`, `unknown`, `spamtrap`, `abuse`, `do_not_mail` |
| `subStatus` | string \| null | Verdict detail: `mailbox_not_found`, `no_dns_entries`, `role_based`, `disposable`, `greylisted`, `possible_trap`, `global_suppression` |
| `account` | string \| null | The part before `@` |
| `domain` | string \| null | The part after `@` |
| `freeEmail` | bool | A free mail provider (gmail.com, mail.ru, yandex.ru, outlook.com) |
| `catchAllDomain` | bool \| null | The domain accepts mail for any address |
| `didYouMean` | string \| null | **Typo suggestion for the domain:** `gmial.com` → `gmail.com` |
| `domainAgeDays` | int \| null | Domain age in days. A very young domain is worth a second look |
| `smtpProvider` | string \| null | The domain's mail platform: `outlook`, `g-suite`, `zoho`, `yandex` |
| `mxFound` | bool | Whether an MX record exists. Without one the domain cannot accept mail at all |
| `mxRecord` | string \| null | The domain's primary MX record |
| `activeInDays` | string \| null | Mailbox activity signal. Optional — may be empty |
| `activeFirstSeen` | string \| null | When activity was first observed. Also optional — usually empty |
| `processedAtUtc` | date-time \| null | When the check ran, UTC. In the sandbox this is the current time, so it changes between calls — every other field stays stable |
| `elapsedMs` | int | How long the check took, ms |
| `deliverable` | bool | **Safe to mail.** `true` **only** when `status = Valid`: catch-all and unknown are uncertainty, not permission |

The response contains no other fields.

## Error handling

| Code | Cause | What to do |
|------|-------|------------|
| `400` | Address missing or syntactically invalid | Check the format. **This request is not billed** |
| `401` | Key missing, expired or invalid | Check the `Authorization` header |
| `402` | Insufficient credit balance | Top up at [atlorium.com](https://atlorium.com) |
| `429` | Rate limit exceeded | Retry with backoff — see the `Retry-After` header |
| `503` | Verification service temporarily unavailable | Retry later. **You are not charged for our failures** |

All six examples map these codes to human-readable causes — see the `AtloriumError` class.

## Pricing and limits

**Pay-as-you-go, no subscription** — you pay per successful request. Current prices and limits: [atlorium.com/pricing](https://atlorium.com/pricing).

An `unknown` outcome (greylisting, the domain's server did not answer) is **not billed**: you pay only for a meaningful verdict. Neither is a `400` on a malformed address.

Current prices and limits: **[atlorium.com/pricing](https://atlorium.com/pricing)**

**About the limits — plainly.** They are enforced **per IP** and for this service they are tight — and they are identical for the demo key and a live key: the sandbox shows you exactly the terms you get after paying. A list of four addresses will certainly hit `429` — and the examples **handle that as a normal condition**: they wait and retry (see `retry_after` / `retryAfter`). So a run takes about a minute. That is not a bug; it is an honest demonstration of the product's real limits — the sandbox hands out exactly the limits a paying user gets. Run the examples back to back and you can exhaust the hourly limit too, at which point the example says so plainly and exits rather than hanging.

## FAQ

**Do you send an email to the address being checked?** No. The service talks to the domain's mail server and infers from its reply whether it would accept a message for that mailbox. Nothing is delivered — the owner of the address sees nothing.

**Why `catch-all` instead of just valid or invalid?** Because the domain is configured to accept mail for **any** address and answers "accepted" even for a mailbox that does not exist. This is an SMTP limitation: from the outside it is simply impossible to establish whether a specific mailbox exists on such a domain. The honest answer is "uncertain", not invented confidence.

**What is a spamtrap and why does it matter more than everything else?** It is an address nobody created and nobody subscribed — published on purpose to catch senders mailing purchased or scraped lists. The mailbox answers like a live one, so you cannot identify it yourself. One hit damages your sending domain's reputation, after which the whole campaign goes to spam. This is the reason the service exists.

**Am I charged for `unknown`?** No. If the domain's mail server greylisted, timed out or blocked the probe, there is no verdict — and you are not billed. Retry later.

**What should I do with `do_not_mail`?** Remove it from the list. It is either a role address (`info@`, `sales@`), read by a department rather than a person and the fastest source of spam complaints, or a disposable inbox created to last ten minutes.

**How much does this actually cut bounce rate?** Every `invalid` leaves the list — those are exactly the addresses that hard-bounce. Mailbox providers treat a bounce rate above roughly 2 % as a sign of a dirty list and start throttling deliverability, so the cleanup pays for itself on the very first campaign.

**Do I need to register to try it?** No. The demo key is public and works without an account — but it returns mocks, not the result of a real SMTP probe.

## Other Atlorium APIs

The same account and the same key also give you:

- [AI chat](https://github.com/atlorium-api/ai-chat-api-client) — models, sessions, text summarization
- [Phone validation](https://github.com/atlorium-api/phone-validation-api-client) — format, line type, range operator
- [Image OCR](https://github.com/atlorium-api/image-ocr-api-client) — extract text from an image or Base64
- [Address standardization](https://github.com/atlorium-api/address-standardization-api-client) — parse a string into components, quality score
- [EGRUL/EGRIP](https://github.com/atlorium-api/egrul-api-client) — Russian company check by INN/OGRN: status, address, capital
- [GAR/FIAS addresses](https://github.com/atlorium-api/gar-fias-address-api-client) — search and suggestions from the official registry

Full catalogue: [atlorium.com](https://atlorium.com)

## Links

- **API reference (Swagger):** [atlorium.com/emailAPI](https://atlorium.com/emailAPI)
- **Service description:** [atlorium.com/emailDescription](https://atlorium.com/emailDescription)
- **Web UI:** [atlorium.com/emailGUI](https://atlorium.com/emailGUI)
- **OpenAPI spec:** [email_en-US.json](https://atlorium.com/openapi/email_en-US.json)
- **Support:** support@atlorium.com

## License

[MIT](LICENSE)
