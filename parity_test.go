package litesvm

import (
	"crypto/ed25519"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/sysvar"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LiteSVM/litesvm-go/internal/elf"
	"github.com/LiteSVM/litesvm-go/internal/gates"
)

// --- ComputeBudget ---------------------------------------------------------

func TestComputeBudgetDefaultAndGetSet(t *testing.T) {
	svm := mustNew(t)

	_, has, err := svm.ComputeBudget()
	require.NoError(t, err)
	assert.False(t, has, "fresh instance must have no custom budget")

	def := DefaultComputeBudget()
	assert.Equal(t, uint64(1_400_000), def.ComputeUnitLimit)
	assert.Equal(t, uint32(32*1024), def.HeapSize)
	assert.Equal(t, uint64(5), def.MaxInstructionStackDepth)
	assert.Equal(t, uint64(64), def.MaxInstructionTraceLength)
	assert.Equal(t, uint64(8), def.HeapCost)

	require.NoError(t, svm.SetComputeBudget(def))
	got, has, err := svm.ComputeBudget()
	require.NoError(t, err)
	require.True(t, has)
	assert.Equal(t, def, got)
}

func TestComputeBudgetRejectsUnhonoredFields(t *testing.T) {
	svm := mustNew(t)

	b := DefaultComputeBudget()
	b.HeapCost = 9
	err := svm.SetComputeBudget(b)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HeapCost")

	b = DefaultComputeBudget()
	b.MaxInstructionStackDepth = 9
	err = svm.SetComputeBudget(b)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MaxInstructionStackDepth")

	b = DefaultComputeBudget()
	b.MaxInstructionTraceLength = 128
	err = svm.SetComputeBudget(b)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MaxInstructionTraceLength")

	b = DefaultComputeBudget()
	b.ComputeUnitLimit = 2_000_000 // above mithril's 1.4M cap
	err = svm.SetComputeBudget(b)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ComputeUnitLimit")

	b = DefaultComputeBudget()
	b.HeapSize = 1024 // below the 32KiB minimum
	err = svm.SetComputeBudget(b)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HeapSize")

	// A rejected budget must not be installed.
	_, has, err := svm.ComputeBudget()
	require.NoError(t, err)
	assert.False(t, has)
}

func TestComputeBudgetUnitLimitHonored(t *testing.T) {
	svm := mustNew(t)
	payer := solana.NewWallet()
	recipient := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 5*lamportsPerSol))

	// The transfer (150 CU) plus the injected SetComputeUnitLimit (150 CU)
	// fit exactly into a 300 CU budget.
	b := DefaultComputeBudget()
	b.ComputeUnitLimit = 300
	require.NoError(t, svm.SetComputeBudget(b))

	txBytes := signedTransfer(t, svm, payer.PrivateKey, recipient.PublicKey(), lamportsPerSol)
	outcome, err := svm.SendLegacyTransaction(txBytes)
	require.NoError(t, err)
	require.True(t, outcome.IsOk(), "transfer within limit failed: %s", outcome.Error())
	assert.Equal(t, uint64(300), outcome.ComputeUnits())

	// With a 100 CU budget the transfer itself (150 CU) must fail.
	b.ComputeUnitLimit = 100
	require.NoError(t, svm.SetComputeBudget(b))
	txBytes = signedTransfer(t, svm, payer.PrivateKey, recipient.PublicKey(), lamportsPerSol)
	outcome, err = svm.SendLegacyTransaction(txBytes)
	require.NoError(t, err)
	assert.False(t, outcome.IsOk(), "transfer must exceed a 100 CU budget")

	// A transaction with its own compute-budget instructions keeps them.
	limitIx := solana.NewInstruction(
		solana.MustPublicKeyFromBase58("ComputeBudget111111111111111111111111111111"),
		solana.AccountMetaSlice{},
		[]byte{2, 0x2c, 0x01, 0, 0}, // SetComputeUnitLimit(300)
	)
	blockhash, err := svm.LatestBlockhash()
	require.NoError(t, err)
	transferIx := system.NewTransferInstruction(lamportsPerSol, payer.PublicKey(), recipient.PublicKey()).Build()
	tx, err := solana.NewTransaction([]solana.Instruction{limitIx, transferIx}, blockhash,
		solana.TransactionPayer(payer.PublicKey()))
	require.NoError(t, err)
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(payer.PublicKey()) {
			k := payer.PrivateKey
			return &k
		}
		return nil
	})
	require.NoError(t, err)
	txBytes, err = tx.MarshalBinary()
	require.NoError(t, err)
	outcome, err = svm.SendLegacyTransaction(txBytes)
	require.NoError(t, err)
	require.True(t, outcome.IsOk(), "tx with explicit 300 CU budget failed: %s", outcome.Error())
	assert.Equal(t, uint64(300), outcome.ComputeUnits())
}

