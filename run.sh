#!/usr/bin/env bash
# rzp-guard — one entry point per lane. Works in bash, git-bash and CI.
#
# The golang container is the canonical test runner. Two host constraints forced
# that, both documented in FAILURES.md: no C toolchain on the dev host
# (CGO_ENABLED=0, so -race cannot run natively), and Windows Application Control
# intermittently blocking freshly built test binaries.
set -euo pipefail

# THE TOOLCHAIN IS PINNED BY DIGEST, not by tag.
#
# This was golang:1.26. A tag is mutable: the same command six months from now
# could pull a different compiler and produce different compilation and test
# behaviour, while the repository still presented its gate output as
# reproducible evidence. "Runnable today" is not the same claim as
# "reproducible", and only a digest closes the gap.
#
# GOIMAGE/ALPINE_IMAGE remain overridable for development, but an override is
# reported in gate output so no run can silently claim pinned provenance.
GO_IMAGE_PINNED="golang@sha256:e2f96d803d39f4cb681fa82801be6eacad6337d9f00769918e1e21b5555723ea"
ALPINE_IMAGE_PINNED="alpine@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b"
GOIMAGE="${GOIMAGE:-$GO_IMAGE_PINNED}"
ALPINE="${ALPINE:-$ALPINE_IMAGE_PINNED}"

# Every gate prints this, so the artifacts that produced a result are recorded
# with the result instead of being inferred from the shell environment later.
provenance() {
  echo "toolchain: $GOIMAGE"
  echo "verifier:  $ALPINE"
  if [ "$GOIMAGE" != "$GO_IMAGE_PINNED" ] || [ "$ALPINE" != "$ALPINE_IMAGE_PINNED" ]; then
    echo "WARNING: image override in effect; this run is NOT pinned-reproducible" >&2
  fi
}
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
# The study provider. `proxy` by default: it is what this project has
# credentials for. It speaks the Anthropic Messages format but is NOT Anthropic
# -- it is a third-party endpoint that routes to several vendors' models.
RZP_STUDY_PROVIDER="${RZP_STUDY_PROVIDER:-proxy}"

need_study_creds() {
  case "$RZP_STUDY_PROVIDER" in
    proxy)
      if [ -z "${NIHAL_CUSTOM_KEY:-}" ]; then
        echo "NIHAL_CUSTOM_KEY is not set (RZP_STUDY_PROVIDER=proxy)." >&2
        echo "Put it in .env (gitignored) and re-run:" >&2
        echo "  set -a && . ./.env && set +a && ./run.sh study-model -model gpt-5.6-sol" >&2
        exit 2
      fi
      ;;
    openai)
      if [ -z "${OPENAI_API_KEY:-}" ]; then
        echo "OPENAI_API_KEY is not set (RZP_STUDY_PROVIDER=openai)." >&2
        exit 2
      fi
      ;;
    *)
      echo "unknown RZP_STUDY_PROVIDER=$RZP_STUDY_PROVIDER (expected proxy|openai)" >&2
      exit 2
      ;;
  esac
}

# The study runs in the GO image, not alpine.
#
# requireCommittedModelFreeze shells out to git to prove the model choice was
# committed before any trace existed. Alpine has no git, so the check would
# fail closed and block every real run -- correct behaviour, useless outcome.
# Verified: no git in the pinned alpine, git 2.47 in the pinned golang.
study_docker() {
  MSYS_NO_PATHCONV=1 docker run --rm -v "$PWDW":/src -w /src \
      -e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory -e GIT_CONFIG_VALUE_0=/src \
      -e RZP_STUDY_PROVIDER -e RZP_STUDY_PROXY_BASE \
      -e NIHAL_CUSTOM_KEY -e OPENAI_API_KEY \
      "$GOIMAGE" "$@"
}

