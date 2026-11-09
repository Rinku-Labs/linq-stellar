# Deploying to Koyeb

Two services from one image: `server` answers HTTP, `worker` settles payments.
They are split so a slow Horizon call in settlement can never stop the API
answering, and so they can be scaled independently.

---

## Before you start

Four things must be true, or the service starts and then fails on real money.

**1. A funded sponsor account.** Its seed becomes `STELLAR_SPONSOR_KEY`. It pays
the reserves for every deposit account and gets them back when the account is
merged, so it does not drain — but it needs a working balance. Around 50 XLM
comfortably covers a few hundred concurrent orders plus fees.

**2. A treasury account that already holds a USDC trustline.** Swept USDC lands
here. Without the trustline every sweep fails *after the merchant has already
been paid*, which is the worst moment to discover it. The worker warns at
startup; check the log line.

**3. A Postgres database.** Koyeb's managed Postgres is fine. The service
migrates its own schema on boot.

**4. `/internal/payout` deployed on the Linq backend, with a secret set.** The
endpoint lives on branch `feature/stellar-internal-payout` (PR #44), which is
**not deployed yet** — `POST /internal/payout` on the running backend answers
`404`, the same as any path that was never registered. Separately,
`INTERNAL_API_SECRET` is not set on the backend either: the pre-existing
`POST /internal/status` route, which the same secret gates, currently answers
`503 {"error":"Internal API not configured"}`, and no such secret exists in the
Koyeb project at all. Both are needed — the route deployed and the secret set —
before either endpoint will do anything. Until they are, deposits are detected
and swept correctly but **no Naira moves**.

---

## Environment variables

Both services take the same variables, with one exception noted below. Set
them once per Koyeb service.

### Required — the service refuses to start without these

| Variable | What it is | Where to get it |
|---|---|---|
| `DATABASE_URL` | Postgres connection string | Koyeb Postgres, connection details |
| `STELLAR_SPONSOR_KEY` | Sponsor account **secret** seed (`S...`) | The keypair you funded above |
| `STELLAR_TREASURY_WALLET` | Treasury **public** key (`G...`) | The account with the USDC trustline |
| `ENCRYPTION_KEY` | Encrypts deposit-account seeds at rest | Generate: `openssl rand -hex 16` (gives 32 chars) |
| `LINQ_API_URL` | The Linq backend's base URL | `https://confidential-brianna-uselinq-52e2b233.koyeb.app` |
| `LINQ_INTERNAL_SECRET` | Shared password with the Linq backend | Generate one, and set the **same value** as `INTERNAL_API_SECRET` on the Linq backend |
| `ORDERS_API_KEY` | **`server` only.** Shared secret callers send as `X-API-Key` to `POST /orders` and `GET /orders/{id}` | Generate: `openssl rand -hex 32`, and give the same value to whatever calls this service to create orders |

`ORDERS_API_KEY` guards the only two routes that cost the sponsor money
(provisioning an account) or return order details — `/healthz`, the trustline
preflight, `stellar.toml` and SEP-10 all stay open to anyone, as designed. The
`worker` binary never serves HTTP, so it neither needs nor reads this
variable; setting it there is harmless but does nothing.

### Public API (`server` only, both optional)

| Variable | Default | What it is |
|---|---|---|
| `PUBLIC_API_ENABLED` | off | Serves `/v1` and `/admin` when exactly `true`. Orders taken on `/v1` move money, so this is the off switch: unset it and redeploy, and those routes stop existing whatever keys were issued. The legacy `/orders` routes do not depend on it |
| (no variable) | | With the switch on, the integrator guide is served at `/docs` and the spec at `/docs/openapi.yaml`, unauthenticated. Sources are in `docs/api/`, embedded in the binary |
| `ADMIN_API_KEY` | empty | Sent as `X-Admin-Key` to `/admin`. Empty refuses every admin request. Generate: `openssl rand -hex 32` |
| `DB_MAX_OPEN_CONNS` | `20` | Connections the server may hold to Postgres, per instance. Keep instances x this below the database's `max_connections`, leaving room for the worker |

Setting up an integrator (the full key appears once, in the second response):

```bash
curl -X POST $HOST/admin/integrators -H "X-Admin-Key: $ADMIN_API_KEY" \
  -d '{"name":"Acme"}'   # optional "rateLimitPerMinute"
curl -X POST $HOST/admin/integrators/$INTEGRATOR_ID/keys -H "X-Admin-Key: $ADMIN_API_KEY"
```

`POST /v1/orders` takes one of `amountNgn` or `amountUsdc` and no `rate`. This
service prices the order from `GET /b2b/rate` on `LINQ_API_URL`, reused for up to
30 seconds, and returns the rate it used. If that route cannot be reached, orders
get 503 until it can. Sending `rate` or both amounts is a 400.

The other admin routes are `PUT /admin/integrators/{id}/rate-limit`
(`{"rateLimitPerMinute": 300}`, or `null` to go back to the default of 120),
`POST /admin/keys/{id}/rotate`, `POST /admin/keys/{id}/revoke` and
`GET /admin/credential-events` (`integratorId`, `keyId`, `limit`). Rate limit
refusals are logged as `rate limit refused` with the key ID and time.

To send an integrator its order events, give it a webhook. The reply has the
signing secret, shown only this once:

```bash
curl -X PUT $HOST/admin/integrators/$INTEGRATOR_ID/webhook -H "X-Admin-Key: $ADMIN_API_KEY" \
  -d '{"url":"https://hooks.example.com/linq"}'   # "" clears it
```

Sending a new URL later moves the webhook and keeps the secret. To replace a
secret, send `"rotateSecret": true` with the URL. That takes effect at once, with
no overlap, and the reply holds the new secret.

The URL must be https and public. An integrator with no webhook polls instead:
its events are closed unreported, and setting a URL later does not replay them.
Reports already owed when a webhook is cleared wait 24 hours for a new URL.
Linq-B2B's webhook stays in `MERCHANT_WEBHOOK_URL` and can't be set here. Every
attempt, for every integrator, is a row in `webhook_deliveries`.
Revoking a key also revokes every key it was rotated to, because whoever holds a
leaked key can rotate it. The reply lists them in `alsoRevoked`, and the owner
then needs a newly issued key. `linq-b2b` can't hold issued keys.

### Reconciliation (`worker` only, all optional)

The worker checks the books against the ledger every 15 minutes, starting at
boot, and records each cycle in `reconciliation_cycles` and each check in
`reconciliation_items`. It only reads: a mismatch is written down, never acted
on. A cycle logs `reconciliation cycle finished` with its counts, and each
mismatch as `reconciliation mismatch`.

| Variable | Default | What it is |
|---|---|---|
| `RECONCILE_INTERVAL` | `15m` | Time between cycles, at least `1m` |
| `RECONCILE_HORIZON_CALLS` | `200` | Horizon requests one cycle may make. Open deposit accounts get half of it first, taking turns across cycles when there are more than that, and whatever is left at the end. Finished orders get the rest, newest first. What a cycle cannot reach waits for the next one. Four cycles an hour at 200 is 800 requests, well inside Horizon's 3,600 an hour |
| `RECONCILE_PAYOUT_CALLS` | `50` | `GET /internal/payout` requests one cycle may make to the Linq backend. An order is asked about under each payout reference it used, so a payout retried under new references costs one request per reference |
| `RECONCILE_SPONSORED_ACCOUNTS` | off | Exactly `true` turns on the check for accounts the sponsor pays reserves for with no order or pool row behind them. **Leave it off on mainnet until you know no other service uses the same sponsor account** (Linq-v2 has a `STELLAR_SPONSOR_KEY` too): every account another service opens would be reported |

To record why a mismatch is not a problem, or what was done about it:

```bash
curl -X POST $HOST/admin/reconciliation/items/$ITEM_ID/resolve -H "X-Admin-Key: $ADMIN_API_KEY" \
  -d '{"resolution":"Induced failure for the SD-11 demo","resolvedBy":"ify"}'
```

**This route only exists with `PUBLIC_API_ENABLED=true` on the server**, like
every `/admin` route. That is deliberate: the switch turns off everything an
admin key can do. With it off, mismatches are still recorded and shown on
`/reconciliation`; they just can't be resolved until it is on.

What a resolution does:

- **It covers every copy of that problem.** An unexplained mismatch is recorded
  again every cycle, and the note goes on all of its copies.
- **A transaction check is then done.** Its answer can't change.
- **Any other check keeps running**, because its answer can: a payout that was
  "still in progress" can pay later. It records nothing while the answer is the
  one explained, and a new mismatch, needing its own note, if the answer
  changes.
- **Resolving an error ("could not check") does not stop it being checked.** It
  was never answered.
- **A resolution cannot be rewritten.**

With more than one worker instance, each runs its own cycles. That is safe,
since the cycles only read, but it records each check twice.

### Reconciliation page (`server` only, optional)

`/reconciliation` shows a reviewer what the worker recorded: every cycle with
its counts, each cycle's checks (filterable to mismatches) with their
resolutions, every mismatch in one list, and a running total of completed
cycles. Transactions and accounts link to Stellar Expert, on testnet or public
to match `STELLAR_NETWORK_PASSPHRASE`. It is read-only, and reads only the two
reconciliation tables, so it has no bank detail to show.

| Variable | Default | What it is |
|---|---|---|
| `REVIEWER_TOKEN` | empty | The one secret that opens the page. Empty means the page is not served at all. At least 32 characters, or the server refuses to start. Generate: `openssl rand -hex 32` |
| `PROOF_INTEGRATOR_ID` | empty | The integrator whose orders `/reconciliation/proof` shows as the SCF D4 proof: `linq-v2-b2b`'s ID. Set with `PROOF_SINCE` |
| `PROOF_SINCE` | empty | When routing was switched on for the business, RFC3339 (`2026-11-09T10:00:00Z`). Earlier orders settled elsewhere and are not shown |
| `PROOF_BUSINESS_ID` | empty | Optional: only this Linq business's orders (the unaffiliated one) |
| `PROOF_DEMO_INTEGRATOR_ID` | empty | Optional: the `linq-demo` integrator, whose induced failure demos the page lists |

Give a reviewer the address and the token. They paste the token once; the
browser keeps a cookie made from it (not the token itself) for 30 days, and a
password manager can save the token.
Changing `REVIEWER_TOKEN` signs every reviewer out. A script can send the token
as a header instead:

```bash
curl $HOST/reconciliation -H "Authorization: Bearer $REVIEWER_TOKEN"
```

The page does not depend on `PUBLIC_API_ENABLED`, and the token opens nothing
else: not `/admin`, not `/v1`, not `/orders`.

Missing variables are all reported at once on the first boot, not one per
restart.

#### About `LINQ_INTERNAL_SECRET`

A shared password between two of your own servers, nothing more.

This service asks the Linq backend to pay a merchant in Naira. That endpoint
moves money, so it demands a secret header and ignores callers without it.

**The Linq backend has neither the route nor the secret yet.** PR #44 is not in
the running deployment, so `POST /internal/payout` doesn't exist there and
returns `404`. The secret is also unset — no `INTERNAL_API_SECRET` exists in
the Koyeb project — which is visible today on the older `POST /internal/status`
route, already deployed and gated by the same variable:

```
HTTP 503  {"error":"Internal API not configured"}
```

Nothing calls either route today, so nothing is broken by that. Once PR #44 is
deployed, `/internal/payout` will answer the same 503 until the variable is
set too.

**You choose the value.** Generate one and set it in two places:

```bash
openssl rand -hex 32
```

```
Linq backend (Koyeb)           Stellar service (Koyeb)
INTERNAL_API_SECRET=<value>    LINQ_INTERNAL_SECRET=<same value>
```

Two names for one string, because each service names it from its own side. If
they differ, every payout returns 401. If the backend's is missing entirely,
every payout returns 503.

#### About `ENCRYPTION_KEY`

This is **not** the same key as the Linq backend's `ENCRYPTION_KEY`, and it does
not need to match it. This service encrypts its own deposit-account seeds, in
its own database. Generate a fresh one.

It must be exactly 16, 24 or 32 bytes — `openssl rand -hex 16` produces a
32-character string, which is 32 bytes. **Changing it later makes every existing
deposit account unrecoverable**, because their seeds were sealed with the old
key and nothing can sweep or reclaim them afterwards.

### Optional

| Variable | Default | Notes |
|---|---|---|
| `PORT` | `8080` | Koyeb sets this itself |
| `LOG_LEVEL` | `info` | `debug` for a noisy first deploy |
| `STELLAR_HORIZON_URL` | `https://horizon.stellar.org` | Pubnet |
| `STELLAR_NETWORK_PASSPHRASE` | Pubnet passphrase | Set for testnet |
| `STELLAR_USDC_ISSUER` | Circle's mainnet issuer | **Must be set for testnet** — different issuer entirely, and a payment to the wrong one is unrecoverable |
| `STELLAR_SCAN_INTERVAL` | `15s` | Polling backstop cadence |
| `STELLAR_DEPOSIT_WINDOW` | `30m` | How long an order waits for its deposit |
| `STELLAR_MAX_STREAMS` | `200` | Concurrent Horizon streams; past this, orders fall back to polling |
| `STELLAR_ACCOUNT_POOL` | `10` | Deposit accounts kept provisioned ahead of demand, so `POST /orders` costs a database round-trip instead of a ledger close. `0` disables it and returns to provisioning inline. Each waiting account holds ~1 XLM of sponsor reserves, returned when the account is merged back after settlement |
| `STELLAR_BASE_FEE` | `10000` stroops | Headroom for network congestion |
| `PAYOUT_NEW_REFERENCE_ON_RETRY` | off | `worker` only. When exactly `true`, a payout retry after a failure the Linq backend confirms as retry-safe (`retrySafe: true` on `GET /internal/payout`) uses a new reference (`<order>-p2`, `-p3`), so the retry really pays. A failure without that confirmation stops the order in `failed` for a person to check, instead of retrying or refunding. Off, every retry reuses the order ID and reads back the first failure, then refunds after three, as before |

### SEP-10 (optional, all three or none)

| Variable | Notes |
|---|---|
| `SEP10_HOME_DOMAIN` | e.g. `linqswitch.xyz` |
| `SEP10_SIGNING_KEY` | Secret seed (`S...`). Its public half is published in `stellar.toml` as both `SIGNING_KEY` and `URI_REQUEST_SIGNING_KEY` |
| `SEP10_JWT_SECRET` | At least 32 bytes. Two deployments sharing this accept each other's tokens |

Partial configuration switches SEP-10 off rather than half-enabling it — an
authenticator issuing challenges nothing can verify is worse than one plainly
absent.

### Organisation details (published in `stellar.toml`)

`ORG_NAME`, `ORG_URL`, `ORG_DESCRIPTION`, `ORG_EMAIL`.

---

## Deploying

Create **two** Koyeb services from this repository. Both build the same
`Dockerfile`; only the run command differs.

**Service 1 — `linq-stellar-server`**
- Run command: `server`
- Port: `8080`, HTTP
- Health check: `GET /healthz`

**Service 2 — `linq-stellar-worker`**
- Run command: `worker`
- No ports, no health check — it serves nothing

Give both the same environment variables and the same database.

Run exactly **one** worker instance to start with. More than one is safe — every
detector claims an order before acting, so two workers cannot pay the same order
twice — but one is easier to read in the logs while you are still watching it.

---

## Verifying the deploy

**1. The server is up**

```bash
curl https://<your-server>.koyeb.app/healthz
# {"status":"ok"}
```

**2. The trustline preflight answers** — unauthenticated, safe to call

```bash
curl "https://<your-server>.koyeb.app/stellar/trustline?address=GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
# {"address":"GA5Z...","trustsUSDC":true,"assetCode":"USDC","assetIssuer":"GA5Z..."}
```

**3. The stellar.toml is published**

```bash
curl https://<your-server>.koyeb.app/.well-known/stellar.toml
```

Check `ACCOUNTS` lists your sponsor and treasury. That entry is what lets anyone
audit your provisioning and settlement volume on Horizon without asking you for
anything.

**4. The worker started cleanly**

Look for `worker starting` with the sponsor, treasury and issuer it resolved.
Then look for the treasury trustline check. If you see
`treasury has no USDC trustline; sweeps will fail`, stop and fix it before
taking a payment.

**5. End to end, with a real payment**

Create an order, send USDC to the address it returns, and watch the order move:

```
awaiting_deposit → deposit_detected → payout_queued
  → payout_processing → disbursed → sweep_queued → settled_in_treasury
```

Start with about $1. The first real payment is where a wrong issuer, an
unfunded sponsor or a mismatched internal secret shows up.

---

## When something is wrong

| Symptom | Cause |
|---|---|
| Exits immediately, lists several variables | Config validation. It reports every missing variable at once — fix them together |
| `server` exits with `ORDERS_API_KEY is required` | Not set on the `server` service. The `worker` never hits this check — it doesn't serve HTTP |
| `POST /orders` or `GET /orders/{id}` returns 401 | `X-API-Key` is missing or doesn't match `ORDERS_API_KEY` |
| `encryption key must be 16, 24 or 32 bytes` | `ENCRYPTION_KEY` is the wrong length. `openssl rand -hex 16` |
| `invalid sponsor seed` | You set a public key (`G...`) where a secret seed (`S...`) belongs |
| Payouts fail with 503 `Internal API not configured` | `INTERNAL_API_SECRET` is not set on the Linq backend |
| Payouts fail with 401 | `LINQ_INTERNAL_SECRET` does not match the backend's `INTERNAL_API_SECRET` |
| Payouts fail with 404 | `/internal/payout` is not deployed on the Linq backend yet |
| `treasury has no USDC trustline` | Add the trustline before taking payments |
| Deposits never detected | Check the issuer matches the one payers are actually sending |
| Orders reach `failed` after sweeping | Read the log line — funds stay in the deposit account, which is the safest place for them until someone looks |

Orders that fail permanently keep their USDC in their own deposit account rather
than being pushed anywhere automatic. Nothing is lost; it waits for a person.
