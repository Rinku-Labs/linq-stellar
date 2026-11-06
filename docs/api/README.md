# Linq Settlement API

Take USDC on Stellar and pay naira to a Nigerian bank account. You create an order, your payer sends USDC to the order's deposit address, and Linq pays the bank.

The machine-readable spec is at `/docs/openapi.yaml`.

The examples below use three shell variables:

```text
LINQ_API   the host you were given, such as https://example.com
LINQ_KEY   your API key
ORDER_ID   an order's id
```

## Quick start

Create an order for ₦15,000:

```bash
curl -sS -X POST "$LINQ_API/v1/orders" \
  -H "X-API-Key: $LINQ_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "idempotencyKey": "invoice-1042",
    "amountNgn": 15000,
    "bankCode": "058",
    "bankAccount": "0123456789",
    "accountName": "Ada Stores Ltd"
  }'
```

The reply is `201` with the order:

```json
{
  "id": "6f1c1f3e-2a53-4a4e-9d3e-0b6a0c6f7c11",
  "status": "awaiting_deposit",
  "depositAddress": "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H",
  "quotedUsdc": 10,
  "quotedNgn": 15000,
  "amountUsdc": 10,
  "amountNgn": 15000,
  "rate": 1500,
  "feeUsdc": 0,
  "currency": "NGN",
  "assetCode": "USDC",
  "assetIssuer": "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
  "depositDeadline": "2026-11-08T12:30:00Z",
  "paymentUri": "web+stellar:pay?amount=10&asset_code=USDC&...",
  "createdAt": "2026-11-08T12:00:00Z",
  "updatedAt": "2026-11-08T12:00:00Z"
}
```

Ask your payer to send `quotedUsdc` of the asset named by `assetCode` and `assetIssuer` to `depositAddress` before `depositDeadline`. `paymentUri` is a SEP-7 link a wallet can open or scan, and carries the same details.

Then check on the order:

```bash
curl -sS "$LINQ_API/v1/orders/$ORDER_ID" -H "X-API-Key: $LINQ_KEY"
```

## Authentication

Send your key in the `X-API-Key` header on every request. Linq issues your first key, and it is shown only once. Linq stores a hash of it, so a lost key can't be recovered, only replaced.

Keys look like `lq_live_<prefix>_<secret>`, or `lq_test_<prefix>_<secret>` in test mode. Keep them on your server, never in a browser or mobile app.

## Creating an order

Send one of `amountNgn` or `amountUsdc`, not both. You don't send a rate: Linq sets it and returns the one it used in `rate`.

- **With `amountNgn`**, the bank receives exactly that amount. `quotedUsdc` is worked out at the current rate and rounded up to two decimals.
- **With `amountUsdc`**, the amount is rounded up to two decimals and `quotedNgn` is what that buys at the current rate.

The quote is fixed when the order is created and holds until `depositDeadline`, which is 30 minutes later by default.

`quotedUsdc` and `quotedNgn` never change. `amountUsdc` and `amountNgn` start equal to them, and once a deposit arrives they show what was received and what is being paid.

What gets paid depends on the deposit:

| Deposit | Bank receives |
|---|---|
| Covers `quotedUsdc` | `quotedNgn` |
| More than `quotedUsdc`, on an order with `manualDeposit: true` | What the deposit is worth at the order's rate |
| Less than `quotedUsdc` | What the deposit is worth at the order's rate. The order then shows `underpaid: true` and `shortfallNgn` |

Keep `customerRef` to 28 bytes or fewer. It becomes the memo in `paymentUri`, and Stellar text memos are limited to 28 bytes.

Set `refundAddress` to choose where a refund goes. It must be able to receive USDC. Without it, a refund goes back to the account that paid.

## Idempotency

`idempotencyKey` is required, up to 255 characters, and unique to you. Use one key per order you mean to create, such as your invoice number.

If you send a key again, you get the original order back with `200` instead of `201`, at its original rate. The rest of the request is not compared, so a retry with different amounts still returns the first order. It's always safe to retry after a timeout with the same key.

## Key rotation

Replace a key using the key itself:

```bash
curl -sS -X POST "$LINQ_API/v1/keys/rotate" -H "X-API-Key: $LINQ_KEY"
```

The reply is `201` with the new key in `key`, shown only this once:

```json
{
  "id": "key_7Hq2mZpLx0aRt9Vc",
  "integratorId": "int_cOGtaM0qnMqVKRn2",
  "keyPrefix": "<prefix>",
  "key": "lq_live_<prefix>_<secret>",
  "createdAt": "2026-11-08T12:00:00Z",
  "previousKey": {
    "id": "key_2Lk9sQwEr4tYu6Io",
    "expiresAt": "2026-11-09T12:00:00Z"
  }
}
```

The old key keeps working for 24 hours, until `previousKey.expiresAt`, so you can deploy the new one without downtime. An old key can't be rotated a second time, which returns `409`. Rotate from the new key instead.

If a key may have leaked, contact Linq to have it revoked at once. Rotating alone leaves the old key working for 24 hours.

## Rate limits

