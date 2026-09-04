package litesvm

import (
	"encoding/binary"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LiteSVM/litesvm-go/internal/elf"
)

const (
	splMintLen         = 82
	splTokenAccountLen = 165
)

// signAndSend builds a legacy transaction from instrs, signs it with the
// given signers (payer first), and submits it. Extra solana.TransactionOption
// values (e.g. address tables) are forwarded to the builder.
func signAndSend(t *testing.T, svm *LiteSVM, payer solana.PrivateKey, instrs []solana.Instruction, extraSigners ...solana.PrivateKey) *TxOutcome {
	t.Helper()
	blockhash, err := svm.LatestBlockhash()
	require.NoError(t, err)
	tx, err := solana.NewTransaction(instrs, blockhash, solana.TransactionPayer(payer.PublicKey()))
	require.NoError(t, err)
	signers := append([]solana.PrivateKey{payer}, extraSigners...)
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		for i := range signers {
			if key.Equals(signers[i].PublicKey()) {
				return &signers[i]
			}
		}
		return nil
	})
	require.NoError(t, err)
	b, err := tx.MarshalBinary()
	require.NoError(t, err)
	outcome, err := svm.SendLegacyTransaction(b)
	require.NoError(t, err)
	return outcome
}

// requireTxOK asserts the transaction executed successfully and burned
// compute units.
//
// It does NOT assert non-empty logs: mithril's log recorder only captures
// sol_log* syscall output (pkg/sealevel/syscalls_log.go is the only writer),
// never Agave's "Program X invoke [N]"/"Program X success" framing lines, and
// p-token emits no sol_log messages on success. Tests assert logs where the
// program actually logs (memo output, p-token error paths).
func requireTxOK(t *testing.T, outcome *TxOutcome, what string) {
	t.Helper()
	require.True(t, outcome.IsOk(), "%s failed: %s logs=%v", what, outcome.Error(), outcome.Logs())
	assert.Greater(t, outcome.ComputeUnits(), uint64(0), "%s consumed no CU", what)
}

// tokenAmount decodes the u64 amount field of an SPL token account (offset
// 64 in the 165-byte layout: mint 0..32, owner 32..64, amount 64..72).
func tokenAmount(t *testing.T, svm *LiteSVM, addr solana.PublicKey) uint64 {
	t.Helper()
	acct := svm.GetAccount(addr)
	require.NotNil(t, acct, "token account %s missing", addr)
	require.Len(t, acct.Data, splTokenAccountLen)
	return binary.LittleEndian.Uint64(acct.Data[64:72])
}

// TestTokenE2E drives the vendored p-token ELF (installed at the spl-token
// program id by SetDefaultPrograms) through a full mint lifecycle:
// InitializeMint2, two InitializeAccount3, MintTo, and Transfer, asserting
// account state after each step.
func TestTokenE2E(t *testing.T) {
	svm := mustNew(t)
	require.NoError(t, svm.SetDefaultPrograms())
	require.NoError(t, svm.WithNativeMints())

	tokenProgram := elf.Defaults[0].ProgramID // Tokenkeg... backed by p-token
	require.True(t, tokenProgram.Equals(solana.TokenProgramID))

	payer := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 10*lamportsPerSol))

	mint := solana.NewWallet()
	alice := solana.NewWallet()
	bob := solana.NewWallet()
	acctA := solana.NewWallet()
	acctB := solana.NewWallet()

	mintRent, err := svm.MinimumBalanceForRentExemption(splMintLen)
	require.NoError(t, err)
	tokenRent, err := svm.MinimumBalanceForRentExemption(splTokenAccountLen)
	require.NoError(t, err)

	// Tx 1: create the mint account and initialize it (InitializeMint2, no
	// rent sysvar account meta needed).
	const decimals = uint8(6)
	initMint, err := token.NewInitializeMint2InstructionBuilder().
		SetDecimals(decimals).
		SetMintAuthority(payer.PublicKey()).
		SetMintAccount(mint.PublicKey()).
		ValidateAndBuild()
	require.NoError(t, err)
	outcome := signAndSend(t, svm, payer.PrivateKey, []solana.Instruction{
		system.NewCreateAccountInstruction(mintRent, splMintLen, tokenProgram,
			payer.PublicKey(), mint.PublicKey()).Build(),
		initMint,
	}, mint.PrivateKey)
	requireTxOK(t, outcome, "create+init mint")

	mintAcct := svm.GetAccount(mint.PublicKey())
	require.NotNil(t, mintAcct)
	assert.Equal(t, tokenProgram, mintAcct.Owner)
	require.Len(t, mintAcct.Data, splMintLen)
	assert.Equal(t, decimals, mintAcct.Data[44], "mint decimals")
	assert.Equal(t, uint8(1), mintAcct.Data[45], "mint is_initialized")

	// Tx 2: create and initialize both token accounts (InitializeAccount3
	// carries the owner in instruction data; no rent sysvar meta needed).
	initA, err := token.NewInitializeAccount3Instruction(
		alice.PublicKey(), acctA.PublicKey(), mint.PublicKey()).ValidateAndBuild()
	require.NoError(t, err)
	initB, err := token.NewInitializeAccount3Instruction(
		bob.PublicKey(), acctB.PublicKey(), mint.PublicKey()).ValidateAndBuild()
	require.NoError(t, err)
	outcome = signAndSend(t, svm, payer.PrivateKey, []solana.Instruction{
		system.NewCreateAccountInstruction(tokenRent, splTokenAccountLen, tokenProgram,
			payer.PublicKey(), acctA.PublicKey()).Build(),
		initA,
		system.NewCreateAccountInstruction(tokenRent, splTokenAccountLen, tokenProgram,
			payer.PublicKey(), acctB.PublicKey()).Build(),
		initB,
	}, acctA.PrivateKey, acctB.PrivateKey)
	requireTxOK(t, outcome, "create+init token accounts")

	for _, addr := range []solana.PublicKey{acctA.PublicKey(), acctB.PublicKey()} {
		acct := svm.GetAccount(addr)
		require.NotNil(t, acct)
		assert.Equal(t, tokenProgram, acct.Owner)
		require.Len(t, acct.Data, splTokenAccountLen)
		assert.Equal(t, mint.PublicKey().Bytes(), acct.Data[0:32], "token account mint")
	}
	aliceAcct := svm.GetAccount(acctA.PublicKey())
	assert.Equal(t, alice.PublicKey().Bytes(), aliceAcct.Data[32:64], "token account owner")
	assert.Zero(t, tokenAmount(t, svm, acctA.PublicKey()))

	// Tx 3: mint 1_000_000 base units to Alice's account.
	const minted = uint64(1_000_000)
	mintTo, err := token.NewMintToInstruction(minted, mint.PublicKey(),
		acctA.PublicKey(), payer.PublicKey(), nil).ValidateAndBuild()
	require.NoError(t, err)
	outcome = signAndSend(t, svm, payer.PrivateKey, []solana.Instruction{mintTo})
	requireTxOK(t, outcome, "mint to")

	assert.Equal(t, minted, tokenAmount(t, svm, acctA.PublicKey()))
	mintAcct = svm.GetAccount(mint.PublicKey())
	assert.Equal(t, minted, binary.LittleEndian.Uint64(mintAcct.Data[36:44]), "mint supply")

	// Tx 4: transfer 250_000 from Alice to Bob; Alice signs as owner.
	const moved = uint64(250_000)
	transfer, err := token.NewTransferInstruction(moved, acctA.PublicKey(),
		acctB.PublicKey(), alice.PublicKey(), nil).ValidateAndBuild()
	require.NoError(t, err)
	outcome = signAndSend(t, svm, payer.PrivateKey, []solana.Instruction{transfer}, alice.PrivateKey)
	requireTxOK(t, outcome, "token transfer")

	assert.Equal(t, minted-moved, tokenAmount(t, svm, acctA.PublicKey()))
	assert.Equal(t, moved, tokenAmount(t, svm, acctB.PublicKey()))
}

