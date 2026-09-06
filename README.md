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

## SCF reviewers

[`docs/scf-criteria.md`](docs/scf-criteria.md) maps each tranche acceptance
criterion to the code that implements it.
