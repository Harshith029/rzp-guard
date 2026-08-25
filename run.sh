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

# GOFLAGS carries -buildvcs=false into every build the container performs,
# including the ones tests spawn as subprocesses. Go stamps VCS metadata when it
# finds a repo, and a repo it can find but not read -- a CI export, a shallow
# tree, mismatched ownership inside a container -- fails with
# "error obtaining VCS status: exit status 128". Reproduced against a tree with
# an unreadable .git: eleven operator tests failed before reaching a single
# assertion.
gorun() {
  MSYS_NO_PATHCONV=1 docker run --rm -v "$PWDW":/src -w /src     -e GOFLAGS=-buildvcs=false "$GOIMAGE" "$@"
}

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
cmd_lifecycle()  { gorun go test -tags testhook -v ./cmd/rzp-guard/; }
cmd_lifecycle_race() { gorun go test -tags testhook -race ./cmd/rzp-guard/; }
cmd_all() {
  cmd_test; echo
  cmd_lifecycle; echo
  echo "--- race (default lane) ---"; cmd_race; echo
  echo "--- race (lifecycle lane, -tags testhook) ---"; cmd_lifecycle_race
}

cmd_operator_setup() {
  cmd_build
  echo "Creating the recovery credential. This is a DEPLOYMENT STEP: the guard"
  echo "refuses to start against a state file that has none."
  ./rzp-guard-operator.exe -mandate "$MANDATE" -state "$EV/block_state.db" init
}

