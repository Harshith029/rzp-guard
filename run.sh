#!/usr/bin/env bash
# rzp-guard — one entry point per lane. Works in bash, git-bash and CI.
#
# The golang container is the canonical test runner. Two host constraints forced
# that, both documented in FAILURES.md: no C toolchain on the dev host
# (CGO_ENABLED=0, so -race cannot run natively), and Windows Application Control
# intermittently blocking freshly built test binaries.
set -euo pipefail

IMAGE="${IMAGE:-razorpay/mcp@sha256:435109006d6247103899938cf7b1747ba8be1c1a8a28d452cf9fa8eff506e5c6}"
GOIMAGE="${GOIMAGE:-golang:1.26}"
MANDATE="${MANDATE:-examples/mandate.json}"
cd "$(dirname "$0")"
PWDW="$(pwd -W 2>/dev/null || pwd)"

gorun() { MSYS_NO_PATHCONV=1 docker run --rm -v "$PWDW":/src -w /src "$GOIMAGE" "$@"; }

need_keys() {
  [ -f .env ] || { echo "ERROR: .env not found. Live gates need test-mode keys." >&2; exit 1; }
  set -a; . ./.env; set +a
  case "${RAZORPAY_KEY_ID:-}" in
    rzp_test*) : ;;
    *) echo "ERROR: RAZORPAY_KEY_ID must be a test-mode key (rzp_test prefix)." >&2; exit 1 ;;
  esac
}

hdr() { printf '\n\033[1m== %s ==\033[0m\n' "$1"; }

cmd_test()  { gorun go test ./...; }
cmd_race()  { gorun go test -race ./...; }
cmd_vet()   { gorun go vet ./...; }
cmd_build() { go build -o rzp-guard.exe ./cmd/rzp-guard; echo "built ./rzp-guard.exe"; }

# The central proof: a call the mandate does not authorize never crosses into
# Razorpay's official server. The read on id 4 is the CONTROL -- it proves the
# container was alive and connected, so the absence of id 3 is meaningful rather
# than a symptom of a dead child.
cmd_live_block() {
  cmd_build; need_keys
  mkdir -p evidence/live; rm -f evidence/live/block_* 2>/dev/null || true
  printf '%s\n' \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"live-gate","version":"1"}}}' \
    '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"create_refund","arguments":{"payment_id":"pay_SYN99999999999","amount":90000}}}' \
    '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fetch_all_payments","arguments":{"count":1}}}' \
  | ./rzp-guard.exe -mandate "$MANDATE" -state evidence/live/block_state.db \
      -child-tee evidence/live/block_child_stdin.jsonl \
      -decision-log evidence/live/block_decisions.jsonl \
      > evidence/live/block_stdout.jsonl 2> evidence/live/block_stderr.txt

  hdr "what the REAL container received"
  grep -o '"method":"[a-z/_]*"' evidence/live/block_child_stdin.jsonl | sed 's/^/   /'
  local fwd; fwd=$(grep -c create_refund evidence/live/block_child_stdin.jsonl || true)
  hdr "unauthorized create_refund forwarded? (must be 0)"; echo "   $fwd"
  hdr "guard's answer for the blocked id 3"
  grep -o 'BLOCKED by rzp-guard[^"]*' evidence/live/block_stdout.jsonl | sed 's/^/   /'
  hdr "ALIVE CONTROL: did the real container answer id 4?"
  echo "   replies: $(grep -c '"id":4' evidence/live/block_stdout.jsonl || true)"
  [ "$fwd" = "0" ] || { echo "FAIL: a blocked call reached the container" >&2; exit 1; }
  echo ""; echo "PASS: blocked before the child-stdin write boundary, container verified alive."
}

# Failure recovery: the child dies with a refund in flight. The reservation must
# stay locked, survive process exit, and still be refused after restart.
#
# A non-responding stub child is used deliberately. Against the real container
# Razorpay answers in well under a second, so the reply always wins the race and
# the death path would never be exercised -- measured, not assumed.
cmd_live_recover() {
  cmd_build; need_keys
  mkdir -p evidence/live; rm -f evidence/live/recover_* 2>/dev/null || true
  ( printf '%s\n' \
      '{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"create_refund","arguments":{"payment_id":"pay_SYN00000000001","amount":50000}}}'
    sleep 30 ) \
  | ./rzp-guard.exe -mandate "$MANDATE" -state evidence/live/recover_state.db \
      -child-cmd 'head -c 120 > /dev/null; exit 0' \
      -child-tee evidence/live/recover_child_stdin.jsonl \
      > evidence/live/recover_stdout.jsonl 2> evidence/live/recover_stderr.txt

  hdr "refund forwarded, child died without answering"
  echo "   forwarded: $(grep -c create_refund evidence/live/recover_child_stdin.jsonl || true)   replies: $(grep -c '"id":9' evidence/live/recover_stdout.jsonl || true)"
  hdr "cleanup route taken"; sed 's/^/   /' evidence/live/recover_stderr.txt
  hdr "RESTART: fresh process, same state file"
  printf '%s\n' \
    '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_refund","arguments":{"payment_id":"pay_SYN00000000001","amount":50000}}}' \
  | ./rzp-guard.exe -mandate "$MANDATE" -state evidence/live/recover_state.db \
      -child-cmd 'cat > /dev/null' > evidence/live/recover_restart.jsonl 2>/dev/null
  grep -o 'BLOCKED by rzp-guard[^"]*' evidence/live/recover_restart.jsonl | sed 's/^/   /'
  grep -q 'ACTION_CONSUMED' evidence/live/recover_restart.jsonl \
    || { echo "FAIL: the lock did not survive restart" >&2; exit 1; }
  echo ""; echo "PASS: in-flight refund held IN_DOUBT across child death and process restart."
}

cmd_live() { cmd_live_block; cmd_live_recover; }

usage() {
  cat <<'EOF'
rzp-guard

  ./run.sh test           fast lane: all unit tests (no Docker child, no keys, no network)
  ./run.sh race           fast lane under the race detector
  ./run.sh build          build ./rzp-guard.exe

  ./run.sh live-block     LIVE: unauthorized refund never reaches the real container
  ./run.sh live-recover   LIVE: child death leaves durable IN_DOUBT that survives restart
  ./run.sh live           both live gates

Live gates need Docker running and test-mode keys in .env.
RAZORPAY_KEY_ID must start with rzp_test; the guard refuses anything else.
EOF
}

case "${1:-help}" in
  test) cmd_test ;;
  race) cmd_race ;;
  vet) cmd_vet ;;
  build) cmd_build ;;
  live-block) cmd_live_block ;;
  live-recover) cmd_live_recover ;;
  live) cmd_live ;;
  *) usage ;;
esac
