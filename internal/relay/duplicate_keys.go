package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// A JSON object may not contain the same key twice -- not because the grammar
// forbids it (RFC 8259 permits it and calls the result unpredictable) but
// because parsers do not agree on which occurrence wins.
//
// THE BYPASS THIS CLOSES. Go's encoding/json takes the LAST occurrence. Given
//
//	{"id":1,"method":"tools/call","method":"tools/list","params":
//	 {"name":"create_refund","arguments":{"payment_id":"pay_X","amount":900000}}}
//
// this relay read method = "tools/list", classified it as a read, and forwarded
// the line BYTE-FOR-BYTE -- carrying an unauthorized 900,000-paise create_refund
// to a child whose parser may well take the FIRST occurrence. Reproduced against
// the real relay before this existed.
//
// The relay already states the right principle: "the child must never receive
// bytes this relay could not inspect." Duplicate keys are bytes it inspected and
// read one way while the next hop may read another, which is the same failure
// wearing a disguise.
//
// WHY REFUSE RATHER THAN CANONICALISE. Re-serialising every message would make
// the guard's reading authoritative, and that is what already protects
// tools/call -- its arguments are rebuilt, which is why the duplicate-amount
// variant of this attack does not work. But rewriting arbitrary pass-through
// MCP traffic means reordering keys and reformatting numbers in messages this
// relay does not understand, which risks breaking protocol details to fix an
// input that should never have been sent. Refusing is fail-closed and needs no
// knowledge of the message.
func duplicateKey(line []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()

	// One set of seen keys per open object. Arrays push a nil frame so the
	// depth tracking stays aligned without collecting their elements as keys.
	var stack []map[string]bool
	expectKey := false

	for {
		tok, err := dec.Token()
		if err != nil {
			// Malformed input is handled by the caller's own decode, which
			// reports the parse error. Nothing to say about duplicates here.
			return "", false
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, map[string]bool{})
				expectKey = true
			case '[':
				stack = append(stack, nil)
				expectKey = false
			case '}', ']':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				expectKey = len(stack) > 0 && stack[len(stack)-1] != nil
			}
		case string:
			if expectKey && len(stack) > 0 && stack[len(stack)-1] != nil {
				if stack[len(stack)-1][t] {
					return t, true
				}
				stack[len(stack)-1][t] = true
				expectKey = false
				continue
			}
			expectKey = len(stack) > 0 && stack[len(stack)-1] != nil
		default:
			expectKey = len(stack) > 0 && stack[len(stack)-1] != nil
		}
	}
}

func duplicateKeyError(key string) string {
	return fmt.Sprintf("rzp-guard: message contains the key %q more than once. "+
		"Parsers disagree about which occurrence wins, so this relay cannot know "+
		"that what it inspected is what the next hop will act on. Send each key "+
		"once", key)
}
