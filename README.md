# linq-stellar

Stellar USDC settlement for Linq's B2B off-ramp: a payer sends USDC to a
single-use Stellar account, NGN is disbursed to the merchant's Nigerian bank
account, and the USDC is swept to treasury.

Backend only. The merchant dashboard and checkout live in the Linq B2B frontend.

## How a payment works

1. **Provision** — a fresh Stellar keypair is created for the order, and one
   atomic transaction opens the account and its USDC trustline using
   [sponsored reserves](https://developers.stellar.org/docs/learn/encyclopedia/transactions-specialized/sponsored-reserves):

   ```
   BeginSponsoringFutureReserves(sponsoredID: orderAccount)
   CreateAccount(destination: orderAccount, startingBalance: 0)
   ChangeTrust(USDC, limit: max)
   EndSponsoringFutureReserves(source: orderAccount)
   ```

   The sponsor pays the reserves; the order account never holds XLM.

2. **Detect** — a Horizon stream watches the account, with a polling sweep
   behind it catching anything the stream drops across a reconnect. Both enter
   through the same compare-and-swap claim, so a deposit is only ever acted on
   once.

3. **Disburse** — the Linq B2B API pays the merchant in NGN, using the same
   payout stack it already runs for every other supported chain.

4. **Sweep** — USDC moves to treasury and the deposit account is merged back
   into the sponsor, returning every sponsored lumen:

   ```
   ChangeTrust(USDC, limit: 0)
   Payment(USDC -> treasury)
   AccountMerge(destination: sponsor)
   ```

Because the reserves come back, a single-use account costs only network fees —
which is what makes Stellar the zero-fee option on the platform. The payer is
charged nothing by Linq; they pay only Stellar's own ~0.00001 XLM.

## Why single-use accounts

A permanent per-merchant address would lock reserves indefinitely and make
payments attributable only by memo, which payers get wrong. One account per
order makes every payment unambiguous, leaves no stale address to pay into, and
holds funds only between deposit and sweep.

## Layout

| Path | Contains |
|---|---|
| `cmd/server` | HTTP API and the Horizon stream listener |
| `cmd/worker` | Settlement workers: sweep, refund, payout dispatch |
| `internal/stellar` | Keypairs, provisioning, sweep, balances, trustlines |
| `internal/settle` | Deposit detection, claiming, refunds |
| `internal/payout` | NGN disbursement via the Linq B2B API |
| `internal/store` | Order schema, lifecycle states, CAS guards |
| `internal/sep` | SEP-1, SEP-7, SEP-10 |

## Running

```bash
cp .env.example .env   # fill in sponsor key, treasury, database
go run ./cmd/server
go run ./cmd/worker
```

Requires PostgreSQL. `STELLAR_SPONSOR_KEY` must belong to a funded account, and
`STELLAR_TREASURY_WALLET` must already hold a USDC trustline.

For deploying to Koyeb — every environment variable, what to set it to, and how
to verify the first real payment — see [DEPLOYMENT.md](DEPLOYMENT.md).

## Reviewing this

[`docs/scf-criteria.md`](docs/scf-criteria.md) maps each acceptance criterion to
the code implementing it.

### Check the live service without installing anything

The deployment is public and unauthenticated on these routes. Nothing below
needs a key, an account, or our cooperation.

```bash
SERVICE=https://linq-stellar-uselinq-4c0e2a4f.koyeb.app

# 1. Service health
curl $SERVICE/healthz

# 2. SEP-1 — the info file wallets and anchors read.
#    Note SIGNING_KEY, WEB_AUTH_ENDPOINT, URI_REQUEST_SIGNING_KEY and ACCOUNTS.
curl $SERVICE/.well-known/stellar.toml

# 3. SEP-10 — request a real authentication challenge for any Stellar account.
#    Returns a base64 transaction with sequence number 0, signed by the
#    SIGNING_KEY published above.
curl "$SERVICE/sep10/auth?account=GB2LEGZMXI44AMJNEM5RRWXB7YWUGSKRZJDJPMS2APJVNGMHTOHOSU4K"

# 4. Trustline preflight — whether an address can receive USDC at all.
curl "$SERVICE/stellar/trustline?address=GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
```

Order creation and lookup require an API key, since they mint an on-chain
account and cost real reserves. Ask us and we will issue one.

### Audit the settlement history on-chain

The `ACCOUNTS` entry in the `stellar.toml` is not decoration. It publishes the
two accounts this service operates, so the entire settlement history can be
read from Horizon without asking us for anything:

- **Sponsor** — every deposit account ever provisioned, and the matching merge
  returning its reserves. Unfunded orders appear here too; nothing is filtered.
- **Treasury** — every settled payment, with amounts and timestamps.

A settlement is two linked transactions: the payer funds a single-use deposit
account, then that account pays treasury and is merged away. Cross-referencing
them shows both the payer and the amount for every order.

### How an amount is quoted

A naira invoice rarely converts to a round USDC figure: ₦100 at ₦1,364.21 per
USDC is 0.0733018… USDC. Shortening that is unavoidable, and the direction it
is shortened in is a decision about whose money absorbs the difference.

Two rules settle it, both in `internal/money`:

1. **A quote rounds up**, to six decimals — one tighter than Stellar's seven,
   so the same figure is exact on screen, in the SEP-7 URI, and in wallets that
   carry six decimals for USDC on other chains. The payer is asked for at most
   a ten-thousandth of a cent more than the invoice, never less.
2. **A covered deposit pays the quote.** Once the payment reaches the quoted
   amount, the merchant is paid the naira they were promised, not a figure
   recomputed from the deposit. Recomputing is how a rounding loss in the quote
   becomes a shortfall in the payout.

A deposit that does not cover the quote is paid out at what it is actually
worth and marked `underpaid`, with the gap in `shortfallNgn`. Nothing is
silently absorbed in either direction: an order says what it asked for
(`quotedUsdc`, `quotedNgn`), what arrived (`amountUsdc`), and what was paid
(`amountNgn`).

### Run the tests

The suite needs no database, no network and no keys.

```bash
go test ./...          # 52 tests
go test ./... -race    # the concurrency guarantees below
```

Worth looking at specifically:

- `TestConcurrentDetectorsCreditOnce` races eight detectors at one order and
  asserts exactly one wins and the payout is queued exactly once. Deposit
  detection is deliberately redundant, and this is what makes that safe.
- `TestQuotingAndSettlingRoundTripsExactly` quotes a spread of invoices at a
  spread of rates, settles each with a payment of exactly the quoted amount,
  and asserts the merchant receives the invoice to the kobo. See
  "How an amount is quoted" below for why that needs proving.
- `TestSignatureRoundTripsAndDetectsTampering` proves a SEP-7 payment request
  fails verification if its destination is altered.
- `TestHorizonOutageFailsClosed` proves SEP-10 refuses to authenticate when it
  cannot check an account's signers, rather than falling back to weaker
  verification.

### Run it locally

```bash
cp .env.example .env   # sponsor key, treasury, database
docker compose up      # Postgres + server + worker
```

Or without Docker, against your own Postgres:

```bash
go run ./cmd/server    # HTTP API on :8080
go run ./cmd/worker    # settlement loops
```

The server refuses to start if configuration is missing, and reports every
missing variable at once rather than one per restart. `STELLAR_SPONSOR_KEY`
must belong to a funded account and `STELLAR_TREASURY_WALLET` must already hold
a USDC trustline — the worker warns loudly at startup if it does not.
