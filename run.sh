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

# WHERE RAW PROVIDER RECORDS GO -- and it is not here.
#
# They used to go to gitignored evidence/*/raw/. That is wrong for this
# repository in a way gitignore cannot fix: the working tree is OneDrive-backed,
# so a file that is never committed is still uploaded. Three raw files under
# evidence/ were found holding a live email address, phone number, card
# last-four and acquirer auth code while sitting in a synced directory. The same
# mistake was already recorded for operator credentials (FAILURES.md F12).
#
# Default is the OS temp directory, which on this host resolves outside
# OneDrive. Overridable, but redact.py refuses any path inside the repo.
RAW_ROOT="${RZP_RAW_EVIDENCE_DIR:-${TMPDIR:-/tmp}/rzp-guard-raw}"
mkdir -p "$RAW_ROOT"
RAW_ROOT_W="$(cd "$RAW_ROOT" && (pwd -W 2>/dev/null || pwd))"
case "$RAW_ROOT_W" in
  "$PWDW"*)
    echo "RAW_ROOT ($RAW_ROOT_W) is inside the workspace." >&2
    echo "This tree is cloud-synced; raw provider records must live outside it." >&2
    exit 2
    ;;
esac
EV=evidence/live

# GOFLAGS carries -buildvcs=false into every build the container performs,
# including the ones tests spawn as subprocesses. Go stamps VCS metadata when it
# finds a repo, and a repo it can find but not read -- a CI export, a shallow
# tree, mismatched ownership inside a container -- fails with
# "error obtaining VCS status: exit status 128". Reproduced against a tree with
# an unreadable .git: eleven operator tests failed before reaching a single
# assertion.
# A named volume holds the Go module cache across runs.
#
# Without it every container starts empty and re-downloads the whole dependency
# set: measured at 56 seconds of `go: downloading` before the demo printed its
# first line, on every single invocation. A reviewer's first impression of this
# project was a minute of scrolling module names.
#
# It stays hermetic. go.sum verifies the hash of every module, so a cache that
# has been tampered with fails the build rather than poisoning it -- the volume
# is a download cache, not a trust boundary.
gorun() {
  MSYS_NO_PATHCONV=1 docker run --rm -v "$PWDW":/src -w /src \
    -v rzpguard-gomodcache:/go/pkg/mod \
    -e GOFLAGS=-buildvcs=false "$GOIMAGE" "$@"
}

need_keys() {
  [ -f .env ] || { echo "ERROR: .env not found. Live gates need test-mode keys." >&2; exit 1; }
  set -a; . ./.env; set +a
  case "${RAZORPAY_KEY_ID:-}" in
    rzp_test*) : ;;
    *) echo "ERROR: RAZORPAY_KEY_ID must be a test-mode key (rzp_test prefix)." >&2; exit 1 ;;
  esac
}

# The 15-second look at what this actually does. No credentials, no network,
# no money: the far side is a buffer, not a provider.
cmd_demo()  { gorun go run ./cmd/rzp-demo; }
cmd_test()  { gorun go test ./...; }
cmd_race()  { gorun go test -race ./...; }
cmd_vet()   { gorun go vet ./...; }

# Process-lifecycle tests live behind -tags testhook, so plain "go test ./..."
# does NOT run them -- cmd/rzp-guard reports "no test files" in the default
# lane. They were being counted in the advertised suite while being excluded
# from it. This lane runs them explicitly and is reported separately.
cmd_lifecycle()  { gorun go test -tags testhook -v ./cmd/rzp-guard/; }
cmd_lifecycle_race() { gorun go test -tags testhook -race ./cmd/rzp-guard/; }
# NOT everything, despite the name.
#
# cmd_all runs the Go test lanes and nothing else: no redteam-negative, no
# preflight, no fuzz, no study, no live gates. Reporting "full suite green" from
# this and letting it imply the negative suite passed is a claim it does not
# support -- and I made exactly that claim. The name is kept for compatibility;
# the usage text states the exclusion, and so does this.
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

# ---------------------------------------------------------------------------
# THE ISOLATED LANE, for external red-team work.
#
# Round 10 of review said the "hard rules" were prose around a runner that
# mounted .env with networking on. Round 11 said this lane still had three holes,
# and it did. Both were right. What follows is the third attempt and it states
# only what it enforces.
#
#   1. EXPORT: tracked files plus untracked-not-ignored files, copied from the
#      WORKING TREE. `git archive HEAD` was wrong -- a reviewer who wrote a
#      failing test could not run it, because the archive holds the last commit.
#      That is an incentive to leave the lane, which is worse than the risk it
#      avoided. Gitignored paths are still excluded, so .env cannot appear.
#   2. SECRETS: the export is scanned for several credential shapes, not only
#      Razorpay's. The scan is a backstop for a mistake, not a guarantee.
#   3. IMAGE: the pinned digest constant directly, never the $GOIMAGE override,
#      and --pull=never on BOTH stages.
#   4. MODULE CACHE: a volume created per invocation and destroyed with the run.
#      A persistent shared cache is mutable state carried between supposedly
#      isolated runs; a poisoned one would survive.
#   5. OFFLINE: the stage that runs your code has --network=none, no Docker
#      socket, credentials emptied, and the cache mounted READ-ONLY.
#   6. CHILD: built with `-tags redteam`, whose newChild has no shell branch at
#      all. Not an environment switch -- a different compiled program.
#
# Usage:  ./run.sh redteam <command...>
#         ./run.sh redteam go test ./...
#         ./run.sh redteam go test -tags redteam,testhook ./cmd/rzp-guard/
#         ./run.sh redteam-selfcheck      # proves the properties above

