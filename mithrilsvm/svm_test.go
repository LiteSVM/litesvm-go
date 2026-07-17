package mithrilsvm

import (
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LiteSVM/litesvm-go/mithrilsvm/elf"
)

func mustNew(t *testing.T) *LiteSVM {
	t.Helper()
	svm, err := New()
	require.NoError(t, err)
	return svm
}

func signedTransfer(t *testing.T, svm *LiteSVM, from solana.PrivateKey, to solana.PublicKey, lamports uint64) []byte {
	t.Helper()
	blockhash, err := svm.LatestBlockhash()
	require.NoError(t, err)
	ix := system.NewTransferInstruction(lamports, from.PublicKey(), to).Build()
	tx, err := solana.NewTransaction([]solana.Instruction{ix}, blockhash,
		solana.TransactionPayer(from.PublicKey()))
	require.NoError(t, err)
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(from.PublicKey()) {
			return &from
		}
		return nil
	})
	require.NoError(t, err)
	b, err := tx.MarshalBinary()
	require.NoError(t, err)
	return b
}

func TestAirdropAndBalance(t *testing.T) {
	svm := mustNew(t)
	w := solana.NewWallet()

	require.NoError(t, svm.Airdrop(w.PublicKey(), 5*lamportsPerSol))

	bal, exists, err := svm.Balance(w.PublicKey())
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, uint64(5*lamportsPerSol), bal)

	_, exists, err = svm.Balance(solana.NewWallet().PublicKey())
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestTransferSendAndHistory(t *testing.T) {
	svm := mustNew(t)
	payer := solana.NewWallet()
	recipient := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 2*lamportsPerSol))

	txBytes := signedTransfer(t, svm, payer.PrivateKey, recipient.PublicKey(), lamportsPerSol)
	outcome, err := svm.SendLegacyTransaction(txBytes)
	require.NoError(t, err)
	require.True(t, outcome.IsOk(), "transfer failed: %s", outcome.Error())
	assert.Equal(t, uint64(5000), outcome.Fee())
	assert.Equal(t, uint64(150), outcome.ComputeUnits())

	bal, _, err := svm.Balance(recipient.PublicKey())
	require.NoError(t, err)
	assert.Equal(t, uint64(lamportsPerSol), bal)

	payerBal, _, err := svm.Balance(payer.PublicKey())
	require.NoError(t, err)
	assert.Equal(t, uint64(lamportsPerSol-5000), payerBal)

	// History records the outcome; a duplicate send is rejected.
	recorded := svm.GetTransaction(outcome.Signature())
	require.NotNil(t, recorded)
	assert.True(t, recorded.IsOk())

	dup, err := svm.SendLegacyTransaction(txBytes)
	require.NoError(t, err)
	assert.False(t, dup.IsOk())
	assert.Contains(t, dup.Error(), "AlreadyProcessed")
}

func TestSimulateDoesNotCommit(t *testing.T) {
	svm := mustNew(t)
	payer := solana.NewWallet()
	recipient := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 2*lamportsPerSol))

	txBytes := signedTransfer(t, svm, payer.PrivateKey, recipient.PublicKey(), lamportsPerSol)
	outcome, err := svm.SimulateLegacyTransaction(txBytes)
	require.NoError(t, err)
	require.True(t, outcome.IsOk(), "simulate failed: %s", outcome.Error())

	post, err := outcome.PostAccounts()
	require.NoError(t, err)
	assert.NotEmpty(t, post)

	_, exists, err := svm.Balance(recipient.PublicKey())
	require.NoError(t, err)
	assert.False(t, exists, "simulation must not commit state")
}

func TestSigverifyToggle(t *testing.T) {
	svm := mustNew(t)
	payer := solana.NewWallet()
	recipient := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 2*lamportsPerSol))

	txBytes := signedTransfer(t, svm, payer.PrivateKey, recipient.PublicKey(), lamportsPerSol)
	// Corrupt the signature.
	corrupted := append([]byte(nil), txBytes...)
	corrupted[2] ^= 0xff

	outcome, err := svm.SendLegacyTransaction(corrupted)
	require.NoError(t, err)
	assert.False(t, outcome.IsOk())
	assert.Contains(t, outcome.Error(), "SignatureFailure")

	require.NoError(t, svm.SetSigverify(false))
	outcome, err = svm.SendLegacyTransaction(corrupted)
	require.NoError(t, err)
	assert.True(t, outcome.IsOk(), "with sigverify off the corrupted tx must pass: %s", outcome.Error())
}

func TestExpireBlockhashAndCheckToggle(t *testing.T) {
	svm := mustNew(t)
	payer := solana.NewWallet()
	recipient := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 3*lamportsPerSol))

	txBytes := signedTransfer(t, svm, payer.PrivateKey, recipient.PublicKey(), lamportsPerSol)

	before, err := svm.LatestBlockhash()
	require.NoError(t, err)
	require.NoError(t, svm.ExpireBlockhash())
	after, err := svm.LatestBlockhash()
	require.NoError(t, err)
	assert.NotEqual(t, before, after)

	outcome, err := svm.SendLegacyTransaction(txBytes)
	require.NoError(t, err)
	assert.False(t, outcome.IsOk())
	assert.Contains(t, outcome.Error(), "BlockhashNotFound")

	require.NoError(t, svm.SetBlockhashCheck(false))
	outcome, err = svm.SendLegacyTransaction(txBytes)
	require.NoError(t, err)
	assert.True(t, outcome.IsOk(), "with blockhash check off the expired tx must pass: %s", outcome.Error())
}

