package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/harshith/rzp-guard/internal/policy"
)

// A LISTENING SOCKET ON A MONEY PATH is a new attack surface that did not exist
// before this endpoint. The only defensible version binds where nothing can
// route to it, and it REFUSES anything else rather than warning -- because the
// failure mode of a warning is a metrics port on a public interface enumerating
// a merchant's refund activity.
func TestTheAdminEndpointRefusesAnythingButLoopback(t *testing.T) {
	for _, addr := range []string{
		"0.0.0.0:9090",   // the spelling somebody uses to make it reachable
		":9090",          // the same thing, shorter
		"8.8.8.8:9090",   // an address that is plainly not local
		"example.com:80", // a name that resolves off-box
	} {
		t.Run(addr, func(t *testing.T) {
			if err := loopbackOnly(addr); err == nil {
				t.Fatalf("%q was accepted; this endpoint reports a merchant's refund "+
					"activity and must not be routable", addr)
			}
		})
	}
	for _, addr := range []string{"127.0.0.1:9090", "localhost:9090", "[::1]:9090"} {
		t.Run(addr, func(t *testing.T) {
			if err := loopbackOnly(addr); err != nil {
				t.Fatalf("%q was refused, but it is loopback: %v", addr, err)
			}
		})
	}
	// A malformed address must fail rather than default to something.
	if err := loopbackOnly("nonsense"); err == nil {
		t.Error("a malformed -admin-addr was accepted")
	}
}

// The three endpoints have to say different things, or a monitor cannot tell
// "alive" from "should be sent work".
func TestTheEndpointsAnswer(t *testing.T) {
	g := testGuard(t)
	a, err := newAdminServer("127.0.0.1:0", g, &adminCounters{}, "mnd_status")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	base := "http://" + a.Addr()
	for _, tc := range []struct{ path, want string }{
		{"/healthz", "ok"},
		{"/readyz", "ready"},
		{"/metrics", "rzp_guard_up 1"},
	} {
		body, code := get(t, base+tc.path)
		if code != http.StatusOK {
			t.Errorf("%s returned %d", tc.path, code)
		}
		if !strings.Contains(body, tc.want) {
			t.Errorf("%s does not contain %q:\n%s", tc.path, tc.want, body)
		}
	}
}

// AGGREGATES ONLY. A scrape endpoint is read by things that store and forward,
// and payment identifiers must not travel that way. The status file already
// carries them and is 0600 for exactly that reason.
func TestMetricsNeverExposeIdentifiers(t *testing.T) {
	g := testGuard(t)
	// Consume an action so there is something identifying to leak.
	g.Decide(policy.RefundTool,
		map[string]any{"payment_id": "pay_SYN0001", "amount": int64(50000)},
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))

	a, err := newAdminServer("127.0.0.1:0", g, &adminCounters{}, "mnd_status")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	body, _ := get(t, "http://"+a.Addr()+"/metrics")
	for _, secret := range []string{"pay_SYN0001", "rfa_001", "rzpg_"} {
		if strings.Contains(body, secret) {
			t.Errorf("/metrics leaks %q; a scrape is stored and forwarded", secret)
		}
	}
}

// The counters must distinguish an ordinary allow from one a human had to
// approve. A rising operator-approved count is the signal that mandates are
// being written narrower than merchants intend, which is the one number that
// would have told this project where its false positives came from.
func TestCountersSeparateOperatorApprovalsFromOrdinaryAllows(t *testing.T) {
	c := &adminCounters{}
	c.observe(policy.Decision{Allowed: true, Rule: policy.Allowed})
	c.observe(policy.Decision{Allowed: true, Rule: policy.Allowed})
	c.observe(policy.Decision{Allowed: false, Rule: policy.NoAuthorizedAction})
	c.observe(policy.Decision{Allowed: true, Rule: policy.OperatorApproved})

	if got := c.decisionsAllowed.Load(); got != 3 {
		t.Errorf("allowed = %d, want 3 (an operator approval is still an allow)", got)
	}
	if got := c.decisionsRefused.Load(); got != 1 {
		t.Errorf("refused = %d, want 1", got)
	}
	if got := c.operatorApproved.Load(); got != 1 {
		t.Errorf("operator-approved = %d, want 1: it must be countable on its own", got)
	}
}

func get(t *testing.T, url string) (string, int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
