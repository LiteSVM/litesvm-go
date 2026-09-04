package litesvm

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LiteSVM/litesvm-go/internal/elf"
	"github.com/LiteSVM/litesvm-go/internal/gates"
)

// TestFailedTxRecordedAndReplayRejected pins the litesvm history semantics
// for executed-but-failed transactions: the fee is charged exactly once, the
// outcome is recorded (GetTransaction non-nil), and replaying the identical
// bytes returns AlreadyProcessed without re-charging.
func TestFailedTxRecordedAndReplayRejected(t *testing.T) {
	svm := mustNew(t)
	payer := solana.NewWallet()
	recipient := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 1*lamportsPerSol))

	// Transfer more than the payer holds: executes, fails with an
	// InstructionError, and charges the 5000-lamport fee.
	txBytes := signedTransfer(t, svm, payer.PrivateKey, recipient.PublicKey(), 2*lamportsPerSol)
	first, err := svm.SendLegacyTransaction(txBytes)
	require.NoError(t, err)
	require.False(t, first.IsOk())
	assert.Contains(t, first.Error(), "InstructionError")
	assert.Equal(t, uint64(5000), first.Fee())

	bal, _, err := svm.Balance(payer.PublicKey())
	require.NoError(t, err)
	require.Equal(t, uint64(lamportsPerSol-5000), bal, "fee must be charged once")

	recorded := svm.GetTransaction(first.Signature())
	require.NotNil(t, recorded, "failed-executed tx must be recorded in history")
	assert.False(t, recorded.IsOk())

	second, err := svm.SendLegacyTransaction(txBytes)
	require.NoError(t, err)
	assert.False(t, second.IsOk())
	assert.Contains(t, second.Error(), "AlreadyProcessed")

	bal, _, err = svm.Balance(payer.PublicKey())
	require.NoError(t, err)
	assert.Equal(t, uint64(lamportsPerSol-5000), bal, "replay must not charge the fee again")
}

// TestSigverifyOffReplayReexecutes pins Rust's maybe_history_check gating:
// with sigverify disabled the duplicate check is skipped and identical bytes
// re-execute.
func TestSigverifyOffReplayReexecutes(t *testing.T) {
	svm := mustNew(t)
	require.NoError(t, svm.SetSigverify(false))
	payer := solana.NewWallet()
	recipient := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 3*lamportsPerSol))

	txBytes := signedTransfer(t, svm, payer.PrivateKey, recipient.PublicKey(), lamportsPerSol)
	for i := 0; i < 2; i++ {
		outcome, err := svm.SendLegacyTransaction(txBytes)
		require.NoError(t, err)
		require.True(t, outcome.IsOk(), "send %d failed: %s", i, outcome.Error())
		// Recording is not gated on sigverify (Rust records every included
		// tx), so the outcome stays visible through GetTransaction.
		require.NotNil(t, svm.GetTransaction(outcome.Signature()))
	}

	bal, _, err := svm.Balance(recipient.PublicKey())
	require.NoError(t, err)
	assert.Equal(t, uint64(2*lamportsPerSol), bal, "replay with sigverify off must re-execute")
}

// TestExpireThenReplayBlockhashNotFound pins Rust's check ordering:
// blockhash age is validated before the history lookup, so a duplicate with
// an expired blockhash yields BlockhashNotFound, not AlreadyProcessed.
func TestExpireThenReplayBlockhashNotFound(t *testing.T) {
	svm := mustNew(t)
	payer := solana.NewWallet()
	recipient := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 2*lamportsPerSol))

	txBytes := signedTransfer(t, svm, payer.PrivateKey, recipient.PublicKey(), lamportsPerSol)
	first, err := svm.SendLegacyTransaction(txBytes)
	require.NoError(t, err)
	require.True(t, first.IsOk(), "first send failed: %s", first.Error())

	require.NoError(t, svm.ExpireBlockhash())

	replayed, err := svm.SendLegacyTransaction(txBytes)
	require.NoError(t, err)
	require.False(t, replayed.IsOk())
	assert.Contains(t, replayed.Error(), "BlockhashNotFound")

	bal, _, err := svm.Balance(recipient.PublicKey())
	require.NoError(t, err)
	assert.Equal(t, uint64(lamportsPerSol), bal, "replay must not re-execute")
}

