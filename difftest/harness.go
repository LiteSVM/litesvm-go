// Package difftest is a differential test and benchmark harness that runs
// identical scenarios against two views of the engine:
//
//   - the root package (github.com/LiteSVM/litesvm-go), and
//   - the mithrilsvm package it re-exports
//     (github.com/LiteSVM/litesvm-go/mithrilsvm).
//
// Each scenario produces named observations; the runner compares them
// pairwise and reports MATCH/DRIFT per observation. Hard observations
// (ok-status, balances, fees, error variants, full log arrays including the
// system program's ic_msg detail lines) fail the test on drift; soft
// observations (compute units) are reported without failing.
//
// See README.md for background: DRIFT.md and BENCH.md are historical
// records from the development-time comparison against the engine this one
// replaced.
package difftest

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	litesvm "github.com/LiteSVM/litesvm-go"
	"github.com/LiteSVM/litesvm-go/mithrilsvm"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/sysvar"
)

const lamportsPerSol = 1_000_000_000

// memoV3ProgramID is SPL Memo v3 (loader v2), part of the default program
// set both engines are aligned to.
var memoV3ProgramID = solana.MustPublicKeyFromBase58("MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr")

// ---------------------------------------------------------------------------
// Engine abstraction
// ---------------------------------------------------------------------------

// TxResult is the engine-neutral view of a transaction outcome.
type TxResult struct {
	Ok   bool
	Sig  solana.Signature
	Fee  uint64
	CU   uint64
	Logs []string
	Err  string // raw engine rendering; normalize with normalizeError
	Post []PostBalance
}

// PostBalance is the lamport balance of one post-execution account returned
// by a successful simulation.
type PostBalance struct {
	Address  solana.PublicKey
	Lamports uint64
}

// PostLamports returns the post-simulation lamports of addr. The bool is
// false if addr is not among the post accounts.
func (r TxResult) PostLamports(addr solana.PublicKey) (uint64, bool) {
	for _, p := range r.Post {
		if p.Address.Equals(addr) {
			return p.Lamports, true
		}
	}
	return 0, false
}

// Engine abstracts the operations both engines share.
type Engine interface {
	Name() string
	Close()

	Airdrop(pubkey solana.PublicKey, lamports uint64) error
	Balance(pubkey solana.PublicKey) (uint64, bool, error)
	LatestBlockhash() (solana.Hash, error)
	ExpireBlockhash() error
	SendTx(txBytes []byte) (TxResult, error)
	SimulateTx(txBytes []byte) (TxResult, error)
	WarpToSlot(slot uint64) error
	Clock() (*sysvar.Clock, error)
	SetClock(c *sysvar.Clock) error
	MinimumBalanceForRentExemption(dataLen int) (uint64, error)
	GetAccount(pubkey solana.PublicKey) *solana.Account
	SetAccount(pubkey solana.PublicKey, acct *solana.Account) error
	SetSigverify(enabled bool) error
	SetBlockhashCheck(enabled bool) error
	SetDefaultPrograms() error
	// GetTransaction reports whether the signature is recorded in the
	// engine's transaction history.
	GetTransaction(sig solana.Signature) bool
}

// ---------------------------------------------------------------------------
// root-package adapter
// ---------------------------------------------------------------------------

type rootEngine struct {
	svm *litesvm.LiteSVM
}

func newRootEngine() (Engine, error) {
	svm, err := litesvm.New()
	if err != nil {
		return nil, err
	}
	return &rootEngine{svm: svm}, nil
}

func (e *rootEngine) Name() string { return "root" }
func (e *rootEngine) Close()       { e.svm.Close() }

func (e *rootEngine) Airdrop(pk solana.PublicKey, lamports uint64) error {
	return e.svm.Airdrop(pk, lamports)
}

func (e *rootEngine) Balance(pk solana.PublicKey) (uint64, bool, error) {
	return e.svm.Balance(pk)
}

func (e *rootEngine) LatestBlockhash() (solana.Hash, error) { return e.svm.LatestBlockhash() }
func (e *rootEngine) ExpireBlockhash() error                { return e.svm.ExpireBlockhash() }

