# SCF acceptance criteria → implementation

Where each named criterion from the Pre-Launch #1 tranche lives in this repo.

| Criterion | Where |
|---|---|
| Indexer schema | [`internal/store/order.go`](../internal/store/order.go), [`state.go`](../internal/store/state.go) |
| Payment events captured and indexed | [`settle/stream.go`](../internal/settle/stream.go) (Horizon SSE), [`scanner.go`](../internal/settle/scanner.go) (polling backstop) |
| Matched to merchant records | `Order.BusinessID`, claimed via [`claim.go`](../internal/store/claim.go) |
| NGN disbursement on confirmation | [`payout/worker.go`](../internal/payout/worker.go) → [`linq.go`](../internal/payout/linq.go) |
| Failure → reprocessing and refund | [`payout/worker.go`](../internal/payout/worker.go), [`settle/workers.go`](../internal/settle/workers.go) |
| Zero fees deducted from user wallet | `Order.FeeUSDC`, fixed at 0 in [`api.go`](../internal/api/api.go) |
| SEP-7 generator | [`sep/sep7.go`](../internal/sep/sep7.go) — with request signing |
| SEP-10 handler | [`sep/sep10.go`](../internal/sep/sep10.go) |
| SEP-1 stellar.toml | [`sep/sep1.go`](../internal/sep/sep1.go) |
| Sponsored-reserve provisioning | [`stellar/provision.go`](../internal/stellar/provision.go) |
| Treasury sweep and reserve reclaim | [`stellar/sweep.go`](../internal/stellar/sweep.go) |

## Verifying it yourself

The `ACCOUNTS` entry in the published `stellar.toml` lists the sponsor and
treasury accounts. Their Horizon history is a complete, public record of every
deposit account provisioned and every payment settled — auditable without
asking Linq for anything, and cross-referenceable against any reconciliation
export.

## A dependency worth naming

NGN payout is delegated to the Linq B2B API over two internal endpoints
(`POST`/`GET /internal/payout`, documented in
[`payout/linq.go`](../internal/payout/linq.go)). The public `POST /b2b/offramp`
cannot serve this: it mints its own deposit wallet and waits for a deposit,
which is precisely the half this service has already done.

Those endpoints are implemented in the Linq backend on branch
`feature/stellar-internal-payout`. They reuse the existing multi-provider
payout stack rather than duplicating bank integration here, which is why this
repository holds no bank or partner credentials.

## Not in this repo

- **Stellar Wallets Kit** — browser-side by nature. Implemented in the Linq B2B
  frontend on branch `feature/stellar-wallets-kit`: payers can connect
  Freighter, xBull or Albedo at checkout and sign the USDC payment in place,
  alongside the SEP-7 QR and copyable address that most volume still arrives
  through.
- **Chain indexing framework** — deposit detection here is purpose-built against
  Horizon. The multi-chain indexer Linq runs elsewhere is third-party open
  source and is not represented as Linq's work.
- **Payout partner integration** — delegated to the Linq B2B API. This service
  holds no bank or partner credentials.