// TestZeroBlockhashRejected pins the LatestEvictedBlockhash sentinel: a
// transaction signed with the all-zero recent blockhash must fail age
// validation with BlockhashNotFound instead of slipping through the
// zero-value comparison.
func TestZeroBlockhashRejected(t *testing.T) {
	svm := mustNew(t)
	payer := solana.NewWallet()
	recipient := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 2*lamportsPerSol))

	ix := transferIx(payer.PublicKey(), recipient.PublicKey(), lamportsPerSol)
	tx, err := solana.NewTransaction([]solana.Instruction{ix}, solana.Hash{},
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
	require.False(t, outcome.IsOk(), "zero-blockhash tx must be rejected")
	assert.Contains(t, outcome.Error(), "BlockhashNotFound")

	bal, _, err := svm.Balance(payer.PublicKey())
	require.NoError(t, err)
	assert.Equal(t, uint64(2*lamportsPerSol), bal, "no fee for a pre-execution rejection")
}

// TestZeroLamportAccountDeleted pins Rust's zero-lamport deletion: draining
// an account to zero removes it from the store (GetAccount nil, Balance
// exists=false), and SetAccount with zero lamports deletes as well.
func TestZeroLamportAccountDeleted(t *testing.T) {
	svm := mustNew(t)
	payer := solana.NewWallet()
	recipient := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 1*lamportsPerSol))

	// Transfer everything minus the fee: the payer ends at exactly zero.
	txBytes := signedTransfer(t, svm, payer.PrivateKey, recipient.PublicKey(), lamportsPerSol-5000)
	outcome, err := svm.SendLegacyTransaction(txBytes)
	require.NoError(t, err)
	require.True(t, outcome.IsOk(), "drain transfer failed: %s", outcome.Error())

	assert.Nil(t, svm.GetAccount(payer.PublicKey()), "zero-lamport account must be deleted")
	_, exists, err := svm.Balance(payer.PublicKey())
	require.NoError(t, err)
	assert.False(t, exists)

	// SetAccount with zero lamports deletes an existing account.
	pk := solana.NewWallet().PublicKey()
	require.NoError(t, svm.SetAccount(pk, &solana.Account{Lamports: 42, Owner: solana.SystemProgramID}))
	require.NotNil(t, svm.GetAccount(pk))
	require.NoError(t, svm.SetAccount(pk, &solana.Account{Lamports: 0, Owner: solana.SystemProgramID}))
	assert.Nil(t, svm.GetAccount(pk))
}

// memoTx builds a signed transaction invoking programID with data, no
// accounts (memo-shaped).
func memoTx(t *testing.T, svm *LiteSVM, payer solana.PrivateKey, programID solana.PublicKey, data []byte) []byte {
	t.Helper()
	blockhash, err := svm.LatestBlockhash()
	require.NoError(t, err)
	ix := solana.NewInstruction(programID, solana.AccountMetaSlice{}, data)
	tx, err := solana.NewTransaction([]solana.Instruction{ix}, blockhash,
		solana.TransactionPayer(payer.PublicKey()))
	require.NoError(t, err)
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(payer.PublicKey()) {
			return &payer
		}
		return nil
	})
	require.NoError(t, err)
	b, err := tx.MarshalBinary()
	require.NoError(t, err)
	return b
}