redteam_export() {
  local work="$1"

  # SYMLINKS ARE REFUSED, and this is the PRIMARY control.
  #
  # The previous version listed untracked-not-ignored files, tested each with
  # `[ -f ]`, and copied with `cp -p`. Both follow symlinks. An untracked,
  # non-ignored link named anything at all could point at the gitignored .env,
  # pass the regular-file test, and have its CONTENTS copied into the export
  # under a different name -- where the literal `.env` name check never looks.
  # Reproduced on Linux before fixing: the copy came out a regular file
  # containing the secret.
  #
  # The credential scan below is a backstop for a mistake. This is the control.
  # There is no reason for a symlink to exist in this lane.
  local rejected=0
  while IFS= read -r -d "" f; do
    if [ -L "$f" ]; then
      echo "REFUSING: $f is a symlink; the export copies file CONTENTS, so a" >&2
      echo "          link can pull a host secret in under an unrelated name" >&2
      rejected=1
    fi
  done < <( git ls-files -z -c -o --exclude-standard )
  [ "$rejected" = 0 ] || exit 1

  while IFS= read -r -d "" f; do
    [ -L "$f" ] && continue          # belt and braces; already refused above
    [ -f "$f" ] || continue
    mkdir -p "$work/$(dirname "$f")"
    cp -p "$f" "$work/$f"
  done < <( git ls-files -z -c -o --exclude-standard )

  local leaked=0 bad
  for bad in .env .env.local .gotmp dist evidence/live; do
    if [ -e "$work/$bad" ]; then
      echo "REFUSING: export contains $bad" >&2
      leaked=1
    fi
  done

  # Credential shapes, NUL-delimited.
  #
  # This loop used to be `for f in $(grep -rlE ...)`, which word-splits: a
  # copied file named "review artifact" became two nonexistent paths and was
  # never scanned. Combined with the symlink hole that was a complete bypass.
  local pat='rzp_(test|live)_[A-Za-z0-9]{10,}|sk-[A-Za-z0-9_-]{20,}|sk-ant-[A-Za-z0-9_-]{20,}|ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|AKIA[A-Z0-9]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----'
  local scan_rc=0
  while IFS= read -r -d "" f; do
    local tail hit=0 tok
    for tok in $(grep -ohE "$pat" "$f" 2>/dev/null); do
      tail="$(printf %s "$tok" | sed -E 's/^(rzp_(test|live)_|sk-ant-|sk-|ghp_|github_pat_)//')"
      case "$(printf %s "$tail" | tr -s 'A-Za-z0-9')" in
        x|X|0|a|A) continue ;;
      esac
      case "$tail" in
        xxx*|XXX*|000*|abc*|your*|stub*|studystub*|REDACTED*) continue ;;
      esac
      hit=1
    done
    if [ "$hit" = 1 ]; then
      echo "REFUSING: $f contains something shaped like a real credential" >&2
      leaked=1
    fi
  done < <( grep -rlZE "$pat" "$work" 2>/dev/null || { scan_rc=$?; [ "$scan_rc" = 1 ] || echo FAIL; } )

  # A scanner that errored is not a scanner that found nothing.
  if [ "$scan_rc" != 0 ] && [ "$scan_rc" != 1 ]; then
    echo "REFUSING: the credential scan failed (exit $scan_rc); a scan that did" >&2
    echo "          not run is not a scan that passed" >&2
    leaked=1
  fi

  [ "$leaked" = 0 ] || exit 1
}

cmd_redteam() {
  [ "$#" -gt 0 ] || { echo "usage: ./run.sh redteam <command...>" >&2; exit 2; }

  # NOT `local`: the EXIT trap runs in global scope where a function-local is
  # already gone, and `set -u` would then abort AFTER a green run.
  REDTEAM_WORK="$(mktemp -d)"
  REDTEAM_VOL="rzpguard-rt-$$-$(date +%s)"
  trap 'rm -rf "${REDTEAM_WORK:-}"; docker volume rm -f "${REDTEAM_VOL:-}" >/dev/null 2>&1 || true' EXIT

  redteam_export "$REDTEAM_WORK"
  local workw; workw="$(cygpath -w "$REDTEAM_WORK" 2>/dev/null || printf %s "$REDTEAM_WORK")"

  # Dependencies fetched HERE, with network, mounting only the manifests --
  # never the export, never the working tree. The pinned digest directly, so a
  # GOIMAGE override cannot redirect the isolated lane.
  echo "--- fetching modules (network on, no source mounted, fresh volume) ---"
  MSYS_NO_PATHCONV=1 docker run --rm --pull=never \
      -v "${REDTEAM_VOL}:/go/pkg/mod" \
      -v "$workw/go.mod:/m/go.mod:ro" -v "$workw/go.sum:/m/go.sum:ro" \
      -w /m -e GOFLAGS=-buildvcs=false "$GO_IMAGE_PINNED" \
      go mod download >/dev/null

  echo "--- running: no network, no socket, no credentials, cache read-only ---"
  MSYS_NO_PATHCONV=1 docker run --rm \
      --network=none --pull=never \
      -v "${REDTEAM_VOL}:/go/pkg/mod:ro" \
      -v "$workw":/src -w /src \
      -e GOFLAGS=-buildvcs=false \
      -e GOPROXY=off \
      -e RAZORPAY_KEY_ID= -e RAZORPAY_KEY_SECRET= \
      -e RZP_STUDY_PROXY_API_KEY= -e OPENAI_API_KEY= -e ANTHROPIC_API_KEY= \
      -e RZP_STUDY_PROVIDER= -e RZP_STUDY_PROXY_BASE= \
      "$GO_IMAGE_PINNED" \
      sh -c '
        # Build the stub the redteam child names, in the SAME container that
        # will run the command -- /tmp does not survive between containers, and
        # a comment in child_redteam.go claimed the lane did this "immediately
        # before use" while nothing did. Building it here does NOT establish
        # identity: anything that can write to the path afterwards can replace
        # it. The comment now says that too.
        #
        # Failure is not fatal: most red-team commands never spawn a child, and
        # a guard binary that needs one already refuses clearly when it is
        # absent.
        # mkdir first and DO NOT swallow the failure.
        #
        # This was `go build ... 2>/dev/null || true`, which discarded any error.
        # Go does create the parent for -o (checked), so the build was in fact
        # working -- but a silently-discarded failure means the claim "the stub
        # is built at the path the constant names" could have been false on every
        # clean run and nothing would have said so. Review was right that it was
        # unproven; it is now enforced.
        mkdir -p /tmp/rzp-redteam-child
        if ! go build -o /tmp/rzp-redteam-child/mcp-stub ./cmd/mcp-stub; then
          echo "redteam lane: FAILED to build the child stub at" >&2
          echo "  /tmp/rzp-redteam-child/mcp-stub -- a guard built with -tags" >&2
          echo "  redteam will refuse to start until this succeeds" >&2
          exit 1
        fi
        exec "$@"
      ' -- "$@"
}

