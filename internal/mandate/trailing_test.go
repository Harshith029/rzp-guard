package mandate

import (
	"strings"
	"testing"
	"time"
)

func oneDoc() string {
	return `{"mandate_id":"mnd_test","expires_at":"` +
		time.Now().Add(4*time.Hour).Format(time.RFC3339) + `",
		"allowed_tools":["create_refund"],
		"authorized_refund_actions":[{"action_id":"rfa_001",
		  "payment_id":"pay_SYN0001","amount_paise":20000}],
		"global":{"max_cumulative_paise":500000,"max_calls_per_minute":10}}`
}

// A mandate file holds exactly one document.
//
// json.Decoder.Decode stops at the end of the first value, so a second document
// appended to the file was silently ignored: the authority a reviewer reads
// would not be the authority the guard enforces. Signature verification covers
// the whole file and would catch it, but signing is opt-in, so the parser cannot
// lean on it.
func TestLoadRefusesTrailingContent(t *testing.T) {
	for _, tc := range []struct{ name, extra string }{
		{"a second mandate", ` {"mandate_id":"mnd_evil","expires_at":"2030-01-01T00:00:00Z",
			"allowed_tools":["create_refund"],"authorized_refund_actions":[],
			"global":{"max_cumulative_paise":1,"max_calls_per_minute":1}}`},
		{"a bare object", ` {}`},
		{"junk", ` not json at all`},
		{"a second array", ` [1,2,3]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load([]byte(oneDoc() + tc.extra)); err == nil {
				t.Fatal("accepted a file carrying more than one JSON value")
			} else if !strings.Contains(err.Error(), "more than one JSON value") {
				t.Fatalf("refused for the wrong reason: %v", err)
			}
		})
	}
}

// Trailing whitespace is not trailing content: real files end with a newline.
func TestLoadAcceptsTrailingWhitespace(t *testing.T) {
	for _, suffix := range []string{"", "\n", "  \n\t\n", "\r\n"} {
		if _, err := Load([]byte(oneDoc() + suffix)); err != nil {
			t.Fatalf("rejected a valid mandate ending in %q: %v", suffix, err)
		}
	}
}