// TestTokenTransferInsufficientFunds asserts the p-token program surfaces
// its custom error (1 = InsufficientFunds) through the outcome mapping.
func TestTokenTransferInsufficientFunds(t *testing.T) {
	svm := mustNew(t)
	require.NoError(t, svm.SetDefaultPrograms())

	tokenProgram := elf.Defaults[0].ProgramID
	payer := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 10*lamportsPerSol))

	mint := solana.NewWallet()
	alice := solana.NewWallet()
	acctA := solana.NewWallet()
	acctB := solana.NewWallet()

	mintRent, err := svm.MinimumBalanceForRentExemption(splMintLen)
	require.NoError(t, err)
	tokenRent, err := svm.MinimumBalanceForRentExemption(splTokenAccountLen)
	require.NoError(t, err)

	initMint, err := token.NewInitializeMint2InstructionBuilder().
		SetDecimals(0).
		SetMintAuthority(payer.PublicKey()).
		SetMintAccount(mint.PublicKey()).
		ValidateAndBuild()
	require.NoError(t, err)
	initA, err := token.NewInitializeAccount3Instruction(
		alice.PublicKey(), acctA.PublicKey(), mint.PublicKey()).ValidateAndBuild()
	require.NoError(t, err)
	initB, err := token.NewInitializeAccount3Instruction(
		alice.PublicKey(), acctB.PublicKey(), mint.PublicKey()).ValidateAndBuild()
	require.NoError(t, err)
	outcome := signAndSend(t, svm, payer.PrivateKey, []solana.Instruction{
		system.NewCreateAccountInstruction(mintRent, splMintLen, tokenProgram,
			payer.PublicKey(), mint.PublicKey()).Build(),
		initMint,
		system.NewCreateAccountInstruction(tokenRent, splTokenAccountLen, tokenProgram,
			payer.PublicKey(), acctA.PublicKey()).Build(),
		initA,
		system.NewCreateAccountInstruction(tokenRent, splTokenAccountLen, tokenProgram,
			payer.PublicKey(), acctB.PublicKey()).Build(),
		initB,
	}, mint.PrivateKey, acctA.PrivateKey, acctB.PrivateKey)
	requireTxOK(t, outcome, "setup mint and accounts")

	// Transfer from an empty account: p-token custom error 1.
	transfer, err := token.NewTransferInstruction(1, acctA.PublicKey(),
		acctB.PublicKey(), alice.PublicKey(), nil).ValidateAndBuild()
	require.NoError(t, err)
	outcome = signAndSend(t, svm, payer.PrivateKey, []solana.Instruction{transfer}, alice.PrivateKey)
	require.False(t, outcome.IsOk())
	assert.Contains(t, outcome.Error(), "Custom", "expected a custom program error, got: %s", outcome.Error())
	// p-token sol_logs its error before returning it.
	assert.NotEmpty(t, outcome.Logs(), "failed token transfer produced no logs")
	assert.Zero(t, tokenAmount(t, svm, acctB.PublicKey()))
}