redact_evidence() {
  # Rebuild the committed projection from raw/. Requires python3; the gate fails
  # rather than publishing raw provider records.
  # The repo venv first: on Windows a bare `python` resolves to the Microsoft
  # Store alias, which prints an advert and exits 0 without redacting anything.
  if [ -x .venv/Scripts/python.exe ]; then PY=.venv/Scripts/python.exe
  elif [ -x .venv/bin/python ]; then PY=.venv/bin/python
  elif command -v python3 >/dev/null 2>&1; then PY=python3
  elif python -c "" >/dev/null 2>&1; then PY=python
  else
    echo "python is required to redact evidence before publishing it" >&2
    exit 2
  fi
  for d in evidence/*/raw; do
    [ -d "$d" ] || continue
    for f in "$d"/*.txt; do
      [ -f "$f" ] || continue
      cp "$f" "$(dirname "$d")/$(basename "$f")"
    done
  done
  "$PY" evidence/redact.py
}

cmd_live_block() {
  provenance
  need_keys
  # Raw provider responses go to a GITIGNORED raw/ directory; only a redacted
  # projection is committed. Writing straight into evidence/linux/ meant every
  # gate run silently republished a payment's contact details, card id and
  # acquirer auth code -- and undid the redaction that had just been applied.
  mkdir -p evidence/linux/raw; rm -f evidence/linux/raw/* 2>/dev/null || true

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
        -child-tee evidence/linux/raw/block_child_stdin.jsonl \
        -decision-log evidence/linux/raw/block_decisions.jsonl \
        > evidence/linux/raw/block_stdout.jsonl 2> evidence/linux/raw/block_stderr.txt

  '

  # Project, then assert on the projection -- never on the raw. If these two
  # ever diverge, the committed evidence would not be what was verified.
  redact_evidence
  MSYS_NO_PATHCONV=1 docker run --rm -v "$PWDW":/src -w /src "$ALPINE"       ./.gotmp/linux/gate-verify block evidence/linux
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
# cmd_live_refund WAS HERE AND HAS BEEN REMOVED.
#
# It took an arbitrary payment id and amount, GENERATED ITS OWN AUTHORIZING
# MANDATE for them, and called Razorpay with whatever credentials were in the
# environment. Whatever its intent, its shape was a reusable refund launcher:
# anyone with keys could point it at any payment for any amount. A mandate the
# tool writes for itself is not an authorization boundary.
#
# This repository is a DEFENCE. It must not also ship a generic money-moving
# command, and a track that disqualifies offense-capable work is not the place
# to argue the distinction.
#
# What replaces it:
#   - the G1.6 refund was a ONE-OFF recorded run; its evidence is committed in
#     redacted form under evidence/g16/
#   - `./run.sh verify-refund-evidence` re-checks that captured evidence with
#     gate-verify, which only ever READS json files and cannot move money
#
# Reproducing the refund itself requires deliberately writing a mandate by hand
# and running the shipped guard. That is a considered act by an operator, not a
# command this repo hands out.
cmd_verify_refund_evidence() {
  provenance
  # READ-ONLY. Parses committed evidence and asserts. No network, no credentials,
  # no child container, nothing that can move money.
  MSYS_NO_PATHCONV=1 docker run --rm -v "$PWDW":/src -w /src -e CGO_ENABLED=0 \
      -e GOOS=linux -e GOARCH=amd64 -e GOFLAGS=-buildvcs=false "$GOIMAGE" \
      go build -buildvcs=false -o .gotmp/linux/gate-verify ./cmd/gate-verify
  MSYS_NO_PATHCONV=1 docker run --rm -v "$PWDW":/src -w /src "$ALPINE" \
      ./.gotmp/linux/gate-verify refund evidence/g16
}

cmd_process_recover() {
  provenance
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

cmd_study_build() {
  # Phase 4b harness. The guard and operator are TEST-HOOK builds, which
  # substitute only the CHILD PROCESS -- policy, relay, ledger and storage are
  # the shipped code paths. The shipped binary against the REAL pinned
  # container is proven separately by live-block and the captured G1.6 evidence.
  MSYS_NO_PATHCONV=1 docker run --rm -v "$PWDW":/src -w /src -e CGO_ENABLED=0 \
      -e GOOS=linux -e GOARCH=amd64 -e GOFLAGS=-buildvcs=false "$GOIMAGE" sh -c '
    mkdir -p .gotmp/linux
    go build -buildvcs=false -tags testhook -o .gotmp/linux/rzp-guard-th ./cmd/rzp-guard
    go build -buildvcs=false -tags testhook -o .gotmp/linux/rzp-guard-operator-th ./cmd/rzp-guard-operator
    go build -buildvcs=false -o .gotmp/linux/mcp-stub ./cmd/mcp-stub
    go build -buildvcs=false -o .gotmp/linux/rzp-study ./cmd/rzp-study
  '
}

cmd_study_verify() {
  cmd_study_build
  MSYS_NO_PATHCONV=1 docker run --rm -v "$PWDW":/src -w /src "$ALPINE" \
      ./.gotmp/linux/rzp-study verify-freeze
}

cmd_study_dry() {
  # Exercises provisioning, guard, stub, decision logging and trace recording
  # with a SCRIPTED fake model. No API key, no spend, and never a study result:
  # every trace it writes is stamped DRY-RUN-SCRIPTED-FAKE.
  cmd_study_build
  MSYS_NO_PATHCONV=1 docker run --rm -v "$PWDW":/src -w /src "$ALPINE" \
      ./.gotmp/linux/rzp-study run -dry-run -out .gotmp/dryrun -runs 1
}

cmd_study_model() {
  need_study_creds
  cmd_study_build
  study_docker ./.gotmp/linux/rzp-study resolve-model -provider "$RZP_STUDY_PROVIDER" "$@"
  echo ""
  echo "Now COMMIT study/model.frozen.json. The runner refuses an uncommitted or"
  echo "modified model freeze: a promise nobody can fail is not a control."
}

cmd_study_smoke() {
  # One REAL trace, to prove the integration before spending the pre-declared
  # 45. Without it, the only way to find a broken transport is to burn the real
  # run, delete it, and start again -- which is the "re-run until it works"
  # freedom the pre-registration exists to remove.
  #
  # It is not a study result: output is forced outside study/ and every trace it
  # writes is stamped "smoke": true.
  need_study_creds
  cmd_study_build
  rm -rf .gotmp/smoke
  study_docker ./.gotmp/linux/rzp-study run -smoke -out .gotmp/smoke "$@"
}

cmd_study_run() {
  need_study_creds
  cmd_study_build
  study_docker ./.gotmp/linux/rzp-study run "$@"
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

  ./run.sh study-verify      Phase 4b: check the frozen protocol is intact
  ./run.sh study-dry         Phase 4b: whole harness on a scripted fake model
                             (no API key, no spend, never a study result)
  ./run.sh study-model       Phase 4b: resolve + record provider, endpoint and
                             model. COMMIT the result BEFORE running traces.
                             Proxy by default; needs NIHAL_CUSTOM_KEY. The
                             proxy publishes no trustworthy model list, so
                             -model <id> is required, e.g. -model gpt-5.6-sol.
  ./run.sh study-smoke       Phase 4b: ONE real trace to prove the integration.
                             Not a study result; cannot write under study/.
  ./run.sh study-run [flags] Phase 4b: run the 45 pre-declared traces
  ./run.sh verify-refund-evidence
                             Re-check the captured G1.6 allow-path evidence.
                             READ-ONLY: no network, no credentials, no refund.
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
  study-verify) cmd_study_verify ;;
  study-dry) cmd_study_dry ;;
  study-model) shift; cmd_study_model "$@" ;;
  study-smoke) shift; cmd_study_smoke "$@" ;;
  study-run) shift; cmd_study_run "$@" ;;
  verify-refund-evidence) cmd_verify_refund_evidence ;;
  live-block) cmd_live_block ;;
  process-recover) cmd_process_recover ;;
  help) usage ;;
  *) usage; exit 1 ;;
esac