# Proves the lane's properties instead of asserting them in a comment. Run it
# before trusting the lane, and in CI so the properties cannot silently lapse.
cmd_redteam_selfcheck() {
  echo "=== red-team lane self-check ==="
  cmd_redteam sh -c '
    fail=0
    check() { if [ "$2" = ok ]; then echo "  PASS  $1"; else echo "  FAIL  $1"; fail=1; fi; }

    [ ! -e .env ] && check "no .env in the export" ok || check "no .env in the export" no
    [ -z "${RAZORPAY_KEY_ID:-}${RAZORPAY_KEY_SECRET:-}${RZP_STUDY_PROXY_API_KEY:-}${OPENAI_API_KEY:-}" ] \
      && check "credential variables empty" ok || check "credential variables empty" no
    [ ! -e /var/run/docker.sock ] && check "no docker socket" ok || check "no docker socket" no
    if getent hosts proxy.golang.org >/dev/null 2>&1; then check "no DNS" no; else check "no DNS" ok; fi
    [ ! -w /go/pkg/mod ] && check "module cache read-only" ok || check "module cache read-only" no
    # Uncommitted work must be visible, or reviewers leave the lane.
    [ -f run.sh ] && check "working-tree files present" ok || check "working-tree files present" no

    # The redteam child must have no shell branch. Build it and look.
    go build -tags redteam -o /tmp/g ./cmd/rzp-guard 2>/dev/null \
      && check "redteam build succeeds" ok || check "redteam build succeeds" no
    if strings /tmp/g 2>/dev/null | grep -q "RZP_GUARD_CHILD_CMD"; then
      check "no RZP_GUARD_CHILD_CMD in the redteam binary" no
    else
      check "no RZP_GUARD_CHILD_CMD in the redteam binary" ok
    fi

    echo
    [ "$fail" = 0 ] && echo "lane self-check PASSED" || { echo "lane self-check FAILED" >&2; exit 1; }
  '
}

# NEGATIVE TESTS: one per bypass that actually got through.
#
# Review's standing objection, and it was right: a PASS banner from
# redteam-selfcheck is a list of symptoms, not evidence of a boundary. What
# demonstrates a boundary is the attack that used to work and now does not.
#
# Every case below is a real bypass from a previous round, re-run as a test that
# must FAIL. If one of them starts passing, the lane has regressed to a state it
# has already been in once.
cmd_preflight() {
  # PRE-PUSH HISTORY SCAN. Run this before the repository is published, and
  # before any push that adds commits.
  #
  # Round 15, external review. F26 purged a self-authorizing refund launcher
  # from history. The tripwire that existed at the time checked HEAD for runner
  # spellings -- but the defect was reachable HISTORICAL content, which no
  # HEAD-only check can see. A clone carries its history; the rule Track 2
  # applies is about the repository, not the working tree.
  #
  # WHAT THIS CHECKS, precisely: that no reachable blob, in any commit, grants a
  # refund action whose target is interpolated from a caller-supplied value.
  # That is the shape of a launcher -- a tool writing its OWN mandate around an
  # id the caller chose. Every legitimate fixture in this repository names a
  # LITERAL payment id, which is what makes the distinction mechanical.
  #
  # WHY IT DOES NOT GREP THE NAME: four commits still contain the identifier
  # `cmd_live_refund` deliberately -- they are exit-2 tombstones that explain
  # the removal. Counting the identifier would flag those safe carriers and
  # would miss the same code under any other name. The shape is the invariant;
  # the name is not.
  #
  # SCOPE, stated as narrowly as the mechanism allows.
  #
  # This is a TEXT-PATTERN SCAN. What it can support is: "no reachable source
  # in this repository matched the prohibited launcher signature." What it
  # cannot support -- and what an earlier version of this message claimed -- is
  # "no reachable commit can launch a caller-selected refund." That is a claim
  # about program behaviour, and no grep establishes it.
  #
  # Specifically it cannot see: the same capability spelled differently, built
  # by string concatenation, read from a data file, expressed in a language
  # this scan does not read, or reached through a dependency. It also cannot
  # see a shape nobody thought to add to the pattern list.
  #
  # It rejects the shape that actually occurred here (FAILURES.md F18, F26) and
  # renamings of it. It is a regression tripwire, not a proof of safety, and
  # F29 records the time it was reported green without being run.
  #
  # Validated against the pre-rewrite history: these patterns flag exactly the
  # four carrier commits and nothing else, and flag nothing in the current
  # repository. `redteam-negative` N9 keeps that true.
  echo "=== pre-push history scan ==="

  if ! git rev-parse --git-dir >/dev/null 2>&1; then
    echo "  not a git repository" >&2; exit 2
  fi

  local revs found hits pat
  revs="$(git rev-list --all)"
  if [ -z "$revs" ]; then echo "  no commits"; return 0; fi
  found=0

  for pat in     '"payment_id"[[:space:]]*:[[:space:]]*"\$'     '"amount_paise"[[:space:]]*:[[:space:]]*\$'     '"payment_id"[[:space:]]*:[[:space:]]*"%s"' ; do
    hits="$(git grep -nE "$pat" $revs 2>/dev/null || true)"
    if [ -n "$hits" ]; then
      found=1
      echo "REFUSING: a reachable commit grants a refund to a caller-supplied" >&2
      echo "target. This is the launcher shape (FAILURES.md F18, F26)." >&2
      echo "  pattern: $pat" >&2
      printf '%s
' "$hits" | head -20 >&2
    fi
  done

  echo "  commits scanned: $(printf '%s
' "$revs" | grep -c .)"
  if [ "$found" != 0 ]; then
    echo "  RESULT: FAILED -- do not publish this history" >&2
    exit 1
  fi
  echo "  RESULT: no reachable source matched the prohibited launcher signature"
  echo "  (a text-pattern scan; see the SCOPE note in this function)"
}