// TestProgramCacheEvictionOnReinstall proves re-installing a program under
// the same id executes the NEW bytes: memo v3 succeeds on arbitrary data,
// p-token rejects the same invocation, so a stale compiled-program cache
// entry is directly observable as a flipped outcome.
func TestProgramCacheEvictionOnReinstall(t *testing.T) {
	memoElf := elf.Defaults[3].Elf   // memo v3
	ptokenElf := elf.Defaults[0].Elf // p-token

	run := func(t *testing.T, install func(svm *LiteSVM, id solana.PublicKey, prog []byte) error) {
		svm := mustNew(t)
		payer := solana.NewWallet()
		require.NoError(t, svm.Airdrop(payer.PublicKey(), 5*lamportsPerSol))
		programID := solana.NewWallet().PublicKey()

		require.NoError(t, install(svm, programID, memoElf))
		outcome, err := svm.SendLegacyTransaction(memoTx(t, svm, payer.PrivateKey, programID, []byte("cache probe 1")))
		require.NoError(t, err)
		require.True(t, outcome.IsOk(), "memo under fresh id failed: %s logs=%v", outcome.Error(), outcome.Logs())

		// Re-install a different ELF at the same id; the cached memo bytes
		// must not be served anymore.
		require.NoError(t, install(svm, programID, ptokenElf))
		outcome, err = svm.SendLegacyTransaction(memoTx(t, svm, payer.PrivateKey, programID, []byte("cache probe 2")))
		require.NoError(t, err)
		require.False(t, outcome.IsOk(),
			"p-token must reject a memo-shaped invocation; stale program cache served old bytes (logs=%v)", outcome.Logs())

		// And back to memo: works again.
		require.NoError(t, install(svm, programID, memoElf))
		outcome, err = svm.SendLegacyTransaction(memoTx(t, svm, payer.PrivateKey, programID, []byte("cache probe 3")))
		require.NoError(t, err)
		require.True(t, outcome.IsOk(), "memo after re-reinstall failed: %s", outcome.Error())
	}

	t.Run("loader-v2/AddProgramWithLoader", func(t *testing.T) {
		run(t, func(svm *LiteSVM, id solana.PublicKey, prog []byte) error {
			return svm.AddProgramWithLoader(id, prog, solana.BPFLoaderProgramID)
		})
	})
	t.Run("loader-v3/AddProgram", func(t *testing.T) {
		run(t, func(svm *LiteSVM, id solana.PublicKey, prog []byte) error {
			return svm.AddProgram(id, prog)
		})
	})
	t.Run("loader-v2/SetAccount", func(t *testing.T) {
		run(t, func(svm *LiteSVM, id solana.PublicKey, prog []byte) error {
			rent, err := svm.MinimumBalanceForRentExemption(len(prog))
			if err != nil {
				return err
			}
			return svm.SetAccount(id, &solana.Account{
				Lamports:   rent,
				Data:       prog,
				Owner:      solana.BPFLoaderProgramID,
				Executable: true,
			})
		})
	})
}

// transferIx builds a raw system-transfer compiled instruction so extra
// account metas can be appended (the system program ignores trailing
// accounts).
func transferIx(from, to solana.PublicKey, lamports uint64, extra ...*solana.AccountMeta) solana.Instruction {
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

// TestPostAccountsSimulateOnlyAllWritables pins the Rust PostAccounts
// contract: sends never carry post accounts; successful simulations carry
// every writable account of the message with its post-execution state,
// including writable accounts the transaction never touched.
func TestPostAccountsSimulateOnlyAllWritables(t *testing.T) {
	svm := mustNew(t)
	payer := solana.NewWallet()
	recipient := solana.NewWallet().PublicKey()
	extra := solana.NewWallet().PublicKey()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 2*lamportsPerSol))
	require.NoError(t, svm.Airdrop(extra, 1*lamportsPerSol))

	const amount = uint64(lamportsPerSol / 2)
	build := func() []byte {
		blockhash, err := svm.LatestBlockhash()
		require.NoError(t, err)
		ix := transferIx(payer.PublicKey(), recipient, amount,
			solana.NewAccountMeta(extra, true, false)) // writable, untouched
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
		b, err := tx.MarshalBinary()
		require.NoError(t, err)
		return b
	}

	sim, err := svm.SimulateLegacyTransaction(build())
	require.NoError(t, err)
	require.True(t, sim.IsOk(), "simulate failed: %s", sim.Error())
	post, err := sim.PostAccounts()
	require.NoError(t, err)

	find := func(pk solana.PublicKey) *PostAccount {
		for i := range post {
			if post[i].Address.Equals(pk) {
				return &post[i]
			}
		}
		return nil
	}
	payerPost := find(payer.PublicKey())
	require.NotNil(t, payerPost, "payer missing from simulate post accounts")
	assert.Equal(t, uint64(2*lamportsPerSol-amount-5000), payerPost.Account.Lamports)
	recipPost := find(recipient)
	require.NotNil(t, recipPost, "recipient missing from simulate post accounts")
	assert.Equal(t, amount, recipPost.Account.Lamports)
	extraPost := find(extra)
	require.NotNil(t, extraPost, "untouched writable account missing from simulate post accounts")
	assert.Equal(t, uint64(lamportsPerSol), extraPost.Account.Lamports)

	// Sends never carry post accounts (Rust from_success).
	sent, err := svm.SendLegacyTransaction(build())
	require.NoError(t, err)
	require.True(t, sent.IsOk(), "send failed: %s", sent.Error())
	sentPost, err := sent.PostAccounts()
	require.NoError(t, err)
	assert.Empty(t, sentPost, "send outcomes must not carry post accounts")
}

