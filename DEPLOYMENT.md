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
| `STELLAR_BASE_FEE` | `10000` stroops | Headroom for network congestion |

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