func TestComputeBudgetHeapSizeHonored(t *testing.T) {
	svm := mustNew(t)
	payer := solana.NewWallet()
	recipient := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 5*lamportsPerSol))

	b := DefaultComputeBudget()
	b.HeapSize = 64 * 1024
	require.NoError(t, svm.SetComputeBudget(b))

	txBytes := signedTransfer(t, svm, payer.PrivateKey, recipient.PublicKey(), lamportsPerSol)
	outcome, err := svm.SendLegacyTransaction(txBytes)
	require.NoError(t, err)
	require.True(t, outcome.IsOk(), "transfer with heap override failed: %s", outcome.Error())
	// transfer + injected SetComputeUnitLimit + injected RequestHeapFrame.
	assert.Equal(t, uint64(450), outcome.ComputeUnits())
}

// --- FeatureSet ------------------------------------------------------------

func TestFeatureSetRoundtrip(t *testing.T) {
	fs, err := NewFeatureSet()
	require.NoError(t, err)
	defer fs.Close()
	assert.Equal(t, 0, fs.ActiveCount())
	assert.Equal(t, len(gates.All), fs.InactiveCount())

	all, err := NewFeatureSetAllEnabled()
	require.NoError(t, err)
	defer all.Close()
	assert.Equal(t, len(gates.All), all.ActiveCount())
	assert.Equal(t, 0, all.InactiveCount())

	id := gates.All[0].Address
	assert.False(t, fs.IsActive(id))
	require.NoError(t, fs.Activate(id, 42))
	assert.True(t, fs.IsActive(id))
	slot, ok := fs.ActivatedSlot(id)
	require.True(t, ok)
	assert.Equal(t, uint64(42), slot)
	assert.Equal(t, 1, fs.ActiveCount())
	assert.Equal(t, len(gates.All)-1, fs.InactiveCount())
	assert.Equal(t, []solana.PublicKey{id}, fs.ActiveFeatures())
	assert.Len(t, fs.InactiveFeatures(), len(gates.All)-1)

	require.NoError(t, fs.Deactivate(id))
	assert.False(t, fs.IsActive(id))
	_, ok = fs.ActivatedSlot(id)
	assert.False(t, ok)
	assert.Equal(t, len(gates.All), fs.InactiveCount())
}

func TestSetFeatureSetThenSendWorks(t *testing.T) {
	svm := mustNew(t)

	require.Error(t, svm.SetFeatureSet(nil))

	// Install the mainnet-active snapshot explicitly and prove execution
	// still works end to end.
	fs, err := NewFeatureSet()
	require.NoError(t, err)
	for _, g := range gates.All {
		if g.MainnetActive {
			require.NoError(t, fs.Activate(g.Address, 0))
		}
	}
	require.NoError(t, svm.SetFeatureSet(fs))

	payer := solana.NewWallet()
	recipient := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 2*lamportsPerSol))
	txBytes := signedTransfer(t, svm, payer.PrivateKey, recipient.PublicKey(), lamportsPerSol)
	outcome, err := svm.SendLegacyTransaction(txBytes)
	require.NoError(t, err)
	require.True(t, outcome.IsOk(), "transfer after SetFeatureSet failed: %s", outcome.Error())

	bal, _, err := svm.Balance(recipient.PublicKey())
	require.NoError(t, err)
	assert.Equal(t, uint64(lamportsPerSol), bal)
}

// --- Setter wrappers -------------------------------------------------------