# rn_exit_guard turns a silent early exit into a loud failure.
#
# cmd_redteam_negative once exited 0 with no summary at all, because N9's
# command substitution tripped `set -e` before any case could report. Silence
# read as success, and a stale summary from an earlier run was quoted as
# current. This makes that impossible: if the suite leaves without emitting its
# terminal summary, the process fails whatever its exit code would have been.
# require_redteam_lane refuses to run the suite when the isolated lane cannot
# execute at all.
#
# N4, N6 and N8 drive cmd_redteam, which runs `docker run --pull=never`. On a
# machine where the pinned image is not local -- every fresh CI runner -- docker
# exits 125 and those tests reported BYPASSED. That is a false security alarm:
# "the bypass worked" and "the lane could not start" are completely different
# facts, and the suite was printing the first when the second was true. Linux CI
# showed N4 and N6 bypassed for months of pushes for exactly this reason.
#
# An environment failure is not a finding. If the lane cannot run, the suite
# says so and stops, rather than emitting security verdicts it did not test.
require_redteam_lane() {
  if ! command -v docker >/dev/null 2>&1; then
    echo "CANNOT RUN: docker is not available, so the isolated lane cannot start." >&2
    echo "  This is an ENVIRONMENT failure, not a security finding. No bypass" >&2
    echo "  verdict below would have been tested." >&2
    RN_SUMMARY_EMITTED=1
    exit 2
  fi
  if ! docker image inspect "$GOIMAGE" >/dev/null 2>&1; then
    echo "CANNOT RUN: the pinned image is not present locally and the isolated" >&2
    echo "  lane uses --pull=never by design, so docker exits 125." >&2
    echo "  image: $GOIMAGE" >&2
    echo "" >&2
    echo "  This is an ENVIRONMENT failure, not a security finding. Pull the" >&2
    echo "  image by digest first, in a separate visible step:" >&2
    echo "    docker pull $GOIMAGE" >&2
    echo "  The execution stage keeps --pull=never; bootstrapping the image is a" >&2
    echo "  distinct, auditable action and does not weaken it." >&2
    RN_SUMMARY_EMITTED=1
    exit 2
  fi
}

rn_exit_guard() {
  if [ "${RN_SUMMARY_EMITTED:-0}" != 1 ]; then
    echo "" >&2
    echo "FAILED: the negative suite exited WITHOUT its terminal summary." >&2
    echo "  cases that reported:${RN_SEEN:- none}" >&2
    echo "  An exit without a summary is not a pass. Do not quote an earlier" >&2
    echo "  run's summary in its place." >&2
    exit 1
  fi
}

