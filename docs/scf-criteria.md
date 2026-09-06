# SCF acceptance criteria → implementation

Where each named criterion from the Pre-Launch #1 tranche lives in this repo.

| Criterion | Where |
|---|---|
| Indexer schema | [`internal/store/order.go`](../internal/store/order.go), [`state.go`](../internal/store/state.go) |
| Payment events captured and indexed | `internal/settle/stream.go` (Horizon SSE), `scanner.go` (polling backstop) |
| Matched to merchant records | `Order.BusinessID`, claimed via [`claim.go`](../internal/store/claim.go) |
| NGN disbursement on confirmation | `internal/payout/` → Linq B2B API |
| Failure → notification and reprocessing | `internal/settle/refund.go` |
| Zero fees deducted from user wallet | `Order.FeeUSDC`, fixed at 0 |
| SEP-7 generator | `internal/sep/sep7.go` |
| SEP-10 handler | `internal/sep/sep10.go` |
| SEP-1 stellar.toml | `internal/sep/sep1.go` |

## Not in this repo

- **Stellar Wallets Kit** — browser-side by nature; lives in the Linq B2B frontend.
- **Chain indexing framework** — deposit detection here is purpose-built against
  Horizon. The multi-chain indexer Linq runs elsewhere is third-party open
  source and is not represented as Linq's work.
- **Payout partner integration** — delegated to the Linq B2B API. This service
  holds no bank or partner credentials.
