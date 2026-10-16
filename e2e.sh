#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

BASE="${BASE_URL:-http://localhost:8080}"
ACME_KEY="${ACME_KEY:-acme-secret-key}"
GLOBEX_KEY="${GLOBEX_KEY:-globex-secret-key}"
CONFIRMATIONS="${CONFIRMATIONS:-5}"
TIMEOUT="${TIMEOUT:-30}"
AML_TIMEOUT="${AML_TIMEOUT:-90}"
RESTART_CMD="${RESTART_CMD-docker compose restart app}"
PAYER="0x1111111111111111111111111111111111111111"
BLACKLISTED="0xbad0000000000000000000000000000000000bad"
RUN_ID="$(date +%s)-$RANDOM"

passed=0
failed=0

section() { printf '\n== %s\n' "$*"; }
pass() { passed=$((passed + 1)); printf '  ok    %s\n' "$*"; }
fail() { failed=$((failed + 1)); printf '  FAIL  %s\n' "$*"; }

expect() {
	local desc=$1 got=$2 want=$3
	if [[ "$got" == "$want" ]]; then pass "$desc"; else fail "$desc: got '$got', want '$want'"; fi
}

request() {
	local method=$1 path=$2 key=$3 data=$4
	shift 4
	local args=(-sS -X "$method" -w $'\n%{http_code}' -H 'Content-Type: application/json')
	[[ -n "$key" ]] && args+=(-H "X-API-Key: $key")
	[[ -n "$data" ]] && args+=(-d "$data")
	local header
	for header in "$@"; do args+=(-H "$header"); done
	local out
	if ! out=$(curl "${args[@]}" "$BASE$path" 2>/dev/null); then
		STATUS=000
		BODY=""
		return 0
	fi
	BODY=${out%$'\n'*}
	STATUS=${out##*$'\n'}
}

json() {
	printf '%s' "$BODY" | grep -o "\"$1\":\"\{0,1\}[^\",}]*" | head -n 1 | sed 's/^[^:]*:"\{0,1\}//'
}

deposit_statuses() {
	printf '%s' "$BODY" | grep -o '"status":"[a-z_]*"' | tail -n +2 | sed 's/.*:"//; s/"//' | paste -sd ' ' -
}

deposit_block_hashes() {
	printf '%s' "$BODY" | grep -o '"block_hash":"[0-9a-fx]*"' | sed 's/.*:"//; s/"//' | paste -sd ' ' -
}

micro() {
	local whole=${1%.*} frac=${1#*.}
	echo $((10#$whole * 1000000 + 10#$frac))
}

balance() {
	request GET /v1/balance "$1" ""
	AVAILABLE=$(micro "$(json available)")
	ON_HOLD=$(micro "$(json on_hold)")
}

create_invoice() {
	request POST /v1/invoices "$1" "{\"amount\":\"$3\"}" "Idempotency-Key: e2e-$RUN_ID-$2"
	INVOICE_ID=$(json id)
	INVOICE_ADDRESS=$(json address)
}

get_invoice() {
	request GET "/v1/invoices/$2" "$1" ""
	INVOICE_STATUS=$(json status)
	DEPOSITS=$(deposit_statuses)
}

send() {
	request POST /sim/transfers "" "{\"from\":\"$1\",\"to\":\"$2\",\"amount\":\"$3\"}"
	[[ "$STATUS" == 201 ]] || fail "send transfer: HTTP $STATUS $BODY"
}

mine() {
	request POST /sim/blocks "" "{\"count\":$1}"
	[[ "$STATUS" == 200 ]] || fail "mine $1 blocks: HTTP $STATUS $BODY"
}

reorg() {
	request POST /sim/reorg "" "{\"depth\":$1,\"reinclude\":$2}"
	[[ "$STATUS" == 200 ]] || fail "reorg depth $1: HTTP $STATUS $BODY"
}

set_aml() {
	request PUT /sim/aml "" "{\"available\":$1}"
	[[ "$STATUS" == 200 ]] || fail "set AML available=$1: HTTP $STATUS $BODY"
}

wait_invoice() {
	local key=$1 id=$2 want_status=$3 want_deposits=$4 timeout=${5:-$TIMEOUT}
	local deadline=$((SECONDS + timeout))
	while :; do
		get_invoice "$key" "$id"
		if [[ "$INVOICE_STATUS" == "$want_status" && "$DEPOSITS" == "$want_deposits" ]]; then
			pass "invoice $id is $want_status [$want_deposits]"
			return 0
		fi
		if ((SECONDS >= deadline)); then
			fail "invoice $id: got $INVOICE_STATUS [$DEPOSITS], want $want_status [$want_deposits] within ${timeout}s"
			return 0
		fi
		sleep 0.5
	done
}

wait_healthy() {
	local deadline=$((SECONDS + ${1:-60}))
	until request GET /healthz "" "" && [[ "$STATUS" == 200 ]]; do
		if ((SECONDS >= deadline)); then
			return 1
		fi
		sleep 1
	done
}

section "Preflight"
if ! wait_healthy 10; then
	echo "service is not reachable at $BASE (start it with: docker compose up --build)"
	exit 1
fi
pass "service is healthy at $BASE"

section "Auth, idempotency and partner isolation"
request GET /v1/balance "" ""
expect "no API key is rejected" "$STATUS" 401
request GET /v1/balance "wrong-key" ""
expect "wrong API key is rejected" "$STATUS" 401
request POST /v1/invoices "$ACME_KEY" "{\"amount\":\"1\"}"
expect "invoice without Idempotency-Key is rejected" "$STATUS" 400
create_invoice "$ACME_KEY" idempotent 100
expect "first create returns 201" "$STATUS" 201
first_id=$INVOICE_ID
first_address=$INVOICE_ADDRESS
create_invoice "$ACME_KEY" idempotent 100
expect "repeated create returns 200" "$STATUS" 200
expect "repeated create returns the same invoice" "$INVOICE_ID/$INVOICE_ADDRESS" "$first_id/$first_address"
create_invoice "$GLOBEX_KEY" idempotent 100
[[ "$INVOICE_ID" != "$first_id" ]] && pass "same key for another partner creates another invoice" || fail "another partner reused invoice $first_id"
request GET "/v1/invoices/$first_id" "$GLOBEX_KEY" ""
expect "partner cannot see another partner's invoice" "$STATUS" 404
request POST /v1/invoices "$ACME_KEY" "{\"amount\":\"200\"}" "Idempotency-Key: e2e-$RUN_ID-idempotent"
expect "same key with another amount is a conflict" "$STATUS" 409

section "Low risk: credited after $CONFIRMATIONS confirmations minus 1.5% + 0.5 USDT fee"
balance "$ACME_KEY"
start_available=$AVAILABLE
start_hold=$ON_HOLD
create_invoice "$ACME_KEY" low-risk 100
low_id=$INVOICE_ID
send "$PAYER" "$INVOICE_ADDRESS" 100
wait_invoice "$ACME_KEY" "$low_id" processing pending
mine $((CONFIRMATIONS - 2))
sleep 2
get_invoice "$ACME_KEY" "$low_id"
expect "not credited with $((CONFIRMATIONS - 1)) confirmations" "$INVOICE_STATUS [$DEPOSITS]" "processing [pending]"
balance "$ACME_KEY"
expect "balance untouched before confirmations" "$AVAILABLE/$ON_HOLD" "$start_available/$start_hold"
mine 1
wait_invoice "$ACME_KEY" "$low_id" paid credited
balance "$ACME_KEY"
expect "available grew by 98.000000" "$((AVAILABLE - start_available))" 98000000
expect "nothing left on hold" "$ON_HOLD" "$start_hold"

section "Invoice paid by two transfers: money stays on hold until the full amount arrives"
balance "$ACME_KEY"
start_available=$AVAILABLE
start_hold=$ON_HOLD
create_invoice "$ACME_KEY" split 100
split_id=$INVOICE_ID
split_address=$INVOICE_ADDRESS
send "$PAYER" "$split_address" 60
mine $((CONFIRMATIONS - 1))
wait_invoice "$ACME_KEY" "$split_id" partially_paid confirmed
expect "received 60 of 100" "$(json received)" 60.000000
balance "$ACME_KEY"
expect "partial payment is on hold, not available" "$((AVAILABLE - start_available))/$((ON_HOLD - start_hold))" "0/60000000"
send "$PAYER" "$split_address" 40
mine $((CONFIRMATIONS - 1))
wait_invoice "$ACME_KEY" "$split_id" paid "credited credited"
balance "$ACME_KEY"
expect "whole invoice credited once, one fee (100 - 1.5 - 0.5)" "$((AVAILABLE - start_available))/$((ON_HOLD - start_hold))" "98000000/0"

section "High risk: refund instead of credit"
balance "$GLOBEX_KEY"
start_available=$AVAILABLE
start_hold=$ON_HOLD
create_invoice "$GLOBEX_KEY" high-risk 40
high_id=$INVOICE_ID
send "$BLACKLISTED" "$INVOICE_ADDRESS" 40
mine $((CONFIRMATIONS - 1))
wait_invoice "$GLOBEX_KEY" "$high_id" awaiting_payment refunded
balance "$GLOBEX_KEY"
expect "refunded payment never reaches the balance" "$AVAILABLE/$ON_HOLD" "$start_available/$start_hold"

section "AML outage: funds wait on hold, then are credited"
balance "$ACME_KEY"
start_available=$AVAILABLE
start_hold=$ON_HOLD
set_aml false
create_invoice "$ACME_KEY" aml-outage 10
outage_id=$INVOICE_ID
send "$PAYER" "$INVOICE_ADDRESS" 10
mine $((CONFIRMATIONS - 1))
wait_invoice "$ACME_KEY" "$outage_id" processing confirmed
balance "$ACME_KEY"
expect "confirmed payment is on hold, not available" "$((AVAILABLE - start_available))/$((ON_HOLD - start_hold))" "0/10000000"
set_aml true
wait_invoice "$ACME_KEY" "$outage_id" paid credited "$AML_TIMEOUT"
balance "$ACME_KEY"
expect "credited after AML recovered (10 - 0.65)" "$((AVAILABLE - start_available))/$((ON_HOLD - start_hold))" "9350000/0"

section "Reorg before confirmations: transfer disappears, nothing credited"
balance "$ACME_KEY"
start_available=$AVAILABLE
start_hold=$ON_HOLD
create_invoice "$ACME_KEY" reorg 10
reorg_id=$INVOICE_ID
send "$PAYER" "$INVOICE_ADDRESS" 10
mine 1
wait_invoice "$ACME_KEY" "$reorg_id" processing pending
reorg 2 false
wait_invoice "$ACME_KEY" "$reorg_id" awaiting_payment orphaned
mine $((CONFIRMATIONS + 1))
sleep 2
get_invoice "$ACME_KEY" "$reorg_id"
expect "orphaned deposit stays orphaned after more blocks" "$INVOICE_STATUS [$DEPOSITS]" "awaiting_payment [orphaned]"
balance "$ACME_KEY"
expect "no hanging credit or hold" "$AVAILABLE/$ON_HOLD" "$start_available/$start_hold"

section "Reorg with re-inclusion: same transaction lands in a new block, credited once"
balance "$ACME_KEY"
start_available=$AVAILABLE
start_hold=$ON_HOLD
create_invoice "$ACME_KEY" reinclusion 10
reincluded_id=$INVOICE_ID
send "$PAYER" "$INVOICE_ADDRESS" 10
wait_invoice "$ACME_KEY" "$reincluded_id" processing pending
get_invoice "$ACME_KEY" "$reincluded_id"
old_hash=$(deposit_block_hashes)
reorg 1 true
deadline=$((SECONDS + TIMEOUT))
while get_invoice "$ACME_KEY" "$reincluded_id"; [[ "$(deposit_block_hashes)" == "$old_hash" ]] && ((SECONDS < deadline)); do sleep 0.5; done
expect "deposit moved to the new block (one deposit, still pending)" "$DEPOSITS/$( [[ "$(deposit_block_hashes)" != "$old_hash" ]] && echo moved || echo stuck)" "pending/moved"
mine $((CONFIRMATIONS - 1))
wait_invoice "$ACME_KEY" "$reincluded_id" paid credited
balance "$ACME_KEY"
expect "re-included transfer credited exactly once (10 - 0.65)" "$((AVAILABLE - start_available))/$((ON_HOLD - start_hold))" "9350000/0"

section "Repeated processing: rescan the whole chain, nothing credited twice"
balance "$ACME_KEY"
acme_before="$AVAILABLE/$ON_HOLD"
balance "$GLOBEX_KEY"
globex_before="$AVAILABLE/$ON_HOLD"
request POST /sim/rescan "" '{"from_block":0}'
expect "scanner cursor rewound to block 0" "$(json rewound)" true
sleep 5
balance "$ACME_KEY"
expect "acme balance unchanged after rescan" "$AVAILABLE/$ON_HOLD" "$acme_before"
balance "$GLOBEX_KEY"
expect "globex balance unchanged after rescan" "$AVAILABLE/$ON_HOLD" "$globex_before"
for pair in "$low_id:paid [credited]" "$split_id:paid [credited credited]" "$reincluded_id:paid [credited]" "$reorg_id:awaiting_payment [orphaned]"; do
	get_invoice "$ACME_KEY" "${pair%%:*}"
	expect "invoice ${pair%%:*} unchanged after rescan" "$INVOICE_STATUS [$DEPOSITS]" "${pair#*:}"
done

if [[ -z "$RESTART_CMD" ]]; then
	section "Restart (skipped: RESTART_CMD is empty)"
else
	section "Restart mid-processing: payment confirmed before restart, credited once after"
	balance "$ACME_KEY"
	start_available=$AVAILABLE
	start_hold=$ON_HOLD
	set_aml false
	create_invoice "$ACME_KEY" restart 20
	restart_id=$INVOICE_ID
	send "$PAYER" "$INVOICE_ADDRESS" 20
	mine $((CONFIRMATIONS - 1))
	wait_invoice "$ACME_KEY" "$restart_id" processing confirmed
	request GET /sim/head "" ""
	head_before=$(json head)

	echo "  ...   $RESTART_CMD"
	$RESTART_CMD >/dev/null 2>&1 || fail "restart command failed: $RESTART_CMD"
	if wait_healthy 90; then pass "service is healthy after restart"; else fail "service did not come back after restart"; fi
	request GET /sim/head "" ""
	expect "chain survived the restart" "$(json head)" "$head_before"

	wait_invoice "$ACME_KEY" "$restart_id" paid credited "$AML_TIMEOUT"
	balance "$ACME_KEY"
	expect "credited once after restart (20 - 0.8)" "$((AVAILABLE - start_available))/$((ON_HOLD - start_hold))" "19200000/0"

	acme_before="$AVAILABLE/$ON_HOLD"
	request POST /sim/rescan "" '{"from_block":0}'
	sleep 5
	balance "$ACME_KEY"
	expect "rescan after restart credits nothing again" "$AVAILABLE/$ON_HOLD" "$acme_before"
	get_invoice "$ACME_KEY" "$restart_id"
	expect "restart invoice keeps a single credited deposit" "$INVOICE_STATUS [$DEPOSITS]" "paid [credited]"
fi

printf '\n%d passed, %d failed\n' "$passed" "$failed"
((failed == 0))