cmd_redteam_negative() {
  # Deliberately NOT `local`: the EXIT trap below runs after this function's
  # scope is gone and must still be able to see whether the summary was
  # emitted. N9 aborted this function mid-run and the suite exited 0 with no
  # summary at all -- silence that read as success. That must never be possible
  # again, so the guard lives outside the function's own control flow.
  RN_PASS=0; RN_FAIL=0; RN_SKIP=0
  RN_SEEN=""
  RN_SUMMARY_EMITTED=0
  RN_EXPECTED_CASES="N1 N2 N3 N4 N5 N6 N7 N8 N9 N10"
  trap rn_exit_guard EXIT

  rn_mark() { RN_SEEN="$RN_SEEN ${1%% *}"; }
  ok()   { echo "  BLOCKED   $1"; RN_PASS=$((RN_PASS+1)); rn_mark "$1"; }
  bad()  { echo "  BYPASSED  $1" >&2; RN_FAIL=$((RN_FAIL+1)); rn_mark "$1"; }
  skipped() { echo "  SKIP      $1"; RN_SKIP=$((RN_SKIP+1)); rn_mark "$1"; }
  local pass=0 fail=0 skip=0

  require_redteam_lane
  echo "=== negative tests: each of these once worked ==="

  # N1 -- Round 12. An untracked, non-ignored SYMLINK to the gitignored .env.
  # `git ls-files -o` listed it, `[ -f ]` followed it, `cp -p` copied the
  # CONTENTS in under an unrelated name, and the literal .env check never saw it.
  local tmpdir; tmpdir="$(mktemp -d)"
  # Inside an `if`, not bare: `set -e` is on, and a host where symlink creation
  # returns non-zero would abort the whole suite before reaching the SKIP.
  # This host happens to return success (it copies), which is why the bug was
  # invisible here.
  if ln -s "$PWD/.gitignore" ".redteam-negative-link" 2>/dev/null && [ -L ".redteam-negative-link" ]; then
    if ( redteam_export "$tmpdir" ) >/dev/null 2>&1; then
      bad "N1 untracked symlink was exported"
    else
      ok "N1 untracked symlink refused"
    fi
  else
    skipped "N1 (this host cannot create symlinks; CI runs it on Linux)"
  fi
  rm -f ".redteam-negative-link"; rm -rf "$tmpdir"

  # N2 -- Round 12. A credential-shaped value in a file whose name contains a
  # space. The scan was `for f in $(grep -rlE ...)`, which word-split the path
  # into two nonexistent ones and scanned neither.
  tmpdir="$(mktemp -d)"
  # Assembled at runtime. Writing the literal here would put a credential-
  # shaped string INTO run.sh, which the scanner then correctly flags --
  # refusing every export in the repository. That happened on the first run.
  local fake="rzp_""live_9f3kd82mQ7xZ01aB"
  printf 'SECRET=%s\n' "$fake" > "review artifact.txt"
  if ( redteam_export "$tmpdir" ) >/dev/null 2>&1; then
    bad "N2 credential in a spaced filename was exported"
  else
    ok "N2 credential in a spaced filename refused"
  fi
  rm -f "review artifact.txt"; rm -rf "$tmpdir"

  # N3 -- Round 11. `git archive HEAD` exported the last COMMIT, so a reviewer's
  # new test silently did not run. The positive direction: an untracked sentinel
  # must be present. selfcheck checked `run.sh`, which is tracked, and therefore
  # proved nothing about this.
  tmpdir="$(mktemp -d)"
  local sentinel=".redteam-negative-sentinel"
  printf 'uncommitted\n' > "$sentinel"
  local n3out n3rc
  n3out="$( ( redteam_export "$tmpdir" ) 2>&1 )" && n3rc=0 || n3rc=1
  if [ "$n3rc" != 0 ]; then
    bad "N3 export refused for an unrelated reason: $n3out"
  elif [ -f "$tmpdir/$sentinel" ]; then
    ok "N3 untracked sentinel reaches the export (dirty tree visible)"
  else
    bad "N3 untracked sentinel missing from the export"
  fi
  rm -f "$sentinel"; rm -rf "$tmpdir"

  # N4 -- Rounds 11 and 13. "No configurable child" was first one legacy string,
  # then a deny-list of spellings -- which is the evasion class it claimed to
  # solve. Now a POSITIVE structural assertion over the AST of exactly the files
  # `go list -tags redteam` compiles: one process launch, exec.CommandContext(
  # ctx, redteamChildPath), two arguments, no configuration reads.
  #
  # It runs INSIDE the lane. The previous version called gorun, which mounts the
  # real tree including .env with networking on -- so the evidence command was
  # the bypass.
  if cmd_redteam sh -c '
        set -e
        files=$(go list -tags redteam -f "{{range .GoFiles}}{{\$.Dir}}/{{.}} {{end}}{{range .CgoFiles}}{{\$.Dir}}/{{.}} {{end}}" ./cmd/rzp-guard)
        go run ./cmd/redteam-audit child $files
      ' >/dev/null 2>&1; then
    ok "N4 redteam child has exactly one launch and no configurable path"
  else
    bad "N4 redteam child structure violated (run it directly for the detail)"
  fi

  # N5 -- Round 11. cmd_fuzz called gorun, so the brief's own fuzz command went
  # around the lane. Assert the wrapper still routes through cmd_redteam.
  if sed -n '/^cmd_fuzz()/,/^}/p' run.sh | grep -q 'cmd_redteam'; then
    if sed -n '/^cmd_fuzz()/,/^}/p' run.sh | grep -qE '^\s*gorun '; then
      bad "N5 cmd_fuzz still calls gorun"
    else
      ok "N5 cmd_fuzz routes through the isolated lane"
    fi
  else
    bad "N5 cmd_fuzz does not go through cmd_redteam"
  fi

  # N6 -- Rounds 11 and 13. The stub once accepted any pay_SYN* value and
  # returned its refusal as a SUCCESS carrying an error-shaped body.
  #
  # TWO defects in the test itself, both from review:
  #   - it ran `gorun`, which BUILDS AND EXECUTES cmd/mcp-stub with the real
  #     tree mounted -- .env included -- and network on. A hostile edit to the
  #     stub could read /src/.env and make an outbound request while the
  #     "safe" evidence command ran. That is the whole reason this suite exists,
  #     reintroduced inside it.
  #   - the assertion was a substring match for '"isError":true', which the
  #     stub could satisfy from inside ordinary tool content.
  #
  # Now: built and executed inside the isolated lane, and the reply is PARSED.
  if cmd_redteam sh -c '
        set -e
        mkdir -p /tmp/nt
        # A tracked fixture, not a JSON literal built through three
        # levels of shell quoting. The first attempt mangled the escapes
        # and the stub received unparseable input, which the suite then
        # reported as the stub accepting a bad id.
        go build -o /tmp/nt/stub ./cmd/mcp-stub
        /tmp/nt/stub < ./cmd/redteam-audit/testdata/nonfixture_refund.jsonl > /tmp/nt/out.jsonl
        go run ./cmd/redteam-audit stub /tmp/nt/out.jsonl
      ' >/dev/null 2>&1; then
    ok "N6 stub refuses a non-fixture id with a parsed result.isError"
  else
    bad "N6 stub accepted a fabricated pay_SYN id, or the reply was not a real tool error"
  fi

  # N7 -- Rounds 13 and 14. The evidence command must not itself leave the lane.
  #
  # SCOPE, stated exactly because the first version overclaimed: this checks that
  # THIS FUNCTION contains no call to any container runner other than
  # cmd_redteam. It greps for `gorun`, a bare `docker run`, and the other cmd_*
  # wrappers that use gorun. It does NOT prove the lane is unreachable from
  # elsewhere in run.sh -- only the two execution sites that once escaped, N4 and
  # N6, plus anything added to this function later.
  local body escapes
  body="$(sed -n '/^cmd_redteam_negative()/,/^}/p' run.sh)"
  escapes="$(printf %s "$body" | grep -nE '^[[:space:]]*(gorun |docker run |cmd_test\b|cmd_race\b|cmd_lifecycle\b|cmd_vet\b|cmd_build\b)' || true)"
  if [ -n "$escapes" ]; then
    echo "  BYPASSED  N7 the negative suite runs code outside the isolated lane:" >&2
    printf '%s\n' "$escapes" >&2
    fail=$((fail+1))
  else
    ok "N7 this function runs untrusted code only via cmd_redteam"
  fi

  # N8 -- Round 14. CONTAINMENT, with a dummy sentinel and no real credential.
  #
  # Round 13 proved containment by temporarily editing cmd/mcp-stub to read the
  # real .env and make an outbound HTTPS request. It worked and it was reverted,
  # but review was right that it is poor hygiene for a strictly defense-only
  # track: never mutate a program to read real credentials or demonstrate live
  # egress when a sentinel proves the same thing.
  #
  # So: a gitignored sentinel with invented content, and a DNS lookup. Neither
  # touches a credential, and the test is repeatable rather than a one-off.
  mkdir -p .gotmp
  local sentinel_body="NOT-A-REAL-SECRET-redteam-containment-sentinel"
  printf '%s\n' "$sentinel_body" > .gotmp/containment-sentinel.txt
  local contain
  contain="$(cmd_redteam sh -c '
      # .gotmp is gitignored, so it must not be in the export at all.
      if [ -e /src/.gotmp/containment-sentinel.txt ]; then echo SENTINEL_VISIBLE; fi
      if [ -e /src/.env ]; then echo ENV_VISIBLE; fi
      # And no egress, checked without contacting anything sensitive.
      if getent hosts example.com >/dev/null 2>&1; then echo DNS_WORKS; fi
      echo CONTAINMENT_PROBE_DONE
    ' 2>/dev/null)"
  rm -f .gotmp/containment-sentinel.txt
  case "$contain" in
    *SENTINEL_VISIBLE*) bad "N8 a gitignored file reached the export" ;;
    *ENV_VISIBLE*)      bad "N8 .env is visible inside the lane" ;;
    *DNS_WORKS*)        bad "N8 the lane resolved DNS; egress is possible" ;;
    *CONTAINMENT_PROBE_DONE*) ok "N8 gitignored files absent, .env absent, no DNS" ;;
    *)                  bad "N8 containment probe did not run: $contain" ;;
  esac

  # N9 -- Round 15. The pre-push history scan must be able to FAIL.
  #
  # F26 purged a self-authorizing refund launcher from reachable history, and
  # cmd_preflight is the tripwire that stops one coming back under another
  # name. A tripwire that has only ever passed is not evidence of anything --
  # that was the Round 11 lesson, and this is the same mistake waiting to be
  # made again.
  #
  # So: a throwaway repository containing the SHAPE and nothing else. One line
  # of mandate JSON whose payment id is interpolated. It is a labelled fixture,
  # not a launcher -- there is no key, no request, no guard, nothing to run --
  # and the scan must refuse it.
  local n9dir n9out n9rc
  n9dir="$(mktemp -d 2>/dev/null || echo "./.gotmp/n9.$$")"
  mkdir -p "$n9dir"
  (
    cd "$n9dir" || exit 1
    git init -q .
    git config user.email redteam@example.invalid
    git config user.name redteam
    # ASSEMBLED AT RUNTIME, never written literally here.
    #
    # The literal form of this line matches cmd_preflight's own rule, so
    # committing it made the gate refuse this repository's history -- the
    # scanner correctly flagging its own test fixture. Same self-inflicted
    # refusal as the credential scan in N2, same fix: the source carries a
    # placeholder, the fixture file carries the real shape.
    printf '%s
' '{ "authorized_refund_actions": [ { "payment_id": "PAYVAR", "amount_paise": AMTVAR } ] }'       | sed 's/PAYVAR/'"$(printf %s '$')"'PAY/; s/AMTVAR/'"$(printf %s '$')"'AMT/' > fixture.json
    git add -A
    git commit -qm "synthetic launcher-shape fixture (N9)"
  ) >/dev/null 2>&1
  cp run.sh "$n9dir/run.sh"
  # Captured with && / || like N3, not as a bare assignment.
  #
  # Under `set -e`, `x="$(cmd)"` terminates the shell when cmd exits non-zero
  # -- and a non-zero exit is exactly what this test requires. The bare form
  # aborted cmd_redteam_negative right here: N9 never reported, the
  # blocked/bypassed/skipped summary never printed, and I quoted an older
  # run's summary as though it were current. A test that kills the harness
  # proving it is worse than no test at all.
  # bash, EXPLICITLY, not `sh`.
  #
  # This read `sh ./run.sh preflight`. run.sh declares #!/usr/bin/env bash and
  # uses 22 bash-only constructs, so invoking it through `sh` only worked here
  # by accident: on Git Bash /usr/bin/sh IS bash 5.2. On Linux /bin/sh is dash,
  # and the same call produces
  #
  #   ./run.sh: 157: Syntax error: redirection unexpected   EXIT=2
  #
  # which is not the REFUSING that N9 checks for -- so on every Linux run,
  # including CI, N9 was reporting the wrong-reason branch rather than testing
  # preflight at all. The one platform where N1 can run is the platform where
  # N9 was broken.
  #
  # The interpreter is now an intentional, asserted dependency rather than
  # whatever /bin/sh happens to be.
  command -v bash >/dev/null 2>&1 || {
    bad "N9 cannot run: bash is required and was not found"
    return 1
  }
  n9out="$( cd "$n9dir" && bash ./run.sh preflight 2>&1 )" && n9rc=0 || n9rc=1
  rm -rf "$n9dir"
  if [ "$n9rc" = 0 ]; then
    bad "N9 the history scan PASSED a self-authorizing refund shape"
  else
    case "$n9out" in
      *REFUSING*) ok "N9 the history scan refuses an interpolated refund grant" ;;
      *)          bad "N9 the scan failed for the wrong reason: $n9out" ;;
    esac
  fi

  # N10 -- the suite must invoke run.sh with its DECLARED interpreter.
  #
  # N9 called `sh ./run.sh preflight`. On this dev host /usr/bin/sh is bash, so
  # it passed. On Linux /bin/sh is dash, run.sh uses 22 bash-only constructs,
  # and the call died with "Syntax error: redirection unexpected" -- which is
  # not the REFUSING string N9 looks for, so N9 silently reported the
  # wrong-reason branch on every Linux run, CI included. A test that only works
  # on the platform where the thing it tests cannot run is not a test.
  # Comment lines are stripped first: the two paragraphs above legitimately
  # quote the bad form while explaining it, and a detector that flags its own
  # documentation is the preflight-vs-N9-fixture mistake again.
  local wrongshell
  wrongshell="$(sed -n '/^cmd_redteam_negative()/,/^}/p' run.sh \
    | grep -nE '(^|[^a-z])sh \\./run\\.sh' \
    | grep -vE ':[[:space:]]*#' || true)"
  if [ -n "$wrongshell" ]; then
    echo "  BYPASSED  N10 the suite invokes run.sh through sh, not bash:" >&2
    printf '%s