func fromRootOutcome(o *litesvm.TxOutcome) TxResult {
	r := TxResult{
		Ok:   o.IsOk(),
		Sig:  o.Signature(),
		Fee:  o.Fee(),
		CU:   o.ComputeUnits(),
		Logs: o.Logs(),
		Err:  o.Error(),
	}
	post, _ := o.PostAccounts()
	for _, pa := range post {
		if pa.Account == nil {
			continue
		}
		r.Post = append(r.Post, PostBalance{Address: pa.Address, Lamports: pa.Account.Lamports})
	}
	return r
}

func (e *rootEngine) SendTx(txBytes []byte) (TxResult, error) {
	o, err := e.svm.SendLegacyTransaction(txBytes)
	if err != nil {
		return TxResult{}, err
	}
	return fromRootOutcome(o), nil
}

func (e *rootEngine) SimulateTx(txBytes []byte) (TxResult, error) {
	o, err := e.svm.SimulateLegacyTransaction(txBytes)
	if err != nil {
		return TxResult{}, err
	}
	return fromRootOutcome(o), nil
}

func (e *rootEngine) WarpToSlot(slot uint64) error   { return e.svm.WarpToSlot(slot) }
func (e *rootEngine) Clock() (*sysvar.Clock, error)  { return e.svm.Clock() }
func (e *rootEngine) SetClock(c *sysvar.Clock) error { return e.svm.SetClock(c) }
func (e *rootEngine) MinimumBalanceForRentExemption(n int) (uint64, error) {
	return e.svm.MinimumBalanceForRentExemption(n)
}
func (e *rootEngine) GetAccount(pk solana.PublicKey) *solana.Account { return e.svm.GetAccount(pk) }
func (e *rootEngine) SetAccount(pk solana.PublicKey, a *solana.Account) error {
	return e.svm.SetAccount(pk, a)
}
func (e *rootEngine) SetSigverify(enabled bool) error      { return e.svm.SetSigverify(enabled) }
func (e *rootEngine) SetBlockhashCheck(enabled bool) error { return e.svm.SetBlockhashCheck(enabled) }
func (e *rootEngine) GetTransaction(sig solana.Signature) bool {
	return e.svm.GetTransaction(sig) != nil
}

// SetDefaultPrograms is normally a no-op: litesvm.New() installs the
// default program set. It stays idempotent.
func (e *rootEngine) SetDefaultPrograms() error { return e.svm.SetDefaultPrograms() }

// ---------------------------------------------------------------------------
// pure-Go adapter
// ---------------------------------------------------------------------------

type pureEngine struct {
	svm *mithrilsvm.LiteSVM
}

func newPureEngine() (Engine, error) {
	svm, err := mithrilsvm.New()
	if err != nil {
		return nil, err
	}
	return &pureEngine{svm: svm}, nil
}

func (e *pureEngine) Name() string { return "pure" }
func (e *pureEngine) Close()       { e.svm.Close() }

func (e *pureEngine) Airdrop(pk solana.PublicKey, lamports uint64) error {
	return e.svm.Airdrop(pk, lamports)
}

func (e *pureEngine) Balance(pk solana.PublicKey) (uint64, bool, error) {
	return e.svm.Balance(pk)
}

func (e *pureEngine) LatestBlockhash() (solana.Hash, error) { return e.svm.LatestBlockhash() }
func (e *pureEngine) ExpireBlockhash() error                { return e.svm.ExpireBlockhash() }

func fromPureOutcome(o *mithrilsvm.TxOutcome) TxResult {
	r := TxResult{
		Ok:   o.IsOk(),
		Sig:  o.Signature(),
		Fee:  o.Fee(),
		CU:   o.ComputeUnits(),
		Logs: o.Logs(),
		Err:  o.Error(),
	}
	post, _ := o.PostAccounts()
	for _, pa := range post {
		if pa.Account == nil {
			continue
		}
		r.Post = append(r.Post, PostBalance{Address: pa.Address, Lamports: pa.Account.Lamports})
	}
	return r
}