func TestSetSysvarsBuiltinsPrecompiles(t *testing.T) {
	svm := mustNew(t)

	// SetSysvars resets the sysvars (Clock back to slot 0) but keeps the
	// blockhash queue.
	require.NoError(t, svm.WarpToSlot(99))
	before, err := svm.LatestBlockhash()
	require.NoError(t, err)
	require.NoError(t, svm.SetSysvars())
	clock, err := svm.Clock()
	require.NoError(t, err)
	assert.Equal(t, uint64(0), clock.Slot)
	after, err := svm.LatestBlockhash()
	require.NoError(t, err)
	assert.Equal(t, before, after)

	// SlotHistory is part of the defaults.
	sh, err := svm.SlotHistory()
	require.NoError(t, err)
	assert.Equal(t, uint64(1), sh.NextSlot)

	// SetBuiltins / SetPrecompiles reinstall the native accounts.
	systemProgram := solana.SystemProgramID
	ed25519Program := solana.MustPublicKeyFromBase58("Ed25519SigVerify111111111111111111111111111")
	require.NoError(t, svm.SetAccount(systemProgram, &solana.Account{Lamports: 1, Owner: solana.SystemProgramID}))
	require.NoError(t, svm.SetAccount(ed25519Program, &solana.Account{Lamports: 1, Owner: solana.SystemProgramID}))

	require.NoError(t, svm.SetBuiltins())
	acct := svm.GetAccount(systemProgram)
	require.NotNil(t, acct)
	assert.True(t, acct.Executable)
	assert.Equal(t, []byte("system_program"), acct.Data)

	require.NoError(t, svm.SetPrecompiles())
	acct = svm.GetAccount(ed25519Program)
	require.NotNil(t, acct)
	assert.True(t, acct.Executable)
	assert.Equal(t, []byte("ed25519_program"), acct.Data)
}

// --- BuildTransferTx -------------------------------------------------------

func TestBuildTransferTxSends(t *testing.T) {
	svm := mustNew(t)

	seed := [32]byte{1, 2, 3, 4, 5}
	payer := solana.PrivateKey(ed25519.NewKeyFromSeed(seed[:])).PublicKey()
	recipient := solana.NewWallet().PublicKey()
	require.NoError(t, svm.Airdrop(payer, 2*lamportsPerSol))

	blockhash, err := svm.LatestBlockhash()
	require.NoError(t, err)
	txBytes, err := BuildTransferTx(seed, recipient, lamportsPerSol, blockhash)
	require.NoError(t, err)
	require.NotEmpty(t, txBytes)

	outcome, err := svm.SendLegacyTransaction(txBytes)
	require.NoError(t, err)
	require.True(t, outcome.IsOk(), "BuildTransferTx send failed: %s", outcome.Error())

	bal, _, err := svm.Balance(recipient)
	require.NoError(t, err)
	assert.Equal(t, uint64(lamportsPerSol), bal)
}

// --- AddProgramWithLoader --------------------------------------------------

func TestAddProgramWithLoaderDeprecatedExecutes(t *testing.T) {
	svm := mustNew(t)
	payer := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 2*lamportsPerSol))

	// The vendored memo v1 blob under the deprecated loader at a fresh id.
	programID := solana.NewWallet().PublicKey()
	memoV1 := elf.Defaults[2]
	require.Equal(t, elf.LoaderV1, memoV1.Loader, "elf.Defaults[2] must be memo v1")
	require.NoError(t, svm.AddProgramWithLoader(programID, memoV1.Elf, solana.BPFLoaderDeprecatedProgramID))

	blockhash, err := svm.LatestBlockhash()
	require.NoError(t, err)
	ix := solana.NewInstruction(programID, solana.AccountMetaSlice{}, []byte("memo v1 via deprecated loader"))
	tx, err := solana.NewTransaction([]solana.Instruction{ix}, blockhash,
		solana.TransactionPayer(payer.PublicKey()))
	require.NoError(t, err)
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(payer.PublicKey()) {
			k := payer.PrivateKey
			return &k
		}
		return nil
	})
	require.NoError(t, err)
	txBytes, err := tx.MarshalBinary()
	require.NoError(t, err)

	outcome, err := svm.SendLegacyTransaction(txBytes)
	require.NoError(t, err)
	require.True(t, outcome.IsOk(), "memo v1 tx failed: %s logs=%v", outcome.Error(), outcome.Logs())
}