' "$wrongshell" >&2
    RN_FAIL=$((RN_FAIL+1)); rn_mark "N10 x"
  else
    ok "N10 the suite invokes run.sh with its declared interpreter"
  fi

  echo
  # TERMINAL SUMMARY, machine-checkable.
  #
  # Every expected case must have produced exactly one terminal state, and the
  # three counters must account for all of them. Anything else -- a case that
  # never ran, a case that reported twice, an early exit -- fails here rather
  # than being read as success.
  local total missing dupes c n
  total=$((RN_PASS+RN_FAIL+RN_SKIP))
  echo "  blocked: $RN_PASS   bypassed: $RN_FAIL   skipped: $RN_SKIP   total: $total"
  echo "  cases reporting:$RN_SEEN"

  missing=""; dupes=""
  for n in $RN_EXPECTED_CASES; do
    c=0
    for seen in $RN_SEEN; do [ "$seen" = "$n" ] && c=$((c+1)); done
    [ "$c" = 0 ] && missing="$missing $n"
    [ "$c" -gt 1 ] && dupes="$dupes $n(x$c)"
  done

  RN_SUMMARY_EMITTED=1

  if [ -n "$missing" ]; then
    echo "  FAILED: these cases produced NO terminal state:$missing" >&2
    echo "  A case that does not report is not a case that passed." >&2
    exit 1
  fi
  if [ -n "$dupes" ]; then
    echo "  FAILED: these cases reported more than once:$dupes" >&2
    exit 1
  fi
  if [ "$total" != 10 ]; then
    echo "  FAILED: $total terminal states, expected 10" >&2
    exit 1
  fi

  if [ "$RN_SKIP" != 0 ]; then
    echo "  NOT a clean result: $RN_SKIP case(s) did not run here. Cite the Linux CI"
    echo "  output for those, never this local summary alone."
  fi
  [ "$RN_FAIL" = 0 ] || { echo "A PREVIOUSLY-FIXED BYPASS IS OPEN AGAIN" >&2; exit 1; }
}