func (e *pureEngine) SendTx(txBytes []byte) (TxResult, error) {
	o, err := e.svm.SendLegacyTransaction(txBytes)
	if err != nil {
		return TxResult{}, err
	}
	return fromPureOutcome(o), nil
}

func (e *pureEngine) SimulateTx(txBytes []byte) (TxResult, error) {
	o, err := e.svm.SimulateLegacyTransaction(txBytes)
	if err != nil {
		return TxResult{}, err
	}
	return fromPureOutcome(o), nil
}

func (e *pureEngine) WarpToSlot(slot uint64) error   { return e.svm.WarpToSlot(slot) }
func (e *pureEngine) Clock() (*sysvar.Clock, error)  { return e.svm.Clock() }
func (e *pureEngine) SetClock(c *sysvar.Clock) error { return e.svm.SetClock(c) }
func (e *pureEngine) MinimumBalanceForRentExemption(n int) (uint64, error) {
	return e.svm.MinimumBalanceForRentExemption(n)
}
func (e *pureEngine) GetAccount(pk solana.PublicKey) *solana.Account { return e.svm.GetAccount(pk) }
func (e *pureEngine) SetAccount(pk solana.PublicKey, a *solana.Account) error {
	return e.svm.SetAccount(pk, a)
}
func (e *pureEngine) SetSigverify(enabled bool) error      { return e.svm.SetSigverify(enabled) }
func (e *pureEngine) SetBlockhashCheck(enabled bool) error { return e.svm.SetBlockhashCheck(enabled) }

// SetDefaultPrograms is normally a no-op: mithrilsvm.New() installs the
// default program set like Rust LiteSVM::new(). It stays idempotent.
func (e *pureEngine) SetDefaultPrograms() error { return e.svm.SetDefaultPrograms() }

func (e *pureEngine) GetTransaction(sig solana.Signature) bool {
	return e.svm.GetTransaction(sig) != nil
}

// ---------------------------------------------------------------------------
// Observations and scenarios
// ---------------------------------------------------------------------------

// Observation is one named comparable value produced by a scenario. Soft
// observations report drift without failing the test (compute units);
// everything else is hard (ok-status, balances, fees, error variants, log
// framing and system-program ic_msg detail lines).
type Observation struct {
	Name  string
	Value string
	Soft  bool
}

func obs(name string, value any) Observation {
	return Observation{Name: name, Value: fmt.Sprint(value)}
}

func softObs(name string, value any) Observation {
	return Observation{Name: name, Value: fmt.Sprint(value), Soft: true}
}

// Scenario is a deterministic workload run identically on both engines.
// Wallets come from fixed seeds so keys are byte-identical across engines;
// blockhashes differ per engine, so transactions are built against each
// engine's own LatestBlockhash and outcomes (not signatures) are compared.
type Scenario struct {
	Name string
	Run  func(e Engine) ([]Observation, error)
}

// seededKey derives a deterministic keypair from a name.
func seededKey(name string) solana.PrivateKey {
	sum := sha256.Sum256([]byte("litesvm-difftest:" + name))
	return solana.PrivateKey(ed25519.NewKeyFromSeed(sum[:]))
}

// buildTransfer builds and signs a legacy transfer against the engine's own
// latest blockhash.
func buildTransfer(e Engine, from solana.PrivateKey, to solana.PublicKey, lamports uint64) ([]byte, error) {
	blockhash, err := e.LatestBlockhash()
	if err != nil {
		return nil, err
	}
	ix := system.NewTransferInstruction(lamports, from.PublicKey(), to).Build()
	return buildSignedTx(e, []solana.Instruction{ix}, blockhash, from)
}

// buildTransferAt is buildTransfer against an explicit blockhash.
func buildTransferAt(e Engine, from solana.PrivateKey, to solana.PublicKey, lamports uint64, blockhash solana.Hash) ([]byte, error) {
	ix := system.NewTransferInstruction(lamports, from.PublicKey(), to).Build()
	return buildSignedTx(e, []solana.Instruction{ix}, blockhash, from)
}

