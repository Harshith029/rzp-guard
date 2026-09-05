package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/harshith/rzp-guard/internal/buildinfo"
	"github.com/harshith/rzp-guard/internal/policy"
)

// OBSERVABILITY, which the engineering audit scored 5/10 with three lines in
// the "present?" column reading No: metrics endpoint, health check, drift
// detection. The question it asked was "if production broke at 3am, what would
// an engineer have?" and the honest answer was a greppable log token, a JSONL
// decision log, and a status file somebody has to know to look at.
//
// This adds the three missing ones. It is deliberately small, and three
// decisions about it are worth stating.
//
// LOOPBACK ONLY, ENFORCED. The guard is a stdio proxy on a money path; giving
// it a listening socket is a new attack surface that did not exist before, and
// the only defensible version binds where nothing can route to it. A non-local
// -admin-addr is REFUSED at startup rather than warned about, because the
// failure mode of a warning is a metrics port on 0.0.0.0 enumerating a
// merchant's payment ids.
//
// OFF BY DEFAULT. Same reason. A deployment that has not asked for a socket
// does not get one.
//
// NO CLIENT LIBRARY. The Prometheus exposition format is a documented text
// format and this emits about twenty lines of it. Pulling in a dependency tree
// for that, on a component whose whole boring-by-design story is ONE direct
// dependency beyond the standard library, would be a bad trade.
//
// WHAT IT DOES NOT DO. It does not expose action ids, payment ids or receipts.
// Aggregate counters only. The status file already carries the identifiers and
// is 0600 for that reason; a metrics endpoint is scraped by things that store
// and forward, and payment identifiers should not travel that way.

// adminCounters are the process-lifetime totals a monitor needs.
//
// Kept as atomics rather than behind the guard's mutex: a scrape must never be
// able to contend with an authorization decision. The numbers are allowed to be
// a few nanoseconds stale; the money path is not allowed to wait for a scrape.
type adminCounters struct {
	decisionsAllowed  atomic.Int64
	decisionsRefused  atomic.Int64
	operatorApproved  atomic.Int64
	inDoubtTransition atomic.Int64
	queueWriteFailed  atomic.Int64
	deadlineExpired   atomic.Int64
	auditBroken       atomic.Int64
}

// observe records one decision. Called from the decision sink, which is already
// on every decision's path, so this adds an atomic increment and nothing else.
func (c *adminCounters) observe(d policy.Decision) {
	switch {
	case d.Rule == policy.OperatorApproved:
		c.operatorApproved.Add(1)
		c.decisionsAllowed.Add(1)
	case d.Allowed:
		c.decisionsAllowed.Add(1)
	default:
		c.decisionsRefused.Add(1)
	}
}

// adminServer serves /healthz, /readyz and /metrics on a loopback address.
type adminServer struct {
	guard    *policy.Guard
	counters *adminCounters
	mandate  string
	born     time.Time
	srv      *http.Server

	// denials reports refusals the deny-path rate cap could not record. Nil in
	// tests that do not wire one.
	denials *denialRecorder

	// bound is the address actually listened on. It differs from srv.Addr when
	// the port was 0, which is how a test gets a free port -- and reporting the
	// requested address rather than the real one is how such a test then talks
	// to nothing.
	bound string
}

// Addr is the address this endpoint is really serving on.
func (a *adminServer) Addr() string { return a.bound }

// loopbackOnly refuses any address that is not on the loopback interface.
//
// Checked by RESOLVING the host rather than by string-matching it, so
// "localhost", "127.0.0.1", "::1" and an IPv4-mapped form all pass and a
// hostname that happens to start with those characters does not. An empty host
// ("" or ":9090") means all interfaces and is refused: that is the spelling
// somebody reaches for when they want the port reachable, which is exactly the
// case this exists to prevent.
func loopbackOnly(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-admin-addr %q must be host:port, e.g. 127.0.0.1:9090: %w",
			addr, err)
	}
	if port == "" {
		return fmt.Errorf("-admin-addr %q has no port", addr)
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("-admin-addr %q binds every interface. This endpoint "+
			"reports a merchant's refund activity from a process on a money path; "+
			"it is loopback-only by design. Use 127.0.0.1:%s", addr, port)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("-admin-addr %q: cannot resolve %q: %w", addr, host, err)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return fmt.Errorf("-admin-addr %q resolves to %s, which is not loopback. "+
				"This endpoint is refused rather than warned about: the failure mode "+
				"of a warning is a metrics port on a public interface", addr, ip)
		}
	}
	return nil
}

func newAdminServer(addr string, g *policy.Guard, c *adminCounters, mandateID string) (*adminServer, error) {
	if err := loopbackOnly(addr); err != nil {
		return nil, err
	}
	a := &adminServer{guard: g, counters: c, mandate: mandateID, born: time.Now().UTC()}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.healthz)
	mux.HandleFunc("/readyz", a.readyz)
	mux.HandleFunc("/metrics", a.metrics)
	a.srv = &http.Server{
		Addr:    addr,
		Handler: mux,
		// A scrape that hangs must not hold a connection open indefinitely
		// against a process whose real job is elsewhere.
		ReadHeaderTimeout: 2 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	return a, nil
}

// Start listens and serves until Close. Listening happens synchronously so a
// port conflict is a startup error rather than a silent absence of monitoring.
func (a *adminServer) Start() error {
	ln, err := net.Listen("tcp", a.srv.Addr)
	if err != nil {
		return fmt.Errorf("admin endpoint: %w", err)
	}
	a.bound = ln.Addr().String()
	go func() { _ = a.srv.Serve(ln) }()
	return nil
}

