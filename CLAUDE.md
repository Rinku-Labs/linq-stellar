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

- `money.QuoteUSDC` rounds **up**, at `QuoteDecimals` (6). Six rather than
  Stellar's seven so the same figure is exact on screen, in the SEP-7 URI, and
  in wallets carrying six decimals for USDC on other chains.
- `money.RoundNGN` is kobo. Bank rails cannot pay a fraction of a kobo.
- Never round a quote to "nearest" or to two decimals. Two decimals is a naira
  habit; at these rates the third decimal is worth ₦1.36.
- Scaling comes before rounding, never after.

`ceilTo` snaps values already exact at the target precision before rounding —
`0.07 * 1e6` is `70000.00000000001` in float64, and a bare `Ceil` turns an
exact 0.07 into 0.070001.

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

## Verify

```bash
go build ./... && go vet ./... && go test ./...
```

No database, network or keys needed. `TestQuotingAndSettlingRoundTripsExactly`
is the invariant: quote an invoice, pay exactly that quote, merchant receives
the invoice to the kobo.