// TestDeterministicBlockhashes pins the Rust create_blockhash mirror: the
// genesis blockhash is sha256("genesis") on every instance, and
// ExpireBlockhash chains sha256(previous hash).
func TestDeterministicBlockhashes(t *testing.T) {
	svmA := mustNew(t)
	svmB := mustNew(t)

	wantGenesis := solana.Hash(sha256.Sum256([]byte("genesis")))
	hashA, err := svmA.LatestBlockhash()
	require.NoError(t, err)
	hashB, err := svmB.LatestBlockhash()
	require.NoError(t, err)
	assert.Equal(t, wantGenesis, hashA)
	assert.Equal(t, wantGenesis, hashB)

	require.NoError(t, svmA.ExpireBlockhash())
	expired, err := svmA.LatestBlockhash()
	require.NoError(t, err)
	assert.Equal(t, solana.Hash(sha256.Sum256(wantGenesis[:])), expired)
}

// TestNewInstallsDefaults pins the LiteSVM::new parity batch: the default
// SPL program set, on-chain feature-gate accounts, the seeded SlotHashes
// sysvar, the deprecated Fees sysvar, and the stake-config account are all
// present on a fresh instance.
func TestNewInstallsDefaults(t *testing.T) {
	svm := mustNew(t)

	// Default programs (with_default_programs) without SetDefaultPrograms.
	for _, p := range elf.Defaults {
		acct := svm.GetAccount(p.ProgramID)
		require.NotNil(t, acct, "default program %s missing after New()", p.Name)
		assert.True(t, acct.Executable, "default program %s not executable", p.Name)
	}
	// Native mints stay opt-in.
	assert.Nil(t, svm.GetAccount(elf.NativeMintSPL), "native mint must stay opt-in")

	// Feature-gate accounts for active features only (with_feature_accounts).
	var active, inactive *gates.Gate
	for i := range gates.All {
		if gates.All[i].MainnetActive && active == nil {
			active = &gates.All[i]
		}
		if !gates.All[i].MainnetActive && inactive == nil {
			inactive = &gates.All[i]
		}
	}
	require.NotNil(t, active)
	require.NotNil(t, inactive)
	featAcct := svm.GetAccount(active.Address)
	require.NotNil(t, featAcct, "active feature %s has no on-chain account", active.Name)
	assert.Equal(t, solana.MustPublicKeyFromBase58("Feature111111111111111111111111111111111111"), featAcct.Owner)
	require.Len(t, featAcct.Data, 9)
	assert.Equal(t, byte(1), featAcct.Data[0], "activated_at must be Some")
	assert.Equal(t, uint64(0), binary.LittleEndian.Uint64(featAcct.Data[1:9]), "activated_at slot")
	min, err := svm.MinimumBalanceForRentExemption(9)
	require.NoError(t, err)
	assert.Equal(t, min, featAcct.Lamports)
	assert.Nil(t, svm.GetAccount(inactive.Address), "inactive feature %s must have no account", inactive.Name)

	// SlotHashes carries the (slot 0, genesis blockhash) entry.
	slotHashes, err := svm.SlotHashes()
	require.NoError(t, err)
	require.Len(t, slotHashes, 1)
	assert.Equal(t, uint64(0), slotHashes[0].Slot)
	genesis, err := svm.LatestBlockhash()
	require.NoError(t, err)
	assert.Equal(t, genesis, slotHashes[0].Hash)

	// Deprecated Fees sysvar: Fees::default() (lamports_per_signature 0).
	feesAcct := svm.GetAccount(solana.MustPublicKeyFromBase58("SysvarFees111111111111111111111111111111111"))
	require.NotNil(t, feesAcct)
	require.Len(t, feesAcct.Data, 8)
	assert.Equal(t, uint64(0), binary.LittleEndian.Uint64(feesAcct.Data))

	// Deprecated stake-config account: ConfigKeys header + Config::default().
	cfgAcct := svm.GetAccount(solana.MustPublicKeyFromBase58("StakeConfig11111111111111111111111111111111"))
	require.NotNil(t, cfgAcct)
	assert.Equal(t, solana.MustPublicKeyFromBase58("Config1111111111111111111111111111111111111"), cfgAcct.Owner)
	require.Len(t, cfgAcct.Data, 17)
	assert.Equal(t, uint64(0), binary.LittleEndian.Uint64(cfgAcct.Data[0:8]), "0 config keys")
	assert.Equal(t, 0.25, math.Float64frombits(binary.LittleEndian.Uint64(cfgAcct.Data[8:16])), "warmup_cooldown_rate")
	assert.Equal(t, byte(12), cfgAcct.Data[16], "slash_penalty")
	assert.Equal(t, uint64(1), cfgAcct.Lamports)

	// The RecentBlockhashes genesis entry carries lamports_per_signature 0
	// (Fees::default()), while fee charging stays at 5000.
	rbhAcct := svm.GetAccount(solana.MustPublicKeyFromBase58("SysvarRecentB1ockHashes11111111111111111111"))
	require.NotNil(t, rbhAcct)
	require.GreaterOrEqual(t, len(rbhAcct.Data), 8+32+8)
	assert.Equal(t, uint64(0), binary.LittleEndian.Uint64(rbhAcct.Data[8+32:8+32+8]))
}

