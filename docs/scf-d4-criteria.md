# SCF Tranche 2, Deliverable 4: criteria → proof

Deliverable 4, Public Settlement API & Reconciliation: where each success
criterion and each required proof lives, and how to check it. The style follows
[`scf-criteria.md`](scf-criteria.md).

**Status key:** ✅ done and checkable now · ⏳ built, proof produced after the
mainnet rollout (SD-11) · ❌ not done. Nothing marked ✅ depends on a
screenshot alone.

## How requests reach the API

The integrator's requests enter through our existing B2B endpoint, which now
calls the public settlement API. Before D4, that endpoint settled Stellar
through an earlier implementation in our main backend.

In detail: a business already integrated with Linq's B2B API (`POST /b2b/offramp`
on the Linq backend) keeps its URL, key and fields. For a business switched on,
the Linq backend creates each Stellar USDC order on this service's
`POST /v1/orders` as integrator `linq-v2-b2b`, and this service (the D2
settlement layer) watches the deposit, requests the NGN payout, sweeps to
treasury, refunds on failure and reports every status change back. NGN is paid
by Nomba through the Linq backend's `/internal/payout`.

**Only orders created after routing was switched on for the business are
counted** as proof. The export (`scf-d4-evidence -since`) enforces this.

## Success criteria

