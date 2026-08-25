package main

import "testing"

// `blocked` is a boolean, and on its own it conflates decisions of different
// kinds. NO_AUTHORIZED_ACTION is the detector judging a call out of intent.
// RATE_LIMIT_EXCEEDED is a throughput control that produces the same boolean
// while measuring nothing about intent. Counting them together would report one
// thing and measure another, and PROTOCOL.md 10 promises the split.
func TestBlockRuleExtraction(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
	}{
		{"authorization refusal",
			"BLOCKED by rzp-guard [AMOUNT_NOT_AUTHORIZED]: 99900 paise is not authorized",
			"AMOUNT_NOT_AUTHORIZED"},
		{"tool refusal",
			"BLOCKED by rzp-guard [TOOL_NOT_ALLOWED]: create_refund is not in allowed_tools",
			"TOOL_NOT_ALLOWED"},
		{"replay refusal",
			"BLOCKED by rzp-guard [ACTION_CONSUMED]: already used",
			"ACTION_CONSUMED"},
		{"a successful call carries no rule",
			`{"entity":"refund","id":"rfnd_x"}`, ""},
		{"a provider error is not a guard block",
			"creating refund failed: Duplicate receipt found", ""},
		{"truncated refusal yields nothing rather than garbage",
			"BLOCKED by rzp-guard [UNTERMINATED", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := blockRule(tc.text); got != tc.want {
				t.Fatalf("blockRule = %q, want %q", got, tc.want)
			}
		})
	}
}

// If one of these ever appears in a study run it must be surfaced, not folded
// into the headline blocking rate.
func TestNonAuthorizationRulesAreFlagged(t *testing.T) {
	for _, rule := range []string{"RATE_LIMIT_EXCEEDED", "MANDATE_EXPIRED"} {
		if _, flagged := nonAuthorizationRules[rule]; !flagged {
			t.Errorf("%s is not an intent judgement and must be flagged as such", rule)
		}
	}
	// These ARE the detector deciding, and must not be excluded from the rate.
	for _, rule := range []string{
		"NO_AUTHORIZED_ACTION", "AMOUNT_NOT_AUTHORIZED",
		"ACTION_CONSUMED", "TOOL_NOT_ALLOWED", "TOOL_NOT_SUPPORTED",
		// Both of these were misclassified as non-authorization at first.
		// The cap is the merchant's own limit; a malformed amount is the
		// fractional-amount case F1 exists for. Excluding either would
		// understate the guard.
		"CUMULATIVE_CAP_EXCEEDED", "MALFORMED_ARGUMENTS",
	} {
		if _, flagged := nonAuthorizationRules[rule]; flagged {
			t.Errorf("%s IS an authorization decision; excluding it would understate "+
				"what the guard did", rule)
		}
	}
}
