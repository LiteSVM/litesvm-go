package mithrilsvm

import (
	"github.com/gagliardetto/solana-go"
)

// TxOutcome holds the result of a transaction submission or simulation. It
// always carries metadata (signature, logs, compute units, fee) regardless
// of whether the transaction succeeded; if it failed, Error returns a
// non-empty string.
//
// Error strings are Agave wire-format JSON, e.g. "BlockhashNotFound" or
// {"InstructionError":[0,{"Custom":1}]}.
type TxOutcome struct {
	ok                  bool
	signature           solana.Signature
	computeUnits        uint64
	fee                 uint64
	err                 string
	returnDataProgramID solana.PublicKey
	returnData          []byte
	logs                []string
	inner               solana.InnerInstructionsList
	postAccounts        []PostAccount
}

// PostAccount is a (address, account) pair returned by a successful
// simulation.
type PostAccount struct {
	Address solana.PublicKey
	Account *solana.Account
}

// All TxOutcome accessors tolerate a nil receiver (returning zero values):
// patterns like svm.GetTransaction(sig).IsOk() stay panic-free when the
// lookup misses.

// Close is a no-op kept for API compatibility.
func (o *TxOutcome) Close() {}

// IsOk reports whether the transaction executed successfully. Returns false
// if the receiver is nil (treating an unusable outcome as "not ok").
func (o *TxOutcome) IsOk() bool { return o != nil && o.ok }

// Signature returns the transaction signature.
func (o *TxOutcome) Signature() solana.Signature {
	if o == nil {
		return solana.Signature{}
	}
	return o.signature
}

// ComputeUnits returns the compute units consumed.
func (o *TxOutcome) ComputeUnits() uint64 {
	if o == nil {
		return 0
	}
	return o.computeUnits
}

// Fee returns the fee charged in lamports.
func (o *TxOutcome) Fee() uint64 {
	if o == nil {
		return 0
	}
	return o.fee
}

// Logs returns the program log messages.
func (o *TxOutcome) Logs() []string {
	if o == nil {
		return nil
	}
	return o.logs
}

// Error returns the failure rendering, or "" on success.
func (o *TxOutcome) Error() string {
	if o == nil {
		return ""
	}
	return o.err
}

// ReturnData returns the last instruction's return data. The bool reports
// whether return data was set. Empty return data is indistinguishable from
// never-set return data and reports false.
func (o *TxOutcome) ReturnData() (solana.PublicKey, []byte, bool) {
	if o == nil || len(o.returnData) == 0 {
		return solana.PublicKey{}, nil, false
	}
	return o.returnDataProgramID, o.returnData, true
}

// InnerInstructions returns recorded CPIs grouped by top-level instruction.
func (o *TxOutcome) InnerInstructions() solana.InnerInstructionsList {
	if o == nil {
		return nil
	}
	return o.inner
}

// PostAccounts returns the modified accounts after a successful execution.
// The error result is always nil and retained for backward compatibility.
func (o *TxOutcome) PostAccounts() ([]PostAccount, error) {
	if o == nil {
		return nil, nil
	}
	return o.postAccounts, nil
}