Each key may make 120 requests per clock minute unless Linq has set a different limit for you. Every authenticated request counts, including refused ones. Limits are per key, not per IP address.

Each authenticated reply includes:

| Header | Meaning |
|---|---|
| `X-RateLimit-Limit` | Requests allowed per minute |
| `X-RateLimit-Remaining` | Requests left this minute |

Over the limit you get `429` with a `Retry-After` header, the number of seconds until the next minute starts.

## Order statuses

| Status | Meaning | Final |
|---|---|---|
| `initiated` | Created, and the deposit address isn't ready yet | |
| `awaiting_deposit` | Waiting for the payer | |
| `deposit_detected` | The deposit arrived | |
| `payout_queued` | The payout is waiting to be sent | |
| `payout_processing` | The payout is with the bank | |
| `disbursed` | The bank has been paid | |
| `sweep_queued` | Linq is moving the USDC to its treasury | |
| `sweep_processing` | Linq is moving the USDC to its treasury | |
| `settled_in_treasury` | Finished. The bank was paid and the USDC is with Linq | Yes |
| `refund_queued` | The payout failed for good and a refund is waiting | |
| `refund_processing` | The refund is being sent | |
| `refunded` | The USDC went back. See `refundTxHash` and `refundDestination` | Yes |
| `expired` | The deadline passed with no deposit. No money moved | Yes |
| `failed` | Linq needs to look at this order. Contact Linq | Yes |

From `disbursed` onwards your merchant has their money, so treat `disbursed` as paid.

An order also reports `statusHistory`, each move with its time, and `statusReason`, a short fixed message when a payout failed, such as "The payout could not be completed, so the deposit is being refunded." Contact Linq for the specific cause. Once it applies, `payoutReference` identifies the bank transfer, and `depositTxHash`, `sweepTxHash` and `refundTxHash` are Stellar transactions. `payoutAttempts` lists each try at the payout, first to last, with the reference sent to the payout provider; a retry after a confirmed failure has a reference of its own.

## Webhooks

Linq can send order updates to a URL of yours, so you don't have to poll. Give Linq an `https` URL on a public address, and you receive a signing secret, shown only once. You can change the URL later and keep the same secret. Without a webhook, poll `GET /v1/orders/{id}`.

You receive a `POST` with a JSON body when an order reaches one of these statuses: `deposit_detected`, `disbursed`, `settled_in_treasury`, `refund_queued`, `refunded`, `expired` or `failed`.

```json
{
  "event": "order.disbursed",
  "orderId": "6f1c1f3e-2a53-4a4e-9d3e-0b6a0c6f7c11",
  "status": "disbursed",
  "amountNGN": 15000,
  "amountStableCoin": 10,
  "txHash": "8f2b...",
  "quotedNgn": 15000,
  "quotedStableCoin": 10,
  "payoutReference": "7c0e...",
  "depositAddress": "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H",
  "occurredAt": "2026-11-08T12:04:18Z",
  "timestamp": "2026-11-08T12:04:19Z"
}
```

Fields with nothing to report are left out. All of them are in `WebhookEvent` in the spec. `occurredAt` is when the order moved, and `timestamp` is when this attempt was sent.

**Reply with any `2xx` within 10 seconds.** Anything else counts as a failure, including a redirect, which is not followed. Failed deliveries retry after 10 seconds, doubling up to every 10 minutes, until one succeeds. Reports that fall due before you add a webhook are not sent later.

**Expect duplicates.** A delivery can arrive more than once, so ignore any you've already handled, using `orderId`, `status` and `occurredAt` together. Each order's reports are sent oldest first, but a retry can arrive after a later report.

**Verify every delivery.** The `x-linq-signature` header holds `sha256=` followed by the hex HMAC-SHA256 of the raw request body. Check it against the raw bytes before parsing the JSON, then reject a `timestamp` more than 5 minutes old, which stops an old delivery being replayed. Each retry is signed afresh with a new `timestamp`.

```go
func verifyLinqSignature(body []byte, header, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(header))
}
```

## Errors

Errors are JSON with a `message`:

```json
{ "message": "rate is set by this service; leave it out" }
```

| Status | When | What to do |
|---|---|---|
| `400` | The request is invalid. `message` says why | Fix the request |
| `401` | The key is missing, wrong, revoked or expired | Check your key |
| `403` | Your account is suspended | Contact Linq |
| `404` | No such order on your account | Check the id. Other accounts' orders are also 404 |
| `409` | This key is revoked, expired or already rotated | Rotate from your newest key |
| `429` | Over your rate limit | Wait for `Retry-After` |
| `500` | Something went wrong at Linq | Retry with the same `idempotencyKey` |
| `503` | Linq can't price or prepare the order right now | Retry shortly with the same `idempotencyKey` |

Request bodies over 64 KiB are refused with `400`.

## Test mode

Test mode is a separate deployment on the Stellar test network, with its own host and its own `lq_test_` keys. Live and test keys don't work on each other's deployment.

The API is the same. Deposits use testnet USDC, which has no value, and the issuer in each order's `assetIssuer` is the testnet one. Always use the `assetIssuer` from the order: USDC from another issuer won't be recognised as a deposit.
