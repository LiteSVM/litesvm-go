// Package litesvm provides an in-process Solana VM for fast testing of
// Solana programs and clients, intended as a drop-in alternative to
// solana-test-validator for Go unit and integration tests: a fresh handle
// boots in milliseconds and runs in the same process as the test.
//
// The package is pure Go. The engine is the mithrilsvm subpackage, built
// on mithril's SVM (sealevel runtime, sbpf VM, pure transaction pipeline);
// every type here is an alias for its mithrilsvm counterpart, so values
// are interchangeable between the two import paths.
//
// It uses github.com/gagliardetto/solana-go for the core Solana types
// (PublicKey, Hash, Signature) so that values returned from this package
// can be passed directly into solana-go helpers (transaction construction,
// signing, base58 encoding) and vice-versa.
//
// A LiteSVM handle is not safe for concurrent use from multiple goroutines.
// Either confine it to a single goroutine, or serialize access with your
// own sync.Mutex.
package litesvm

import (
	"github.com/gagliardetto/solana-go"

	"github.com/LiteSVM/litesvm-go/mithrilsvm"
)

// ErrLiteSVM is the sentinel that wraps every error returned by this
// package. Use errors.Is to detect a litesvm-originated failure:
//
//	if errors.Is(err, litesvm.ErrLiteSVM) { ... }
//
// It is the same sentinel value as mithrilsvm.ErrMithrilSVM (message
// "litesvm"), so errors.Is works across both import paths.
var ErrLiteSVM = mithrilsvm.ErrMithrilSVM

// LiteSVM is an in-process Solana VM.
type LiteSVM = mithrilsvm.LiteSVM

// TxOutcome holds the result of a transaction submission or simulation. It
// always carries metadata (signature, logs, compute units, fee) regardless
// of whether the transaction succeeded; if it failed, Error returns a
// non-empty string.
type TxOutcome = mithrilsvm.TxOutcome

// PostAccount is a (address, account) pair returned by a successful
// simulation.
type PostAccount = mithrilsvm.PostAccount

// InnerInstruction is one cross-program invocation (CPI) recorded while a
// transaction executed, together with the stack height at which it ran.
type InnerInstruction = mithrilsvm.InnerInstruction

// InnerInstructionsList groups the recorded CPIs of one transaction, indexed
// by top-level instruction.
type InnerInstructionsList = mithrilsvm.InnerInstructionsList

// ComputeBudget mirrors solana_compute_budget::compute_budget::ComputeBudget.
// Fields typed as `usize` on the Solana side are surfaced as uint64;
// HeapSize stays uint32.
type ComputeBudget = mithrilsvm.ComputeBudget

// FeatureSet describes which runtime features are enabled: active feature
// ids with their activation slot, plus the set of known-but-inactive
// feature ids. Use NewFeatureSet or NewFeatureSetAllEnabled to obtain the
// feature catalog, mutate it with Activate/Deactivate, then install it with
// (*LiteSVM).SetFeatureSet.
type FeatureSet = mithrilsvm.FeatureSet

// New creates a fresh LiteSVM with default settings. Close is a no-op for
// the pure-Go engine and may be called (or omitted) freely.
func New() (*LiteSVM, error) {
	return mithrilsvm.New()
}

// Version reports the engine backing this package.
func Version() string {
	return mithrilsvm.Version()
}

// NewFeatureSet returns the default feature set: every known feature,
// inactive.
func NewFeatureSet() (*FeatureSet, error) {
	return mithrilsvm.NewFeatureSet()
}

// NewFeatureSetAllEnabled returns a FeatureSet with every known feature
// activated at slot 0.
func NewFeatureSetAllEnabled() (*FeatureSet, error) {
	return mithrilsvm.NewFeatureSetAllEnabled()
}

// DefaultComputeBudget returns the Agave runtime default compute budget.
func DefaultComputeBudget() ComputeBudget {
	return mithrilsvm.DefaultComputeBudget()
}

// BuildTransferTx produces a signed, wire-encoded legacy Transaction that
// transfers `lamports` from the keypair derived from `payerSeed` to `to`,
// using `blockhash`. It remains a convenience for tests; application code
// can build the same transaction directly with solana-go (see the package
// README and examples/transfer).
func BuildTransferTx(payerSeed [32]byte, to solana.PublicKey, lamports uint64, blockhash solana.Hash) ([]byte, error) {
	return mithrilsvm.BuildTransferTx(payerSeed, to, lamports, blockhash)
}
