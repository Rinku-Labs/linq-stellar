#!/usr/bin/env bash
# Shows idempotency and rate limiting on the public API, for the SCF D4
# evidence pack (SD-10). Every request and its answer go to the log.
#
#   BASE_URL=https://... API_KEY=lq_test_... scripts/scf-d4-rate-limit-demo.sh [log]
#
# Run it against the test deployment: the create provisions a real deposit
# account, which costs reserves on mainnet. The burst reads one order, so it
# creates nothing. The key's limit is 120 a minute unless its integrator has an
# override; BURST must be larger than the limit.
set -euo pipefail

: "${BASE_URL:?set BASE_URL}" "${API_KEY:?set API_KEY}"
BURST="${BURST:-130}"
LOG="${1:-scf-d4-rate-limit-demo.log}"
KEY_PREFIX="${API_KEY:0:16}"   # enough to say which key; never the secret
IDEM="scf-d4-demo-$(date -u +%Y%m%dT%H%M%SZ)"
BODY="{\"idempotencyKey\":\"$IDEM\",\"amountNgn\":1000,\"bankCode\":\"058\",\"bankAccount\":\"0000000000\",\"accountName\":\"SCF Demo\"}"

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)" "$*" | tee -a "$LOG"; }
create() {
  curl -sS -o /tmp/scf-d4-body.$$ -w '%{http_code}' -X POST "$BASE_URL/v1/orders" \
    -H "X-API-Key: $API_KEY" -H 'Content-Type: application/json' -d "$BODY"
}
order_id() { sed -n 's/.*"id":"\([^"]*\)".*/\1/p' /tmp/scf-d4-body.$$ | head -1; }

: > "$LOG"
log "base=$BASE_URL key=${KEY_PREFIX}… idempotencyKey=$IDEM burst=$BURST"

log "== Idempotency: the same create twice"
code1=$(create); id1=$(order_id); log "create #1 -> HTTP $code1 order=$id1"
code2=$(create); id2=$(order_id); log "create #2 -> HTTP $code2 order=$id2"
if [[ -n "$id1" && "$id1" == "$id2" ]]; then
  log "RESULT same order returned for the duplicate request: PASS"
else
  log "RESULT duplicate returned a different order or failed: FAIL"
fi

log "== Rate limit: $BURST reads of order $id1 inside one minute"
ok=0; limited=0; other=0; first429=""
for i in $(seq 1 "$BURST"); do
  hdr=$(curl -sS -D - -o /dev/null "$BASE_URL/v1/orders/$id1" -H "X-API-Key: $API_KEY")
  code=$(printf '%s' "$hdr" | head -1 | awk '{print $2}')
  remaining=$(printf '%s' "$hdr" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-ratelimit-remaining"{print $2}')
  retry=$(printf '%s' "$hdr" | tr -d '\r' | awk -F': ' 'tolower($1)=="retry-after"{print $2}')
  case "$code" in
    200) ok=$((ok+1)) ;;
    429) limited=$((limited+1)); [[ -z "$first429" ]] && first429=$i ;;
    *) other=$((other+1)) ;;
  esac
  log "read $i -> HTTP $code remaining=${remaining:-?}${retry:+ retry-after=$retry}"
done
log "RESULT $ok answered, $limited refused with 429 (first at request ${first429:-none}), $other other"
[[ "$limited" -gt 0 ]] && log "RESULT rate limit enforced: PASS" || log "RESULT no 429 seen: FAIL"
rm -f /tmp/scf-d4-body.$$