cmd_build() {
  go build -buildvcs=false -o rzp-guard.exe ./cmd/rzp-guard
  go build -buildvcs=false -o gate-verify.exe ./cmd/gate-verify
  go build -buildvcs=false -o rzp-guard-operator.exe ./cmd/rzp-guard-operator
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
  need_keys
  mkdir -p evidence/linux; rm -f evidence/linux/* 2>/dev/null || true

  # THE END-TO-END GATE, ON THE DECLARED DEPLOYMENT TARGET.
  #
  # Everything runs on Linux: static linux/amd64 binaries, the SHIPPED operator
  # with no escape flags (Linux honours 0600 and supports directory fsync, so
  # the supported provisioning path works exactly as documented), and the
  # production guard spawning Razorpay's official pinned container as a sibling
  # through the mounted Docker socket.
  #
  # A previous version ran the native Windows guard, which meant the strongest
  # evidence was produced on a platform the project declares unsupported, using
  # test-hook escapes to provision. That gap is what this closes.
  MSYS_NO_PATHCONV=1 docker run --rm -v "$PWDW":/src -w /src -e CGO_ENABLED=0 \
      -e GOOS=linux -e GOARCH=amd64 -e GOFLAGS=-buildvcs=false "$GOIMAGE" sh -c '
    mkdir -p .gotmp/linux
    go build -buildvcs=false -o .gotmp/linux/rzp-guard ./cmd/rzp-guard
    go build -buildvcs=false -o .gotmp/linux/rzp-guard-operator ./cmd/rzp-guard-operator
    go build -buildvcs=false -o .gotmp/linux/gate-verify ./cmd/gate-verify
  '

  MSYS_NO_PATHCONV=1 docker run --rm \
      -v /var/run/docker.sock:/var/run/docker.sock \
      -v "$PWDW":/src -w /src \
      -e RAZORPAY_KEY_ID -e RAZORPAY_KEY_SECRET \
      docker:cli sh -c '
    set -e
    GATE=$(mktemp -d)
    trap "rm -rf $GATE" EXIT

    # Shipped operator, supported path, no escape flags. The token goes to an OS
    # temp dir, never into this OneDrive-backed tree.
    ./.gotmp/linux/rzp-guard-operator -mandate examples/mandate.json \
        -state "$GATE/state.db" init -out "$GATE/token" > /dev/null
    echo "provisioned with the shipped operator; token mode $(stat -c %a "$GATE/token")"

    printf "%s\n" \
     "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2024-11-05\",\"capabilities\":{},\"clientInfo\":{\"name\":\"linux-gate\",\"version\":\"1\"}}}" \
     "{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}" \
     "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"create_refund\",\"arguments\":{\"payment_id\":\"pay_SYN99999999999\",\"amount\":90000}}}" \
     "{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"tools/call\",\"params\":{\"name\":\"fetch_all_payments\",\"arguments\":{\"count\":1}}}" \
    | ./.gotmp/linux/rzp-guard -mandate examples/mandate.json -state "$GATE/state.db" \
        -child-tee evidence/linux/block_child_stdin.jsonl \
        -decision-log evidence/linux/block_decisions.jsonl \
        > evidence/linux/block_stdout.jsonl 2> evidence/linux/block_stderr.txt

    echo
    ./.gotmp/linux/gate-verify block evidence/linux
  '
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
  # Runs entirely INSIDE the golang container.
  #
  # Two reasons, both measured. The gate needs no Docker child -- it uses a
  # local non-responding stub -- so nothing here wants the host. And Windows
  # Application Control persistently refuses to execute the -tags testhook
  # guard binary on this host (FAILURES.md F9), while shipped builds run fine;
  # a fresh output path did not help. The container sidesteps that and is
  # already the canonical runner for everything else.
  mkdir -p "$EV"; rm -f "$EV"/recover_* 2>/dev/null || true
  MSYS_NO_PATHCONV=1 docker run --rm -v "$PWDW":/src -w /src       -e GOFLAGS=-buildvcs=false "$GOIMAGE" sh -c '
    set -e
    go build -buildvcs=false -tags testhook -o /tmp/guard-th ./cmd/rzp-guard
    go build -buildvcs=false -o /tmp/op ./cmd/rzp-guard-operator
    go build -buildvcs=false -o /tmp/gate-verify ./cmd/gate-verify
    EV=evidence/live

    # REAL provisioning, on the SHIPPED operator binary, with no escape flags.
    # Linux supports directory fsync and honours 0600, so the supported path
    # works here exactly as a deployment would use it. The token goes to an OS
    # temp dir, never into this OneDrive-backed tree.
    GATE_DIR=$(mktemp -d)
    /tmp/op -mandate examples/mandate.json -state "$EV/recover_state.db"         init -out "$GATE_DIR/token" > /dev/null

    ( printf "%s\n" "{\"jsonrpc\":\"2.0\",\"id\":9,\"method\":\"tools/call\",\"params\":{\"name\":\"create_refund\",\"arguments\":{\"payment_id\":\"pay_SYN00000000001\",\"amount\":50000}}}"
      sleep 20 )     | RZP_GUARD_CHILD_CMD="head -c 120 > /dev/null; exit 0"       RAZORPAY_KEY_ID=rzp_test_stub RAZORPAY_KEY_SECRET=stub       /tmp/guard-th -mandate examples/mandate.json -state "$EV/recover_state.db"         -child-tee "$EV/recover_child_stdin.jsonl"         > "$EV/recover_stdout.jsonl" 2> "$EV/recover_stderr.txt" || true

    printf "%s\n" "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"create_refund\",\"arguments\":{\"payment_id\":\"pay_SYN00000000001\",\"amount\":50000}}}"     | RZP_GUARD_CHILD_CMD="cat > /dev/null"       RAZORPAY_KEY_ID=rzp_test_stub RAZORPAY_KEY_SECRET=stub       /tmp/guard-th -mandate examples/mandate.json -state "$EV/recover_state.db"         > "$EV/recover_restart.jsonl" 2>/dev/null || true

    echo
    rm -rf "$GATE_DIR"
    /tmp/gate-verify recover "$EV"
  '
}

usage() {
  cat <<'EOF'
rzp-guard

  ./run.sh test              fast lane: all unit tests (no Docker child, no keys, no network)
  ./run.sh race              fast lane under the race detector
  ./run.sh lifecycle         process-lifecycle tests (-tags testhook; NOT in the
                             default lane, reported separately)
  ./run.sh lifecycle-race    lifecycle lane under the race detector
  ./run.sh all               every lane: default, lifecycle, and BOTH race runs
  ./run.sh build             build all three binaries
  ./run.sh operator-setup    ONCE: create the recovery credential (deployment step)

  ./run.sh live-block        LIVE, ON LINUX: production guard + shipped operator
                             (no escape flags) -> official pinned container.
                             Proves BLOCKING ONLY, with an enforced alive-control.
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
  lifecycle-race) cmd_lifecycle_race ;;
  all) cmd_all ;;
  vet) cmd_vet ;;
  build) cmd_build ;;
  operator-setup) cmd_operator_setup ;;
  live-block) cmd_live_block ;;
  process-recover) cmd_process_recover ;;
  help) usage ;;
  *) usage; exit 1 ;;
esac
