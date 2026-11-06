# linq-stellar

Stellar USDC → NGN settlement. Payers send USDC to a single-use deposit
account; NGN is disbursed to the merchant's bank, then the USDC is swept to
treasury.

## Money precision — read before touching any amount

A ₦100 invoice at ₦1,364.21/USDC is 0.0733018… USDC. There is no exact decimal
form, so every place that shortens it chooses whose money absorbs the
difference. Getting this wrong once already paid a merchant ₦95.49 on a ₦100
order.

**`internal/money` is the only place rounding rules live.** Do not inline
`math.Round`, `math.Ceil` or `* 100` arithmetic on an amount anywhere else.

- `money.QuoteUSDC` rounds **up**, at `QuoteDecimals` (**2**). Two so a payer
  reads and types 0.08 rather than 0.073302. Up so that figure is never less
  than the invoice needs. The payer absorbs the rounding — at most 0.01 USDC,
  about ₦14 — never the merchant. That trade was chosen deliberately.
- `money.RoundNGN` is kobo. Bank rails cannot pay a fraction of a kobo.
- **Never round a quote to nearest, and never down.** Two decimals to nearest
  is the original bug: 0.0733 became 0.07 and a merchant was paid ₦95.49 for
  ₦100. Two decimals *up* is correct; two decimals any other way is not.
- `DustUSDC` stays at 1e-6 and is **not** tied to `QuoteDecimals`. Widening it
  to 0.01 would accept a 0.07 deposit against a 0.08 quote as payment in full.
- Every surface must quote the same figure: the screen, the SEP-7 URI, and the
  Linq backend's `ceil(amountNGN/rate*100)/100`. Change `QuoteDecimals` without
  changing the others and a QR will prefill a different amount than the screen
  shows.
- Scaling comes before rounding, never after.

`ceilTo` snaps values already exact at the target precision before rounding —
`0.57 * 100` is `57.00000000000001` in float64, and a bare `Ceil` turns an
exact 0.57 into 0.58.

## The order's quote is immutable

`Order.QuotedUSDC` / `QuotedNGN` are set at creation and **never rewritten**.
`AmountUSDC` / `AmountNGN` are what arrived and what was paid.

Keeping them apart is what lets `settle.Reconcile` ask whether a deposit
actually settled the invoice — a question nothing could answer while the quote
was being overwritten by the deposit meant to satisfy it.

Settlement rule, in `settle.Reconcile`:

- Deposit covers the quote → pay `QuotedNGN`. Do not recompute from the
  deposit; that is how a rounding loss in the quote becomes a real shortfall.
- Manual order, deposit over the quote → pay for what arrived.
- Deposit short of the quote → pay what it is worth, set `Underpaid` and
  `ShortfallNGN`. Never silently absorb a gap in either direction.

Orders predating the quote columns carry their quote in `AmountUSDC`/
`AmountNGN`; `Deposits.Record` falls back to those, so don't remove that path
while old rows exist.

## Workers are signalled, not just polled

Every loop is driven from the order table — select the rows in your state, work
them, sleep — and an idle loop widens its interval (`internal/pace`) so a quiet
service is not billed for asking an empty table. That backoff used to be the
service's latency: a deposit detected in one second waited 76 more for the
payout worker's next pass, and the merchant heard about it 62 seconds after
that.

So `internal/wake` makes the sleep interruptible. A worker that creates work for
another signals its topic; the waiting worker returns at once and resets to its
base interval. Signals cross processes over Postgres `LISTEN`/`NOTIFY`, because
the API server and the worker are separate deployments.

- **`waker` in `cmd/worker/main.go` is the whole hand-off.** It maps the state an
  order just entered to the loop that owns what comes next. Adding a state with
  its own queue means adding it there, or that queue is worked at its polling
  interval.
- The hook is `store.OnTransition`, called from `recordTransition` — the one
  funnel every claim already goes through. Signal **after** the row is written;
  a worker woken first finds nothing and goes back to sleep.
- **A retry is deliberately not signalled.** The interval between payout
  attempts is the only thing stopping a provider's bad minute from consuming all
  three of an order's attempts inside one second.
- Signals carry no payload: "look again", never "here is the order". A lost
  signal costs a delay, not a settlement — the polling sweep is still the
  guarantee.

## Deposit accounts are provisioned ahead of the order