func TestAddProgramWithLoaderErrors(t *testing.T) {
	svm := mustNew(t)
	programID := solana.NewWallet().PublicKey()

	err := svm.AddProgramWithLoader(programID, []byte{1}, solana.SystemProgramID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown loader")

	err = svm.AddProgramWithLoader(programID, nil, solana.BPFLoaderDeprecatedProgramID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty program bytes")
}

// --- SlotHistory -----------------------------------------------------------

func TestSlotHistoryDefaultAndRoundtrip(t *testing.T) {
	svm := mustNew(t)

	sh, err := svm.SlotHistory()
	require.NoError(t, err)
	assert.Equal(t, uint64(1), sh.NextSlot)
	assert.Len(t, sh.Bits, sysvar.SlotHistoryMaxEntries/64)
	assert.Equal(t, sysvar.SlotHistoryFound, sh.Check(0))

	sh.Add(42)
	require.NoError(t, svm.SetSlotHistory(sh))

	got, err := svm.SlotHistory()
	require.NoError(t, err)
	assert.Equal(t, uint64(43), got.NextSlot)
	assert.Equal(t, sysvar.SlotHistoryFound, got.Check(42))
	assert.Equal(t, sysvar.SlotHistoryNotFound, got.Check(41))

	require.Error(t, svm.SetSlotHistory(nil))
	require.Error(t, svm.SetSlotHistory(&sysvar.SlotHistory{Bits: []uint64{1}, NextSlot: 1}))
}

// --- Durable nonce advance on failed sends ---------------------------------

func TestFailedDurableNonceTxAdvancesNonce(t *testing.T) {
	svm := mustNew(t)
	payer := solana.NewWallet()
	recipient := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 2*lamportsPerSol))

	// Hand-build an initialized nonce account holding a stale durable nonce.
	noncePk := solana.NewWallet().PublicKey()
	staleNonce := solana.DurableNonceFromBlockhash(solana.Hash{0xAA, 0xBB})
	nonceData, err := solana.NewNonceVersions(
		solana.NewInitializedNonceState(payer.PublicKey(), staleNonce, defaultLamportsPerSignature),
	).MarshalAccountData()
	require.NoError(t, err)
	rentMin, err := svm.MinimumBalanceForRentExemption(len(nonceData))
	require.NoError(t, err)
	require.NoError(t, svm.SetAccount(noncePk, &solana.Account{
		Lamports: rentMin,
		Data:     nonceData,
		Owner:    solana.SystemProgramID,
	}))

	// A durable-nonce tx: AdvanceNonceAccount first, then a transfer that
	// fails during execution (more lamports than the payer holds).
	advanceIx := system.NewAdvanceNonceAccountInstruction(
		noncePk, solana.SysVarRecentBlockHashesPubkey, payer.PublicKey(),
	).Build()
	transferIx := system.NewTransferInstruction(100*lamportsPerSol, payer.PublicKey(), recipient.PublicKey()).Build()
	tx, err := solana.NewTransaction(
		[]solana.Instruction{advanceIx, transferIx},
		staleNonce.AsHash(),
		solana.TransactionPayer(payer.PublicKey()),
	)
	require.NoError(t, err)
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(payer.PublicKey()) {
			k := payer.PrivateKey
			return &k
		}
		return nil
	})
	require.NoError(t, err)
	txBytes, err := tx.MarshalBinary()
	require.NoError(t, err)

	outcome, err := svm.SendLegacyTransaction(txBytes)
	require.NoError(t, err)
	require.False(t, outcome.IsOk(), "the oversized transfer must fail")
	assert.Contains(t, outcome.Error(), "InstructionError")

	// The nonce must have advanced to the durable nonce of the current
	// blockhash even though the tx failed, so the tx cannot be replayed.
	blockhash, err := svm.LatestBlockhash()
	require.NoError(t, err)
	wantNonce := solana.DurableNonceFromBlockhash(blockhash)
	acct := svm.GetAccount(noncePk)
	require.NotNil(t, acct)
	versions, err := solana.DecodeNonceVersions(acct.Data)
	require.NoError(t, err)
	assert.Equal(t, wantNonce, versions.State.Data.DurableNonce, "failed durable-nonce tx must advance the nonce")

	// The fee was charged for the executed-but-failed transaction.
	bal, _, err := svm.Balance(payer.PublicKey())
	require.NoError(t, err)
	assert.Equal(t, uint64(2*lamportsPerSol-5000), bal)

	// Replaying the same bytes now fails age validation.
	replay, err := svm.SendLegacyTransaction(txBytes)
	require.NoError(t, err)
	assert.False(t, replay.IsOk())
	assert.Contains(t, replay.Error(), "BlockhashNotFound")
}