# Fuzzing INSIDE the isolated lane.
#
# This used to call gorun, which mounts the real workspace with networking --
# so the brief told reviewers to fuzz through the exact path the lane exists to
# replace. Round 11 caught that.
cmd_fuzz() {
  local target="${1:-FuzzChildReplyNeverFalselyCommits}"
  local budget="${2:-60s}"
  cmd_redteam go test ./internal/relay/ -run '^$' -fuzz "$target" -fuzztime "$budget"
}

cmd_build() {
  go build -buildvcs=false -o rzp-guard.exe ./cmd/rzp-guard
  go build -buildvcs=false -o gate-verify.exe ./cmd/gate-verify
  go build -buildvcs=false -o rzp-guard-operator.exe ./cmd/rzp-guard-operator
  go build -buildvcs=false -o rzp-mandate.exe ./cmd/rzp-mandate
  echo "built ./rzp-guard.exe ./gate-verify.exe ./rzp-guard-operator.exe ./rzp-mandate.exe"
}

# The authoring layer, end to end, on the example intent.
#
# It exists as a runner command for the same reason everything else does: the
# documented door is the one people use. But it is also the demonstration that
# matters most to a reviewer, because it shows the one failure class the guard
# structurally cannot catch being caught upstream of it -- and it shows the
# hand-written examples/mandate.json failing the check that the compiled one
# passes.
cmd_mandate_demo() {
  go build -buildvcs=false -o rzp-mandate.exe ./cmd/rzp-mandate
  local out; out="$(mktemp -d)"
  echo "--- compiling examples/intent.json ---"
  ./rzp-mandate.exe compile -intent examples/intent.json -out "$out/mandate.json"
  echo
  echo "--- verifying the grant is still exactly what that intent compiles to ---"
  ./rzp-mandate.exe verify -mandate "$out/mandate.json"
  echo
  echo "--- what the guard would have been handed instead, hand-written ---"
  echo "    examples/mandate.json caps cumulative spend at 200000 paise over a"
  echo "    single 50000 paise action: 150000 paise of authority no sentence"
  echo "    asked for. The compiled mandate caps it at 50000, by construction."
  rm -rf "$out"
}

# Every compiled mandate in the tree must still be the one its intent produces.
#
# This is the CI shape of the authoring guarantee: compile-time refusal stops a
# bad grant being written, and this stops a good one being edited afterwards.
cmd_mandate_verify_all() {
  go build -buildvcs=false -o rzp-mandate.exe ./cmd/rzp-mandate
  local n=0 f
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    ./rzp-mandate.exe verify -mandate "${f%.intent.json}.json" || return 1
    n=$((n+1))
  done < <(find . -name '*.intent.json' -not -path './study/*' 2>/dev/null)
  echo "verified $n compiled mandate(s)"
}

# Benchmarks. Run in the pinned container like everything else, or the numbers
# describe the host rather than the code.
#
# THE RECOVERY BENCHMARK IS DELIBERATELY EXCLUDED from the default sweep: each
# iteration writes n durable reservations before timing anything, and at ~11ms
# per commit that takes minutes. Run it explicitly with a small -benchtime.
#
# What these measure, and why the ratio is the point: the authorization decision
# is sub-microsecond, and one durable commit is ~11ms because synchronous=FULL
# fsyncs. An authorized refund performs two commits. Against a Razorpay round
# trip of 100-500ms that is 4-20% overhead -- acceptable, and now known rather
# than assumed.
cmd_bench() {
  gorun go test ./internal/policy/ ./internal/storage/ \
    -run '^$' -bench 'Decide|Receipt|Reserve|RecordCall|SetState' \
    -benchtime 200x -benchmem
}

