package storage

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// The policy benchmark showed the full authorized path costs ~13ms while the
// in-memory decision is sub-microsecond. Essentially all of it is here, in the
// durable layer, and these isolate which write is responsible.
//
// This matters more than a headline number: Guard.Decide holds its mutex across
// BOTH writes, so this cost is the serialisation point for every concurrent
// refund in a session. It sets the throughput ceiling.

func benchStore(b *testing.B) *Store {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "bench.db"), "mnd_bench")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	return s
}

// One durable reservation: the write that must land before any byte is
// forwarded.
func BenchmarkReserve(b *testing.B) {
	s := benchStore(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Reserve(
			fmt.Sprintf("rfa_%08d", i),
			fmt.Sprintf("rzpg_%012x", i),
			10000); err != nil {
			b.Fatal(err)
		}
	}
}

// One rate-window append. Decide() performs this in addition to Reserve.
func BenchmarkRecordCall(b *testing.B) {
	s := benchStore(b)
	now := time.Now().UTC().UnixNano()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.RecordCall(now + int64(i)); err != nil {
			b.Fatal(err)
		}
	}
}

// A state transition, which the relay performs on every reply.
func BenchmarkSetState(b *testing.B) {
	s := benchStore(b)
	for i := 0; i < b.N; i++ {
		if err := s.Reserve(fmt.Sprintf("rfa_%08d", i),
			fmt.Sprintf("rzpg_%012x", i), 10000); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.SetState(fmt.Sprintf("rfa_%08d", i), "RESERVED", "COMMITTED"); err != nil {
			b.Fatal(err)
		}
	}
}

// Startup recovery over a state file holding n mid-flight reservations. This is
// on the restart path, so it is latency a human waits through after a crash.
//
// Setup dominates: each iteration writes n durable reservations before timing
// anything, so run this with a small -benchtime (e.g. 5x) or it takes minutes.
func BenchmarkRecoverStartup(b *testing.B) {
	for _, n := range []int{10, 200} {
		b.Run(fmt.Sprintf("reserved=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				s, err := Open(filepath.Join(b.TempDir(), "r.db"), "mnd_bench")
				if err != nil {
					b.Fatal(err)
				}
				for j := 0; j < n; j++ {
					if err := s.Reserve(fmt.Sprintf("rfa_%08d", j),
						fmt.Sprintf("rzpg_%012x", j), 1000); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()
				if _, err := s.RecoverStartup(); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				_ = s.Close()
				b.StartTimer()
			}
		})
	}
}

// RecordCall now prunes in the same transaction as the insert. If that ever
// becomes a second commit, this benchmark roughly doubles -- which is the whole
// reason the DELETE shares the transaction rather than following it.
func BenchmarkRecordCallStaysOneCommit(b *testing.B) {
	s := benchStore(b)
	base := time.Now().UTC().UnixNano()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.RecordCall(base + int64(i)); err != nil {
			b.Fatal(err)
		}
	}
}