func TestWarpAndClock(t *testing.T) {
	svm := mustNew(t)
	require.NoError(t, svm.WarpToSlot(1234))

	clock, err := svm.Clock()
	require.NoError(t, err)
	assert.Equal(t, uint64(1234), clock.Slot)

	clock.UnixTimestamp = 1_700_000_000
	require.NoError(t, svm.SetClock(clock))
	clock2, err := svm.Clock()
	require.NoError(t, err)
	assert.Equal(t, int64(1_700_000_000), clock2.UnixTimestamp)
	assert.Equal(t, uint64(1234), clock2.Slot)
}

func TestRentAndMinimumBalance(t *testing.T) {
	svm := mustNew(t)
	min, err := svm.MinimumBalanceForRentExemption(0)
	require.NoError(t, err)
	assert.Equal(t, uint64(890880), min) // agave default rent, 0-byte account

	// Rust litesvm's mainnet feature set activates
	// deprecate_rent_exemption_threshold, so the raw sysvar carries
	// 6960 / 1.0 instead of the classic 3480 / 2.0; minimum_balance
	// results are identical.
	rent, err := svm.Rent()
	require.NoError(t, err)
	assert.Equal(t, uint64(6960), rent.LamportsPerByte)
	assert.Equal(t, 1.0, rent.ExemptionThreshold)
}

func TestGetSetAccount(t *testing.T) {
	svm := mustNew(t)
	pk := solana.NewWallet().PublicKey()
	require.Nil(t, svm.GetAccount(pk))

	acct := &solana.Account{
		Lamports: 42,
		Data:     []byte{1, 2, 3},
		Owner:    solana.SystemProgramID,
	}
	require.NoError(t, svm.SetAccount(pk, acct))

	got := svm.GetAccount(pk)
	require.NotNil(t, got)
	assert.Equal(t, uint64(42), got.Lamports)
	assert.Equal(t, []byte{1, 2, 3}, got.Data)
}

// TestMemoProgramExecutes proves end-to-end BPF execution in the pure-Go
// engine: the vendored SPL Memo v3 ELF runs under loader v2 and logs the
// memo string.
func TestMemoProgramExecutes(t *testing.T) {
	svm := mustNew(t)
	require.NoError(t, svm.SetDefaultPrograms())

	payer := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 2*lamportsPerSol))

	memoProgram := elf.Defaults[3].ProgramID // Memo v3 (MemoSq4...)
	blockhash, err := svm.LatestBlockhash()
	require.NoError(t, err)

	ix := solana.NewInstruction(memoProgram, solana.AccountMetaSlice{}, []byte("hello from mithrilsvm"))
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
	require.True(t, outcome.IsOk(), "memo tx failed: %s logs=%v", outcome.Error(), outcome.Logs())
	assert.Greater(t, outcome.ComputeUnits(), uint64(150))

	joined := strings.Join(outcome.Logs(), "\n")
	assert.Contains(t, joined, "hello from mithrilsvm")
}

// TestInstanceIsolation runs two engines side by side: state is fully
// isolated even though both instances share the deterministic genesis
// blockhash (sha256("genesis"), like Rust litesvm), and expiring one
// instance's blockhash does not affect the other.
func TestInstanceIsolation(t *testing.T) {
	svmA := mustNew(t)
	svmB := mustNew(t)

	payer := solana.NewWallet()
	require.NoError(t, svmA.Airdrop(payer.PublicKey(), 2*lamportsPerSol))
	require.NoError(t, svmB.Airdrop(payer.PublicKey(), 2*lamportsPerSol))

	recipient := solana.NewWallet()
	txA := signedTransfer(t, svmA, payer.PrivateKey, recipient.PublicKey(), lamportsPerSol)

	// Expire B's blockhash: A's tx (built on the shared genesis hash) must
	// now be rejected by B but still accepted by A.
	require.NoError(t, svmB.ExpireBlockhash())
	outcome, err := svmB.SendLegacyTransaction(txA)
	require.NoError(t, err)
	assert.False(t, outcome.IsOk(), "instance B must not accept an expired blockhash")
	assert.Contains(t, outcome.Error(), "BlockhashNotFound")

	outcome, err = svmA.SendLegacyTransaction(txA)
	require.NoError(t, err)
	assert.True(t, outcome.IsOk(), "instance A must accept its own tx: %s", outcome.Error())

	// A's send must not leak into B.
	_, exists, err := svmB.Balance(recipient.PublicKey())
	require.NoError(t, err)
	assert.False(t, exists, "state leaked between instances")
	balA, _, err := svmA.Balance(recipient.PublicKey())
	require.NoError(t, err)
	assert.Equal(t, uint64(lamportsPerSol), balA)
}
