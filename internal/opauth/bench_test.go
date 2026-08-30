package opauth

import "testing"

// The verifier's cost IS the rate limit.
//
// There is no attempt counter on the operator CLI, and this is why: Argon2id at
// t=3, m=64MiB, p=4 makes each guess cost real time and real memory. Combined
// with a token that is 256 bits from crypto/rand, online guessing is not a
// threat model -- it is arithmetic.
//
// Measured rather than assumed, because "Argon2id is slow" is exactly the kind
// of received wisdom this project has been burned by.
func BenchmarkVerify(b *testing.B) {
	token, err := NewToken()
	if err != nil {
		b.Fatal(err)
	}
	stored, err := Verifier(token)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := Verify(token, stored); err != nil {
			b.Fatal(err)
		}
	}
}

// The rejection path must cost the same as the acceptance path, or the timing
// difference is itself an oracle.
func BenchmarkVerifyWrongToken(b *testing.B) {
	stored, err := Verifier("rzpop_correct")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := Verify("rzpop_wrong", stored); err == nil {
			b.Fatal("wrong token accepted")
		}
	}
}
