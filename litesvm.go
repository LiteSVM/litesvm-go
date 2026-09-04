// Package litesvm provides an in-process Solana VM for fast testing of
// Solana programs and clients, intended as a drop-in alternative to
// solana-test-validator for Go unit and integration tests: a fresh handle
// boots in milliseconds and runs in the same process as the test.
//
// The package is pure Go. The engine is mithril's SVM
// (github.com/sonicfromnewyoke/mithril): sealevel runtime, sbpf VM, and
// the pure transaction pipeline replay.LoadAndExecuteTransaction. That
// dependency is an implementation detail and never appears in this
// package's API.
//
// It uses github.com/gagliardetto/solana-go for the core Solana types
// (PublicKey, Hash, Signature, Account) so that values returned from this
// package can be passed directly into solana-go helpers (transaction
// construction, signing, base58 encoding) and vice-versa.
//
// A LiteSVM handle is not safe for concurrent use from multiple goroutines.
// Either confine it to a single goroutine, or serialize access with your
// own sync.Mutex.
package litesvm
