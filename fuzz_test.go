package litesvm

import (
	"errors"
	"testing"
)

// FuzzSendLegacyTransaction explores the bincode decoder + signature
// verifier path inside SendLegacyTransaction. The engine returns a non-nil
// error for malformed bytes; any input that panics the pure-Go engine will
// be captured by the fuzzer as a failing seed.
//
// Each iteration constructs its own LiteSVM because the type is not safe
// for concurrent use and the Go fuzzer schedules iterations across
// multiple workers.
func FuzzSendLegacyTransaction(f *testing.F) {
	// Seeds: empty, garbage, plausible-but-wrong-shape.
	f.Add([]byte{})
	f.Add([]byte{0xDE, 0xAD, 0xBE, 0xEF})
	f.Add(make([]byte, 64))
	f.Add(make([]byte, 256))

	f.Fuzz(func(t *testing.T, txBytes []byte) {
		svm, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer svm.Close()

		out, err := svm.SendLegacyTransaction(txBytes)
		if err != nil {
			// Every error must be tagged with the package sentinel — that
			// contract is part of the public API surface (errors.Is).
			if !errors.Is(err, ErrLiteSVM) {
				t.Fatalf("error not wrapped with ErrLiteSVM: %v", err)
			}
			return
		}
		if out != nil {
			out.Close()
		}
	})
}