Provisioning is four operations and a ledger close, five to eight seconds, and
none of it depends on the order that waits for it. `internal/pool` keeps
`STELLAR_ACCOUNT_POOL` accounts ready and `POST /orders` claims one, which is
why creating an order is now a database round-trip.

- The pool is a buffer, never a dependency. Empty means provision inline and be
  slow; it must never mean refuse.
- `ClaimPoolAccount` is a compare-and-swap for the same reason `ClaimStatus` is:
  one address handed to two orders settles one payer's deposit against the
  other's invoice.
- Rows are written **before** the transaction is submitted. The seed is the only
  way back to an account the sponsor has paid reserves for, and a crash between
  submitting and recording would strand it. `Ready` is what makes a row usable,
  and the keeper resolves unconfirmed rows by asking Horizon whether the account
  actually exists.

## One sponsor account, one sequence number

Provisioning, sweeping, refunding and reclaiming are all sourced from the
sponsor, and a Stellar account has a single sequence number. Two transactions
built from the same reading of it are not both valid — the second comes back
`tx_bad_seq`, having done nothing.

**`Client.submitSponsored` is the only place that submits one of these.** It
serialises them within the process and retries on a stale sequence, reloading
the account each attempt. Building and submitting a sponsor-sourced transaction
anywhere else reintroduces the race, and the moment it bites is the worst one:
an empty pool means an order provisioning inline at the same instant the keeper
is minting a batch, in a different deployment.

`tx_bad_seq` is the only code that may be retried by resending the same
transaction. Everything else is Stellar refusing what was asked for, and for a
sweep or a refund, resending moves money twice.

Fee bumps are exempt — a fee account does not spend a sequence number — which is
why `FeeBumpAndSubmit` submits directly.

## Integrators, keys and rate limits

`/v1` and `/admin` live in `internal/api/public.go` and are only served when
`PUBLIC_API_ENABLED=true`. `/v1/orders` runs the same handlers as `/orders`; the
caller comes from the request context, so the two cannot drift apart.

- **An integrator is the caller; `Order.BusinessID` is the merchant.** Do not
  reuse one for the other. Every order query a caller can reach filters on
  `integrator_id`. `ORDERS_API_KEY` maps to `store.LegacyIntegratorID`, which is
  also the column default, so a row written without one belongs to Linq-B2B.
- **An issued key never sets the rate.** Settlement pays `QuotedNGN` once
  `QuotedUSDC` arrives, so whoever sets the rate decides what a deposit is
  worth: 0.01 USDC against ₦10,000,000 passes every other check. On `/v1` a
  `rate`, or both amounts, is a 400; the rate comes from `Server.Rates`
  (`GET /b2b/rate` on the Linq backend, held `payout.RateMaxAge`). No rate
  means no order: never fall back to a stale or supplied one. Only the legacy
  key sends a rate, which Linq-B2B fetches from that same route.
- **A key is never stored or logged.** Only `key_prefix` and a SHA-256 of the
  whole key. `writeIssuedKey` is the one place a full key leaves the service.
- **`credential_events` is the rotation and revocation proof, and a Postgres
  trigger makes it append-only.** Any new way to
  change a key must write a row there, in the same transaction as the change.
- **A key inside its rotation overlap cannot rotate again.** Otherwise a leaked
  key could mint a successor and outlive the rotation.
- **The limit is counted after authentication, by key, never by address.**
  Counting first would let anyone who has seen a prefix lock its owner out. The
  legacy routes have no key row, so they carry no limit.
- **The counter is one atomic upsert per request** (`HitRateCounter`), in
  Postgres so it holds across instances. A read then a write would let
  concurrent requests through. If it cannot be written the request gets 503: the
  limit never silently stops applying.
- **`rate limit refused` log lines are the proof of enforcement.** Counter rows
  are gone within the hour, so nothing else records a refusal.

## Logs name their source

Each loop gets `log.With("component", ...)` at the composition root, and every
Horizon call logs under `component=horizon` with its op, account and elapsed
time (`Client.call`). Five loops interleaved in one stream, with no field saying
which is which, means "why was this slow" has to be answered by recognising
message wording.

Successful Horizon reads are debug — the scanner makes one per waiting order per
pass. Submissions and observed payments are info, because they are where the
seconds and the money are.

## Verify

```bash
go build ./... && go vet ./... && go test ./...
```

No database, network or keys needed. `TestQuotingAndSettlingRoundTripsExactly`
is the invariant: quote an invoice, pay exactly that quote, merchant receives
the invoice to the kobo.