// TestHistoryCapacityDefault pins the Rust TransactionHistory default: 32
// entries with FIFO eviction, after which an evicted signature is no longer
// deduplicated and re-executes.
func TestHistoryCapacityDefault(t *testing.T) {
	svm := mustNew(t)
	payer := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 2*lamportsPerSol))

	recipient1 := solana.NewWallet().PublicKey()
	const amount = uint64(2 * 890880) // rent-exempt for a 0-byte account
	tx1 := signedTransfer(t, svm, payer.PrivateKey, recipient1, amount)
	first, err := svm.SendLegacyTransaction(tx1)
	require.NoError(t, err)
	require.True(t, first.IsOk(), "tx1 failed: %s", first.Error())
	require.NotNil(t, svm.GetTransaction(first.Signature()))

	// 32 more recorded transactions evict tx1 (FIFO at capacity 32).
	var last *TxOutcome
	for i := 0; i < 32; i++ {
		to := solana.NewWallet().PublicKey()
		out, err := svm.SendLegacyTransaction(signedTransfer(t, svm, payer.PrivateKey, to, amount))
		require.NoError(t, err)
		require.True(t, out.IsOk(), "filler tx %d failed: %s", i, out.Error())
		last = out
	}
	require.NotNil(t, svm.GetTransaction(last.Signature()), "newest entry must be retained")
	require.Nil(t, svm.GetTransaction(first.Signature()), "oldest entry must be evicted at capacity 32")

	// The evicted signature is no longer deduplicated: the identical bytes
	// re-execute (blockhash is still the genesis hash).
	replayed, err := svm.SendLegacyTransaction(tx1)
	require.NoError(t, err)
	require.True(t, replayed.IsOk(), "evicted tx replay failed: %s", replayed.Error())
	bal, _, err := svm.Balance(recipient1)
	require.NoError(t, err)
	assert.Equal(t, 2*amount, bal)
}

// TestReturnDataEmptySemantics pins the ReturnData contract: empty return
// data is indistinguishable from never-set return data.
func TestReturnDataEmptySemantics(t *testing.T) {
	o := &TxOutcome{returnDataProgramID: solana.MustPublicKeyFromBase58("MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr")}
	_, _, ok := o.ReturnData()
	assert.False(t, ok, "empty return data must report absent even with a program id set")

	o.returnData = []byte{1}
	pid, data, ok := o.ReturnData()
	assert.True(t, ok)
	assert.Equal(t, o.returnDataProgramID, pid)
	assert.Equal(t, []byte{1}, data)
}

