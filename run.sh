#!/usr/bin/env bash
# rzp-guard — one entry point per lane. Works in bash, git-bash and CI.
#
# The golang container is the canonical test runner. Two host constraints forced
# that, both documented in FAILURES.md: no C toolchain on the dev host
# (CGO_ENABLED=0, so -race cannot run natively), and Windows Application Control
# intermittently blocking freshly built test binaries.
set -euo pipefail

GOIMAGE="${GOIMAGE:-golang:1.26}"
MANDATE="${MANDATE:-examples/mandate.json}"
cd "$(dirname "$0")"
PWDW="$(pwd -W 2>/dev/null || pwd)"
EV=evidence/live

gorun() { MSYS_NO_PATHCONV=1 docker run --rm -v "$PWDW":/src -w /src "$GOIMAGE" "$@"; }

need_keys() {
  [ -f .env ] || { echo "ERROR: .env not found. Live gates need test-mode keys." >&2; exit 1; }
  set -a; . ./.env; set +a
  case "${RAZORPAY_KEY_ID:-}" in
    rzp_test*) : ;;
    *) echo "ERROR: RAZORPAY_KEY_ID must be a test-mode key (rzp_test prefix)." >&2; exit 1 ;;
  esac
}

cmd_test()  { gorun go test ./...; }
cmd_race()  { gorun go test -race ./...; }
cmd_vet()   { gorun go vet ./...; }

# Process-lifecycle tests live behind -tags testhook, so plain "go test ./..."
# does NOT run them -- cmd/rzp-guard reports "no test files" in the default
# lane. They were being counted in the advertised suite while being excluded
# from it. This lane runs them explicitly and is reported separately.
cmd_lifecycle() { gorun go test -tags testhook -v -run Terminates -run . ./cmd/rzp-guard/; }
cmd_all()       { cmd_test; echo; cmd_lifecycle; }

cmd_build() {
  go build -o rzp-guard.exe ./cmd/rzp-guard
  go build -o gate-verify.exe ./cmd/gate-verify
  go build -o rzp-guard-operator.exe ./cmd/rzp-guard-operator
  echo "built ./rzp-guard.exe ./gate-verify.exe ./rzp-guard-operator.exe"
}

# THE central proof: a call the mandate does not authorize never crosses into
# Razorpay's official server.
#
# Every condition is enforced by gate-verify, which parses the captured JSON.
# The earlier version printed its control and exited 0 whenever the blocked call
# was simply absent from the tee -- which also passes against a dead container
# or invalid credentials, the exact cases the control exists to rule out.
cmd_live_block() {
  cmd_build; need_keys
  mkdir -p "$EV"; rm -f "$EV"/block_* 2>/dev/null || true
  printf '%s\n' \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"live-gate","version":"1"}}}' \
    '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"create_refund","arguments":{"payment_id":"pay_SYN99999999999","amount":90000}}}' \
    '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fetch_all_payments","arguments":{"count":1}}}' \
  | ./rzp-guard.exe -mandate "$MANDATE" -state "$EV/block_state.db" \
      -child-tee "$EV/block_child_stdin.jsonl" \
      -decision-log "$EV/block_decisions.jsonl" \
      > "$EV/block_stdout.jsonl" 2> "$EV/block_stderr.txt"
  echo ""
  ./gate-verify.exe block "$EV"
}

# Process-boundary recovery. Deliberately NOT called "live": it exercises the
# guard's cleanup wiring with a local non-responding stub, not the official
# container. Reserving "live" for paths that really drive razorpay/mcp keeps the
# evidence honest.
#
# The stub is required, and the reason was measured rather than assumed: against
# the real container Razorpay answers in well under a second, so the reply won
# the race at both a 2s and a 0.15s kill and the death path was never exercised.
#
# Built with -tags testhook. The shipped binary has no arbitrary-child path, and
# the stub is given NO Razorpay credentials.
cmd_process_recover() {
  cmd_build
  go build -tags testhook -o rzp-guard-testhook.exe ./cmd/rzp-guard
  mkdir -p "$EV"; rm -f "$EV"/recover_* 2>/dev/null || true
  ( printf '%s\n' \
      '{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"create_refund","arguments":{"payment_id":"pay_SYN00000000001","amount":50000}}}'
    sleep 30 ) \
  | RZP_GUARD_CHILD_CMD='head -c 120 > /dev/null; exit 0' \
    RAZORPAY_KEY_ID=rzp_test_stub RAZORPAY_KEY_SECRET=stub \
    ./rzp-guard-testhook.exe -mandate "$MANDATE" -state "$EV/recover_state.db" \
      -child-tee "$EV/recover_child_stdin.jsonl" \
      > "$EV/recover_stdout.jsonl" 2> "$EV/recover_stderr.txt"

  printf '%s\n' \
    '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_refund","arguments":{"payment_id":"pay_SYN00000000001","amount":50000}}}' \
  | RZP_GUARD_CHILD_CMD='cat > /dev/null' \
    RAZORPAY_KEY_ID=rzp_test_stub RAZORPAY_KEY_SECRET=stub \
    ./rzp-guard-testhook.exe -mandate "$MANDATE" -state "$EV/recover_state.db" \
      > "$EV/recover_restart.jsonl" 2>/dev/null
  echo ""
  ./gate-verify.exe recover "$EV"
}

usage() {
  cat <<'EOF'
rzp-guard

  ./run.sh test              fast lane: all unit tests (no Docker child, no keys, no network)
  ./run.sh race              fast lane under the race detector
  ./run.sh lifecycle         process-lifecycle tests (-tags testhook; NOT in the
                             default lane, reported separately)
  ./run.sh all               fast lane + lifecycle lane
  ./run.sh build             build ./rzp-guard.exe and ./gate-verify.exe

  ./run.sh live-block        LIVE: unauthorized refund never reaches the real
                             pinned container, with an enforced alive-control
  ./run.sh process-recover   child death -> durable IN_DOUBT surviving restart
                             (local stub child; not the official container)

live-block needs Docker running and test-mode keys in .env.
RAZORPAY_KEY_ID must start with rzp_test; the guard refuses anything else.
EOF
}

case "${1:-help}" in
  test) cmd_test ;;
  race) cmd_race ;;
  lifecycle) cmd_lifecycle ;;
  all) cmd_all ;;
  vet) cmd_vet ;;
  build) cmd_build ;;
  live-block) cmd_live_block ;;
  process-recover) cmd_process_recover ;;
  help) usage ;;
  *) usage; exit 1 ;;
esac
