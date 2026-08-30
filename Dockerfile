# rzp-guard as a deployable artifact.
#
# ONE THING TO UNDERSTAND BEFORE DEPLOYING THIS. The guard runs Razorpay's
# official MCP server as a CHILD CONTAINER, so a container running the guard
# needs access to a Docker socket:
#
#   docker run --rm -i \
#     -v /var/run/docker.sock:/var/run/docker.sock \
#     -v "$PWD/state:/state" \
#     -e RAZORPAY_KEY_ID -e RAZORPAY_KEY_SECRET \
#     rzp-guard:VERSION -mandate /state/mandate.json -state /state/rzp-guard.db
#
# Mounting the Docker socket grants control of the host's Docker daemon, which
# is close to root. That is a real cost and it is stated here rather than
# discovered later. It is inherent to the design decision to run the official
# server unmodified rather than fork or re-implement it (ARCHITECTURE.md), and
# the same pattern is already used by `./run.sh live-block`.
#
# The alternative -- vendoring Razorpay's server into this image -- trades a
# privilege problem for a trust problem: a fork needs auditing forever. The
# privilege problem is the one with known mitigations (a socket proxy
# restricted to `create` on one pinned digest), so it is the one kept.

# ---- build -----------------------------------------------------------------
# Digest-pinned, matching run.sh. A tag is mutable; a digest is the only thing
# that makes "the same source produces the same binary" true six months later.
FROM golang@sha256:e2f96d803d39f4cb681fa82801be6eacad6337d9f00769918e1e21b5555723ea AS build

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# CGO_ENABLED=0 is load-bearing, not a habit: the SQLite driver is pure Go
# (modernc.org/sqlite) precisely so this produces one static binary with no
# libc dependency, which is what lets the runtime stage be `scratch`-adjacent.
ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-buildvcs=false
RUN go build -trimpath \
      -ldflags "-s -w \
        -X github.com/harshith/rzp-guard/internal/buildinfo.Version=${VERSION} \
        -X github.com/harshith/rzp-guard/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/harshith/rzp-guard/internal/buildinfo.BuildDate=${BUILD_DATE}" \
      -o /out/rzp-guard ./cmd/rzp-guard \
 && go build -trimpath \
      -ldflags "-s -w \
        -X github.com/harshith/rzp-guard/internal/buildinfo.Version=${VERSION} \
        -X github.com/harshith/rzp-guard/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/harshith/rzp-guard/internal/buildinfo.BuildDate=${BUILD_DATE}" \
      -o /out/rzp-guard-operator ./cmd/rzp-guard-operator

# ---- runtime ---------------------------------------------------------------
# alpine rather than scratch because the guard execs `docker`, so it needs a
# shell and the client. Digest-pinned for the same reason as the builder.
FROM alpine@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

RUN apk add --no-cache docker-cli ca-certificates \
 && adduser -D -u 10001 rzpguard

# The state file holds an Argon2id operator verifier and payment-linked action
# ids. It belongs on a mounted volume the operator controls, never in the image.
VOLUME ["/state"]
WORKDIR /state
USER rzpguard

COPY --from=build /out/rzp-guard /usr/local/bin/rzp-guard
COPY --from=build /out/rzp-guard-operator /usr/local/bin/rzp-guard-operator

# No HEALTHCHECK: the guard speaks MCP on stdio and has no port to probe.
# Liveness is the -status-file document, which a sidecar or the host reads
# without contending for the state file's exclusive lock.

ENTRYPOINT ["/usr/local/bin/rzp-guard"]