// TestGetAccountDataIsolated pins the value semantics of the accounts API:
// mutating slices returned by GetAccount (or passed to SetAccount) must not
// corrupt engine state.
func TestGetAccountDataIsolated(t *testing.T) {
	svm := mustNew(t)
	pk := solana.NewWallet().PublicKey()
	orig := []byte{1, 2, 3}
	require.NoError(t, svm.SetAccount(pk, &solana.Account{Lamports: 42, Data: orig, Owner: solana.SystemProgramID}))

	orig[0] = 99 // caller keeps mutating its slice
	got := svm.GetAccount(pk)
	require.NotNil(t, got)
	assert.Equal(t, []byte{1, 2, 3}, got.Data, "SetAccount must copy the caller's data")

	got.Data[1] = 88
	again := svm.GetAccount(pk)
	assert.Equal(t, []byte{1, 2, 3}, again.Data, "GetAccount must return a copy")
}

// TestLegacyEntryPointsRejectV0 pins the decode behavior: the legacy entry
// points bincode-decode a legacy Transaction and error on version-prefixed
// bytes, while the versioned entry points accept both wire formats.
func TestLegacyEntryPointsRejectV0(t *testing.T) {
	svm := mustNew(t)
	payer := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 2*lamportsPerSol))

	// Minimal v0 transaction (no lookups needed for the wire format). The
	// amount is rent-exempt for a 0-byte account so the transfer succeeds.
	blockhash, err := svm.LatestBlockhash()
	require.NoError(t, err)
	ix := transferIx(payer.PublicKey(), solana.NewWallet().PublicKey(), lamportsPerSol/4)
	tx, err := solana.NewTransaction([]solana.Instruction{ix}, blockhash,
		solana.TransactionPayer(payer.PublicKey()))
	require.NoError(t, err)
	_, err = tx.Message.SetVersion(solana.MessageVersionV0)
	require.NoError(t, err)
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(payer.PublicKey()) {
			k := payer.PrivateKey
			return &k
		}
		return nil
	})
	require.NoError(t, err)
	v0Bytes, err := tx.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, byte(0x80), v0Bytes[1+64]&0x80, "test tx must be v0 on the wire")

	_, err = svm.SendLegacyTransaction(v0Bytes)
	require.Error(t, err, "SendLegacyTransaction must reject v0 bytes")
	_, err = svm.SimulateLegacyTransaction(v0Bytes)
	require.Error(t, err, "SimulateLegacyTransaction must reject v0 bytes")

	// The versioned entry points accept both formats.
	outcome, err := svm.SendVersionedTransaction(v0Bytes)
	require.NoError(t, err)
	require.True(t, outcome.IsOk(), "v0 via versioned entry failed: %s", outcome.Error())

	legacyBytes := signedTransfer(t, svm, payer.PrivateKey, solana.NewWallet().PublicKey(), lamportsPerSol/2)
	outcome, err = svm.SendVersionedTransaction(legacyBytes)
	require.NoError(t, err)
	require.True(t, outcome.IsOk(), "legacy via versioned entry failed: %s", outcome.Error())
}

// TestSetAccountSysvarCoherence pins the address-keyed sysvar refresh:
// writing a sysvar ADDRESS refreshes the typed cache regardless of owner,
// and undecodable sysvar data is rejected without storing anything.
func TestSetAccountSysvarCoherence(t *testing.T) {
	svm := mustNew(t)

	// Valid clock data under a non-sysvar owner still refreshes the cache.
	clock, err := svm.Clock()
	require.NoError(t, err)
	clock.Slot = 777
	data, err := clock.MarshalBinary()
	require.NoError(t, err)
	require.NoError(t, svm.SetAccount(solana.SysVarClockPubkey, &solana.Account{
		Lamports: 1,
		Data:     data,
		Owner:    solana.SystemProgramID, // wrong owner on purpose
	}))
	got, err := svm.Clock()
	require.NoError(t, err)
	assert.Equal(t, uint64(777), got.Slot, "clock cache must refresh on address match")

	// Undecodable sysvar data is rejected and nothing changes.
	err = svm.SetAccount(solana.SysVarClockPubkey, &solana.Account{
		Lamports: 1,
		Data:     []byte{1, 2},
		Owner:    solana.SysVarClockPubkey,
	})
	require.Error(t, err, "undecodable sysvar data must be rejected")
	got, err = svm.Clock()
	require.NoError(t, err)
	assert.Equal(t, uint64(777), got.Slot, "rejected write must not change the account")
}
