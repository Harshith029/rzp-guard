package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// THE OMISSION BYPASS.
//
// The study's central defence against an untrusted endpoint is that every
// response's `model` is compared with what was requested. That check read
//
//	if out.Model != "" && out.Model != req.Model
//
// so an endpoint could skip it entirely by leaving the field out. The party the
// check exists to constrain was able to opt out of it, silently, by sending
// less rather than something wrong. Trace validation had the same shape: it
// skipped traces with no served model, so zero distinct models passed the
// "exactly one served model" control.
//
// Found in external review. These are the regression tests.

// serveJSON stands up an endpoint returning exactly the body given.
func serveJSON(t *testing.T, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestAnthropicRejectsAResponseWithNoModel(t *testing.T) {
	// A well-formed reply in every respect EXCEPT that it names no model.
	srv := serveJSON(t, `{"id":"msg_01","type":"message","role":"assistant",
		"content":[{"type":"text","text":"done"}],
		"usage":{"input_tokens":1,"output_tokens":1}}`)

	c := newAnthropicClient(srv.URL, "test-key")
	_, _, err := c.messages(messagesRequest{Model: "gpt-4o", MaxTokens: 16})
	if err == nil {
		t.Fatal("accepted a response carrying no model field; an untrusted " +
			"endpoint could then produce a whole study with no provenance by " +
			"sending less rather than something wrong")
	}
	if !strings.Contains(err.Error(), "no model field") {
		t.Fatalf("error does not name the cause: %v", err)
	}
}

func TestAnthropicRejectsAResponseWithNoID(t *testing.T) {
	srv := serveJSON(t, `{"id":"","type":"message","role":"assistant","model":"gpt-4o",
		"content":[{"type":"text","text":"done"}],
		"usage":{"input_tokens":1,"output_tokens":1}}`)

	c := newAnthropicClient(srv.URL, "test-key")
	_, _, err := c.messages(messagesRequest{Model: "gpt-4o", MaxTokens: 16})
	if err == nil {
		t.Fatal("accepted a response with no id; the turn cannot then be " +
			"identified against the endpoint's own records")
	}
	if !strings.Contains(err.Error(), "no id") {
		t.Fatalf("error does not name the cause: %v", err)
	}
}

// The substitution case must still fail, unchanged.
func TestAnthropicStillRejectsASubstitutedModel(t *testing.T) {
	srv := serveJSON(t, `{"id":"msg_01","type":"message","role":"assistant","model":"grok-4.6",
		"content":[{"type":"text","text":"done"}],
		"usage":{"input_tokens":1,"output_tokens":1}}`)

	c := newAnthropicClient(srv.URL, "test-key")
	_, _, err := c.messages(messagesRequest{Model: "gpt-5.6", MaxTokens: 16})
	if err == nil {
		t.Fatal("accepted grok-4.6 for a gpt-5.6 request")
	}
	if !strings.Contains(err.Error(), "grok-4.6") {
		t.Fatalf("error does not name what was served: %v", err)
	}
}

// And the honest case must still pass, or the control is just an outage.
func TestAnthropicAcceptsAMatchingModel(t *testing.T) {
	srv := serveJSON(t, `{"id":"msg_01","type":"message","role":"assistant","model":"gpt-4o",
		"content":[{"type":"text","text":"done"}],
		"usage":{"input_tokens":3,"output_tokens":4}}`)

	c := newAnthropicClient(srv.URL, "test-key")
	out, _, err := c.messages(messagesRequest{Model: "gpt-4o", MaxTokens: 16})
	if err != nil {
		t.Fatalf("rejected a correct response: %v", err)
	}
	if out.Model != "gpt-4o" || out.ID != "msg_01" {
		t.Fatalf("provenance not carried through: model=%q id=%q", out.Model, out.ID)
	}
}

// Validation reads FILES, and a file can be hand-edited even when no live run
// could produce it. A trace with no provenance must be refused there too.
func TestValidateRejectsTracesWithNoProvenance(t *testing.T) {
	atRepoRoot(t)

	for _, tc := range []struct {
		name   string
		mutate func(ts []trace)
		want   string
	}{
		{
			name:   "no served model on the trace",
			mutate: func(ts []trace) { ts[0].ServedModel = "" },
			want:   "reports no served model",
		},
		{
			name:   "no served model on a turn",
			mutate: func(ts []trace) { ts[0].Messages[0].ServedModel = "" },
			want:   "no served model or response id",
		},
		{
			name:   "no response id on a turn",
			mutate: func(ts []trace) { ts[0].Messages[0].ResponseID = "" },
			want:   "no served model or response id",
		},
		{
			name: "EVERY trace omits the model",
			// The vacuous case: with the old code, zero distinct served models
			// was not "more than one", so a study with no provenance anywhere
			// passed the served-model control outright.
			mutate: func(ts []trace) {
				for i := range ts {
					ts[i].ServedModel = ""
				}
			},
			want: "reports no served model",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			traces, m, mf := goodTraceSet(t)
			tc.mutate(traces)

			err := validateTraceSet(traces, "study/traces", m, mf, nil)
			if err == nil {
				t.Fatal("accepted a trace set with missing provenance; the " +
					"served-model control is only meaningful if a trace that " +
					"reports nothing is rejected rather than skipped")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error does not name the cause (want %q): %v", tc.want, err)
			}
		})
	}
}

// The committed study must satisfy the stronger rule. If it did not, tightening
// the check would have quietly invalidated a published result.
func TestTheCommittedTracesCarryFullProvenance(t *testing.T) {
	atRepoRoot(t)

	for _, dir := range []string{"study/traces", "study/traces-armB"} {
		t.Run(dir, func(t *testing.T) {
			traces, err := loadTraces(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(traces) == 0 {
				t.Fatalf("%s holds no traces", dir)
			}
			for _, tr := range traces {
				key := fmt.Sprintf("%s/run%d", tr.BriefID, tr.RunIndex)
				if tr.ServedModel == "" {
					t.Errorf("%s: committed trace reports no served model", key)
				}
				for _, m := range tr.Messages {
					if m.ServedModel == "" || m.ResponseID == "" {
						t.Errorf("%s turn %d: committed turn lacks provenance", key, m.Turn)
					}
				}
			}
		})
	}
}