// rawTransferIx builds a system-transfer compiled instruction from raw parts
// so extra account metas can be appended (the system program ignores
// trailing accounts).
func rawTransferIx(from, to solana.PublicKey, lamports uint64, extra ...*solana.AccountMeta) solana.Instruction {
	data := make([]byte, 12)
	binary.LittleEndian.PutUint32(data[0:4], 2) // SystemInstruction::Transfer
	binary.LittleEndian.PutUint64(data[4:12], lamports)
	metas := solana.AccountMetaSlice{
		solana.NewAccountMeta(from, true, true),
		solana.NewAccountMeta(to, true, false),
	}
	metas = append(metas, extra...)
	return solana.NewInstruction(solana.SystemProgramID, metas, data)
}

// buildMemo builds and signs a legacy memo-v3 transaction (no accounts, the
// memo string as instruction data).
func buildMemo(e Engine, payer solana.PrivateKey, memo string) ([]byte, error) {
	blockhash, err := e.LatestBlockhash()
	if err != nil {
		return nil, err
	}
	ix := solana.NewInstruction(memoV3ProgramID, solana.AccountMetaSlice{}, []byte(memo))
	return buildSignedTx(e, []solana.Instruction{ix}, blockhash, payer)
}

// buildSignedTx builds and signs a legacy transaction. The first signer is
// the fee payer; extra signers cover instructions whose non-payer accounts
// are marked as signers (e.g. CreateAccount's new account).
func buildSignedTx(_ Engine, ixs []solana.Instruction, blockhash solana.Hash, payer solana.PrivateKey, extraSigners ...solana.PrivateKey) ([]byte, error) {
	tx, err := solana.NewTransaction(ixs, blockhash, solana.TransactionPayer(payer.PublicKey()))
	if err != nil {
		return nil, fmt.Errorf("build tx: %w", err)
	}
	signers := append([]solana.PrivateKey{payer}, extraSigners...)
	if _, err := tx.Sign(func(k solana.PublicKey) *solana.PrivateKey {
		for i := range signers {
			if k.Equals(signers[i].PublicKey()) {
				return &signers[i]
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("sign tx: %w", err)
	}
	b, err := tx.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("marshal tx: %w", err)
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// Error normalization
//
// Rust litesvm renders TransactionError with Rust Debug formatting, e.g.
//
//	BlockhashNotFound
//	InstructionError(0, Custom(1))
//	InsufficientFundsForRent { account_index: 1 }
//	DuplicateInstruction(2)
//
// This engine emits Agave wire-format JSON, e.g.
//
//	"BlockhashNotFound"
//	{"InstructionError":[0,{"Custom":1}]}
//	{"InsufficientFundsForRent":{"account_index":1}}
//	{"DuplicateInstruction":2}
//
// normalizeError maps both shapes onto one canonical form:
//
//	BlockhashNotFound
//	InstructionError(0, Custom(1))
//	InsufficientFundsForRent(account_index=1)
//	DuplicateInstruction(2)
// ---------------------------------------------------------------------------

// mithrilSystemErrCode maps mithril's system-program error sentinel names
// onto Agave SystemError custom codes. Agave surfaces system-program
// failures as InstructionError Custom(n), while mithril's Agave-JSON
// rendering leaks its internal sentinel names for these (agaveInstrErrName
// only strips InstrErr/TxErr prefixes). The two shapes describe the
// identical failure, so the harness maps mithril's names onto the Custom
// codes; the rendering divergence itself is documented in DRIFT.md.
var mithrilSystemErrCode = map[string]uint32{
	"SystemProgErrAccountAlreadyInUse":           0, // SystemError::AccountAlreadyInUse
	"SystemProgErrResultWithNegativeLamports":    1, // SystemError::ResultWithNegativeLamports
	"SystemProgErrInvalidProgramId":              2, // SystemError::InvalidProgramId
	"SystemProgErrInvalidAccountDataLength":      3, // SystemError::InvalidAccountDataLength
	"SystemProgErrMaxSeedLengthExceeded":         4, // SystemError::MaxSeedLengthExceeded
	"SystemProgErrAddressWithSeedMismatch":       5, // SystemError::AddressWithSeedMismatch
	"SystemProgErrNonceNoRecentBlockhashes":      6, // SystemError::NonceNoRecentBlockhashes
	"SystemProgErrNonceBlockhashNotExpired":      7, // SystemError::NonceBlockhashNotExpired
	"SystemProgErrNonceUnexpectedBlockhashValue": 8, // SystemError::NonceUnexpectedBlockhashValue
}

// canonicalInstrVariant canonicalizes an inner InstructionError variant,
// folding mithril's system-program sentinels onto Agave Custom codes.
func canonicalInstrVariant(s string) string {
	if code, ok := mithrilSystemErrCode[s]; ok {
		return fmt.Sprintf("Custom(%d)", code)
	}
	return s
}

// splitErrorVariant splits a canonical error into its variant name and the
// argument detail, e.g. "InsufficientFundsForRent(account_index=1)" ->
// ("InsufficientFundsForRent", "account_index=1"). Bare variants return an
// empty detail.
func splitErrorVariant(s string) (variant, detail string) {
	if i := strings.Index(s, "("); i > 0 && strings.HasSuffix(s, ")") {
		return s[:i], s[i+1 : len(s)-1]
	}
	return s, ""
}

// normalizeError maps either engine's error rendering onto the canonical
// variant form. Empty input (success) maps to "".
func normalizeError(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if s[0] == '"' || s[0] == '{' {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err == nil {
			return normalizeJSONError(v)
		}
	}
	return normalizeRustDebugError(s)
}

// normalizeJSONError canonicalizes a decoded Agave wire-format JSON value.
func normalizeJSONError(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatInt(int64(x), 10)
	case map[string]any:
		if len(x) == 1 {
			for k, inner := range x {
				switch k {
				case "InstructionError":
					if arr, ok := inner.([]any); ok && len(arr) == 2 {
						return fmt.Sprintf("InstructionError(%s, %s)",
							normalizeJSONError(arr[0]),
							canonicalInstrVariant(normalizeJSONError(arr[1])))
					}
				case "Custom":
					return fmt.Sprintf("Custom(%s)", normalizeJSONError(inner))
				case "BorshIoError":
					// The Rust Debug rendering keeps the inner string quoted
					// (BorshIoError("msg")); quote the JSON string too so
					// both engines normalize identically.
					if msg, ok := inner.(string); ok {
						return fmt.Sprintf("BorshIoError(%q)", msg)
					}
					return fmt.Sprintf("BorshIoError(%s)", normalizeJSONError(inner))
				case "InsufficientFundsForRent", "ProgramExecutionTemporarilyRestricted":
					if m, ok := inner.(map[string]any); ok {
						return fmt.Sprintf("%s(account_index=%s)", k, normalizeJSONError(m["account_index"]))
					}
				}
				// Generic single-key tuple variant, e.g. DuplicateInstruction.
				return fmt.Sprintf("%s(%s)", k, normalizeJSONError(inner))
			}
		}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// normalizeRustDebugError canonicalizes a Rust Debug rendering of
// solana_transaction_error::TransactionError.
func normalizeRustDebugError(s string) string {
	// Struct variant: Name { field: v, field2: v2 } -> Name(field=v,field2=v2)
	if i := strings.Index(s, " {"); i > 0 && strings.HasSuffix(s, "}") {
		name := s[:i]
		body := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s[i+2:]), "}"))
		parts := strings.Split(body, ",")
		fields := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			fields = append(fields, strings.ReplaceAll(p, ": ", "="))
		}
		return name + "(" + strings.Join(fields, ",") + ")"
	}
	// Tuple variant: Name(a, b) -> canonical spacing.
	if i := strings.Index(s, "("); i > 0 && strings.HasSuffix(s, ")") {
		name := s[:i]
		inner := s[i+1 : len(s)-1]
		if name == "InstructionError" {
			if j := strings.Index(inner, ","); j >= 0 {
				return fmt.Sprintf("InstructionError(%s, %s)",
					strings.TrimSpace(inner[:j]),
					canonicalInstrVariant(normalizeRustDebugError(strings.TrimSpace(inner[j+1:]))))
			}
		}
		return name + "(" + strings.TrimSpace(inner) + ")"
	}
	// Unit variant: bare name.
	return s
}