func (a *adminServer) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = a.srv.Shutdown(ctx)
}

// healthz answers "is this process alive". It deliberately checks NOTHING else.
//
// A liveness probe that fails when a dependency is unhappy gets the process
// killed and restarted for a problem a restart cannot fix -- and on this
// process a restart promotes every in-flight reservation to IN_DOUBT, so a
// wrong liveness answer manufactures work for a human. Liveness here means the
// goroutine serving this handler is running. That is all it should ever mean.
func (a *adminServer) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "ok %s %s\n", buildinfo.Version, buildinfo.Commitish())
}

// readyz answers "should this process be sent work".
//
// It reports NOT ready when the guard has lost sight of operator grants, and
// the reason is worth stating: a guard whose grant source is failing looks
// exactly like one nobody has issued grants to, and it will refuse refunds a
// human has already approved. That is a silent regression to the behaviour the
// unblock workflow exists to fix, so it is surfaced rather than left to be
// discovered by a customer.
//
// IN_DOUBT actions do NOT make it unready. They need a human, not a load
// balancer, and taking the guard out of rotation for one would stop every
// legitimate refund to draw attention to a single stuck one.
func (a *adminServer) readyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err := a.guard.GrantSourceError(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "not ready: operator grants cannot be read: %v\n", err)
		return
	}
	fmt.Fprintln(w, "ready")
}

// metrics emits the Prometheus text exposition format.
//
// AGGREGATES ONLY. No action ids, no payment ids, no receipts. A scrape
// endpoint is read by things that store and forward, and payment identifiers
// should not travel that way -- the status file already carries them and is
// 0600 for exactly that reason.
func (a *adminServer) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	inDoubt := len(a.guard.InDoubtActions())
	var droppedDenials int64
	if a.denials != nil {
		droppedDenials = a.denials.Dropped()
	}
	grantErr := 0
	if a.guard.GrantSourceError() != nil {
		grantErr = 1
	}

	type m struct {
		name, help, typ string
		value           int64
	}
	for _, v := range []m{
		{"rzp_guard_up", "1 while the process is serving.", "gauge", 1},
		{"rzp_guard_uptime_seconds", "Seconds since start.", "gauge",
			int64(time.Since(a.born).Seconds())},

		{"rzp_guard_decisions_allowed_total",
			"Authorization decisions that permitted a call.", "counter",
			a.counters.decisionsAllowed.Load()},
		{"rzp_guard_decisions_refused_total",
			"Authorization decisions that refused a call.", "counter",
			a.counters.decisionsRefused.Load()},
		{"rzp_guard_operator_approved_total",
			"Refunds the mandate refused and a human approved. A rising number " +
				"means mandates are being written narrower than merchants intend.",
			"counter", a.counters.operatorApproved.Load()},

		// THE ONE TO ALERT ON. Money whose fate is unknown, waiting on a person.
		{"rzp_guard_in_doubt_actions", "Actions locked IN_DOUBT, awaiting an operator.",
			"gauge", int64(inDoubt)},
		{"rzp_guard_in_doubt_total", "IN_DOUBT transitions since start.", "counter",
			a.counters.inDoubtTransition.Load()},
		{"rzp_guard_refund_deadline_expired_total",
			"Forwarded refunds locked because the child never answered in time.",
			"counter", a.counters.deadlineExpired.Load()},

		// Budget. The drift signal the audit named as absent: a refusal-rate
		// spike means a bad mandate, and nothing watched it.
		{"rzp_guard_encumbered_paise", "Reserved plus committed, against the cap.",
			"gauge", a.guard.Encumbered()},
		{"rzp_guard_committed_paise", "Settled spend.", "gauge", a.guard.Committed()},
		{"rzp_guard_remaining_paise", "Cumulative cap headroom.", "gauge",
			a.guard.Remaining()},

		// The two ways this process can go quietly blind. Both were previously
		// a single line on stderr.
		{"rzp_guard_audit_broken_total",
			"Failures writing the evidence tee: the guard can no longer prove what " +
				"crossed the boundary.", "counter", a.counters.auditBroken.Load()},
		{"rzp_guard_queue_write_failed_total",
			"Failures recording a refusal for operator review.", "counter",
			a.counters.queueWriteFailed.Load()},
		{"rzp_guard_denials_unrecorded_total",
			"Refusals the deny-path rate cap did not record. Non-zero means the " +
				"queue is incomplete -- a short queue because it overflowed must not " +
				"look like a short queue because nothing was refused.",
			"counter", droppedDenials},
		{"rzp_guard_grant_source_failing",
			"1 when operator grants cannot be read, so approved refunds are being " +
				"refused anyway.", "gauge", int64(grantErr)},
	} {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %d\n",
			v.name, v.help, v.name, v.typ, v.name, v.value)
	}

	// Build identity as a labelled gauge, the conventional shape, so a dashboard
	// can tell which commit produced a number.
	fmt.Fprintf(w, "# HELP rzp_guard_build_info Build identity.\n"+
		"# TYPE rzp_guard_build_info gauge\n"+
		"rzp_guard_build_info{version=%q,commit=%q,mandate=%q} 1\n",
		buildinfo.Version, buildinfo.Commitish(), a.mandate)
}
