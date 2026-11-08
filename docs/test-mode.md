# Try Linq on the Stellar test network

This walks you through a real payment on Linq's test deployment, start to
finish, in about 20 minutes. You pay test USDC on the Stellar test network,
watch the order settle and its webhooks arrive, make a payout fail on purpose,
and see it all checked against the ledger.

**Nothing here uses real money.** The test network's USDC has no value, and the
test deployment pays no bank: every naira payout is pretend, and says so
(`payoutReference` starts `fake_`).

## What you need

- **The API key Linq gave you.** It starts `lq_test_`.
- **A terminal with `curl`.** On Windows, use Git Bash or WSL.
- **A browser with the Freighter wallet extension**, from <https://www.freighter.app>.

The test deployment:

| | |
|---|---|
| Address | `https://<test-host>` (Linq fills this in when the deployment is up) |
| Reconciliation page token | `<reviewer-token>` (likewise) |

Set these in your terminal, and keep it open for the whole walkthrough:

```bash
export HOST=https://<test-host>
export KEY=lq_test_...        # your key
```

## 1. Get test USDC

Do these in this order. A USDC payment to an account without a USDC trustline
fails, so step 4 must come before step 5.

1. **Make a wallet.** Install Freighter, create a new wallet, and keep its
   recovery phrase. Copy your address: it starts with `G`.
2. **Switch to the test network.** In Freighter's settings, choose the network
   "Test Net".
3. **Get test XLM.** Freighter offers to fund a new test account with
   Friendbot. If it doesn't, open
   `https://friendbot.stellar.org/?addr=<your address>` in your browser. You
   get 10,000 test XLM, which pays for everything below.
4. **Add USDC.** In Freighter, add an asset: USDC, issued by
   `GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5`. Confirm the
   transaction. This is the USDC trustline.
5. **Get USDC.** Go to <https://faucet.circle.com>, choose Stellar, paste your
   address, and ask for tokens. You get 10 USDC, enough for every step here.

## 2. Point your webhooks at a page you can watch

Open <https://webhook.site>. It gives you a unique URL, and shows every request
sent to it. Keep that tab open, and send the URL to Linq:

```bash
curl -X PUT $HOST/v1/webhook -H "X-API-Key: $KEY" \
  -d '{"url":"https://webhook.site/<your-id>"}'
```

The reply holds `webhookSecret`, starting `whsec_`, shown only this once. Keep
it if you want to check the signatures in step 5.

## 3. Create an order

```bash
curl -X POST $HOST/v1/orders -H "X-API-Key: $KEY" \
  -d '{"idempotencyKey":"try-1","amountNgn":1500,"bankCode":"058","bankAccount":"0123456789","accountName":"Test Merchant"}'
```

The test deployment quotes at a fixed ₦1,500 per USDC, so this order asks for
**1.00 USDC** (`quotedUsdc`). The bank details can be anything: no bank is paid.

From the reply, note:

- `id`, the order. Set `export ORDER=<id>`.
- `depositAddress`, where to pay. It's a new account, made for this order alone.
- `quotedUsdc`, how much to pay.
- `depositDeadline`, 30 minutes from now. After that the order expires.

## 4. Pay it

In Freighter, send **exactly `quotedUsdc` USDC** to the `depositAddress`. No
memo is needed. (A wallet that reads SEP-7 links can open `paymentUri` from the
reply instead, with everything filled in.)

Then follow the order:

```bash
curl $HOST/v1/orders/$ORDER -H "X-API-Key: $KEY"
```

Within seconds of your payment, `status` moves through `deposit_detected`,
`payout_queued`, `disbursed` and on to `settled_in_treasury`.

- `statusHistory` lists every step, with its time.
- `payoutAttempts` shows one attempt, `paid`.
- `depositTxHash` is your payment, and `sweepTxHash` is the USDC moving on to
  Linq's treasury. Look either up at
  `https://stellar.expert/explorer/testnet/tx/<hash>`.

## 5. Watch the webhooks arrive

On webhook.site you'll see one `POST` per step that matters:
`order.deposit_detected`, `order.disbursed` and `order.settled_in_treasury`.

Each is signed. The `x-linq-signature` header is `sha256=` followed by the
HMAC-SHA256 of the raw body, keyed with your `webhookSecret`. To check one, copy
the raw body exactly as received:

```bash
printf '%s' '<raw body>' | openssl dgst -sha256 -hmac '<your webhookSecret>'
```

The result should match the header after `sha256=`.

## 6. Make a payout fail, and watch what happens

Your test account may ask for an order's payout to fail, with `testFailure`.
Nothing is sent to any bank either way; the failure is recorded as `induced`.

**A failure that a retry fixes:**

```bash
curl -X POST $HOST/v1/orders -H "X-API-Key: $KEY" \
  -d '{"idempotencyKey":"try-2","amountNgn":1500,"bankCode":"058","bankAccount":"0123456789","testFailure":"transient"}'
```

Pay it as in step 4, then follow it. `payoutAttempts` shows:

1. The first attempt, `failed`, with `induced: true`.
2. A few seconds later, a retry under its own reference (`<order id>-p2`),
   `paid`.

The order then settles as before.

**A failure that ends in a refund:**

```bash
curl -X POST $HOST/v1/orders -H "X-API-Key: $KEY" \
  -d '{"idempotencyKey":"try-3","amountNgn":1500,"bankCode":"058","bankAccount":"0123456789","testFailure":"terminal"}'
```

Pay it, and every attempt fails. After the third, the order moves to
`refund_queued` and then `refunded`. Your USDC comes back to your wallet:
`refundDestination` is your address, and `refundTxHash` is the refund
transaction. `statusReason` says why, and your webhook page gets
`order.refund_queued` and `order.refunded`.

## 7. See the reconciliation record

Every 2 minutes, Linq checks its books against the Stellar ledger and the payout
records, and writes down every result. Open `$HOST/reconciliation` in your
browser and paste the reconciliation page token from the top of this page.

- The first page lists the cycles, newest first, with how many checks passed and
  how many didn't.
- Open a cycle to see its checks. Your orders appear in the first cycle after
  they finish:
  - your deposit, and the sweep or refund, each confirmed on the ledger;
  - the deposit account closed afterwards;
  - the payout record matching the order.
- Every transaction and account links to Stellar Expert, so you can check
  anything yourself.

The page never shows bank details.

## If something goes wrong

| What you see | What it means |
|---|---|
| `401` | The key is missing or wrong. Check `$KEY` |
| `403` asking for `testFailure` | Your account isn't allowed to ask for failures. Ask Linq |
| `429` | Too many requests this minute. Wait for `Retry-After` |
| The order is `expired` | 30 minutes passed with no payment. Create another |
| Your USDC payment failed with `op_no_trust` | The account you paid from has no USDC trustline. Do step 1.4 |
| The faucet's payment didn't arrive | Same: add USDC to your wallet first, then ask the faucet again |
| You paid less than `quotedUsdc` | The order shows `underpaid: true` and pays what your USDC was worth |
| No webhooks arrive | Check the URL you sent in step 2. Webhooks need an `https` address |

The full API guide is at `$HOST/docs`.