# A locally reproducible, STAMPED release build.
#
# Mirrors .github/workflows/release.yml so a developer can produce the same
# artifact the pipeline does. The build date comes from the commit, not the
# clock: two builds of the same commit must be byte-identical, and a wall-clock
# timestamp silently breaks that.
cmd_release() {
  local version="${1:-dev}"
  local commit date pkg
  commit="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
  date="$(git show -s --format=%cI "$commit" 2>/dev/null || echo '')"
  pkg="github.com/harshith/rzp-guard/internal/buildinfo"

  mkdir -p dist
  MSYS_NO_PATHCONV=1 docker run --rm -v "$PWDW":/src -w /src \
    -e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=amd64 -e GOFLAGS=-buildvcs=false \
    "$GOIMAGE" sh -c "
      set -e
      for c in rzp-guard rzp-guard-operator; do
        go build -trimpath -ldflags \"-s -w \
          -X $pkg.Version=$version \
          -X $pkg.Commit=$commit \
          -X $pkg.BuildDate=$date\" \
          -o dist/\${c}_${version}_linux_amd64 ./cmd/\$c
      done
    "
  ( cd dist && sha256sum ./*_"${version}"_linux_amd64 > "SHA256SUMS-${version}" )
  echo
  echo "artifacts in ./dist:"
  ls -1 dist
  echo
  # Prove the stamp actually landed. A broken -ldflags path ships silently and
  # is only discovered mid-incident, when the binary cannot say what it is.
  MSYS_NO_PATHCONV=1 docker run --rm -v "$PWDW":/src -w /src "$ALPINE" \
    "./dist/rzp-guard_${version}_linux_amd64" -version
}

# THE central proof: a call the mandate does not authorize never crosses into
# Razorpay's official server.
#
# Every condition is enforced by gate-verify, which parses the captured JSON.
# The earlier version printed its control and exited 0 whenever the blocked call
# was simply absent from the tee -- which also passes against a dead container
# or invalid credentials, the exact cases the control exists to rule out.
# The study provider. `proxy` by default: it is the only credential available.
#
# It is UNTRUSTED instrumentation and the repository says so everywhere it
# matters (PROTOCOL.md 4.5). It silently served grok-4.6 for a gpt-5.6 request,
# names no operator and publishes no retention policy. The response is not to
# pretend otherwise but to scope the claim: the study reports what the guard did
# with a PUBLISHED set of emitted calls, and does not claim to know which model
# produced them.
RZP_STUDY_PROVIDER="${RZP_STUDY_PROVIDER:-proxy}"

need_study_creds() {
  case "$RZP_STUDY_PROVIDER" in
    proxy)
      if [ -z "${RZP_STUDY_PROXY_API_KEY:-}" ]; then
        echo "RZP_STUDY_PROXY_API_KEY is not set (RZP_STUDY_PROVIDER=proxy)." >&2
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
      -e RZP_STUDY_PROXY_API_KEY -e OPENAI_API_KEY \
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
  # NOTHING is copied out of raw verbatim.
  #
  # This used to `cp` every raw/*.txt straight into the published directory
  # before running the redactor -- and the redactor only inspected .jsonl, and
  # returned any non-JSON line unchanged. A provider error or diagnostic
  # carrying a payment record would have been republished having passed through
  # no check at all. Text projections are not worth that risk: the .jsonl
  # projection carries every field the gates assert on, and redact.py now scans
  # every file under evidence/ regardless of extension.
  RZP_RAW_EVIDENCE_DIR="$RAW_ROOT" "$PY" evidence/redact.py
}

cmd_live_block() {
  provenance
  need_keys
  # Raw provider responses go OUTSIDE the workspace (see RAW_ROOT above); only a
  # redacted projection is written under evidence/. Writing straight into
  # evidence/linux/ meant every gate run silently republished a payment's
  # contact details, card id and acquirer auth code -- and undid the redaction
  # that had just been applied. Putting them in a gitignored subdirectory of a
  # cloud-synced tree fixed the commit and not the exposure.
  mkdir -p "$RAW_ROOT/linux"; rm -f "$RAW_ROOT/linux"/* 2>/dev/null || true

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
      -v "$RAW_ROOT_W":/raw \
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
        -child-tee /raw/linux/block_child_stdin.jsonl \
        -decision-log /raw/linux/block_decisions.jsonl \
        > /raw/linux/block_stdout.jsonl 2> /raw/linux/block_stderr.txt

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
  echo "Now COMMIT the model freeze that was just written (the path is printed"
  echo "above). The runner refuses an uncommitted or modified model freeze:"
  echo "a promise nobody can fail is not a control."
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

  ./run.sh demo              15s: the boundary allows one refund and refuses
                             four, then refuses an altered authorization
  ./run.sh test              fast lane: all unit tests (no Docker child, no keys, no network)
  ./run.sh race              fast lane under the race detector
  ./run.sh lifecycle         process-lifecycle tests (-tags testhook; NOT in the
                             default lane, reported separately)
  ./run.sh lifecycle-race    lifecycle lane under the race detector
  ./run.sh all               every lane: default, lifecycle, and BOTH race runs
  ./run.sh build             build all three binaries
  ./run.sh vet               go vet, in the pinned container
  ./run.sh bench             measure the decision and the durable writes
  ./run.sh fuzz [TARGET] [BUDGET]
                             fuzz in the pinned container (default 60s)
  ./run.sh redteam-selfcheck smoke-checks the isolated lane's properties
  ./run.sh redteam-negative  re-runs every bypass that once worked; all must fail
  ./run.sh redteam <cmd...>  ISOLATED lane for external review: working-tree
                             export, --network=none, no docker socket, no
                             credentials, strict child. Use this one.
  ./run.sh release [VERSION] stamped static linux/amd64 build + checksums
  ./run.sh operator-setup    ONCE: create the recovery credential (deployment step)

  ./run.sh mandate-demo      compile examples/intent.json into a mandate and
                             verify it. The authoring layer: it refuses an
                             ambiguous intent rather than resolving one, and the
                             cumulative cap it emits equals the sum of the lines
  ./run.sh mandate-verify    every compiled mandate in the tree must still be
                             exactly what its intent produces

  ./run.sh preflight         PRE-PUSH: scan history for a self-authorizing
                             refund launcher. Run before publishing.

  ./run.sh study-build       Phase 4b: build the harness binaries (test-hook
                             builds; they substitute only the CHILD process)
  ./run.sh study-verify      Phase 4b: check the frozen protocol is intact
  ./run.sh study-dry         Phase 4b: whole harness on a scripted fake model
                             (no API key, no spend, never a study result)
  ./run.sh study-model       Phase 4b: resolve + record provider, endpoint and
                             model. COMMIT the result BEFORE running traces.
                             Proxy by default; needs RZP_STUDY_PROXY_API_KEY. It has
                             no trustworthy model list, so -model <id> is
                             required, e.g. -model gpt-5.6-sol.
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
  demo) cmd_demo ;;
  test) cmd_test ;;
  race) cmd_race ;;
  lifecycle) cmd_lifecycle ;;
  lifecycle-race) cmd_lifecycle_race ;;
  all) cmd_all ;;
  vet) cmd_vet ;;
  build) cmd_build ;;
  redteam) shift; cmd_redteam "$@" ;;
  redteam-selfcheck) cmd_redteam_selfcheck ;;
  redteam-negative) cmd_redteam_negative ;;
  preflight) cmd_preflight ;;
  fuzz) shift; cmd_fuzz "$@" ;;
  bench) cmd_bench ;;
  release) shift; cmd_release "$@" ;;
  operator-setup) cmd_operator_setup ;;
  mandate-demo) cmd_mandate_demo ;;
  mandate-verify) cmd_mandate_verify_all ;;
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