| Criterion | Built in | Proof | Status |
|---|---|---|---|
| Public API live and documented: authentication, payment initiation, status lookup, signed webhooks, test mode | [`api/public.go`](../internal/api/public.go), [`notify`](../internal/notify/), [`docs/api`](api/) | Docs served at `/docs` (with `PUBLIC_API_ENABLED=true`); OpenAPI at [`docs/api/openapi.yaml`](api/openapi.yaml); a test fails if a reply carries a field the spec doesn't describe | ✅ API and docs · ⏳ test mode: deployment pending (SD-7, linq-stellar #8) |
| Per-business credentials: issuance, rotation, revocation for at least one integrator | [`store/integrator.go`](../internal/store/integrator.go): hashed keys, 24-hour rotation overlap, revocation follows rotations, append-only `credential_events` | `credential-events.csv` and `api-keys.csv` from the export | ⏳ run on the test integrator |
| Rate limiting and idempotent requests under repeated and duplicate calls | [`store/ratelimit.go`](../internal/store/ratelimit.go) (per key, Postgres counter, 429 + `Retry-After`); idempotency per integrator on create | `scripts/scf-d4-rate-limit-demo.sh` log: the duplicate create returns the same order, and the burst gets 429s | ⏳ run on the test deployment |
| At least one unaffiliated business processing Stellar payments through the API | Linq backend routing (Linq-v2 #105) to `/v1` | `payments.csv`: deposit transaction hashes with Stellar Expert links, only orders after the switch | ⏳ after SD-11 · see [Open items](#open-items) on "unaffiliated" |
| 5+ NGN disbursements for API orders, each with transaction hash and bank payout reference | [`payout/worker.go`](../internal/payout/worker.go) → Linq backend `/internal/payout` → Nomba | `disbursements.csv`: deposit hash, NGN amount, Linq payout ID, bank reference, deposit-to-payout seconds | ⏳ after SD-11 |
| 10+ automated reconciliation cycles, a record per cycle | [`reconcile`](../internal/reconcile/reconcile.go): every 15 minutes, six checks, read-only | `reconciliation-cycles.csv`, `reconciliation-mismatches.csv` | ⏳ accrues after deploy (10 cycles ≈ 2.5 hours) |
| Reconciliation dashboard live | [`api/reconciliation_page.go`](../internal/api/reconciliation_page.go) at `/reconciliation`, opened with a reviewer token. `/reconciliation/proof` shows this page's live proof: payments, disbursements, cycles, failure demos | Link plus reviewer token, given on request | ⏳ after deploy |
| Transient payout failure, then an automatic retry that completes, with timestamped logs | [`payout/worker.go`](../internal/payout/worker.go): attempts recorded per reference | `failures.md`: status history with times, each attempt and its reference, webhook deliveries | ⏳ after SD-11 · **induced**, see below |
| Terminal payout failure, then merchant notification and an on-chain refund, with logs and the refund hash | Same, then [`settle/workers.go`](../internal/settle/workers.go) refund | `failures.md`: attempts exhausted, `refund_queued` and `refunded` reported to the merchant, refund transaction hash | ⏳ after SD-11 · **induced** |

## The guarantees D4 carries over from D2

| Guarantee | Where | Proof |
|---|---|---|
| Linq pays provisioning and settlement network fees | Sponsored reserves ([`stellar/provision.go`](../internal/stellar/provision.go)); `FeeUSDC` fixed at 0 | The sponsor and treasury accounts' public Horizon history |
| Merchant notified within 60 seconds | Outbox delivery per integrator ([`notify`](../internal/notify/)) | `notifications.csv`: time from status change to delivery for every live order; the summary lists any over 60 seconds rather than averaging them away |
| Timestamped status history per order | `stellar_order_status_events` | `statusHistory` on `GET /v1/orders/{id}` |

## Required proof

| Proof | Where | Status |
|---|---|---|
| API documentation or OpenAPI spec covering every endpoint and the authentication flow | [`docs/api/README.md`](api/README.md), [`docs/api/openapi.yaml`](api/openapi.yaml), served at `/docs` | ✅ |
| Credential rotation and revocation logs for the test integrator | `credential-events.csv` | ⏳ |
| Transaction hashes from the unaffiliated integrator's live usage | `payments.csv` | ⏳ |
| Transaction hash and bank payout reference for each of the 5+ disbursements | `disbursements.csv`. The bank reference comes from the Linq backend's `GET /internal/payout` (`bankReference`, added in Linq-v2 #105) | ⏳ |
| A reconciliation record per cycle: what was checked, orders matched, any mismatch and its resolution | `reconciliation-cycles.csv`, `reconciliation-mismatches.csv`, and the dashboard | ⏳ |
| Dashboard link or screenshots | `/reconciliation` | ⏳ |
| A test or sandbox environment separate from production | Testnet deployment with its own database, sponsor and treasury, and a payout provider that pays nothing and is refused on mainnet (linq-stellar #8) | ⏳ deployment pending |
| Reviewer test credentials issued on request | `POST /admin/integrators/{id}/keys` on the test deployment | ⏳ |
| A reproducible path: test payment → webhook → simulated failure and retry → reconciliation record | `docs/test-mode.md` (linq-stellar #8) | ⏳ walkthrough to be run end to end on the deployment |

## Failure demonstrations are induced

Both failure demonstrations run on Linq's own test orders, on an integrator
with `allow_failure_injection` set by hand in the database. An induced attempt
is recorded with the reason `induced for demonstration` and never reaches the
payout provider. Everything after it (the retry under a new reference, the
refund, the merchant notification) runs as it does for any order. The export
and this page say so wherever they show them.

A real payout failure is handled more carefully than an induced one. The Linq
backend reports some payouts as failed when the bank never confirmed the
outcome, so a real failure is not retried under a new reference unless the
backend confirms no money moved, which it does not do yet. Until it does, a
real failure stops for a person to check rather than risking a second payout.

## Producing the evidence

```bash
# On the production database, read-only:
DATABASE_URL=... LINQ_API_URL=... LINQ_INTERNAL_SECRET=... \
go run ./cmd/scf-d4-evidence \
  -integrator <linq-v2-b2b integrator id> -business <business id> \
  -since <time routing was switched on, RFC3339> \
  -credential-integrator <test integrator id> -demo-integrator <linq-demo id> \
  -reconciliation-url https://<host>/reconciliation -out scf-d4-evidence

# On the test deployment:
BASE_URL=https://<test host> API_KEY=lq_test_... \
scripts/scf-d4-rate-limit-demo.sh scf-d4-evidence/rate-limit-demo.log
```

`summary.md` in the output marks each criterion met or not yet. The export
reads no bank account numbers, account names, bank names or key secrets.

## Open items

Listed rather than left out.

1. **"Unaffiliated" (SD-12).** The business whose orders are counted must be
   confirmed as unaffiliated with Linq, with no shared ownership or staff.
   Record that decision before submitting.
2. **Closed-beta users moved onto the new API.**
   - Businesses on the Linq backend's B2B API move as their routing is
     switched on, one by one.
   - Linq-B2B, the original closed-beta caller, still uses this service's
     legacy `/orders` routes with its shared key. Those orders are now recorded
     under its own integrator (`int_linq_b2b`) and covered by the same
     reconciliation. Its legacy routes have no per-key rate limit, and it has
     not moved to `/v1` keys.
   - Say this as it is.
3. **Real (not induced) payout retries** are off until the Linq backend reports
   which failures moved no money. See above.
4. **Test mode** is built, but the deployment and the end-to-end walkthrough
   are still to be done (linq-stellar #8).
5. **Bank payout references** depend on Linq-v2 #105 being deployed, which adds
   `bankReference` to `GET /internal/payout`.
